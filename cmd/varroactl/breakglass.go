// Package main: the break-glass command (design D7, delta break-glass-access).
//
// `break-glass NAME` requests a short-lived admin grant for a headless
// controller, then serves a loopback proxy that injects the granted token.
// The command talks to no cluster directly. A caller still needs a running
// `kubectl port-forward` to the controller's Service; the command prints
// that command so the runbook is one screen.
package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

func init() {
	registerRootCommand(registerBreakGlassCommand)
}

func registerBreakGlassCommand(root *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "break-glass NAME",
		Short: "Grant and proxy a break-glass admin session for a headless controller",
		Long: `Request a short-lived admin token for a headless controller, then serve a
loopback proxy that injects the token.

The command prints a "kubectl port-forward" command for the controller's
Service. Run that command in another terminal first, then open the printed
proxy URL. The proxy strips every inbound Authorization and Cookie header
before it forwards a request, so the browser holds no credential of its
own. The session ends, and the command exits, when the token expires or
when the command is interrupted.`,
		Args: cobra.ExactArgs(1),
		RunE: runBreakGlass,
	}
	cmd.Flags().String("reason", "", "reason for the grant, recorded on the audit event (required, at most 256 characters)")
	cmd.Flags().Duration("ttl", 15*time.Minute, "token lifetime")
	cmd.Flags().String("target", "http://127.0.0.1:8080", "address the proxy forwards to (the local end of kubectl port-forward)")
	cmd.Flags().String("listen", "127.0.0.1:0", "local loopback address the proxy listens on")
	_ = cmd.MarkFlagRequired("reason")
	addClusterFlag(cmd)
	root.AddCommand(cmd)
}

func runBreakGlass(cmd *cobra.Command, args []string) error {
	reason, _ := cmd.Flags().GetString("reason")
	ttl, _ := cmd.Flags().GetDuration("ttl")
	target, _ := cmd.Flags().GetString("target")
	listenAddr, _ := cmd.Flags().GetString("listen")

	targetURL, err := url.Parse(target)
	if err != nil || targetURL.Host == "" {
		return usagef("invalid --target %q: must be an absolute URL", target)
	}

	if err := refuseNonLoopbackListen(listenAddr); err != nil {
		return err
	}

	nFlag, _ := cmd.Flags().GetString("namespace")
	cFlag, _ := cmd.Flags().GetString("cluster")
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
	ns, name, err := resolveNSName(args[0], nFlag, rc.defaultNamespace)
	if err != nil {
		return err
	}
	cluster := resolveCluster(cFlag, rc.defaultCluster)

	c, err := apiClient(cmd)
	if err != nil {
		return err
	}

	ttlSeconds := ttlSecondsFromDuration(ttl)
	body := client.BreakGlassControllerJSONRequestBody{Reason: reason, TtlSeconds: &ttlSeconds}

	resp, err := c.BreakGlassControllerWithResponse(cmd.Context(), cluster, ns, name, body)
	if err != nil {
		return err
	}
	if resp.HTTPResponse.StatusCode >= 400 || resp.JSON200 == nil {
		return errFromResponse(resp.Body, resp.StatusCode())
	}
	grant := resp.JSON200

	l, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", listenAddr, err)
	}
	defer func() { _ = l.Close() }()

	proxyBase := "http://" + l.Addr().String()
	proxy := newBreakGlassProxy(targetURL, grant.Token, grant.LocationUrl, proxyBase)

	printBreakGlassPreflight(grant, targetURL, proxyBase)

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- http.Serve(l, proxy) }()

	ctx, cancel := signalCtx(cmd.Context())
	defer cancel()

	remaining := time.Until(grant.ExpiresAt)
	if remaining <= 0 {
		return nil
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return nil
	case <-timer.C:
		return nil
	case serveErr := <-serveErrCh:
		if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
			return serveErr
		}
		return nil
	}
}

// refuseNonLoopbackListen answers a usage error for a --listen host that
// does not resolve to a loopback address. It opens no listener. It reuses
// isLoopback (mcp.go), so both proxy commands apply the same rule.
func refuseNonLoopbackListen(listenAddr string) error {
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		host = listenAddr
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if !isLoopback(host) {
		return usagef("--listen host %q must resolve to a loopback address (127.0.0.1, ::1, or localhost)", host)
	}
	return nil
}

// ttlSecondsFromDuration converts a Go duration to the whole seconds the
// grant endpoint's ttlSeconds field expects.
func ttlSecondsFromDuration(d time.Duration) int {
	return int(d.Round(time.Second) / time.Second)
}

// printBreakGlassPreflight prints the kubectl port-forward command, the
// bound proxy URL, and the grant expiry, in one message.
func printBreakGlassPreflight(grant *client.ComponentsSchemasBreakGlassResponse, targetURL *url.URL, proxyBase string) {
	port := targetURL.Port()
	if port == "" {
		port = "80"
		if targetURL.Scheme == "https" {
			port = "443"
		}
	}
	_, _ = fmt.Fprintf(os.Stdout,
		"Run this in another terminal:\n  kubectl port-forward -n %s svc/%s %s:%d\n\n"+
			"Break-glass proxy listening on %s\nSession expires at %s\n",
		grant.Target.Namespace, grant.Target.Service, port, grant.Target.Port,
		proxyBase, grant.ExpiresAt.Format(time.RFC3339))
}

// ---------------------------------------------------------------------------
// The authenticating proxy
// ---------------------------------------------------------------------------

// newBreakGlassProxy builds the loopback proxy. It deletes every inbound
// Authorization and Cookie header, then sets the granted token as
// Authorization, so the browser holds no credential of its own. It
// forwards unbuffered (FlushInterval -1), and it rewrites a response
// header that carries the controller's in-cluster location URL.
//
// It builds the Director as a struct literal rather than reading and
// wrapping httputil.NewSingleHostReverseProxy's Director field, because
// that field is deprecated.
func newBreakGlassProxy(target *url.URL, token, locationBase, proxyBase string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = joinTargetPath(target.Path, req.URL.Path)
			req.Host = target.Host
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			req.Header.Set("Authorization", "Bearer "+token)
		},
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			rewriteBreakGlassResponseHeaders(resp.Header, locationBase, proxyBase)
			return nil
		},
	}
}

// joinTargetPath joins the target URL's own path (normally empty) with the
// inbound request path, keeping exactly one "/" at the join.
func joinTargetPath(base, reqPath string) string {
	if base == "" {
		return reqPath
	}
	baseSlash := strings.HasSuffix(base, "/")
	reqSlash := strings.HasPrefix(reqPath, "/")
	switch {
	case baseSlash && reqSlash:
		return base + reqPath[1:]
	case !baseSlash && !reqSlash:
		return base + "/" + reqPath
	default:
		return base + reqPath
	}
}

// rewriteBreakGlassResponseHeaders rewrites Location, Content-Location, and
// Refresh headers that carry the controller's in-cluster location URL. A
// browser cannot reach that address; it can reach the proxy.
func rewriteBreakGlassResponseHeaders(h http.Header, locationBase, proxyBase string) {
	for _, name := range []string{"Location", "Content-Location"} {
		if v := h.Get(name); v != "" {
			h.Set(name, rewriteLocationPrefix(v, locationBase, proxyBase))
		}
	}
	if v := h.Get("Refresh"); v != "" {
		h.Set("Refresh", rewriteRefreshHeader(v, locationBase, proxyBase))
	}
}

// rewriteLocationPrefix replaces a value's locationBase prefix with
// proxyBase. It keeps the remaining path, query, and fragment untouched. A
// value that does not carry the prefix passes through unchanged.
func rewriteLocationPrefix(value, locationBase, proxyBase string) string {
	if locationBase == "" || !strings.HasPrefix(value, locationBase) {
		return value
	}
	remainder := strings.TrimPrefix(value, locationBase)
	return strings.TrimSuffix(proxyBase, "/") + "/" + remainder
}

// rewriteRefreshHeader rewrites only the url parameter of a Refresh header.
// The value starts with a delay, such as "0; url=<location URL>/manage".
func rewriteRefreshHeader(value, locationBase, proxyBase string) string {
	delay, param, ok := strings.Cut(value, ";")
	if !ok {
		return value
	}
	trimmed := strings.TrimSpace(param)
	if len(trimmed) < 4 || !strings.EqualFold(trimmed[:4], "url=") {
		return value
	}
	urlValue := trimmed[4:]
	quoted := len(urlValue) >= 2 && strings.HasPrefix(urlValue, `"`) && strings.HasSuffix(urlValue, `"`)
	if quoted {
		urlValue = urlValue[1 : len(urlValue)-1]
	}
	rewritten := rewriteLocationPrefix(urlValue, locationBase, proxyBase)
	if quoted {
		rewritten = `"` + rewritten + `"`
	}
	return delay + "; url=" + rewritten
}
