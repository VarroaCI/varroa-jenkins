package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

// Injectable editor invocation for tests.
var runEditor = func(path string) error {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		if runtime.GOOS == "windows" {
			editor = "notepad"
		} else {
			editor = "vi"
		}
	}
	cmd := exec.Command(editor, path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runEditController implements the edit flow per design §8.1.
func runEditController(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		return usagef("NS/NAME is required")
	}
	c, err := apiClient(cmd)
	if err != nil {
		return err
	}
	nFlag, _ := cmd.Flags().GetString("namespace")
	cFlag, _ := cmd.Flags().GetString("cluster")
	rc, err := resolveContext(func(name string) string {
		f := cmd.Flag(name)
		if f == nil {
			return ""
		}
		return f.Value.String()
	})
	if err != nil {
		return err
	}
	ns, name, err := resolveNSName(args[0], nFlag, rc.defaultNamespace)
	if err != nil {
		return err
	}
	cluster := resolveCluster(cFlag, rc.defaultCluster)

	// Fetch server-rendered CR YAML (same call as 6.2 -o yaml)
	raw, err := fetchServerYAML(cmd.Context(), c, cluster, ns, name)
	if err != nil {
		return err
	}

	editHeader := "# Please edit the object below. Lines starting with '#' will be ignored.\n" +
		"# An empty patch (no effective change) cancels the edit. status edits are ignored.\n#\n"

	tmp, err := os.CreateTemp("", "varroactl-edit-*.yaml")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(editHeader + raw); err != nil {
		_ = tmp.Close()
		return err
	}
	_ = tmp.Close()

	// Run editor
	if err := runEditor(tmpPath); err != nil {
		return fmt.Errorf("editor failed: %w", err)
	}

	// Read edited file and strip hash comments
	editedBytes, err := os.ReadFile(tmpPath)
	if err != nil {
		return err
	}
	edited := stripHashComments(string(editedBytes))

	// Parse both original and edited
	var orig, mod map[string]any
	if err := yaml.Unmarshal([]byte(raw), &orig); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Error: failed to parse original YAML: %v\n", err)
		_, _ = fmt.Fprintf(os.Stderr, "Edits saved to %s\n", tmpPath)
		return fmt.Errorf("parse error")
	}
	if err := yaml.Unmarshal([]byte(edited), &mod); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Error: failed to parse edited YAML: %v\n", err)
		_, _ = fmt.Fprintf(os.Stderr, "Edits saved to %s\n", tmpPath)
		return fmt.Errorf("parse error")
	}

	// Warn if apiVersion/kind/status differ (ignored keys)
	warnIgnoredKeys(orig, mod)

	// Compute merge patch restricted to metadata and spec
	patch := map[string]any{}
	for _, k := range []string{"metadata", "spec"} {
		if p := diffMap(orig[k], mod[k]); p != nil {
			patch[k] = p
		}
	}

	if len(patch) == 0 {
		fmt.Println("Edit cancelled, no changes made.")
		_ = os.Remove(tmpPath)
		return nil
	}

	// Send PATCH
	patchJSON, err := yaml.Marshal(patch)
	if err != nil {
		return err
	}

	resp, err := c.PatchControllerWithBody(cmd.Context(),
		cluster,
		ns,
		name,
		&client.PatchControllerParams{},
		"application/json",
		strings.NewReader(string(patchJSON)),
	)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == 400 {
		apiErr := client.DecodeError(resp)
		_, _ = fmt.Fprintln(os.Stderr, apiErr.Error())
		_, _ = fmt.Fprintf(os.Stderr, "Edits saved to %s\n", tmpPath)
		return fmt.Errorf("patch rejected")
	}

	if resp.StatusCode >= 400 {
		apiErr := client.DecodeError(resp)
		return apiErrorf(apiErr)
	}

	_ = os.Remove(tmpPath)
	_, _ = fmt.Fprintf(os.Stdout, "controller %q edited\n", ns+"/"+name)
	return nil
}

// fetchServerYAML fetches the server-rendered CR YAML body (N4: raw application/yaml).
func fetchServerYAML(ctx context.Context, c *client.ClientWithResponses, cluster, ns, name string) (string, error) {
	resp, err := c.GetControllerYaml(ctx,
		cluster,
		ns,
		name,
	)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		apiErr := client.DecodeError(resp)
		return "", apiErrorf(apiErr)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func stripHashComments(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	for _, line := range lines {
		if strings.HasPrefix(line, "# ") || line == "#" {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// warnIgnoredKeys prints stderr warnings if apiVersion/kind/status differ.
func warnIgnoredKeys(orig, mod map[string]any) {
	for _, k := range []string{"apiVersion", "kind", "status"} {
		if !reflect.DeepEqual(orig[k], mod[k]) {
			_, _ = fmt.Fprintf(os.Stderr, "Warning: changes to %q are ignored (read-only field)\n", k)
		}
	}
}

// diffMap computes an RFC-7386-style merge patch between two map values.
// Returns nil if no differences.
func diffMap(a, b any) any {
	// Both nil → no change
	if a == nil && b == nil {
		return nil
	}
	// b is nil → deletion
	if b == nil {
		return map[string]any{} // nil value would be set at parent
	}

	am, aOK := a.(map[string]any)
	bm, bOK := b.(map[string]any)

	// Both maps → recurse
	if aOK && bOK {
		out := map[string]any{}
		// Keys in b
		for k, bv := range bm {
			av, exists := am[k]
			if !exists {
				out[k] = bv
			} else if sub := diffMap(av, bv); sub != nil {
				if subMap, ok := sub.(map[string]any); ok && len(subMap) > 0 {
					out[k] = sub
				} else if !ok {
					out[k] = sub
				}
			}
		}
		// Keys only in a → deletion (null)
		for k := range am {
			if _, exists := bm[k]; !exists {
				out[k] = nil
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}

	// Not both maps → compare directly
	if reflect.DeepEqual(a, b) {
		return nil
	}
	return b
}
