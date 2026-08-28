package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

func init() {
	registerNoun(addGroupCommands)
}

// addGroupCommands attaches group subcommands.
// Groups are cluster-scoped, list/create/delete only (no detail/edit).
func addGroupCommands(v *verbCommands) {
	getCmd := &cobra.Command{
		Use:     "group",
		Aliases: []string{"groups"},
		Short:   "Get groups",
		RunE:    runGetGroups,
	}
	v.get.AddCommand(getCmd)

	createCmd := &cobra.Command{
		Use:     "group -f FILE|-",
		Aliases: []string{"groups"},
		Short:   "Create a group",
		RunE:    runCreateGroup,
	}
	createCmd.Flags().StringP("file", "f", "", "YAML file (or - for stdin)")
	_ = createCmd.MarkFlagRequired("file")
	v.create.AddCommand(createCmd)

	deleteCmd := &cobra.Command{
		Use:     "group NAME",
		Aliases: []string{"groups"},
		Short:   "Delete a group",
		Args:    cobra.ExactArgs(1),
		RunE:    runDeleteGroup,
	}
	v.delete.AddCommand(deleteCmd)
}

func runGetGroups(cmd *cobra.Command, args []string) error {
	if err := checkClusterScoped(cmd, "group"); err != nil {
		return err
	}
	o, _ := cmd.Flags().GetString("output")
	noHeaders, _ := cmd.Flags().GetBool("no-headers")

	httpResp, err := rawRequest(cmd, "GET", "/groups", nil)
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
	return renderList(env.Items, o, noHeaders, groupColumns, []string{"NAME", "DISPLAY-NAME", "MEMBERS", "SOURCE"})
}

func runCreateGroup(cmd *cobra.Command, args []string) error {
	if err := checkClusterScoped(cmd, "group"); err != nil {
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
	return createClusterScoped(cmd, "/groups", body)
}

func runDeleteGroup(cmd *cobra.Command, args []string) error {
	if err := checkClusterScoped(cmd, "group"); err != nil {
		return err
	}
	return deleteClusterScoped(cmd, "/groups/"+args[0], "group", args[0])
}

func groupColumns(item map[string]any) []string {
	name := itemName(item)

	displayName := ""
	if dn, ok := item["displayName"].(string); ok {
		displayName = dn
	}

	members := "0"
	if mc, ok := item["memberCount"].(float64); ok {
		members = fmt.Sprintf("%.0f", mc)
	} else if m, ok := item["members"].([]any); ok {
		members = fmt.Sprintf("%d", len(m))
	}

	source := ""
	if s, ok := item["source"].(string); ok {
		source = s
	}

	return []string{name, displayName, members, source}
}
