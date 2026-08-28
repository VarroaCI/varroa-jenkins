package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func init() {
	registerNoun(addProvisioningNouns)
}

// addProvisioningNouns registers provisioning-related nouns.
func addProvisioningNouns(v *verbCommands) {
	// Get/describe commands are registered via registerRootCommand due to special
	// rendering and optional-name behavior; writes can use the generic CRD flows.
	addVersionProfileWriteCommands(v)
}

func addVersionProfileWriteCommands(v *verbCommands) {
	opts := crdNounOpts{
		Noun:          "versionprofile",
		Aliases:       []string{"versionprofiles", "vp"},
		Path:          "/version-profiles",
		ClusterScoped: true,
		Columns:       versionProfileColumns,
		Headers:       []string{"NAME", "VERSION", "CHANNEL", "RECOMMENDED", "PLUGINS", "READY"},
	}

	createCmd := &cobra.Command{
		Use:     opts.Noun + " -f FILE|-",
		Aliases: opts.Aliases,
		Short:   fmt.Sprintf("Create a %s from a YAML file", opts.Noun),
		RunE:    makeRunCreate(opts),
	}
	createCmd.Flags().StringP("file", "f", "", "YAML file (or - for stdin)")
	_ = createCmd.MarkFlagRequired("file")
	addClusterFlag(createCmd)
	v.create.AddCommand(createCmd)

	editCmd := &cobra.Command{
		Use:     opts.Noun + " NAME",
		Aliases: opts.Aliases,
		Short:   fmt.Sprintf("Edit a %s in $EDITOR", opts.Noun),
		Args:    cobra.ExactArgs(1),
		RunE:    makeRunEdit(opts),
	}
	addClusterFlag(editCmd)
	v.edit.AddCommand(editCmd)

	deleteCmd := &cobra.Command{
		Use:     opts.Noun + " NAME",
		Aliases: opts.Aliases,
		Short:   fmt.Sprintf("Delete a %s", opts.Noun),
		Args:    cobra.ExactArgs(1),
		RunE:    makeRunDelete(opts),
	}
	addClusterFlag(deleteCmd)
	v.delete.AddCommand(deleteCmd)
}

func init() {
	registerRootCommand(func(root *cobra.Command) {
		// provisioningdefaults — get/describe/edit with NAME defaulting to varroa-defaults
		pdGet := &cobra.Command{
			Use:     "provisioningdefaults [NAME]",
			Aliases: []string{"provisioningdefault", "pd"},
			Short:   "Get provisioning defaults",
			Args:    cobra.MaximumNArgs(1),
			RunE:    runGetProvisioningDefaults,
		}
		addClusterFlag(pdGet)
		findCommand(root, "get").AddCommand(pdGet)

		pdDesc := &cobra.Command{
			Use:     "provisioningdefaults [NAME]",
			Aliases: []string{"provisioningdefault", "pd"},
			Short:   "Describe provisioning defaults",
			Args:    cobra.MaximumNArgs(1),
			RunE:    runDescribeProvisioningDefaults,
		}
		addClusterFlag(pdDesc)
		findCommand(root, "describe").AddCommand(pdDesc)

		pdEdit := &cobra.Command{
			Use:     "provisioningdefaults [NAME]",
			Aliases: []string{"provisioningdefault", "pd"},
			Short:   "Edit provisioning defaults in $EDITOR",
			Args:    cobra.MaximumNArgs(1),
			RunE:    runEditProvisioningDefaults,
		}
		addClusterFlag(pdEdit)
		findCommand(root, "edit").AddCommand(pdEdit)

		// versionprofiles get NAME — client-side filter; describe via detail DTO
		vpGet := &cobra.Command{
			Use:     "versionprofiles [NAME]",
			Aliases: []string{"versionprofile", "vp"},
			Short:   "Get version profiles",
			Args:    cobra.MaximumNArgs(1),
			RunE:    runGetVersionProfiles,
		}
		addClusterFlag(vpGet)
		findCommand(root, "get").AddCommand(vpGet)

		vpDesc := &cobra.Command{
			Use:     "versionprofile NAME",
			Aliases: []string{"versionprofiles", "vp"},
			Short:   "Describe a version profile",
			Args:    cobra.ExactArgs(1),
			RunE:    runDescribeVersionProfile,
		}
		addClusterFlag(vpDesc)
		findCommand(root, "describe").AddCommand(vpDesc)

		// Singletons — get-only, no positional args
		addClusterSingleton(root, "provisioning-config", "/provisioning/config", runGetClusterSingleton)
		addSingleton(root, "builtin-roles", "/builtin-roles", runGetSingleton)
		addSingleton(root, "identity-settings", "/identity-settings", runGetSingleton)

		// deployable-namespaces: cluster-scoped singleton with --cluster flag.
		dn := &cobra.Command{
			Use:   "deployable-namespaces",
			Short: "Get deployable namespaces",
			Args:  cobra.NoArgs,
			RunE:  runGetDeployableNamespaces,
		}
		addClusterFlag(dn)
		findCommand(root, "get").AddCommand(dn)
	})
}

// addSingleton registers a get-only singleton noun under the get verb parent.
func addSingleton(root *cobra.Command, name, path string, run func(*cobra.Command, []string) error) {
	cmd := &cobra.Command{
		Use:                   name,
		Short:                 fmt.Sprintf("Get %s", strings.ReplaceAll(name, "-", " ")),
		Args:                  cobra.NoArgs,
		RunE:                  run,
		DisableFlagsInUseLine: true,
	}
	// Store the path as an annotation
	cmd.Annotations = map[string]string{"path": path}
	findCommand(root, "get").AddCommand(cmd)
}

func addClusterSingleton(root *cobra.Command, name, path string, run func(*cobra.Command, []string) error) {
	cmd := &cobra.Command{
		Use:                   name,
		Short:                 fmt.Sprintf("Get %s", strings.ReplaceAll(name, "-", " ")),
		Args:                  cobra.NoArgs,
		RunE:                  run,
		DisableFlagsInUseLine: true,
	}
	cmd.Annotations = map[string]string{"path": path}
	addClusterFlag(cmd)
	findCommand(root, "get").AddCommand(cmd)
}

// ---------------------------------------------------------------------------
// ProvisioningDefaults
// ---------------------------------------------------------------------------

func resolveProvisioningDefaultsName(args []string) string {
	if len(args) > 0 && args[0] != "" {
		return args[0]
	}
	return "varroa-defaults"
}

func runGetProvisioningDefaults(cmd *cobra.Command, args []string) error {
	o, _ := cmd.Flags().GetString("output")
	name := resolveProvisioningDefaultsName(args)

	path := "/clusters/" + resolveCrdCluster(cmd) + "/provisioningdefaults/" + name
	httpResp, err := rawRequest(cmd, "GET", path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, httpResp.StatusCode)
	}

	body, _ := io.ReadAll(httpResp.Body)
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("failed to decode: %w", err)
	}

	return renderSingle(doc, o, provisioningDefaultsColumns,
		[]string{"NAMESPACE", "NAME", "PHASE", "ITEMS", "LAST-SYNC"})
}

func runDescribeProvisioningDefaults(cmd *cobra.Command, args []string) error {
	o, _ := cmd.Flags().GetString("output")
	name := resolveProvisioningDefaultsName(args)

	path := "/clusters/" + resolveCrdCluster(cmd) + "/provisioningdefaults/" + name
	httpResp, err := rawRequest(cmd, "GET", path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, httpResp.StatusCode)
	}

	body, _ := io.ReadAll(httpResp.Body)
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("failed to decode: %w", err)
	}

	switch o {
	case "json":
		return printJSON(os.Stdout, doc)
	case "yaml":
		return printYAML(os.Stdout, doc)
	default:
		return printDescribe(os.Stdout, doc)
	}
}

func runEditProvisioningDefaults(cmd *cobra.Command, args []string) error {
	name := resolveProvisioningDefaultsName(args)
	path := "/clusters/" + resolveCrdCluster(cmd) + "/provisioningdefaults/" + name

	return editLoop(
		func() (map[string]any, error) {
			httpResp, err := rawRequest(cmd, "GET", path, nil)
			if err != nil {
				return nil, err
			}
			defer func() { _ = httpResp.Body.Close() }()
			if httpResp.StatusCode >= 400 {
				b, _ := io.ReadAll(httpResp.Body)
				return nil, errFromResponse(b, httpResp.StatusCode)
			}
			body, _ := io.ReadAll(httpResp.Body)
			var doc map[string]any
			if err := json.Unmarshal(body, &doc); err != nil {
				return nil, fmt.Errorf("failed to decode: %w", err)
			}
			return doc, nil
		},
		func(doc map[string]any) error {
			jsonBody, _ := json.Marshal(doc)
			httpResp, err := rawRequest(cmd, "PUT", path, jsonBody)
			if err != nil {
				return err
			}
			defer func() { _ = httpResp.Body.Close() }()
			if httpResp.StatusCode >= 400 {
				b, _ := io.ReadAll(httpResp.Body)
				return errFromResponse(b, httpResp.StatusCode)
			}
			return nil
		},
		"provisioningdefaults", name,
	)
}

// ---------------------------------------------------------------------------
// VersionProfiles
// ---------------------------------------------------------------------------

func runGetVersionProfiles(cmd *cobra.Command, args []string) error {
	o, _ := cmd.Flags().GetString("output")
	noHeaders, _ := cmd.Flags().GetBool("no-headers")

	httpResp, err := rawRequest(cmd, "GET", "/clusters/"+resolveCrdCluster(cmd)+"/version-profiles", nil)
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, httpResp.StatusCode)
	}

	body, _ := io.ReadAll(httpResp.Body)
	var env struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("failed to decode: %w", err)
	}

	items := env.Items

	// If NAME arg provided, client-side filter
	if len(args) > 0 {
		var filtered []map[string]any
		for _, item := range items {
			if itemName(item) == args[0] {
				filtered = append(filtered, item)
			}
		}
		if len(filtered) == 0 {
			return fmt.Errorf("error from server (404): versionprofile %q not found", args[0])
		}
		if o == "json" || o == "yaml" || o == "name" {
			return renderSingle(filtered[0], o, versionProfileColumns,
				[]string{"NAME", "VERSION", "CHANNEL", "RECOMMENDED", "PLUGINS", "READY"})
		}
		// table: single row
		row := versionProfileColumns(filtered[0])
		printTable(os.Stdout, []string{"NAME", "VERSION", "CHANNEL", "RECOMMENDED", "PLUGINS", "READY"},
			[][]string{row}, noHeaders)
		return nil
	}

	// Check wide format for extra columns
	if o == "wide" {
		rows := make([][]string, len(items))
		for i, item := range items {
			rows[i] = versionProfileWideColumns(item)
		}
		printTable(os.Stdout, []string{"NAME", "VERSION", "CHANNEL", "RECOMMENDED", "PLUGINS", "READY", "EOL", "CONTENT-REF"},
			rows, noHeaders)
		return nil
	}

	return renderList(items, o, noHeaders, versionProfileColumns,
		[]string{"NAME", "VERSION", "CHANNEL", "RECOMMENDED", "PLUGINS", "READY"})
}

func runDescribeVersionProfile(cmd *cobra.Command, args []string) error {
	o, _ := cmd.Flags().GetString("output")

	// Fetch the full list and client-side filter
	httpResp, err := rawRequest(cmd, "GET", "/clusters/"+resolveCrdCluster(cmd)+"/version-profiles", nil)
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, httpResp.StatusCode)
	}

	body, _ := io.ReadAll(httpResp.Body)
	var env struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("failed to decode: %w", err)
	}

	name := args[0]
	for _, item := range env.Items {
		if itemName(item) == name {
			switch o {
			case "json":
				return printJSON(os.Stdout, item)
			case "yaml":
				return printYAML(os.Stdout, item)
			default:
				return printDescribe(os.Stdout, item)
			}
		}
	}

	return fmt.Errorf("error from server (404): versionprofile %q not found", name)
}

// ---------------------------------------------------------------------------
// Column helpers
// ---------------------------------------------------------------------------

func provisioningDefaultsColumns(item map[string]any) []string {
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

	lastSync := "-"
	if status, ok := item["status"].(map[string]any); ok {
		if ls, ok := status["lastSyncTime"].(string); ok && ls != "" {
			lastSync = ls
		}
	}

	return []string{ns, name, phase, items, lastSync}
}

func versionProfileColumns(item map[string]any) []string {
	// /version-profiles returns the flat VersionProfileDetail DTO
	// (name/version/channel/recommended/pluginCount/conditions/...), not a CR.
	name := itemName(item)

	str := func(key string) string {
		if v, ok := item[key].(string); ok {
			return v
		}
		return ""
	}

	recommended := ""
	if r, ok := item["recommended"]; ok {
		recommended = fmt.Sprintf("%v", r)
	}

	plugins := "0"
	if pc, ok := item["pluginCount"]; ok {
		plugins = fmt.Sprintf("%v", pc)
	}

	ready := "-"
	if conds, ok := item["conditions"].([]any); ok {
		for _, c := range conds {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if cm["type"] == "PluginSetReady" {
				ready = fmt.Sprintf("%v", cm["status"])
				break
			}
		}
	}

	return []string{name, str("version"), str("channel"), recommended, plugins, ready}
}

func versionProfileWideColumns(item map[string]any) []string {
	base := versionProfileColumns(item)

	eol := ""
	if v, ok := item["eol"].(string); ok {
		eol = v
	}
	contentRef := ""
	if v, ok := item["contentRef"].(string); ok {
		contentRef = v
	}
	return append(base, eol, contentRef)
}

// ---------------------------------------------------------------------------
// Singletons — runGetSingleton
// ---------------------------------------------------------------------------

func runGetSingleton(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usagef("%s does not accept arguments", cmd.Name())
	}

	o, _ := cmd.Flags().GetString("output")
	noHeaders, _ := cmd.Flags().GetBool("no-headers")

	path := cmd.Annotations["path"]

	httpResp, err := rawRequest(cmd, "GET", path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, httpResp.StatusCode)
	}

	body, _ := io.ReadAll(httpResp.Body)

	// builtin-roles has N6 envelope
	if path == "/builtin-roles" {
		var env struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(body, &env); err == nil && len(env.Items) > 0 {
			switch o {
			case "json":
				return printJSON(os.Stdout, env.Items)
			case "yaml":
				return printYAML(os.Stdout, env.Items)
			case "table", "wide":
				rows := make([][]string, len(env.Items))
				for i, item := range env.Items {
					rows[i] = builtinRoleColumns(item)
				}
				printTable(os.Stdout, []string{"NAME", "JENKINS-ROLE", "API-RULES", "PERMISSIONS"}, rows, noHeaders)
				return nil
			default:
				return printDescribe(os.Stdout, env.Items)
			}
		}
	}

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("failed to decode: %w", err)
	}

	switch o {
	case "json":
		return printJSON(os.Stdout, doc)
	case "yaml":
		return printYAML(os.Stdout, doc)
	default:
		return printDescribe(os.Stdout, doc)
	}
}

func runGetClusterSingleton(cmd *cobra.Command, args []string) error {
	basePath := cmd.Annotations["path"]
	cluster := resolveCrdCluster(cmd)
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations["path"] = "/clusters/" + url.PathEscape(cluster) + basePath
	return runGetSingleton(cmd, args)
}

func runGetDeployableNamespaces(cmd *cobra.Command, args []string) error {
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
	cluster := resolveCluster(cFlag, rc.defaultCluster)
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations["path"] = "/clusters/" + url.PathEscape(cluster) + "/namespaces/deployable"
	return runGetSingleton(cmd, args)
}

func builtinRoleColumns(item map[string]any) []string {
	name := itemName(item)

	jenkinsRole := ""
	if spec, ok := item["spec"].(map[string]any); ok {
		if jr, ok := spec["jenkinsRoleRef"].(string); ok {
			jenkinsRole = jr
		}
	}

	apiRules := "0"
	if spec, ok := item["spec"].(map[string]any); ok {
		if rules, ok := spec["apiRules"].([]any); ok {
			apiRules = fmt.Sprintf("%d", len(rules))
		}
	}

	permissions := "0"
	if spec, ok := item["spec"].(map[string]any); ok {
		if perms, ok := spec["permissions"].([]any); ok {
			permissions = fmt.Sprintf("%d", len(perms))
		}
	}

	return []string{name, jenkinsRole, apiRules, permissions}
}
