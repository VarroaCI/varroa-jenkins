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
	registerNoun(addTenantCommands)
}

// addTenantCommands attaches tenant subcommands to verb parents.
// Tenants are cluster-scoped (reject -n).
func addTenantCommands(v *verbCommands) {
	// get tenants / get tenant NAME
	getCmd := &cobra.Command{
		Use:     "tenant",
		Aliases: []string{"tenants"},
		Short:   "Get tenant(s)",
		RunE:    runGetTenant,
	}
	v.get.AddCommand(getCmd)

	// describe tenant
	descCmd := &cobra.Command{
		Use:     "tenant NAME",
		Aliases: []string{"tenants"},
		Short:   "Describe a tenant",
		Args:    cobra.ExactArgs(1),
		RunE:    runDescribeTenant,
	}
	v.describe.AddCommand(descCmd)

	// create tenant
	createCmd := &cobra.Command{
		Use:     "tenant -f FILE|-",
		Aliases: []string{"tenants"},
		Short:   "Create a tenant",
		RunE:    runCreateTenant,
	}
	createCmd.Flags().StringP("file", "f", "", "YAML file (or - for stdin)")
	_ = createCmd.MarkFlagRequired("file")
	v.create.AddCommand(createCmd)

	// edit tenant
	editCmd := &cobra.Command{
		Use:     "tenant NAME",
		Aliases: []string{"tenants"},
		Short:   "Edit a tenant in $EDITOR",
		Args:    cobra.ExactArgs(1),
		RunE:    runEditTenant,
	}
	v.edit.AddCommand(editCmd)

	// delete tenant
	deleteCmd := &cobra.Command{
		Use:     "tenant NAME",
		Aliases: []string{"tenants"},
		Short:   "Delete a tenant",
		Args:    cobra.ExactArgs(1),
		RunE:    runDeleteTenant,
	}
	v.delete.AddCommand(deleteCmd)
}

func runGetTenant(cmd *cobra.Command, args []string) error {
	if err := checkClusterScoped(cmd, "tenant"); err != nil {
		return err
	}
	o, _ := cmd.Flags().GetString("output")
	noHeaders, _ := cmd.Flags().GetBool("no-headers")

	if len(args) == 1 {
		return getTenantSingle(cmd, args[0], o)
	}
	return listTenants(cmd, o, noHeaders)
}

func runDescribeTenant(cmd *cobra.Command, args []string) error {
	if err := checkClusterScoped(cmd, "tenant"); err != nil {
		return err
	}
	o, _ := cmd.Flags().GetString("output")
	doc, err := tenantGET(cmd, args[0])
	if err != nil {
		return err
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

func runCreateTenant(cmd *cobra.Command, args []string) error {
	if err := checkClusterScoped(cmd, "tenant"); err != nil {
		return err
	}
	f, _ := cmd.Flags().GetString("file")
	data, err := readStdinOrFile(f)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := yaml.Unmarshal(data, &body); err != nil {
		return fmt.Errorf("failed to parse YAML: %w", err)
	}
	return createClusterScoped(cmd, "/tenants", body)
}

func runEditTenant(cmd *cobra.Command, args []string) error {
	if err := checkClusterScoped(cmd, "tenant"); err != nil {
		return err
	}
	name := args[0]

	// Writable fields projected from the tenantEntry. "pipelines" carries the
	// pipelines tenancy declaration, including the seven ledger and
	// rehydration settings; it round-trips as one nested object.
	writableFields := []string{"name", "displayName", "members", "subjects", "namespaces", "roleRef", "provisionNamespaces", "pipelines"}

	return editLoop(
		func() (map[string]any, error) {
			doc, err := tenantGET(cmd, name)
			if err != nil {
				return nil, err
			}
			projected := make(map[string]any)
			// Check both top-level and spec sub-object
			for _, src := range []map[string]any{doc} {
				if s, ok := src["spec"].(map[string]any); ok {
					for _, k := range writableFields {
						if v, ok := s[k]; ok {
							projected[k] = v
						}
					}
				}
				for _, k := range writableFields {
					if v, ok := src[k]; ok {
						projected[k] = v
					}
				}
			}
			if _, ok := projected["name"]; !ok {
				projected["name"] = name
			}
			return projected, nil
		},
		func(_, doc map[string]any) error {
			return putClusterScoped(cmd, "/tenants/"+name, doc)
		},
		"tenant", name,
	)
}

func runDeleteTenant(cmd *cobra.Command, args []string) error {
	if err := checkClusterScoped(cmd, "tenant"); err != nil {
		return err
	}
	name := args[0]
	return deleteClusterScoped(cmd, "/tenants/"+name, "tenant", name)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func listTenants(cmd *cobra.Command, format string, noHeaders bool) error {
	httpResp, err := rawRequest(cmd, "GET", "/tenants", nil)
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
	return renderList(env.Items, format, noHeaders, tenantColumns, tenantTableHeaders)
}

func getTenantSingle(cmd *cobra.Command, name, format string) error {
	doc, err := tenantGET(cmd, name)
	if err != nil {
		return err
	}
	return renderSingle(doc, format, tenantColumns, tenantTableHeaders)
}

// tenantTableHeaders are the `get tenant`/`get tenants` table columns. PIPELINES
// and CONCURRENCY summarize spec.pipelines; REHYDRATED summarizes the
// Rehydrated condition. The full pipelines settings and rehydration status
// are available via `-o json`/`-o yaml` or `describe tenant`.
var tenantTableHeaders = []string{"NAME", "ROLE", "MEMBERS", "NAMESPACES", "PROVISION", "PIPELINES", "CONCURRENCY", "REHYDRATED"}

func tenantGET(cmd *cobra.Command, name string) (map[string]any, error) {
	httpResp, err := rawRequest(cmd, "GET", "/tenants/"+name, nil)
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
}

func tenantColumns(item map[string]any) []string {
	name := itemName(item)

	roleRef := ""
	if spec, ok := item["spec"].(map[string]any); ok {
		if rr, ok := spec["roleRef"].(string); ok {
			roleRef = rr
		}
	}
	if roleRef == "" {
		if rr, ok := item["roleRef"].(string); ok {
			roleRef = rr
		}
	}

	memberCount := 0
	if spec, ok := item["spec"].(map[string]any); ok {
		if m, ok := spec["members"].([]any); ok {
			memberCount += len(m)
		}
		if subs, ok := spec["subjects"].([]any); ok {
			memberCount += len(subs)
		}
	}
	members := fmt.Sprintf("%d", memberCount)

	namespaces := ""
	if spec, ok := item["spec"].(map[string]any); ok {
		if ns, ok := spec["namespaces"].([]any); ok {
			parts := make([]string, 0, len(ns))
			for _, n := range ns {
				parts = append(parts, fmt.Sprintf("%v", n))
			}
			namespaces = strings.Join(parts, ",")
		}
	}

	provision := "false"
	if spec, ok := item["spec"].(map[string]any); ok {
		if pn, ok := spec["provisionNamespaces"]; ok {
			provision = fmt.Sprintf("%v", pn)
		}
	}

	pipelines := "-"
	concurrency := "-"
	if p, ok := item["pipelines"].(map[string]any); ok {
		pipelines = "true"
		if c, ok := p["concurrency"].(string); ok && c != "" {
			concurrency = c
		} else {
			concurrency = "allow"
		}
	}

	rehydrated := tenantConditionSummary(item, "Rehydrated")

	return []string{name, roleRef, members, namespaces, provision, pipelines, concurrency, rehydrated}
}

// tenantConditionSummary reports the status of one named condition from a
// tenantEntry's conditions array as "True", "False (<reason>)", or "-" when
// the condition is absent.
func tenantConditionSummary(item map[string]any, conditionType string) string {
	conditions, ok := item["conditions"].([]any)
	if !ok {
		return "-"
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := cond["type"].(string); t != conditionType {
			continue
		}
		status, _ := cond["status"].(string)
		if status == "True" {
			return "True"
		}
		reason, _ := cond["reason"].(string)
		if reason != "" {
			return fmt.Sprintf("%s (%s)", status, reason)
		}
		return status
	}
	return "-"
}

// ---------------------------------------------------------------------------
// Generic helpers for cluster-scoped operations
// ---------------------------------------------------------------------------

func checkClusterScoped(cmd *cobra.Command, noun string) error {
	nFlag, _ := cmd.Flags().GetString("namespace")
	if nFlag != "" {
		return usagef("--namespace/-n is not supported for cluster-scoped %s", noun)
	}
	return nil
}

func createClusterScoped(cmd *cobra.Command, path string, body map[string]any) error {
	httpResp, err := rawRequest(cmd, "POST", path, toJSON(body))
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, httpResp.StatusCode)
	}
	b, _ := io.ReadAll(httpResp.Body)
	if len(b) > 0 {
		_, _ = fmt.Fprintln(os.Stdout, string(b))
	}
	return nil
}

func putClusterScoped(cmd *cobra.Command, path string, doc map[string]any) error {
	httpResp, err := rawRequest(cmd, "PUT", path, toJSON(doc))
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, httpResp.StatusCode)
	}
	return nil
}

func deleteClusterScoped(cmd *cobra.Command, path, noun, name string) error {
	httpResp, err := rawRequest(cmd, "DELETE", path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, httpResp.StatusCode)
	}
	fmt.Printf("%s %q deleted\n", noun, name)
	return nil
}

func renderList(items []map[string]any, format string, noHeaders bool, columns func(map[string]any) []string, headers []string) error {
	switch format {
	case "json":
		return printJSON(os.Stdout, items)
	case "yaml":
		return printYAML(os.Stdout, items)
	case "name":
		for _, item := range items {
			_, _ = fmt.Fprintln(os.Stdout, itemName(item))
		}
		return nil
	case "table", "wide":
		rows := make([][]string, len(items))
		for i, item := range items {
			rows[i] = columns(item)
		}
		printTable(os.Stdout, headers, rows, noHeaders)
		return nil
	}
	return nil
}

func renderSingle(doc map[string]any, format string, columns func(map[string]any) []string, headers []string) error {
	switch format {
	case "json":
		return printJSON(os.Stdout, doc)
	case "yaml":
		return printYAML(os.Stdout, doc)
	case "name":
		_, _ = fmt.Fprintln(os.Stdout, itemName(doc))
		return nil
	case "table", "wide":
		row := columns(doc)
		printTable(os.Stdout, headers, [][]string{row}, false)
		return nil
	}
	return nil
}

// toJSON marshals v to JSON bytes.
func toJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
