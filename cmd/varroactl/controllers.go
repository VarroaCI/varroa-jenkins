package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

func init() {
	registerNoun(addControllerCommands)
}

// addControllerCommands attaches controller/controllers/ctrl subcommands
// to each verb parent.
func addControllerCommands(v *verbCommands) {
	// get controller(s)
	getCtrl := &cobra.Command{
		Use:     "controller",
		Aliases: []string{"controllers", "ctrl"},
		Short:   "Get controller(s)",
		Long:    `Get controller(s). Use -A for all namespaces or -n for a specific namespace.`,
		RunE:    runGetController,
	}
	addClusterFlag(getCtrl)
	getCtrl.Flags().Bool("all-clusters", false, "list controllers across all clusters")
	v.get.AddCommand(getCtrl)

	// describe controller
	descCtrl := &cobra.Command{
		Use:     "controller NS/NAME",
		Aliases: []string{"controllers", "ctrl"},
		Short:   "Describe a controller",
		RunE:    runDescribeController,
	}
	addClusterFlag(descCtrl)
	v.describe.AddCommand(descCtrl)

	// create controller
	createCtrl := &cobra.Command{
		Use:     "controller -f FILE|- [-n NS]",
		Aliases: []string{"controllers", "ctrl"},
		Short:   "Create a controller",
		RunE:    runCreateController,
	}
	createCtrl.Flags().StringP("file", "f", "", "YAML file (or - for stdin)")
	_ = createCtrl.MarkFlagRequired("file")
	addClusterFlag(createCtrl)
	v.create.AddCommand(createCtrl)

	// delete controller
	deleteCtrl := &cobra.Command{
		Use:     "controller NS/NAME",
		Aliases: []string{"controllers", "ctrl"},
		Short:   "Delete a controller",
		Args:    cobra.ExactArgs(1),
		RunE:    runDeleteController,
	}
	addClusterFlag(deleteCtrl)
	v.delete.AddCommand(deleteCtrl)

	// patch controller
	patchCtrl := &cobra.Command{
		Use:     "controller NS/NAME -p JSON",
		Aliases: []string{"controllers", "ctrl"},
		Short:   "Patch a controller",
		RunE:    runPatchController,
	}
	patchCtrl.Flags().StringP("patch", "p", "", "JSON merge patch body")
	_ = patchCtrl.MarkFlagRequired("patch")
	addClusterFlag(patchCtrl)
	v.patch.AddCommand(patchCtrl)

	// edit controller
	editCtrl := &cobra.Command{
		Use:     "controller NS/NAME",
		Aliases: []string{"controllers", "ctrl"},
		Short:   "Edit a controller in $EDITOR",
		Args:    cobra.ExactArgs(1),
		RunE:    runEditController,
	}
	addClusterFlag(editCtrl)
	v.edit.AddCommand(editCtrl)
}

// ---------------------------------------------------------------------------
// get controller(s)
// ---------------------------------------------------------------------------

func runGetController(cmd *cobra.Command, args []string) error {
	c, err := apiClient(cmd)
	if err != nil {
		return err
	}
	o, _ := cmd.Flags().GetString("output")
	noHeaders, _ := cmd.Flags().GetBool("no-headers")

	if len(args) == 1 {
		return getSingleController(cmd, c, args[0], o)
	}
	return listControllers(cmd, c, o, noHeaders)
}

func listControllers(cmd *cobra.Command, c *client.ClientWithResponses, format string, noHeaders bool) error {
	nFlag, _ := cmd.Flags().GetString("namespace")
	aFlag, _ := cmd.Flags().GetBool("all-namespaces")
	cFlag, _ := cmd.Flags().GetString("cluster")
	allClustersFlag, _ := cmd.Flags().GetBool("all-clusters")
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
	ns := resolveListNamespace(nFlag, aFlag, rc.defaultNamespace)
	cl, err := resolveListCluster(cFlag, allClustersFlag, rc.defaultCluster)
	if err != nil {
		return err
	}

	params := &client.ListControllersParams{}
	if ns != "" {
		params.Namespace = &ns
	}
	if cl != "" {
		params.Cluster = &cl
	}

	resp, err := c.ListControllersWithResponse(cmd.Context(), params)
	if err != nil {
		return err
	}
	if resp.HTTPResponse.StatusCode >= 400 {
		return errFromResponse(resp.Body, resp.HTTPResponse.StatusCode)
	}
	if resp.JSON200 == nil {
		return fmt.Errorf("unexpected response: no data")
	}

	items := resp.JSON200.Items

	switch format {
	case "json":
		return printJSON(os.Stdout, items)
	case "yaml":
		return printYAML(os.Stdout, items)
	case "name":
		for _, item := range items {
			_, _ = fmt.Fprintf(os.Stdout, "%s/%s\n", item.Namespace, item.Name)
		}
		return nil
	case "table", "wide":
		headers, rows := controllerListTable(items, format == "wide")
		printTable(os.Stdout, headers, rows, noHeaders)
		return nil
	}
	return nil
}

func controllerListTable(items []client.ComponentsSchemasControllerSummary, wide bool) ([]string, [][]string) {
	headers := []string{"CLUSTER", "NAMESPACE", "NAME", "PHASE", "VERSION", "MITE", "HEALTH"}
	if wide {
		headers = append(headers, "ENDPOINT", "BUNDLE", "ROUTING", "JENKINS-VERSION")
	}
	rows := make([][]string, len(items))
	for i, item := range items {
		mite := "-"
		if item.MiteConnected {
			mite = "connected"
		}
		ver := ""
		if item.Version != nil {
			ver = *item.Version
		}
		health := ""
		if item.JenkinsHealth != nil {
			health = *item.JenkinsHealth
		}
		row := []string{item.Cluster, item.Namespace, item.Name, item.Phase, ver, mite, health}
		if wide {
			bundle := effectiveBundleLabel(item.EffectiveBundle, item.ComposedBundleRef)
			row = append(row, item.Endpoint, bundle, item.RoutingMode, strOrEmpty(item.JenkinsVersion))
		}
		rows[i] = row
	}
	return headers, rows
}

// effectiveBundleLabel renders which bundle a controller is using. A nil
// composedBundleRef is not "no bundle": the controller runs the built-in
// starter, and printing "-" told operators their zero-config controllers were
// unconfigured.
func effectiveBundleLabel(eff *client.ComponentsSchemasEffectiveBundle, ref *client.ComponentsSchemasComposedBundleRef) string {
	if eff != nil {
		if eff.BuiltIn {
			return eff.Namespace + "/" + eff.Name + " (built-in)"
		}
		return eff.Namespace + "/" + eff.Name
	}
	// Older server without effectiveBundle: fall back to the raw spec field.
	if ref == nil {
		return "-"
	}
	if ref.Namespace != nil && *ref.Namespace != "" {
		return *ref.Namespace + "/" + ref.Name
	}
	return ref.Name
}

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// getSingleController handles `get controller NS/NAME`.
func getSingleController(cmd *cobra.Command, c *client.ClientWithResponses, arg, format string) error {
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
	ns, name, err := resolveNSName(arg, nFlag, rc.defaultNamespace)
	if err != nil {
		return err
	}
	cluster := resolveCluster(cFlag, rc.defaultCluster)

	// Single-get -o yaml special case (design §10, N4)
	if format == "yaml" {
		resp, err := c.GetControllerYaml(cmd.Context(), cluster, ns, name)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 400 {
			apiErr := client.DecodeError(resp)
			return apiErrorf(apiErr)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		os.Stdout.Write(body)
		if len(body) > 0 && body[len(body)-1] != '\n' {
			_, _ = fmt.Fprintln(os.Stdout)
		}
		return nil
	}

	// Standard DTO fetch
	dtoResp, err := c.GetControllerWithResponse(cmd.Context(), cluster, ns, name)
	if err != nil {
		return err
	}
	if dtoResp.HTTPResponse.StatusCode >= 400 {
		return errFromResponse(dtoResp.Body, dtoResp.HTTPResponse.StatusCode)
	}
	if dtoResp.JSON200 == nil {
		return fmt.Errorf("unexpected response: no data")
	}

	detail := *dtoResp.JSON200

	switch format {
	case "json":
		return printJSON(os.Stdout, detail)
	case "yaml":
		// Already handled above; this is for list yaml which falls through.
		return printYAML(os.Stdout, detail)
	case "name":
		_, _ = fmt.Fprintf(os.Stdout, "%s/%s\n", detail.Namespace, detail.Name)
		return nil
	case "table", "wide":
		headers, rows := controllerDetailTable(detail, format == "wide")
		printTable(os.Stdout, headers, rows, false)
		return nil
	}
	return nil
}

func controllerDetailTable(d client.ComponentsSchemasControllerDetail, wide bool) ([]string, [][]string) {
	headers := []string{"CLUSTER", "NAMESPACE", "NAME", "PHASE", "VERSION", "MITE", "HEALTH"}
	if wide {
		headers = append(headers, "ENDPOINT", "BUNDLE", "ROUTING", "JENKINS-VERSION")
	}
	mite := "-"
	if d.MiteConnected {
		mite = "connected"
	}
	row := []string{d.Cluster, d.Namespace, d.Name, d.Phase, d.Version, mite, strOrEmpty(d.JenkinsHealth)}
	if wide {
		bundle := effectiveBundleLabel(d.EffectiveBundle, d.ComposedBundleRef)
		row = append(row, d.Endpoint, bundle, d.RoutingMode, strOrEmpty(d.JenkinsVersion))
	}
	return headers, [][]string{row}
}

// ---------------------------------------------------------------------------
// describe controller
// ---------------------------------------------------------------------------

func runDescribeController(cmd *cobra.Command, args []string) error {
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

	dtoResp, err := c.GetControllerWithResponse(cmd.Context(), cluster, ns, name)
	if err != nil {
		return err
	}
	if dtoResp.HTTPResponse.StatusCode >= 400 {
		return errFromResponse(dtoResp.Body, dtoResp.HTTPResponse.StatusCode)
	}
	if dtoResp.JSON200 == nil {
		return fmt.Errorf("unexpected response: no data")
	}

	d := *dtoResp.JSON200

	// Render describe sections
	describeIdentity(d)
	describePhase(d)
	describeVersion(d)
	describeEndpointRouting(d)
	describeBundle(d)
	describeReconciliation(d)
	describePending(d)
	describeMite(d)

	return nil
}

func describeIdentity(d client.ComponentsSchemasControllerDetail) {
	_, _ = fmt.Fprintf(os.Stdout, "Name:\t%s\n", d.Name)
	_, _ = fmt.Fprintf(os.Stdout, "Namespace:\t%s\n", d.Namespace)
	_, _ = fmt.Fprintf(os.Stdout, "Cluster:\t%s\n", d.Cluster)
}

func describePhase(d client.ComponentsSchemasControllerDetail) {
	_, _ = fmt.Fprintf(os.Stdout, "Phase:\t%s\n", d.Phase)
	if d.PowerState != nil {
		_, _ = fmt.Fprintf(os.Stdout, "Power State:\t%s\n", *d.PowerState)
	}
}

func describeVersion(d client.ComponentsSchemasControllerDetail) {
	_, _ = fmt.Fprintf(os.Stdout, "Version:\t%s\n", d.Version)
}

func describeEndpointRouting(d client.ComponentsSchemasControllerDetail) {
	_, _ = fmt.Fprintf(os.Stdout, "Endpoint:\t%s\n", d.Endpoint)
	_, _ = fmt.Fprintf(os.Stdout, "Routing Mode:\t%s\n", d.RoutingMode)
}

func describeBundle(d client.ComponentsSchemasControllerDetail) {
	// Unconditional: a zero-config controller has an effective bundle and real
	// hashes. Gating the whole block on a non-nil spec ref hid both.
	_, _ = fmt.Fprintf(os.Stdout, "Bundle:\t%s\n",
		effectiveBundleLabel(d.EffectiveBundle, d.ComposedBundleRef))
	if d.ConfigHash != nil {
		_, _ = fmt.Fprintf(os.Stdout, "Config Hash:\t%s\n", *d.ConfigHash)
	}
	if d.AppliedBundleHash != nil {
		_, _ = fmt.Fprintf(os.Stdout, "Applied Bundle Hash:\t%s\n", *d.AppliedBundleHash)
	}
	if d.DesiredStateHash != nil {
		_, _ = fmt.Fprintf(os.Stdout, "Desired State Hash:\t%s\n", *d.DesiredStateHash)
	}
}

func describeReconciliation(d client.ComponentsSchemasControllerDetail) {
	if d.ReconciliationPolicy != nil {
		_, _ = fmt.Fprintf(os.Stdout, "Reconciliation Policy:\n")
		// Print sub-fields
	}
}

func describePending(d client.ComponentsSchemasControllerDetail) {
	if d.PendingRestart != nil {
		_, _ = fmt.Fprintf(os.Stdout, "Pending Restart:\ttrue\n")
	}
	if d.PendingPluginRoll != nil {
		_, _ = fmt.Fprintf(os.Stdout, "Pending Plugin Roll:\ttrue\n")
	}
	if d.PendingItemDeletions != nil && len(*d.PendingItemDeletions) > 0 {
		_, _ = fmt.Fprintf(os.Stdout, "Pending Item Deletions:\t%d items\n", len(*d.PendingItemDeletions))
	}
}

func describeMite(d client.ComponentsSchemasControllerDetail) {
	_, _ = fmt.Fprintf(os.Stdout, "Mite Connected:\t%t\n", d.MiteConnected)
	if d.MiteVersion != nil {
		_, _ = fmt.Fprintf(os.Stdout, "Mite Version:\t%s\n", *d.MiteVersion)
	}
	if d.LastSeen != nil {
		_, _ = fmt.Fprintf(os.Stdout, "Last Seen:\t%s\n", *d.LastSeen)
	}
	if d.CertExpiry != nil {
		_, _ = fmt.Fprintf(os.Stdout, "Cert Expiry:\t%s\n", *d.CertExpiry)
	}
	if d.JenkinsHealth != nil {
		_, _ = fmt.Fprintf(os.Stdout, "Jenkins Health:\t%s\n", *d.JenkinsHealth)
	}
}

// ---------------------------------------------------------------------------
// create controller
// ---------------------------------------------------------------------------

func runCreateController(cmd *cobra.Command, args []string) error {
	c, err := apiClient(cmd)
	if err != nil {
		return err
	}
	nFlag, _ := cmd.Flags().GetString("namespace")
	fileFlag, _ := cmd.Flags().GetString("file")
	cFlag, _ := cmd.Flags().GetString("cluster")

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

	// Parse YAML into JSON for the API
	var cr map[string]interface{}
	if err := yaml.Unmarshal(data, &cr); err != nil {
		return fmt.Errorf("failed to parse YAML: %w", err)
	}

	// Determine namespace: -n > metadata.namespace
	ns := nFlag
	if ns == "" {
		if meta, ok := cr["metadata"].(map[string]interface{}); ok {
			if mn, ok := meta["namespace"].(string); ok {
				ns = mn
			}
		}
	}
	if ns == "" {
		return usagef("namespace required: use -n or set metadata.namespace in the file")
	}

	// If -n provided and file also has metadata.namespace, they must match
	if nFlag != "" {
		if meta, ok := cr["metadata"].(map[string]interface{}); ok {
			if mn, ok := meta["namespace"].(string); ok && mn != "" && mn != nFlag {
				return usagef("namespace conflict: -n=%q does not match metadata.namespace=%q in file", nFlag, mn)
			}
		}
	}

	// Marshal back to JSON for the API call
	jsonBody, err := json.Marshal(cr)
	if err != nil {
		return err
	}

	resp, err := c.CreateControllerWithBodyWithResponse(cmd.Context(),
		cluster,
		ns,
		"application/json",
		strings.NewReader(string(jsonBody)),
	)
	if err != nil {
		return err
	}

	switch {
	case resp.HTTPResponse.StatusCode >= 200 && resp.HTTPResponse.StatusCode < 300:
		_, _ = fmt.Fprintf(os.Stdout, "controller created successfully\n")
		return nil

	case resp.HTTPResponse.StatusCode == 400:
		if resp.JSON400 != nil && resp.JSON400.Checks != nil && len(*resp.JSON400.Checks) > 0 {
			renderChecksTable(*resp.JSON400.Checks)
			return fmt.Errorf("preflight failed")
		}
		msg := "bad request"
		if resp.JSON400 != nil && resp.JSON400.Error != nil {
			msg = *resp.JSON400.Error
		}
		return fmt.Errorf("error from server (400): %s", msg)

	case resp.HTTPResponse.StatusCode >= 400:
		msg := string(resp.Body)
		if len(msg) > 512 {
			msg = msg[:512]
		}
		return fmt.Errorf("error from server (%d): %s", resp.HTTPResponse.StatusCode, msg)

	default:
		return fmt.Errorf("unexpected status: %d", resp.HTTPResponse.StatusCode)
	}
}

func renderChecksTable(checks []client.ComponentsSchemasCheck) {
	headers := []string{"CHECK", "STATUS", "MESSAGE"}
	rows := make([][]string, len(checks))
	for i, ch := range checks {
		rows[i] = []string{ch.Id, string(ch.Status), ch.Message}
	}
	printTable(os.Stderr, headers, rows, false)
}

// ---------------------------------------------------------------------------
// delete controller
// ---------------------------------------------------------------------------

func runDeleteController(cmd *cobra.Command, args []string) error {
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

	resp, err := c.DeleteControllerWithResponse(cmd.Context(),
		cluster,
		ns,
		name,
	)
	if err != nil {
		return err
	}
	if resp.HTTPResponse.StatusCode >= 400 {
		return errFromResponse(resp.Body, resp.HTTPResponse.StatusCode)
	}
	_, _ = fmt.Fprintf(os.Stdout, "controller %q deleted\n", ns+"/"+name)
	return nil
}

// ---------------------------------------------------------------------------
// patch controller
// ---------------------------------------------------------------------------

func runPatchController(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		return usagef("NS/NAME is required")
	}
	c, err := apiClient(cmd)
	if err != nil {
		return err
	}
	nFlag, _ := cmd.Flags().GetString("namespace")
	patchStr, _ := cmd.Flags().GetString("patch")
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

	// Parse patch JSON
	var patchBody map[string]interface{}
	if err := json.Unmarshal([]byte(patchStr), &patchBody); err != nil {
		return usagef("invalid patch JSON: %v", err)
	}

	resp, err := c.PatchControllerWithBodyWithResponse(cmd.Context(),
		cluster,
		ns,
		name,
		&client.PatchControllerParams{},
		"application/json",
		strings.NewReader(patchStr),
	)
	if err != nil {
		return err
	}
	if resp.HTTPResponse.StatusCode >= 400 {
		return errFromResponse(resp.Body, resp.HTTPResponse.StatusCode)
	}
	_, _ = fmt.Fprintf(os.Stdout, "controller %q patched\n", ns+"/"+name)
	return nil
}
