package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"

	"github.com/spf13/cobra"
)

func init() {
	registerRootCommand(func(root *cobra.Command) {
		searchCmd := &cobra.Command{
			Use:   "search QUERY",
			Short: "Search resources",
			Args:  cobra.ExactArgs(1),
			RunE:  runSearch,
		}
		root.AddCommand(searchCmd)
	})
}

// runSearch implements the search top-level verb.
func runSearch(cmd *cobra.Command, args []string) error {
	query := args[0]
	if query == "" {
		return usagef("search query is required")
	}

	o, _ := cmd.Flags().GetString("output")
	noHeaders, _ := cmd.Flags().GetBool("no-headers")

	encoded := url.QueryEscape(query)
	path := "/search?q=" + encoded

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
	var env struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("failed to decode: %w", err)
	}

	return renderList(env.Items, o, noHeaders, searchColumns, []string{"TYPE", "CLUSTER", "NAMESPACE", "NAME"})
}

func searchColumns(item map[string]any) []string {
	itemType := ""
	if t, ok := item["type"].(string); ok {
		itemType = t
	}
	cluster := ""
	if c, ok := item["cluster"].(string); ok {
		cluster = c
	}

	ns := ""
	if n, ok := item["namespace"].(string); ok {
		ns = n
	}

	name := ""
	if n, ok := item["name"].(string); ok {
		name = n
	}
	if name == "" {
		name = itemName(item)
	}

	return []string{itemType, cluster, ns, name}
}
