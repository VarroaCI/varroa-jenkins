package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func init() {
	registerRootCommand(func(root *cobra.Command) {
		apikeyCmd := &cobra.Command{
			Use:   "apikey",
			Short: "Manage API keys",
		}

		// list
		listCmd := &cobra.Command{
			Use:   "list [--user U]",
			Short: "List API keys",
			RunE:  runApikeyList,
		}
		listCmd.Flags().String("user", "", "User (admin only)")
		apikeyCmd.AddCommand(listCmd)

		// create
		createCmd := &cobra.Command{
			Use:   "create [NAME] [--expires-in DUR]",
			Short: "Create a new API key",
			RunE:  runApikeyCreate,
		}
		createCmd.Flags().String("expires-in", "", "Duration until expiry (e.g. 720h)")
		apikeyCmd.AddCommand(createCmd)

		// revoke
		revokeCmd := &cobra.Command{
			Use:   "revoke PREFIX [--user U]",
			Short: "Revoke an API key",
			Args:  cobra.ExactArgs(1),
			RunE:  runApikeyRevoke,
		}
		revokeCmd.Flags().String("user", "", "User (admin only)")
		apikeyCmd.AddCommand(revokeCmd)

		// rotate
		rotateCmd := &cobra.Command{
			Use:   "rotate PREFIX [--expires-in DUR] [--name N]",
			Short: "Rotate an API key",
			Args:  cobra.ExactArgs(1),
			RunE:  runApikeyRotate,
		}
		rotateCmd.Flags().String("expires-in", "", "Duration until expiry (e.g. 720h)")
		rotateCmd.Flags().String("name", "", "New key name")
		apikeyCmd.AddCommand(rotateCmd)

		root.AddCommand(apikeyCmd)
	})
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

func runApikeyList(cmd *cobra.Command, args []string) error {
	user, _ := cmd.Flags().GetString("user")
	o, _ := cmd.Flags().GetString("output")
	noHeaders, _ := cmd.Flags().GetBool("no-headers")

	path := "/me/apikeys"
	if user != "" {
		path = "/users/" + user + "/apikeys"
	}

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

	return renderList(env.Items, o, noHeaders, apikeyColumns, []string{"PREFIX", "NAME", "CREATED", "EXPIRES", "LAST-USED"})
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

func runApikeyCreate(cmd *cobra.Command, args []string) error {
	expiresIn, _ := cmd.Flags().GetString("expires-in")

	body := map[string]any{}
	if len(args) > 0 {
		body["name"] = args[0]
	}
	if expiresIn != "" {
		d, err := time.ParseDuration(expiresIn)
		if err != nil || d <= 0 {
			return usagef("invalid --expires-in %q: must be a positive Go duration (e.g. 720h)", expiresIn)
		}
		body["expiresIn"] = expiresIn
	}

	return createApikey(cmd, "/me/apikeys", body)
}

// ---------------------------------------------------------------------------
// Revoke
// ---------------------------------------------------------------------------

func runApikeyRevoke(cmd *cobra.Command, args []string) error {
	prefix := args[0]
	user, _ := cmd.Flags().GetString("user")

	path := "/me/apikeys/" + prefix
	if user != "" {
		path = "/users/" + user + "/apikeys/" + prefix
	}

	httpResp, err := rawRequest(cmd, "DELETE", path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, httpResp.StatusCode)
	}

	fmt.Printf("api key %q revoked\n", prefix)
	return nil
}

// ---------------------------------------------------------------------------
// Rotate
// ---------------------------------------------------------------------------

func runApikeyRotate(cmd *cobra.Command, args []string) error {
	prefix := args[0]
	expiresIn, _ := cmd.Flags().GetString("expires-in")
	name, _ := cmd.Flags().GetString("name")

	body := map[string]any{}
	if expiresIn != "" {
		d, err := time.ParseDuration(expiresIn)
		if err != nil || d <= 0 {
			return usagef("invalid --expires-in %q: must be a positive Go duration (e.g. 720h)", expiresIn)
		}
		body["expiresIn"] = expiresIn
	}
	if name != "" {
		body["name"] = name
	}

	path := "/me/apikeys/" + prefix + "/rotate"

	httpResp, err := rawRequest(cmd, "POST", path, toJSON(body))
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()

	respBody, _ := io.ReadAll(httpResp.Body)

	if httpResp.StatusCode >= 400 {
		// Check for partial failure (500 with newToken)
		var partial struct {
			Error    string `json:"error"`
			NewToken string `json:"newToken"`
		}
		if json.Unmarshal(respBody, &partial) == nil && partial.NewToken != "" {
			// Partial failure: new token was created but old wasn't revoked
			_, _ = fmt.Fprintln(os.Stdout, partial.NewToken)
			fmt.Fprintln(os.Stderr, "WARNING: rotation partially complete — the old key was NOT revoked; revoke it manually")
			return fmt.Errorf("rotate partial failure: %s", partial.Error)
		}
		return errFromResponse(respBody, httpResp.StatusCode)
	}

	// Success - decode response
	var result struct {
		Token   string `json:"token"`
		Warning string `json:"warning,omitempty"`
	}
	if err := json.Unmarshal(respBody, &result); err == nil {
		if result.Token != "" {
			_, _ = fmt.Fprintln(os.Stdout, result.Token)
		}
		if result.Warning != "" {
			fmt.Fprintln(os.Stderr, result.Warning)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func createApikey(cmd *cobra.Command, path string, body map[string]any) error {
	httpResp, err := rawRequest(cmd, "POST", path, toJSON(body))
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()

	respBody, _ := io.ReadAll(httpResp.Body)

	if httpResp.StatusCode >= 400 {
		return errFromResponse(respBody, httpResp.StatusCode)
	}

	var result struct {
		Token   string `json:"token"`
		Warning string `json:"warning,omitempty"`
	}
	if err := json.Unmarshal(respBody, &result); err == nil {
		if result.Token != "" {
			_, _ = fmt.Fprintln(os.Stdout, result.Token)
		}
		if result.Warning != "" {
			fmt.Fprintln(os.Stderr, result.Warning)
		}
	}
	return nil
}

func apikeyColumns(item map[string]any) []string {
	prefix := ""
	if p, ok := item["prefix"].(string); ok {
		prefix = p
	}

	name := ""
	if n, ok := item["name"].(string); ok {
		name = n
	}

	created := ""
	if c, ok := item["created"].(string); ok {
		created = c
	}

	expires := "-"
	if e, ok := item["expires"].(string); ok && e != "" {
		expires = e
	} else if e, ok := item["expiresIn"].(string); ok && e != "" {
		expires = e
	}

	lastUsed := "-"
	if lu, ok := item["lastUsed"].(string); ok && lu != "" {
		lastUsed = lu
	}

	return []string{prefix, name, created, expires, lastUsed}
}
