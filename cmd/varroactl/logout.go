package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

func init() {
	registerRootCommand(func(root *cobra.Command) {
		logoutCmd := &cobra.Command{
			Use:   "logout [--revoke]",
			Short: "Log out and optionally revoke the API key",
			RunE:  runLogout,
		}
		logoutCmd.Flags().Bool("revoke", false, "Also revoke the API key server-side")
		root.AddCommand(logoutCmd)
	})
}

func runLogout(cmd *cobra.Command, args []string) error {
	revoke, _ := cmd.Flags().GetBool("revoke")
	ctxFlag, _ := cmd.Flags().GetString("context")

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	// Determine effective context
	ctxName := ctxFlag
	if ctxName == "" {
		ctxName = cfg.CurrentContext
	}

	if ctxName == "" {
		return fmt.Errorf("not logged in")
	}

	var ctx *cliContext
	for i := range cfg.Contexts {
		if cfg.Contexts[i].Name == ctxName {
			ctx = &cfg.Contexts[i]
			break
		}
	}
	if ctx == nil || ctx.APIKey == "" {
		return fmt.Errorf("not logged in")
	}

	if revoke {
		prefix := extractPrefix(ctx.APIKey)
		// Create a client to revoke the key
		c, err := client.New(ctx.Server, ctx.APIKey, client.WithUserAgent("varroactl/"+version))
		if err != nil {
			return err
		}
		r, err := c.RevokeApiKeyWithResponse(cmd.Context(), prefix)
		if err != nil {
			return err
		}
		// 204 or 404 both treated as success
		if r.HTTPResponse.StatusCode != 204 && r.HTTPResponse.StatusCode != 404 {
			apiErr := client.DecodeError(r.HTTPResponse)
			return fmt.Errorf("failed to revoke API key: %s", apiErr.Message)
		}
	}

	// Clear the API key (and optionally the whole entry... keep context, clear key)
	for i := range cfg.Contexts {
		if cfg.Contexts[i].Name == ctxName {
			cfg.Contexts[i].APIKey = ""
			break
		}
	}

	if err := saveConfig(cfg); err != nil {
		return err
	}

	if revoke {
		_, _ = fmt.Fprintln(os.Stdout, "Logged out and API key revoked.")
	} else {
		prefix := extractPrefix(ctx.APIKey)
		_, _ = fmt.Fprintf(os.Stdout, "API key %s remains valid — revoke with \"varroactl logout --revoke\" or from the dashboard\n", prefix)
	}

	return nil
}
