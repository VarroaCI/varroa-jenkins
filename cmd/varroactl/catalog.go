package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func init() {
	registerNoun(addCatalogNouns)
}

func init() {
	registerRootCommand(func(root *cobra.Command) {
		// sync catalogsource NS/NAME [--cluster CLUSTER]
		syncCmd := &cobra.Command{
			Use:   "sync catalogsource NS/NAME [--cluster CLUSTER]",
			Short: "Sync a catalogsource",
			Args:  cobra.ExactArgs(2),
			RunE:  runSyncCatalogsource,
		}
		addClusterFlag(syncCmd)
		root.AddCommand(syncCmd)

		// catalogitems get — attach to existing "get" parent [--cluster CLUSTER]
		getCmd := &cobra.Command{
			Use:     "catalogitems [--cluster CLUSTER]",
			Aliases: []string{"catalogitem", "ci"},
			Short:   "Get catalog items",
			RunE:    runGetCatalogItems,
		}
		getCmd.Flags().String("source", "", "Filter by source")
		getCmd.Flags().String("type", "", "Filter by type")
		getCmd.Flags().String("query", "", "Filter by query")
		addClusterFlag(getCmd)
		findCommand(root, "get").AddCommand(getCmd)

		// catalogitem describe — attach to existing "describe" parent [--cluster CLUSTER]
		descCmd := &cobra.Command{
			Use:     "catalogitem NS/NAME [--cluster CLUSTER]",
			Aliases: []string{"catalogitems", "ci"},
			Short:   "Describe a catalog item",
			Args:    cobra.ExactArgs(1),
			RunE:    runDescribeCatalogItem,
		}
		addClusterFlag(descCmd)
		findCommand(root, "describe").AddCommand(descCmd)
	})
}

// addCatalogNouns registers catalogsource CRUD via addCRDNoun.
func addCatalogNouns(v *verbCommands) {
	addCRDNoun(v, crdNounOpts{
		Noun:          "catalogsource",
		Aliases:       []string{"catalogsources", "cs"},
		Path:          "/catalogsources",
		Namespaced:    true,
		ClusterScoped: true,
		DescribeFrom:  true,
		Columns:       catalogsourceColumns,
		Headers:       []string{"NAMESPACE", "NAME", "URL", "PHASE", "ITEMS", "LAST-SYNC"},
	})
}

// ---------------------------------------------------------------------------
// Sync
// ---------------------------------------------------------------------------

func runSyncCatalogsource(cmd *cobra.Command, args []string) error {
	arg := args[1] // NS/NAME after "sync catalogsource NS/NAME"
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

	cluster := resolveCrdCluster(cmd)
	path := "/clusters/" + cluster + "/catalogsources/" + ns + "/" + name + "/sync"
	httpResp, err := rawRequest(cmd, "POST", path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, httpResp.StatusCode)
	}

	_, _ = fmt.Fprintf(os.Stdout, "sync requested for catalogsource %q\n", ns+"/"+name)
	return nil
}

// ---------------------------------------------------------------------------
// Catalog items list
// ---------------------------------------------------------------------------

func runGetCatalogItems(cmd *cobra.Command, args []string) error {
	o, _ := cmd.Flags().GetString("output")
	noHeaders, _ := cmd.Flags().GetBool("no-headers")

	if len(args) == 1 {
		return getSingleCatalogItem(cmd, args[0], o)
	}
	return listCatalogItems(cmd, o, noHeaders)
}

func listCatalogItems(cmd *cobra.Command, format string, noHeaders bool) error {
	nFlag, _ := cmd.Flags().GetString("namespace")
	aFlag, _ := cmd.Flags().GetBool("all-namespaces")
	source, _ := cmd.Flags().GetString("source")
	itemType, _ := cmd.Flags().GetString("type")
	query, _ := cmd.Flags().GetString("query")

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

	ns := resolveListNamespace(nFlag, aFlag, rc.defaultNamespace)

	var parts []string
	if ns != "" {
		parts = append(parts, "namespace="+ns)
	}
	if source != "" {
		parts = append(parts, "source="+source)
	}
	if itemType != "" {
		parts = append(parts, "type="+itemType)
	}
	if query != "" {
		parts = append(parts, "q="+query)
	}

	cluster := resolveCrdCluster(cmd)
	url := "/clusters/" + cluster + "/catalogitems"
	if len(parts) > 0 {
		url += "?" + strings.Join(parts, "&")
	}

	httpResp, err := rawRequest(cmd, "GET", url, nil)
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, httpResp.StatusCode)
	}

	body, _ := io.ReadAll(httpResp.Body)

	// Special: -o json prints the whole {items, operatorNamespace} envelope verbatim
	if format == "json" {
		_, _ = os.Stdout.Write(body)
		_, _ = fmt.Fprintln(os.Stdout)
		return nil
	}

	var env struct {
		Items             []map[string]any `json:"items"`
		OperatorNamespace string           `json:"operatorNamespace,omitempty"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("failed to decode: %w", err)
	}

	return renderList(env.Items, format, noHeaders, catalogItemColumns,
		[]string{"NAMESPACE", "NAME", "TYPE", "VERSION", "VALID"})
}

func getSingleCatalogItem(cmd *cobra.Command, arg, format string) error {
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

	cluster := resolveCrdCluster(cmd)
	url := "/clusters/" + cluster + "/catalogitems/" + ns + "/" + name
	httpResp, err := rawRequest(cmd, "GET", url, nil)
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

	return renderSingle(doc, format, catalogItemColumns, []string{"NAMESPACE", "NAME", "TYPE", "SOURCE", "DISPLAY-NAME"})
}

func runDescribeCatalogItem(cmd *cobra.Command, args []string) error {
	o, _ := cmd.Flags().GetString("output")
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
	ns, name, err := resolveNSName(args[0], nFlag, rc.defaultNamespace)
	if err != nil {
		return err
	}

	cluster := resolveCrdCluster(cmd)
	url := "/clusters/" + cluster + "/catalogitems/" + ns + "/" + name
	httpResp, err := rawRequest(cmd, "GET", url, nil)
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

// ---------------------------------------------------------------------------
// Column helpers
// ---------------------------------------------------------------------------

func catalogsourceColumns(item map[string]any) []string {
	ns := ""
	if meta, ok := item["metadata"].(map[string]any); ok {
		if n, ok := meta["namespace"].(string); ok {
			ns = n
		}
	}

	name := itemName(item)

	url := ""
	if spec, ok := item["spec"].(map[string]any); ok {
		if u, ok := spec["repoURL"].(string); ok {
			url = u
		}
	}

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

	return []string{ns, name, url, phase, items, lastSync}
}

func catalogItemColumns(item map[string]any) []string {
	ns := ""
	if n, ok := item["namespace"].(string); ok {
		ns = n
	} else if meta, ok := item["metadata"].(map[string]any); ok {
		if n, ok := meta["namespace"].(string); ok {
			ns = n
		}
	}

	name := itemName(item)

	itemType := ""
	if t, ok := item["type"].(string); ok {
		itemType = t
	} else if spec, ok := item["spec"].(map[string]any); ok {
		if t, ok := spec["type"].(string); ok {
			itemType = t
		}
	}

	version := ""
	if v, ok := item["version"].(string); ok {
		version = v
	} else if spec, ok := item["spec"].(map[string]any); ok {
		if v, ok := spec["version"].(string); ok {
			version = v
		}
	}

	valid := "false"
	if v, ok := item["valid"].(bool); ok {
		valid = fmt.Sprintf("%t", v)
	} else if status, ok := item["status"].(map[string]any); ok {
		if v, ok := status["valid"].(bool); ok {
			valid = fmt.Sprintf("%t", v)
		}
	}

	return []string{ns, name, itemType, version, valid}
}
