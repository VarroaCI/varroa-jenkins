package main

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

func init() {
	rootRegistrars = append(rootRegistrars, func(root *cobra.Command) {
		uploadCmd := lookupCommand(root, "upload")
		if uploadCmd == nil {
			uploadCmd = newVerbParent("upload", "Push an artifact into Varroa")
			root.AddCommand(uploadCmd)
		}
		uploadCmd.AddCommand(newUploadPluginCmd())
	})
}

func newUploadPluginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin <file.hpi>",
		Short: "Upload a Jenkins plugin into the update center",
		Long: `plugin pushes a local .hpi/.jpi into the update center.

The upload goes through the BFF with your API key, NOT through uc:// and
VARROACTL_UC_TOKEN like ` + "`varroactl import`" + `. That is deliberate: the shared
import token is unattributable, and an upload has to be recorded against a real
user. It requires the updatecenter/upload verb.

The plugin's transitive MANDATORY dependency closure is resolved and validated
before any byte is committed. Dependencies the declared plugin set already pins
are never fetched — a second writer choosing its own version would shadow the
declaration. Optional dependencies are reported and never resolved.

--dry-run runs the same resolution and prints the plan without storing anything.

Note that storing a plugin is not installing it: the served update-center
metadata carries no dependency list, so a bundle installing an uploaded plugin
must enumerate the closure this command prints.`,
		Args: cobra.ExactArgs(1),
		RunE: runUploadPlugin,
	}
	cmd.Flags().Bool("dry-run", false, "resolve and validate the closure without storing anything")
	return cmd
}

// uploadEnvelope is the subset of the response this command renders. The full
// body is printed verbatim under -o json, so this only has to cover the table.
type uploadEnvelope struct {
	Plugin struct {
		Name         string `json:"name"`
		Version      string `json:"version"`
		SHA256       string `json:"sha256"`
		RequiredCore string `json:"requiredCore"`
	} `json:"plugin"`
	DryRun  bool   `json:"dryRun"`
	PackRef string `json:"packRef"`
	Closure []struct {
		Name            string `json:"name"`
		Min             string `json:"min"`
		Status          string `json:"status"`
		ResolvedVersion string `json:"resolvedVersion"`
		Source          string `json:"source"`
		Fetched         bool   `json:"fetched"`
	} `json:"closure"`
	OptionalDependencies []struct {
		Name string `json:"name"`
		Min  string `json:"min"`
	} `json:"optionalDependencies"`
	Warnings []struct {
		Code    string `json:"code"`
		Plugin  string `json:"plugin"`
		Message string `json:"message"`
	} `json:"warnings"`

	// Rejection fields.
	Error      string `json:"error"`
	Message    string `json:"message"`
	Unresolved []struct {
		Name          string  `json:"name"`
		Min           string  `json:"min"`
		Reason        string  `json:"reason"`
		FoundInStore  *string `json:"foundInStore"`
		FoundDeclared *string `json:"foundDeclared"`
		FoundUpstream *string `json:"foundUpstream"`
		Remediation   string  `json:"remediation"`
	} `json:"unresolved"`
}

func runUploadPlugin(cmd *cobra.Command, args []string) error {
	path := args[0]
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	// Resolve the context (and therefore the API key) BEFORE opening the file,
	// so an unconfigured CLI fails without touching the filesystem or the
	// network.
	c, err := apiClient(cmd)
	if err != nil {
		return err
	}

	f, err := os.Open(path) // #nosec G304 -- operator-supplied path on an operator-run CLI
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	// Stream the multipart encoding through a pipe: a plugin can be hundreds of
	// megabytes and there is no reason to hold it in memory.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, cerr := mw.CreateFormFile("file", filepath.Base(path))
		if cerr != nil {
			_ = pw.CloseWithError(cerr)
			return
		}
		if _, cerr = io.Copy(part, f); cerr != nil {
			_ = pw.CloseWithError(cerr)
			return
		}
		if cerr = mw.Close(); cerr != nil {
			_ = pw.CloseWithError(cerr)
			return
		}
		_ = pw.Close()
	}()

	params := &client.UploadUpdateCenterPluginParams{}
	if dryRun {
		params.DryRun = &dryRun
	}

	resp, err := c.UploadUpdateCenterPluginWithBody(cmd.Context(), params, mw.FormDataContentType(), pr)
	if err != nil {
		return fmt.Errorf("upload %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read upload response: %w", err)
	}

	if o, _ := cmd.Flags().GetString("output"); o == "json" {
		// The envelope is printed verbatim, rejection or not, so a caller
		// scripting against it sees exactly what the server said.
		if !json.Valid(body) {
			return errFromResponse(body, resp.StatusCode)
		}
		if _, werr := cmd.OutOrStdout().Write(append(body, '\n')); werr != nil {
			return werr
		}
		if resp.StatusCode >= 300 {
			return uploadRejectedError(resp.StatusCode, env0(body))
		}
		return nil
	}

	var env uploadEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return errFromResponse(body, resp.StatusCode)
	}

	if resp.StatusCode >= 300 {
		printUploadRejection(cmd.ErrOrStderr(), resp.StatusCode, env)
		return uploadRejectedError(resp.StatusCode, env)
	}

	printUploadSuccess(cmd.OutOrStdout(), env)
	return nil
}

func printUploadSuccess(w io.Writer, env uploadEnvelope) {
	verb := "Uploaded"
	if env.DryRun {
		verb = "Previewed"
	}
	fmt.Fprintf(w, "%s %s@%s\n", verb, env.Plugin.Name, env.Plugin.Version)
	if env.PackRef != "" {
		fmt.Fprintf(w, "  pack: %s\n", env.PackRef)
	}
	fmt.Fprintf(w, "  sha256: %s\n", env.Plugin.SHA256)

	if len(env.Closure) > 0 {
		fetched := 0
		for _, c := range env.Closure {
			if c.Fetched {
				fetched++
			}
		}
		if env.DryRun {
			fmt.Fprintf(w, "\nClosure: %d mandatory dependencies, %d would be downloaded\n", len(env.Closure), fetched)
		} else {
			fmt.Fprintf(w, "\nClosure: %d mandatory dependencies, %d downloaded\n", len(env.Closure), fetched)
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "DEPENDENCY\tMINIMUM\tSTATUS\tRESOLVED\tSOURCE")
		for _, c := range env.Closure {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.Name, c.Min, c.Status, c.ResolvedVersion, c.Source)
		}
		_ = tw.Flush()
	}

	if len(env.OptionalDependencies) > 0 {
		fmt.Fprintf(w, "\nOptional dependencies (not resolved):\n")
		for _, o := range env.OptionalDependencies {
			fmt.Fprintf(w, "  %s >= %s\n", o.Name, o.Min)
		}
	}

	if len(env.Warnings) > 0 {
		fmt.Fprintf(w, "\nWarnings:\n")
		for _, warn := range env.Warnings {
			fmt.Fprintf(w, "  [%s] %s: %s\n", warn.Code, warn.Plugin, warn.Message)
		}
	}
}

func printUploadRejection(w io.Writer, status int, env uploadEnvelope) {
	code := env.Error
	if code == "" {
		code = http.StatusText(status)
	}
	fmt.Fprintf(w, "upload rejected (%d %s)\n", status, code)
	if env.Message != "" {
		fmt.Fprintf(w, "%s\n", env.Message)
	}
	if len(env.Unresolved) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "DEPENDENCY\tMINIMUM\tREASON\tIN STORE\tDECLARED\tUPSTREAM")
	for _, u := range env.Unresolved {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			u.Name, u.Min, u.Reason, derefOrDash(u.FoundInStore), derefOrDash(u.FoundDeclared), derefOrDash(u.FoundUpstream))
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintln(w)
	for _, u := range env.Unresolved {
		if u.Remediation != "" {
			fmt.Fprintf(w, "  %s: %s\n", u.Name, u.Remediation)
		}
	}
}

func derefOrDash(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}

// uploadRejectedError is the non-zero exit for a rejected upload. The detail is
// already on stderr (or, under -o json, on stdout verbatim), so this is a short
// one-line summary rather than a second rendering.
func uploadRejectedError(status int, env uploadEnvelope) error {
	code := env.Error
	if code == "" {
		code = http.StatusText(status)
	}
	return fmt.Errorf("upload rejected (%d %s)", status, code)
}

// env0 decodes just enough of a body to name the rejection, for the -o json
// path where the body was printed verbatim rather than parsed.
func env0(body []byte) uploadEnvelope {
	var env uploadEnvelope
	_ = json.Unmarshal(body, &env)
	return env
}
