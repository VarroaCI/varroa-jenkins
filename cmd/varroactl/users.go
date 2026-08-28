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
	registerNoun(addUserCommands)
}

// addUserCommands attaches user subcommands.
// Users are cluster-scoped. No describe (no detail GET endpoint).
func addUserCommands(v *verbCommands) {
	getCmd := &cobra.Command{
		Use:     "user",
		Aliases: []string{"users"},
		Short:   "Get users",
		RunE:    runGetUsers,
	}
	v.get.AddCommand(getCmd)

	createCmd := &cobra.Command{
		Use:     "user -f FILE|- [--password-stdin]",
		Aliases: []string{"users"},
		Short:   "Create a user",
		RunE:    runCreateUser,
	}
	createCmd.Flags().StringP("file", "f", "", "YAML file (or - for stdin)")
	createCmd.Flags().Bool("password-stdin", false, "Read password from stdin (overrides file)")
	_ = createCmd.MarkFlagRequired("file")
	v.create.AddCommand(createCmd)

	editCmd := &cobra.Command{
		Use:     "user NAME",
		Aliases: []string{"users"},
		Short:   "Edit a user in $EDITOR",
		Args:    cobra.ExactArgs(1),
		RunE:    runEditUser,
	}
	v.edit.AddCommand(editCmd)

	deleteCmd := &cobra.Command{
		Use:     "user NAME",
		Aliases: []string{"users"},
		Short:   "Delete a user",
		Args:    cobra.ExactArgs(1),
		RunE:    runDeleteUser,
	}
	v.delete.AddCommand(deleteCmd)
}

func runGetUsers(cmd *cobra.Command, args []string) error {
	if err := checkClusterScoped(cmd, "user"); err != nil {
		return err
	}
	o, _ := cmd.Flags().GetString("output")
	noHeaders, _ := cmd.Flags().GetBool("no-headers")

	httpResp, err := rawRequest(cmd, "GET", "/users", nil)
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
	return renderList(env.Items, o, noHeaders, userColumns, []string{"NAME", "EMAIL", "GROUPS", "MANAGED-BY", "LAST-LOGIN"})
}

func runCreateUser(cmd *cobra.Command, args []string) error {
	if err := checkClusterScoped(cmd, "user"); err != nil {
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

	// Handle --password-stdin
	stdinPass, _ := cmd.Flags().GetBool("password-stdin")
	if stdinPass {
		passBytes, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("failed to read password from stdin: %w", err)
		}
		pass := strings.TrimRight(string(passBytes), "\n\r")
		body["password"] = pass
	}

	// Check password exists somewhere
	if _, ok := body["password"]; !ok {
		return usagef("password required: set in file or use --password-stdin")
	}

	return createClusterScoped(cmd, "/users", body)
}

func runEditUser(cmd *cobra.Command, args []string) error {
	if err := checkClusterScoped(cmd, "user"); err != nil {
		return err
	}
	name := args[0]

	return editLoop(
		func() (map[string]any, error) {
			// Fetch user list and find entry by name (no GET /users/{name})
			httpResp, err := rawRequest(cmd, "GET", "/users", nil)
			if err != nil {
				return nil, err
			}
			defer func() { _ = httpResp.Body.Close() }()
			if httpResp.StatusCode >= 400 {
				b, _ := io.ReadAll(httpResp.Body)
				return nil, errFromResponse(b, httpResp.StatusCode)
			}
			body, _ := io.ReadAll(httpResp.Body)
			var env struct {
				Items []map[string]any `json:"items"`
			}
			if err := json.Unmarshal(body, &env); err != nil {
				return nil, fmt.Errorf("failed to decode user list: %w", err)
			}

			// Find by name
			for _, item := range env.Items {
				if itemName(item) == name {
					// Buffer exactly {email, displayName}
					buf := map[string]any{
						"email":       item["email"],
						"displayName": item["displayName"],
					}
					return buf, nil
				}
			}
			return nil, fmt.Errorf("error from server (404): user %q not found", name)
		},
		func(doc map[string]any) error {
			// PUT /users/{name} always sending both keys (email + displayName)
			// Ensure both keys present (empty strings are valid — they clear fields)
			if _, ok := doc["email"]; !ok {
				doc["email"] = ""
			}
			if _, ok := doc["displayName"]; !ok {
				doc["displayName"] = ""
			}
			return putClusterScoped(cmd, "/users/"+name, doc)
		},
		"user", name,
	)
}

func runDeleteUser(cmd *cobra.Command, args []string) error {
	if err := checkClusterScoped(cmd, "user"); err != nil {
		return err
	}
	return deleteClusterScoped(cmd, "/users/"+args[0], "user", args[0])
}

func userColumns(item map[string]any) []string {
	name := itemName(item)
	email := ""
	if e, ok := item["email"].(string); ok {
		email = e
	}
	groups := ""
	if g, ok := item["groups"].([]any); ok {
		parts := make([]string, 0, len(g))
		for _, gr := range g {
			parts = append(parts, fmt.Sprintf("%v", gr))
		}
		groups = strings.Join(parts, ",")
	} else if g, ok := item["groups"].(string); ok {
		groups = g
	}
	managedBy := ""
	if mb, ok := item["managedBy"].(string); ok {
		managedBy = mb
	}
	lastLogin := "-"
	if ll, ok := item["lastLogin"].(string); ok && ll != "" {
		lastLogin = ll
	}
	return []string{name, email, groups, managedBy, lastLogin}
}
