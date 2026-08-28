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
	registerNoun(addTeamCommands)
}

// addTeamCommands attaches team subcommands to verb parents.
// Teams are cluster-scoped (reject -n).
func addTeamCommands(v *verbCommands) {
	// get teams / get team NAME
	getCmd := &cobra.Command{
		Use:     "team",
		Aliases: []string{"teams"},
		Short:   "Get team(s)",
		RunE:    runGetTeam,
	}
	v.get.AddCommand(getCmd)

	// describe team
	descCmd := &cobra.Command{
		Use:     "team NAME",
		Aliases: []string{"teams"},
		Short:   "Describe a team",
		Args:    cobra.ExactArgs(1),
		RunE:    runDescribeTeam,
	}
	v.describe.AddCommand(descCmd)

	// create team
	createCmd := &cobra.Command{
		Use:     "team -f FILE|-",
		Aliases: []string{"teams"},
		Short:   "Create a team",
		RunE:    runCreateTeam,
	}
	createCmd.Flags().StringP("file", "f", "", "YAML file (or - for stdin)")
	_ = createCmd.MarkFlagRequired("file")
	v.create.AddCommand(createCmd)

	// edit team
	editCmd := &cobra.Command{
		Use:     "team NAME",
		Aliases: []string{"teams"},
		Short:   "Edit a team in $EDITOR",
		Args:    cobra.ExactArgs(1),
		RunE:    runEditTeam,
	}
	v.edit.AddCommand(editCmd)

	// delete team
	deleteCmd := &cobra.Command{
		Use:     "team NAME",
		Aliases: []string{"teams"},
		Short:   "Delete a team",
		Args:    cobra.ExactArgs(1),
		RunE:    runDeleteTeam,
	}
	v.delete.AddCommand(deleteCmd)
}

func runGetTeam(cmd *cobra.Command, args []string) error {
	if err := checkClusterScoped(cmd, "team"); err != nil {
		return err
	}
	o, _ := cmd.Flags().GetString("output")
	noHeaders, _ := cmd.Flags().GetBool("no-headers")

	if len(args) == 1 {
		return getTeamSingle(cmd, args[0], o)
	}
	return listTeams(cmd, o, noHeaders)
}

func runDescribeTeam(cmd *cobra.Command, args []string) error {
	if err := checkClusterScoped(cmd, "team"); err != nil {
		return err
	}
	o, _ := cmd.Flags().GetString("output")
	doc, err := teamGET(cmd, args[0])
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

func runCreateTeam(cmd *cobra.Command, args []string) error {
	if err := checkClusterScoped(cmd, "team"); err != nil {
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
	return createClusterScoped(cmd, "/teams", body)
}

func runEditTeam(cmd *cobra.Command, args []string) error {
	if err := checkClusterScoped(cmd, "team"); err != nil {
		return err
	}
	name := args[0]

	// Writable fields projected from the teamEntry
	writableFields := []string{"name", "displayName", "members", "subjects", "namespaces", "roleRef", "provisionNamespaces"}

	return editLoop(
		func() (map[string]any, error) {
			doc, err := teamGET(cmd, name)
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
		func(doc map[string]any) error {
			return putClusterScoped(cmd, "/teams/"+name, doc)
		},
		"team", name,
	)
}

func runDeleteTeam(cmd *cobra.Command, args []string) error {
	if err := checkClusterScoped(cmd, "team"); err != nil {
		return err
	}
	name := args[0]
	return deleteClusterScoped(cmd, "/teams/"+name, "team", name)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func listTeams(cmd *cobra.Command, format string, noHeaders bool) error {
	httpResp, err := rawRequest(cmd, "GET", "/teams", nil)
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
	return renderList(env.Items, format, noHeaders, teamColumns, []string{"NAME", "ROLE", "MEMBERS", "NAMESPACES", "PROVISION"})
}

func getTeamSingle(cmd *cobra.Command, name, format string) error {
	doc, err := teamGET(cmd, name)
	if err != nil {
		return err
	}
	return renderSingle(doc, format, teamColumns, []string{"NAME", "ROLE", "MEMBERS", "NAMESPACES", "PROVISION"})
}

func teamGET(cmd *cobra.Command, name string) (map[string]any, error) {
	httpResp, err := rawRequest(cmd, "GET", "/teams/"+name, nil)
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

func teamColumns(item map[string]any) []string {
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

	return []string{name, roleRef, members, namespaces, provision}
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
