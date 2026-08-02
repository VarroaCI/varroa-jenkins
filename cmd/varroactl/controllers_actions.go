package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

func init() {
	registerNoun(addActionCommands)
}

// addActionCommands is a no-op registrar; we use registerRootCommand instead
// to attach controller subcommands to the existing action-verb parents.
func addActionCommands(v *verbCommands) {}

// We use registerRootCommand to find the action verb parents and attach
// controller subcommands.
func init() {
	registerRootCommand(func(root *cobra.Command) {
		for _, c := range root.Commands() {
			switch c.Name() {
			case "restart":
				ctrlCmd := newControllerSub(newRestartCmd, runRestart)
				addClusterFlag(ctrlCmd)
				c.AddCommand(ctrlCmd)
			case "reprovision":
				ctrlCmd := newControllerSub(newReprovisionCmd, runReprovision)
				addClusterFlag(ctrlCmd)
				c.AddCommand(ctrlCmd)
			case "reconcile":
				ctrlCmd := newControllerSub(newReconcileCmd, runReconcile)
				addClusterFlag(ctrlCmd)
				c.AddCommand(ctrlCmd)
			case "approve":
				ctrlCmd := newControllerSub(newApproveCmd, runApprove)
				addClusterFlag(ctrlCmd)
				c.AddCommand(ctrlCmd)
			case "diff":
				ctrlCmd := newControllerSub(newDiffCmd, runDiff)
				addClusterFlag(ctrlCmd)
				c.AddCommand(ctrlCmd)
			case "logs":
				// Replace placeholder
				c.RunE = runLogs
				addClusterFlag(c)
			// Remove any existing subcommands
			case "preflight":
				c.RunE = runPreflight
				c.Flags().StringP("file", "f", "", "YAML file (or - for stdin)")
				addClusterFlag(c)
			case "render":
				c.RunE = runRender
				c.Flags().StringP("file", "f", "", "YAML file (or - for stdin)")
				addClusterFlag(c)
			case "preview":
				ctrlCmd := &cobra.Command{
					Use:     "controller NS/NAME -f OVERLAY [--baseline live|base]",
					Aliases: []string{"controllers", "ctrl"},
					Short:   "Preview a controller overlay",
					RunE:    runPreview,
				}
				ctrlCmd.Flags().StringP("file", "f", "", "Overlay YAML file (or - for stdin)")
				ctrlCmd.Flags().String("baseline", "live", "Baseline: live (default) or base")
				_ = ctrlCmd.MarkFlagRequired("file")
				addClusterFlag(ctrlCmd)
				c.AddCommand(ctrlCmd)
			case "power":
				ctrlCmd := newControllerSub(newPowerCmd, runPower)
				addClusterFlag(ctrlCmd)
				c.AddCommand(ctrlCmd)
			}
		}
	})
}

// newControllerSub creates a "controller NS/NAME" subcommand using a builder.
func newControllerSub(buildFn func() *cobra.Command, runFn func(cmd *cobra.Command, args []string) error) *cobra.Command {
	cmd := buildFn()
	cmd.Use = "controller NS/NAME"
	cmd.Aliases = []string{"controllers", "ctrl"}
	cmd.RunE = runFn
	return cmd
}

// ---------------------------------------------------------------------------
// restart
// ---------------------------------------------------------------------------

func newRestartCmd() *cobra.Command {
	return &cobra.Command{
		Short: "Restart a controller (deletes the Jenkins pod)",
	}
}

func runRestart(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		return usagef("NS/NAME is required")
	}
	c, err := apiClient(cmd)
	if err != nil {
		return err
	}
	nFlag, _ := cmd.Flags().GetString("namespace")
	o, _ := cmd.Flags().GetString("output")
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

	resp, err := c.RestartController(cmd.Context(), cluster, ns, name)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return handleActionResponse(resp, fmt.Sprintf("controller %q restarting", ns+"/"+name), o)
}

// ---------------------------------------------------------------------------
// reprovision
// ---------------------------------------------------------------------------

func newReprovisionCmd() *cobra.Command {
	return &cobra.Command{Short: "Reprovision a controller"}
}

func runReprovision(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		return usagef("NS/NAME is required")
	}
	c, err := apiClient(cmd)
	if err != nil {
		return err
	}
	nFlag, _ := cmd.Flags().GetString("namespace")
	o, _ := cmd.Flags().GetString("output")
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

	resp, err := c.ReprovisionController(cmd.Context(),
		cluster,
		ns,
		name,
	)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return handleActionResponse(resp, fmt.Sprintf("controller %q reprovisioning", ns+"/"+name), o)
}

// ---------------------------------------------------------------------------
// reconcile
// ---------------------------------------------------------------------------

func newReconcileCmd() *cobra.Command {
	return &cobra.Command{Short: "Reconcile a controller"}
}

func runReconcile(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		return usagef("NS/NAME is required")
	}
	c, err := apiClient(cmd)
	if err != nil {
		return err
	}
	nFlag, _ := cmd.Flags().GetString("namespace")
	o, _ := cmd.Flags().GetString("output")
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

	resp, err := c.ReconcileController(cmd.Context(),
		cluster,
		ns,
		name,
	)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return handleActionResponse(resp, fmt.Sprintf("controller %q reconcile triggered", ns+"/"+name), o)
}

// ---------------------------------------------------------------------------
// approve [--action A] [--deletion PATH]
// ---------------------------------------------------------------------------

var validApproveActions = map[string]bool{
	"reload": true, "restart": true, "approve": true,
	"force": true, "force-restart": true, "plugin-roll": true,
}

func newApproveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Short: "Approve a pending action or deletion",
	}
	cmd.Flags().String("action", "approve", "Action: reload|restart|approve|force|force-restart|plugin-roll")
	cmd.Flags().String("deletion", "", "Approve deletion at PATH")
	return cmd
}

func runApprove(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		return usagef("NS/NAME is required")
	}
	c, err := apiClient(cmd)
	if err != nil {
		return err
	}
	nFlag, _ := cmd.Flags().GetString("namespace")
	action, _ := cmd.Flags().GetString("action")
	deletion, _ := cmd.Flags().GetString("deletion")
	o, _ := cmd.Flags().GetString("output")
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

	// --action and --deletion mutually exclusive
	hasActionOverride := action != "approve"
	if hasActionOverride && deletion != "" {
		return usagef("--action and --deletion are mutually exclusive")
	}

	if deletion != "" {
		body := client.ApproveControllerDeletionJSONRequestBody{
			Path: deletion,
		}
		r, err := c.ApproveControllerDeletionWithResponse(cmd.Context(),
			cluster,
			ns,
			name,
			body,
		)
		if err != nil {
			return err
		}
		if r.HTTPResponse.StatusCode >= 400 {
			return errFromResponse(r.Body, r.HTTPResponse.StatusCode)
		}
		if o == "json" {
			return printJSON(os.Stdout, r.JSON202)
		}
		_, _ = fmt.Fprintf(os.Stdout, "controller %q approved deletion at %s\n", ns+"/"+name, deletion)
		return nil
	}

	// Validate action
	if !validApproveActions[action] {
		return usagef("invalid action %q: must be reload, restart, approve, force, force-restart, or plugin-roll", action)
	}

	body := client.ApproveControllerJSONRequestBody{
		Action: client.ApproveControllerJSONBodyAction(action),
	}
	r, err := c.ApproveControllerWithResponse(cmd.Context(),
		cluster,
		ns,
		name,
		body,
	)
	if err != nil {
		return err
	}
	if r.HTTPResponse.StatusCode == 409 {
		return errFromResponse(r.Body, r.HTTPResponse.StatusCode)
	}
	if r.HTTPResponse.StatusCode >= 400 {
		return errFromResponse(r.Body, r.HTTPResponse.StatusCode)
	}
	if o == "json" {
		return printJSON(os.Stdout, r.JSON202)
	}
	_, _ = fmt.Fprintf(os.Stdout, "controller %q approved (action: %s)\n", ns+"/"+name, action)
	return nil
}

// ---------------------------------------------------------------------------
// diff
// ---------------------------------------------------------------------------

func newDiffCmd() *cobra.Command {
	return &cobra.Command{Short: "Diff a controller"}
}

func runDiff(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		return usagef("NS/NAME is required")
	}
	c, err := apiClient(cmd)
	if err != nil {
		return err
	}
	nFlag, _ := cmd.Flags().GetString("namespace")
	o, _ := cmd.Flags().GetString("output")
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

	r, err := c.GetControllerDiffWithResponse(cmd.Context(),
		cluster,
		ns,
		name,
	)
	if err != nil {
		return err
	}
	if r.HTTPResponse.StatusCode >= 400 {
		return errFromResponse(r.Body, r.HTTPResponse.StatusCode)
	}
	if o == "json" {
		return printJSON(os.Stdout, r.JSON200)
	}
	return printYAML(os.Stdout, r.JSON200)
}

// ---------------------------------------------------------------------------
// Top-level: logs (design §8 deliberate grammar exception)
// ---------------------------------------------------------------------------

func runLogs(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		return usagef("NS/NAME is required")
	}
	c, err := apiClient(cmd)
	if err != nil {
		return err
	}
	nFlag, _ := cmd.Flags().GetString("namespace")
	cFlag, _ := cmd.Flags().GetString("cluster")

	// Accept "logs NS/NAME" or "logs controller NS/NAME"
	arg := args[0]
	if arg == "controller" || arg == "controllers" || arg == "ctrl" {
		if len(args) < 2 {
			return usagef("NS/NAME is required")
		}
		arg = args[1]
	}

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
	ns, name, err := resolveNSName(arg, nFlag, rc.defaultNamespace)
	if err != nil {
		return err
	}
	cluster := resolveCluster(cFlag, rc.defaultCluster)

	r, err := c.GetControllerLogsWithResponse(cmd.Context(),
		cluster,
		ns,
		name,
		&client.GetControllerLogsParams{},
	)
	if err != nil {
		return err
	}
	if r.HTTPResponse.StatusCode >= 400 {
		return errFromResponse(r.Body, r.HTTPResponse.StatusCode)
	}
	if r.JSON200 == nil || len(r.JSON200.Items) == 0 {
		return nil
	}

	for _, entry := range r.JSON200.Items {
		// LogEntry: {timestamp, level, source, message} — no "line" field
		_, _ = fmt.Fprintf(os.Stdout, "%s %s\n", entry.Timestamp.Format("2006-01-02T15:04:05Z07:00"), entry.Message)
	}
	return nil
}

// ---------------------------------------------------------------------------
// preflight -n NS -f FILE|-
func runPreflight(cmd *cobra.Command, args []string) error {
	nFlag, _ := cmd.Flags().GetString("namespace")
	fileFlag, _ := cmd.Flags().GetString("file")
	cFlag, _ := cmd.Flags().GetString("cluster")
	if fileFlag == "" {
		return usagef("-f FILE|- is required")
	}
	if nFlag == "" {
		return usagef("-n NS is required")
	}
	c, err := apiClient(cmd)
	if err != nil {
		return err
	}

	rc, cerr := resolveContext(func(name string) string {
		f := cmd.Flag(name)
		if f == nil {
			return ""
		}
		return f.Value.String()
	})
	if cerr != nil {
		return cerr
	}
	cluster := resolveCluster(cFlag, rc.defaultCluster)

	var data []byte
	if fileFlag == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(fileFlag)
	}
	if err != nil {
		return err
	}

	var cr map[string]interface{}
	if err := yaml.Unmarshal(data, &cr); err != nil {
		return fmt.Errorf("failed to parse YAML: %w", err)
	}
	jsonBody, err := json.Marshal(cr)
	if err != nil {
		return err
	}

	resp, err := c.PreflightControllerWithBody(cmd.Context(),
		cluster,
		nFlag,
		"application/json",
		strings.NewReader(string(jsonBody)),
	)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var checksResp struct {
			Error  string                          `json:"error"`
			Checks []client.ComponentsSchemasCheck `json:"checks"`
		}
		if err := json.Unmarshal(body, &checksResp); err == nil && len(checksResp.Checks) > 0 {
			renderChecksTable(checksResp.Checks)
			return fmt.Errorf("preflight failed")
		}
		apiErr := client.DecodeError(resp)
		return apiErrorf(apiErr)
	}

	var preflightResp struct {
		Checks []client.ComponentsSchemasCheck `json:"checks"`
	}
	if err := json.Unmarshal(body, &preflightResp); err == nil && len(preflightResp.Checks) > 0 {
		renderChecksTable(preflightResp.Checks)
		for _, ch := range preflightResp.Checks {
			if ch.Status == "fail" {
				return fmt.Errorf("preflight failed")
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// render -n NS -f FILE|- (raw YAML body → stdout)
func runRender(cmd *cobra.Command, args []string) error {
	nFlag, _ := cmd.Flags().GetString("namespace")
	fileFlag, _ := cmd.Flags().GetString("file")
	cFlag, _ := cmd.Flags().GetString("cluster")
	if fileFlag == "" {
		return usagef("-f FILE|- is required")
	}
	if nFlag == "" {
		return usagef("-n NS is required")
	}
	c, err := apiClient(cmd)
	if err != nil {
		return err
	}

	rc, cerr := resolveContext(func(name string) string {
		f := cmd.Flag(name)
		if f == nil {
			return ""
		}
		return f.Value.String()
	})
	if cerr != nil {
		return cerr
	}
	cluster := resolveCluster(cFlag, rc.defaultCluster)

	var data []byte
	if fileFlag == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(fileFlag)
	}
	if err != nil {
		return err
	}

	var cr map[string]interface{}
	if err := yaml.Unmarshal(data, &cr); err != nil {
		return fmt.Errorf("failed to parse YAML: %w", err)
	}
	jsonBody, err := json.Marshal(cr)
	if err != nil {
		return err
	}

	resp, err := c.RenderControllerWithBody(cmd.Context(),
		cluster,
		nFlag,
		"application/json",
		strings.NewReader(string(jsonBody)),
	)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		apiErr := client.DecodeError(resp)
		return apiErrorf(apiErr)
	}
	body, _ := io.ReadAll(resp.Body)
	os.Stdout.Write(body)
	if len(body) > 0 && body[len(body)-1] != '\n' {
		_, _ = fmt.Fprintln(os.Stdout)
	}
	return nil
}

// ---------------------------------------------------------------------------
// preview controller NS/NAME -f OVERLAY [--baseline live|base]
// ---------------------------------------------------------------------------

func runPreview(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		return usagef("NS/NAME is required")
	}
	c, err := apiClient(cmd)
	if err != nil {
		return err
	}
	nFlag, _ := cmd.Flags().GetString("namespace")
	fileFlag, _ := cmd.Flags().GetString("file")
	baseline, _ := cmd.Flags().GetString("baseline")
	o, _ := cmd.Flags().GetString("output")
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

	var overlayData []byte
	if fileFlag == "-" {
		overlayData, err = io.ReadAll(os.Stdin)
	} else {
		overlayData, err = os.ReadFile(fileFlag)
	}
	if err != nil {
		return err
	}

	var overlay map[string]interface{}
	if err := yaml.Unmarshal(overlayData, &overlay); err != nil {
		return fmt.Errorf("failed to parse overlay YAML: %w", err)
	}

	body := map[string]any{
		"baseline": baseline,
	}
	if podOverrides, ok := overlay["podOverrides"].(map[string]interface{}); ok {
		body["podOverrides"] = podOverrides
	}
	if resourceOverlay, ok := overlay["resourceOverlay"].(map[string]interface{}); ok {
		body["resourceOverlay"] = resourceOverlay
	}
	if probes, ok := overlay["probes"].(map[string]interface{}); ok {
		body["probes"] = probes
	}

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return err
	}

	r, err := c.PreviewControllerWithBodyWithResponse(cmd.Context(),
		cluster,
		ns,
		name,
		"application/json",
		bytes.NewReader(bodyJSON),
	)
	if err != nil {
		return err
	}
	if r.HTTPResponse.StatusCode >= 400 {
		return errFromResponse(r.Body, r.HTTPResponse.StatusCode)
	}
	if o == "json" {
		return printJSON(os.Stdout, r.JSON200)
	}
	return printYAML(os.Stdout, r.JSON200)
}

// ---------------------------------------------------------------------------
// power {running, stopped, hibernated}
// ---------------------------------------------------------------------------

// validPowerStates maps the lowercase CLI argument to the CRD powerState value.
var validPowerStates = map[string]string{
	"running":    "Running",
	"stopped":    "Stopped",
	"hibernated": "Hibernated",
}

func newPowerCmd() *cobra.Command {
	return &cobra.Command{
		Short: "Set power state of a controller",
		Long: `Set the power state of a controller.

Accepts one of: running, stopped, hibernated.

Examples:
  varroactl power controller team-a/my-ctrl hibernated
  varroactl power controller team-a/my-ctrl running
  varroactl power controller team-a/my-ctrl stopped`,
		Args: cobra.ExactArgs(2),
	}
}

func runPower(cmd *cobra.Command, args []string) error {
	if len(args) < 2 {
		return usagef("usage: power controller NS/NAME {running,stopped,hibernated}")
	}
	c, err := apiClient(cmd)
	if err != nil {
		return err
	}
	nFlag, _ := cmd.Flags().GetString("namespace")
	o, _ := cmd.Flags().GetString("output")
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

	state := strings.ToLower(args[1])
	crdState, ok := validPowerStates[state]
	if !ok {
		allowed := make([]string, 0, len(validPowerStates))
		for s := range validPowerStates {
			allowed = append(allowed, s)
		}
		sort.Strings(allowed)
		return usagef("invalid power state %q: must be one of %s", state, strings.Join(allowed, ", "))
	}

	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"powerState": crdState,
		},
	}
	patchBody, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	resp, err := c.PatchControllerWithBodyWithResponse(cmd.Context(),
		cluster,
		ns,
		name,
		&client.PatchControllerParams{},
		"application/merge-patch+json",
		strings.NewReader(string(patchBody)),
	)
	if err != nil {
		return err
	}
	if resp.HTTPResponse.StatusCode >= 400 {
		return errFromResponse(resp.Body, resp.HTTPResponse.StatusCode)
	}

	msg := fmt.Sprintf("controller %q power state set to %s", ns+"/"+name, state)
	if o == "json" {
		return printJSON(os.Stdout, map[string]string{"status": msg})
	}
	_, _ = fmt.Fprintln(os.Stdout, msg)
	return nil
}

// ---------------------------------------------------------------------------
// handleActionResponse - common handler for 2xx/409 action responses
// ---------------------------------------------------------------------------

func handleActionResponse(resp *http.Response, successMsg, format string) error {
	if resp.StatusCode == 409 {
		body, _ := io.ReadAll(resp.Body)
		var errResp struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
			return fmt.Errorf("error from server (409): %s", errResp.Error)
		}
		return fmt.Errorf("error from server (409): %s", string(body))
	}
	if resp.StatusCode >= 400 {
		apiErr := client.DecodeError(resp)
		return apiErrorf(apiErr)
	}

	if format == "json" {
		body, _ := io.ReadAll(resp.Body)
		var raw any
		if json.Unmarshal(body, &raw) == nil {
			return printJSON(os.Stdout, raw)
		}
		os.Stdout.Write(body)
		return nil
	}
	_, _ = fmt.Fprintln(os.Stdout, successMsg)
	return nil
}
