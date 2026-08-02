package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

func init() {
	registerRootCommand(func(root *cobra.Command) {
		whoamiCmd := &cobra.Command{
			Use:   "whoami",
			Short: "Show current user identity and permissions",
			RunE:  runWhoami,
		}
		root.AddCommand(whoamiCmd)
	})
}

func runWhoami(cmd *cobra.Command, args []string) error {
	c, err := apiClient(cmd)
	if err != nil {
		return err
	}

	// Resolve the context to get the server base URL and API key for raw HTTP call
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

	// GET /me
	me, err := c.GetMeWithResponse(cmd.Context())
	if err != nil {
		return err
	}
	if me.HTTPResponse.StatusCode >= 400 {
		apiErr := client.DecodeError(me.HTTPResponse)
		return fmt.Errorf("error from server (%d): %s", apiErr.StatusCode, apiErr.Message)
	}
	if me.JSON200 == nil {
		return fmt.Errorf("unexpected response")
	}

	user := me.JSON200

	// Print identity block
	displayName := user.Email
	if user.PreferredUsername != nil && *user.PreferredUsername != "" {
		displayName = *user.PreferredUsername
	}
	_, _ = fmt.Fprintf(os.Stdout, "Username:\t%s\n", displayName)
	_, _ = fmt.Fprintf(os.Stdout, "Email:\t%s\n", user.Email)
	_, _ = fmt.Fprintf(os.Stdout, "Subject:\t%s\n", user.Subject)
	if user.DisplayName != nil {
		_, _ = fmt.Fprintf(os.Stdout, "Display Name:\t%s\n", *user.DisplayName)
	}
	_, _ = fmt.Fprintf(os.Stdout, "Auth Mode:\t%s\n", string(user.AuthMode))
	if len(user.Groups) > 0 {
		_, _ = fmt.Fprintf(os.Stdout, "Groups:\t%s\n", strings.Join(user.Groups, ", "))
	}

	// GET /me/permissions (no generated endpoint — raw HTTP)
	permReq, err := http.NewRequestWithContext(cmd.Context(), "GET", rc.server+"/api/v1/me/permissions", nil)
	if err != nil {
		return err
	}
	permReq.Header.Set("Authorization", "Bearer "+rc.apiKey)
	permReq.Header.Set("User-Agent", "varroactl/"+version)

	permResp, err := http.DefaultClient.Do(permReq)
	if err != nil {
		return err
	}
	defer func() { _ = permResp.Body.Close() }()

	body, err := io.ReadAll(permResp.Body)
	if err != nil {
		return err
	}

	if permResp.StatusCode >= 400 {
		return fmt.Errorf("failed to fetch permissions")
	}

	// Generic rendering: iterate JSON object, no fixed schema
	var permissions map[string]interface{}
	if err := json.Unmarshal(body, &permissions); err == nil && len(permissions) > 0 {
		_, _ = fmt.Fprintln(os.Stdout, "\nCapabilities:")
		for key, val := range permissions {
			_, _ = fmt.Fprintf(os.Stdout, "  %s:\t%v\n", key, val)
		}
	}

	return nil
}
