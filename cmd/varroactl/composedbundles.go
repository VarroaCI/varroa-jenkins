package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

func init() {
	registerNoun(addBundleNouns)
}

func init() {
	registerRootCommand(func(root *cobra.Command) {
		// validate bundle -f FILE|- [-n NS] [--cluster CLUSTER]
		validateCmd := &cobra.Command{
			Use:   "validate bundle -f FILE|- [-n NS] [--cluster CLUSTER]",
			Short: "Validate a bundle spec",
			RunE:  runValidateBundle,
		}
		validateCmd.Flags().StringP("file", "f", "", "YAML file (or - for stdin)")
		_ = validateCmd.MarkFlagRequired("file")
		addClusterFlag(validateCmd)
		root.AddCommand(validateCmd)

		// preview bundle -n NS -f FILE|- [--cluster CLUSTER]
		previewCmd := &cobra.Command{
			Use:   "bundle -n NS -f FILE|- [--cluster CLUSTER]",
			Short: "Preview a bundle",
			RunE:  runPreviewBundle,
		}
		previewCmd.Flags().StringP("file", "f", "", "YAML file (or - for stdin)")
		_ = previewCmd.MarkFlagRequired("file")
		previewCmd.Flags().StringP("namespace", "n", "", "namespace")
		addClusterFlag(previewCmd)
		findCommand(root, "preview").AddCommand(previewCmd)

		// pause bundle NS/NAME [--cluster CLUSTER]
		pauseCmd := &cobra.Command{
			Use:   "pause bundle NS/NAME [--cluster CLUSTER]",
			Short: "Pause a composed bundle",
			Args:  cobra.ExactArgs(2),
			RunE:  runPauseBundle,
		}
		addClusterFlag(pauseCmd)
		root.AddCommand(pauseCmd)

		// resume bundle NS/NAME [--cluster CLUSTER]
		resumeCmd := &cobra.Command{
			Use:   "resume bundle NS/NAME [--cluster CLUSTER]",
			Short: "Resume a composed bundle",
			Args:  cobra.ExactArgs(2),
			RunE:  runResumeBundle,
		}
		addClusterFlag(resumeCmd)
		root.AddCommand(resumeCmd)
	})
}

// addBundleNouns registers composedbundle CRUD via addCRDNoun.
func addBundleNouns(v *verbCommands) {
	addCRDNoun(v, crdNounOpts{
		Noun:          "composedbundle",
		Aliases:       []string{"composedbundles", "bundle", "bundles", "cb"},
		Path:          "/composedbundles",
		Namespaced:    true,
		ClusterScoped: true,
		DescribeFrom:  true,
		Columns:       bundleColumns,
		Headers:       []string{"NAMESPACE", "NAME", "PHASE", "ITEMS", "PAUSED"},
	})
}

// ---------------------------------------------------------------------------
// Bundle columns
// ---------------------------------------------------------------------------

func bundleColumns(item map[string]any) []string {
	ns := ""
	if meta, ok := item["metadata"].(map[string]any); ok {
		if n, ok := meta["namespace"].(string); ok {
			ns = n
		}
	}

	name := itemName(item)

	phase := ""
	if status, ok := item["status"].(map[string]any); ok {
		if p, ok := status["phase"].(string); ok {
			phase = p
		}
	}

	items := "0"
	if status, ok := item["status"].(map[string]any); ok {
		if ic, ok := status["itemCount"]; ok {
			items = fmt.Sprintf("%v", ic)
		}
	}

	// PAUSED: true when annotation varroa.dev/rollout-paused == "true"
	paused := ""
	if meta, ok := item["metadata"].(map[string]any); ok {
		if ann, ok := meta["annotations"].(map[string]any); ok {
			if v, ok := ann["varroa.dev/rollout-paused"]; ok && fmt.Sprintf("%v", v) == "true" {
				paused = "true"
			}
		}
	}

	return []string{ns, name, phase, items, paused}
}

// ---------------------------------------------------------------------------
// extractBundleSpec — deterministic CR → spec extraction (design §3.5)
// ---------------------------------------------------------------------------

// extractBundleSpec extracts a bare ComposedBundleSpec from a parsed document.
// If the document root contains any of {"apiVersion","kind","metadata"}, it is
// treated as a CR and its .spec value is returned (must be a non-empty map).
// Otherwise the document is returned as-is.
func extractBundleSpec(doc map[string]any) (map[string]any, error) {
	for _, key := range []string{"apiVersion", "kind", "metadata"} {
		if _, ok := doc[key]; ok {
			spec, ok := doc["spec"].(map[string]any)
			if !ok || len(spec) == 0 {
				return nil, usagef("file is a CR but has no spec")
			}
			return spec, nil
		}
	}
	return doc, nil
}

// ---------------------------------------------------------------------------
// Validate
// ---------------------------------------------------------------------------

func runValidateBundle(cmd *cobra.Command, args []string) error {
	f, _ := cmd.Flags().GetString("file")
	data, err := readStdinOrFile(f)
	if err != nil {
		return err
	}

	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("failed to parse YAML: %w", err)
	}

	spec, err := extractBundleSpec(doc)
	if err != nil {
		return err
	}

	cluster := resolveCrdCluster(cmd)
	nFlag, _ := cmd.Flags().GetString("namespace")
	url := "/clusters/" + cluster + "/composedbundles/validate"
	if nFlag != "" {
		url += "?namespace=" + nFlag
	}

	jsonBody, _ := json.Marshal(spec)
	httpResp, err := rawRequest(cmd, "POST", url, jsonBody)
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, httpResp.StatusCode)
	}

	body, _ := io.ReadAll(httpResp.Body)
	var result struct {
		Valid               bool     `json:"valid"`
		Errors              []string `json:"errors,omitempty"`
		Warnings            []string `json:"warnings,omitempty"`
		UnresolvedVariables []string `json:"unresolvedVariables,omitempty"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	printSections(result.Errors, result.Warnings, result.UnresolvedVariables)

	if !result.Valid {
		return fmt.Errorf("validation failed")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Preview
// ---------------------------------------------------------------------------

func runPreviewBundle(cmd *cobra.Command, args []string) error {
	nFlag, _ := cmd.Flags().GetString("namespace")
	if nFlag == "" {
		return usagef("--namespace / -n is required for preview bundle")
	}

	f, _ := cmd.Flags().GetString("file")
	data, err := readStdinOrFile(f)
	if err != nil {
		return err
	}

	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("failed to parse YAML: %w", err)
	}

	spec, err := extractBundleSpec(doc)
	if err != nil {
		return err
	}

	o, _ := cmd.Flags().GetString("output")
	cluster := resolveCrdCluster(cmd)
	url := "/clusters/" + cluster + "/composedbundles/" + nFlag + "/preview"

	jsonBody, _ := json.Marshal(spec)
	httpResp, err := rawRequest(cmd, "POST", url, jsonBody)
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, httpResp.StatusCode)
	}

	body, _ := io.ReadAll(httpResp.Body)

	if o == "json" {
		_, _ = os.Stdout.Write(body)
		_, _ = fmt.Fprintln(os.Stdout)
		return nil
	}

	var preview struct {
		BundleYAML          string   `json:"bundleYaml,omitempty"`
		JenkinsYAML         string   `json:"jenkinsYaml,omitempty"`
		PluginsYAML         string   `json:"pluginsYaml,omitempty"`
		ItemsYAML           string   `json:"itemsYaml,omitempty"`
		RBACYAML            string   `json:"rbacYaml,omitempty"`
		Missing             []string `json:"missing,omitempty"`
		Drifted             []string `json:"drifted,omitempty"`
		Warnings            []string `json:"warnings,omitempty"`
		UnresolvedVariables []string `json:"unresolvedVariables,omitempty"`
	}
	if err := json.Unmarshal(body, &preview); err != nil {
		return fmt.Errorf("failed to decode preview: %w", err)
	}

	// Per-file sections (skip empty/whitespace-only)
	sections := []struct {
		header string
		value  string
	}{
		{"--- jenkins.yaml ---", preview.JenkinsYAML},
		{"--- plugins.yaml ---", preview.PluginsYAML},
		{"--- items.yaml ---", preview.ItemsYAML},
		{"--- rbac.yaml ---", preview.RBACYAML},
		{"--- bundle.yaml ---", preview.BundleYAML},
	}
	for _, s := range sections {
		if strings.TrimSpace(s.value) != "" {
			_, _ = fmt.Fprintln(os.Stdout, s.header)
			_, _ = fmt.Fprintln(os.Stdout, s.value)
		}
	}

	// Lists (omit when empty)
	printListSection("Missing:", preview.Missing)
	printListSection("Drifted:", preview.Drifted)
	printSections(preview.Warnings, preview.UnresolvedVariables, nil)
	// Use printSectionsInline for warnings/unresolved
	if len(preview.Warnings) > 0 {
		_, _ = fmt.Fprintln(os.Stdout, "Warnings:")
		for _, w := range preview.Warnings {
			_, _ = fmt.Fprintf(os.Stdout, "  - %s\n", w)
		}
	}
	if len(preview.UnresolvedVariables) > 0 {
		_, _ = fmt.Fprintln(os.Stdout, "Unresolved Variables:")
		for _, u := range preview.UnresolvedVariables {
			_, _ = fmt.Fprintf(os.Stdout, "  - %s\n", u)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Pause / Resume
// ---------------------------------------------------------------------------

func runPauseBundle(cmd *cobra.Command, args []string) error {
	arg := args[1] // NS/NAME after "pause bundle NS/NAME"
	nFlag, _ := cmd.Flags().GetString("namespace")
	rc, err := resolveContext(func(n string) string {
		f := cmd.Flag(n)
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

	return bundlePauseResume(cmd, ns, name, "pause", "paused")
}

func runResumeBundle(cmd *cobra.Command, args []string) error {
	arg := args[1] // NS/NAME after "resume bundle NS/NAME"
	nFlag, _ := cmd.Flags().GetString("namespace")
	rc, err := resolveContext(func(n string) string {
		f := cmd.Flag(n)
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

	return bundlePauseResume(cmd, ns, name, "resume", "resumed")
}

func bundlePauseResume(cmd *cobra.Command, ns, name, action, status string) error {
	cluster := resolveCrdCluster(cmd)
	path := "/clusters/" + cluster + "/composedbundles/" + ns + "/" + name + "/" + action
	httpResp, err := rawRequest(cmd, "POST", path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode == 404 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, 404)
	}
	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, httpResp.StatusCode)
	}

	_, _ = fmt.Fprintf(os.Stdout, "composedbundle %q %s\n", ns+"/"+name, status)
	return nil
}

// ---------------------------------------------------------------------------
// Output helpers
// ---------------------------------------------------------------------------

func printSections(errors, warnings, unresolved []string) {
	if len(errors) > 0 {
		_, _ = fmt.Fprintln(os.Stdout, "Errors:")
		for _, e := range errors {
			_, _ = fmt.Fprintf(os.Stdout, "  - %s\n", e)
		}
	}
	if len(warnings) > 0 {
		_, _ = fmt.Fprintln(os.Stdout, "Warnings:")
		for _, w := range warnings {
			_, _ = fmt.Fprintf(os.Stdout, "  - %s\n", w)
		}
	}
	if len(unresolved) > 0 {
		_, _ = fmt.Fprintln(os.Stdout, "Unresolved Variables:")
		for _, u := range unresolved {
			_, _ = fmt.Fprintf(os.Stdout, "  - %s\n", u)
		}
	}
}

func printListSection(header string, items []string) {
	if len(items) == 0 {
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, header)
	for _, item := range items {
		_, _ = fmt.Fprintf(os.Stdout, "  - %s\n", item)
	}
}
