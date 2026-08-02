package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client/stream"
)

func init() {
	registerRootCommand(func(root *cobra.Command) {
		mcpCmd := &cobra.Command{
			Use:   "mcp",
			Short: "MCP stdio bridge (default) or proxy server",
			Long: `MCP protocol bridge for the Varroa BFF.

Default mode: reads JSON-RPC messages from stdin and forwards them to the
BFF MCP endpoint, writing responses to stdout. All diagnostics go to stderr.

Subcommand:
  serve --listen ADDR  Start an HTTP reverse proxy for MCP.`,
			RunE: runMCP,
		}
		serveCmd := &cobra.Command{
			Use:   "serve",
			Short: "Start MCP HTTP proxy server",
			RunE:  runMCPServe,
		}
		serveCmd.Flags().String("listen", "127.0.0.1:0", "Listen address")
		mcpCmd.AddCommand(serveCmd)
		root.AddCommand(mcpCmd)
	})
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func resolveMCPServer(cmd *cobra.Command) (server, token string, err error) {
	rc, cerr := resolveContext(func(name string) string {
		f := cmd.Flag(name)
		if f == nil {
			return ""
		}
		return f.Value.String()
	})
	if cerr != nil {
		return "", "", cerr
	}
	return strings.TrimRight(rc.server, "/"), rc.apiKey, nil
}

// ---------------------------------------------------------------------------
// 9.1 stdio bridge (default run)
// ---------------------------------------------------------------------------

func runMCP(cmd *cobra.Command, args []string) error {
	server, token, err := resolveMCPServer(cmd)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpc := &http.Client{
		Transport: &http.Transport{
			MaxConnsPerHost: 16,
		},
		Timeout: 0, // no timeout — MCP tools can be slow
	}

	var outMu sync.Mutex
	writeLine := func(b []byte) {
		outMu.Lock()
		_, _ = os.Stdout.Write(append(b, '\n'))
		outMu.Unlock()
	}

	var wg sync.WaitGroup

	// Stdin reader goroutine — sc.Scan() blocks on the fd and is NOT
	// unblocked by ctx cancellation, so we use a select over ctx.Done()
	// + a lines channel for Ctrl-C to interrupt between messages.
	lines := make(chan []byte, 16)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 0, 64<<10), 10<<20)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) > 0 {
				lines <- append([]byte(nil), line...)
			}
		}
		close(lines)
	}()

	for {
		var msg []byte
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case m, ok := <-lines:
			if !ok {
				wg.Wait()
				return nil
			}
			msg = m
		}

		wg.Add(1)
		go func(msg []byte) {
			defer wg.Done()

			fullURL := server + "/api/v1/mcp"
			req, err := http.NewRequestWithContext(ctx, "POST", fullURL, bytes.NewReader(msg))
			if err != nil {
				writeLine(rpcError(msg, 0, "varroa BFF unreachable: "+err.Error()))
				return
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("User-Agent", "varroactl/"+version)

			resp, err := httpc.Do(req)
			if err != nil {
				writeLine(rpcError(msg, 0, "varroa BFF unreachable: "+err.Error()))
				return
			}
			defer func() { _ = resp.Body.Close() }()

			ct := resp.Header.Get("Content-Type")

			switch {
			case resp.StatusCode == 202 || resp.StatusCode == 204:
				// notification ack — nothing to emit
			case resp.StatusCode >= 200 && resp.StatusCode < 300 && strings.HasPrefix(ct, "application/json"):
				body, _ := io.ReadAll(resp.Body)
				if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 {
					writeLine(trimmed) // NDJSON: exactly one line per message
				}
			case resp.StatusCode >= 200 && resp.StatusCode < 300 && strings.HasPrefix(ct, "text/event-stream"):
				_ = stream.ParseSSE(resp.Body, func(e stream.Event) error {
					writeLine(e.Data)
					return nil
				})
			default:
				body, _ := io.ReadAll(resp.Body)
				writeLine(rpcError(msg, resp.StatusCode, n1Message(body)))
			}
		}(msg)
	}
}

// rpcError constructs a JSON-RPC error response from the original message.
func rpcError(orig []byte, status int, errMsg string) []byte {
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(orig, &probe)
	id := probe.ID
	if id == nil {
		id = json.RawMessage("null")
	}

	errResp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    -32603,
			"message": fmt.Sprintf("varroa BFF error (%d): %s", status, errMsg),
		},
	}
	b, _ := json.Marshal(errResp)
	return b
}

// n1Message extracts the N1 {"error":"..."} envelope from a response body.
func n1Message(body []byte) string {
	var env struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error != "" {
		return env.Error
	}
	if len(body) > 512 {
		body = body[:512]
	}
	return string(body)
}

// ---------------------------------------------------------------------------
// 9.2 mcp serve --listen ADDR
// ---------------------------------------------------------------------------

func runMCPServe(cmd *cobra.Command, args []string) error {
	listenAddr, _ := cmd.Flags().GetString("listen")

	// Validate host resolves to loopback
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

	server, token, err := resolveMCPServer(cmd)
	if err != nil {
		return err
	}

	targetURL, err := url.Parse(server + "/api/v1/mcp")
	if err != nil {
		return fmt.Errorf("invalid server URL: %w", err)
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.URL.Path = targetURL.Path
			// Keep original query
			req.Host = targetURL.Host
			// Strip client auth, inject our own
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("User-Agent", "varroactl/"+version)
		},
		FlushInterval: -1, // SSE-style responses stream through unbuffered
	}

	l, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", listenAddr, err)
	}
	defer func() { _ = l.Close() }()

	boundAddr := l.Addr().String()
	_, _ = fmt.Fprintf(os.Stdout, "MCP proxy listening on http://%s\n", boundAddr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// NotifyContext swallows the signal; close the listener so Serve returns.
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()

	if err := http.Serve(l, proxy); err != nil {
		select {
		case <-ctx.Done():
			return nil
		default:
			return err
		}
	}
	return nil
}

// isLoopback checks if the host resolves to a loopback address.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ips, err := net.LookupHost(host)
	if err != nil {
		return false
	}
	for _, ip := range ips {
		parsed := net.ParseIP(ip)
		if parsed != nil && parsed.IsLoopback() {
			return true
		}
	}
	return false
}
