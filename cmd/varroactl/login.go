package main

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/cli/browser"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

// Injectable browser opener for tests.
var openBrowser = browser.OpenURL

// ---------------------------------------------------------------------------
// login command
// ---------------------------------------------------------------------------

func init() {
	registerRootCommand(func(root *cobra.Command) {
		loginCmd := &cobra.Command{
			Use:   "login [--server URL]",
			Short: "Log in to a Varroa server",
			RunE:  runLogin,
		}
		loginCmd.Flags().String("server", "", "Server URL")
		loginCmd.Flags().String("api-key", "", "API key (or - for stdin)")
		loginCmd.Flags().String("username", "", "Username for local/ldap login")
		loginCmd.Flags().Bool("password-stdin", false, "Read password from stdin")
		loginCmd.Flags().Duration("timeout", 5*time.Minute, "Timeout for browser login")
		root.AddCommand(loginCmd)
	})
}

func runLogin(cmd *cobra.Command, args []string) error {
	apiKey, _ := cmd.Flags().GetString("api-key")
	username, _ := cmd.Flags().GetString("username")
	serverFlag, _ := cmd.Flags().GetString("server")

	// --api-key and --username mutually exclusive
	if apiKey != "" && username != "" {
		return usagef("--api-key and --username are mutually exclusive")
	}

	if apiKey != "" {
		return loginWithAPIKey(cmd, apiKey)
	}
	if username != "" {
		return loginWithUsername(cmd, username)
	}

	// Default: browser loopback flow
	return loginBrowser(cmd, serverFlag)
}

// ---------------------------------------------------------------------------
// validateAndStore — shared by all three flows
// ---------------------------------------------------------------------------

func validateAndStore(cmd *cobra.Command, server, token, ctxFlag string) error {
	c, err := client.New(server, token, client.WithUserAgent("varroactl/"+version))
	if err != nil {
		return err
	}

	// GET /me to validate the token and get identity
	me, err := c.GetMeWithResponse(cmd.Context())
	if err != nil {
		return err
	}
	if me.HTTPResponse.StatusCode >= 400 {
		return fmt.Errorf("invalid API key")
	}
	if me.JSON200 == nil {
		return fmt.Errorf("unexpected response")
	}

	user := me.JSON200
	displayName := user.Email
	if user.PreferredUsername != nil && *user.PreferredUsername != "" {
		displayName = *user.PreferredUsername
	}

	// Derive context name
	ctxName := ctxFlag
	if ctxName == "" {
		u, err := url.Parse(server)
		if err == nil {
			ctxName = u.Host
		} else {
			ctxName = server
		}
	}

	// Upsert context
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	var existing *cliContext
	for i := range cfg.Contexts {
		if cfg.Contexts[i].Name == ctxName {
			existing = &cfg.Contexts[i]
			break
		}
	}

	replacedKey := false
	var oldPrefix string
	if existing != nil && existing.APIKey != "" && existing.APIKey != token {
		replacedKey = true
		oldPrefix = extractPrefix(existing.APIKey)
	}

	if existing == nil {
		cfg.Contexts = append(cfg.Contexts, cliContext{
			Name:   ctxName,
			Server: server,
			APIKey: token,
		})
	} else {
		existing.Server = server
		existing.APIKey = token
	}
	cfg.CurrentContext = ctxName

	if err := saveConfig(cfg); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(os.Stdout, "Logged in to %s as %s (context %q)\n", server, displayName, ctxName)

	if replacedKey && oldPrefix != "" {
		_, _ = fmt.Fprintf(os.Stdout, "API key %s remains valid — revoke with \"varroactl logout --revoke\" or from the dashboard\n", oldPrefix)
	}

	return nil
}

// extractPrefix extracts the prefix from a vk_ token (vk_<prefix>.<secret>).
func extractPrefix(token string) string {
	tok := strings.TrimPrefix(token, "vk_")
	if idx := strings.Index(tok, "."); idx > 0 {
		return tok[:idx]
	}
	return tok
}

// ---------------------------------------------------------------------------
// 4.2 --api-key flow
// ---------------------------------------------------------------------------

func loginWithAPIKey(cmd *cobra.Command, apiKey string) error {
	serverFlag, _ := cmd.Flags().GetString("server")
	ctxFlag, _ := cmd.Flags().GetString("context")

	if apiKey == "-" {
		data, err := readLine(os.Stdin)
		if err != nil {
			return err
		}
		apiKey = strings.TrimSpace(data)
	}

	// Resolve server: flag > env > config
	server := serverFlag
	if server == "" {
		server = os.Getenv("VARROACTL_SERVER")
	}
	if server == "" {
		rc, err := resolveContext(func(name string) string { return "" })
		if err != nil {
			// Try without context — require --server or VARROACTL_SERVER
			return fmt.Errorf("--server is required")
		}
		server = rc.server
	}
	server = strings.TrimRight(server, "/")

	// Validate token by calling /me
	c, err := client.New(server, apiKey, client.WithUserAgent("varroactl/"+version))
	if err != nil {
		return err
	}
	me, err := c.GetMeWithResponse(cmd.Context())
	if err != nil {
		return err
	}
	if me.HTTPResponse.StatusCode >= 400 {
		return fmt.Errorf("invalid API key")
	}

	return validateAndStore(cmd, server, apiKey, ctxFlag)
}

// ---------------------------------------------------------------------------
// 4.1 Browser loopback flow
// ---------------------------------------------------------------------------

func loginBrowser(cmd *cobra.Command, serverFlag string) error {
	ctxFlag, _ := cmd.Flags().GetString("context")
	timeout, _ := cmd.Flags().GetDuration("timeout")

	// Resolve server
	server := serverFlag
	if server == "" {
		server = os.Getenv("VARROACTL_SERVER")
	}
	if server == "" {
		// Try to use --server from context
		rc, err := resolveContext(func(name string) string {
			if name == "server" {
				return serverFlag
			}
			return ""
		})
		if err == nil && rc.server != "" {
			server = rc.server
		}
	}
	if server == "" {
		return fmt.Errorf("--server is required for browser login")
	}
	server = strings.TrimRight(server, "/")

	// 1. Listen on 127.0.0.1:0
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port

	// 2. Generate nonce
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return err
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)

	// 3. Build login URL
	hostname, _ := os.Hostname()
	name := "varroactl@" + hostname
	loginURL := fmt.Sprintf("%s/cli-auth?port=%d&state=%s&name=%s",
		server, port, url.QueryEscape(nonce), url.QueryEscape(name))

	// Open browser
	if err := openBrowser(loginURL); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Open this URL:\n  %s\n", loginURL)
	}

	// 4. Loopback server
	type result struct {
		token  string
		denied bool
	}
	ch := make(chan result, 1)

	trySend := func(ch chan result, r result) {
		select {
		case ch <- r:
		default:
		}
	}

	const closePageTpl = `<html><body><script>history.replaceState({},"","/done")</script><p>%s — you may close this tab.</p></body></html>`

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || r.URL.Path != "/callback" {
				http.NotFound(w, r)
				return
			}
			q := r.URL.Query()
			stateParam := q.Get("state")
			if subtle.ConstantTimeCompare([]byte(stateParam), []byte(nonce)) != 1 {
				http.Error(w, "state mismatch", http.StatusBadRequest)
				return
			}
			if q.Get("error") == "denied" {
				_, _ = fmt.Fprintf(w, closePageTpl, "Request denied")
				trySend(ch, result{denied: true})
				return
			}
			tok := q.Get("token")
			if !strings.HasPrefix(tok, "vk_") {
				http.Error(w, "bad token", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprintf(w, closePageTpl, "Authenticated")
			trySend(ch, result{token: tok})
		}),
	}

	// Handle shutdown via interrupt
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	select {
	case r := <-ch:
		if r.denied {
			return fmt.Errorf("login denied in browser")
		}
		return validateAndStore(cmd, server, r.token, ctxFlag)
	case <-time.After(timeout):
		return fmt.Errorf("timed out waiting for browser login; URL:\n  %s\nOn headless machines use: varroactl login --api-key", loginURL)
	case <-sigCh:
		return fmt.Errorf("interrupted")
	}
}

// ---------------------------------------------------------------------------
// 4.3 Username/password flow (local/ldap)
// ---------------------------------------------------------------------------

func loginWithUsername(cmd *cobra.Command, username string) error {
	serverFlag, _ := cmd.Flags().GetString("server")
	ctxFlag, _ := cmd.Flags().GetString("context")
	pwStdin, _ := cmd.Flags().GetBool("password-stdin")

	// Resolve server
	server := serverFlag
	if server == "" {
		server = os.Getenv("VARROACTL_SERVER")
	}
	if server == "" {
		return fmt.Errorf("--server is required for username login")
	}
	server = strings.TrimRight(server, "/")

	c, err := client.New(server, "", client.WithUserAgent("varroactl/"+version))
	if err != nil {
		return err
	}

	// 1. GET /auth-config
	authCfg, err := c.GetAuthConfigWithResponse(cmd.Context())
	if err != nil {
		return err
	}
	if authCfg.HTTPResponse.StatusCode >= 400 {
		return fmt.Errorf("unable to determine auth mode")
	}
	if authCfg.JSON200 != nil && authCfg.JSON200.Mode == "oidc" {
		return fmt.Errorf("server uses OIDC; run \"varroactl login\" for the browser flow")
	}

	// 2. Get password
	var password string
	if pwStdin {
		data, err := readLine(os.Stdin)
		if err != nil {
			return err
		}
		password = strings.TrimRight(data, "\n\r")
	} else {
		_, _ = fmt.Fprintf(os.Stderr, "Password: ")
		pwBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr)
		password = string(pwBytes)
	}

	// 3. POST /login
	loginResp, err := c.LoginWithResponse(cmd.Context(), client.LoginJSONRequestBody{
		Username: username,
		Password: password,
	})
	if err != nil {
		return err
	}
	if loginResp.HTTPResponse.StatusCode == 401 {
		return fmt.Errorf("invalid credentials")
	}
	if loginResp.HTTPResponse.StatusCode == 429 {
		return fmt.Errorf("rate limited")
	}
	if loginResp.HTTPResponse.StatusCode >= 400 {
		msg := string(loginResp.Body)
		return fmt.Errorf("login failed: %s", msg)
	}
	if loginResp.JSON200 == nil {
		return fmt.Errorf("unexpected login response")
	}

	idToken := loginResp.JSON200.IdToken

	// 4. POST /me/apikeys with Bearer JWT
	hostname, _ := os.Hostname()
	keyName := "varroactl@" + hostname

	// Create a separate client with the JWT
	jwtClient, err := client.New(server, idToken, client.WithUserAgent("varroactl/"+version))
	if err != nil {
		return err
	}

	keyResp, err := jwtClient.CreateApiKeyWithResponse(cmd.Context(), client.CreateApiKeyJSONRequestBody{
		Name: &keyName,
	})
	if err != nil {
		return err
	}
	if keyResp.HTTPResponse.StatusCode >= 400 {
		apiErr := client.DecodeError(keyResp.HTTPResponse)
		return fmt.Errorf("failed to create API key: %s", apiErr.Message)
	}
	if keyResp.JSON201 == nil {
		return fmt.Errorf("unexpected API key response")
	}

	token := keyResp.JSON201.Token

	// 5. Validate and store (discard JWT, only vk_ token stored)
	return validateAndStore(cmd, server, token, ctxFlag)
}

// readLine reads one line from a reader.
func readLine(r interface{ Read([]byte) (int, error) }) (string, error) {
	scanner := bufio.NewScanner(r)
	if scanner.Scan() {
		return scanner.Text(), scanner.Err()
	}
	return "", scanner.Err()
}
