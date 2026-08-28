// Package jenkins provides an HTTP client for the Jenkins API. It is used
// by mite sidecars to manage Jenkins instances.
package jenkins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// retryAfterCap is the maximum sleep duration honoured from a
// Retry-After response header, to bound server-requested delays.
const retryAfterCap = 30 * time.Second

// ErrItemNotFound is returned by DeleteItem when the item does not exist (404).
// Callers can use errors.Is to distinguish not-found from other errors.
var ErrItemNotFound = errors.New("jenkins: item not found")

// Client is an HTTP client for the Jenkins API with basic auth and CSRF crumb support.
// It applies rate limiting (5 req/s, burst 10) and retries transient failures
// (429, 502, 503, 504, and transport errors) up to 3 times with exponential backoff.
type Client struct {
	baseURL    string
	httpClient *http.Client
	token      string
	username   string

	// tokenFunc is an optional function that returns the current Bearer token.
	// When set, it is called on every request instead of using the static token
	// field. This enables long-lived clients whose token rotates over time.
	tokenFunc func() string

	// OnTokenExpired is an optional callback fired when a 401/403 is received
	// and the current token is expired. The client will call this to trigger a
	// token refresh before re-establishing the session. If nil, the client
	// retries with the current token (which may still be valid but the session
	// was lost).
	OnTokenExpired func()

	// sessionCookie stores the Jenkins session cookie (JSESSIONID) so it can
	// be re-sent on subsequent requests. We manage this manually because Go's
	// cookiejar does not reliably carry Jetty 12's JSESSIONID.<hash> cookies
	// (the cookie name varies per node, and the jar may reject it on domain
	// or path mismatches).
	sessionCookie   string
	sessionCookieMu sync.Mutex

	crumb   string
	crumbMu sync.Mutex

	// Rate limiting: token bucket shared across all API calls through this client.
	limiter *rate.Limiter

	// Retry behaviour for transient HTTP failures (429, 5xx) and transport errors.
	maxRetries    int
	retryBaseWait time.Duration
	retryMaxWait  time.Duration

	Logger *slog.Logger
}

// Info contains basic information about a Jenkins instance.
type Info struct {
	Version      string `json:"version"`
	Mode         string `json:"mode"`
	NumExecutors int    `json:"numExecutors"`
	QuietingDown bool   `json:"quietingDown"`
	UseSecurity  bool   `json:"useSecurity"`
}

// SetTokenFunc sets a dynamic token source. When set, the client calls this
// function on every request instead of using the static token field.
func (c *Client) SetTokenFunc(fn func() string) {
	c.tokenFunc = fn
}

// currentToken returns the token to use for the current request. When a
// dynamic token function is set it takes precedence; otherwise the static
// token set at construction is used.
func (c *Client) currentToken() string {
	if c.tokenFunc != nil {
		return c.tokenFunc()
	}
	return c.token
}

// NewClient creates a new Jenkins API client with a 30-second timeout,
// rate limiting (5 req/s, burst 10), and retry for transient failures.
// The baseURL should not include a trailing slash.
func NewClient(baseURL, username, token string) *Client {
	// No cookie jar: session reuse is handled explicitly via c.sessionCookie
	// (captured from responses, re-sent only by do()). A jar would auto-attach
	// the JSESSIONID to EVERY request — including crumb fetches, which must
	// authenticate via Bearer alone. A stale post-restart JSESSIONID on a crumb
	// fetch trips the plugin's hasExistingSession check, skipping JWT validation
	// and yielding anonymous 403s. Go's cookiejar is also unreliable for Jetty's
	// JSESSIONID.<node-hash> names, which is why the manual mechanism exists.
	return &Client{
		baseURL:       strings.TrimRight(baseURL, "/"),
		httpClient:    &http.Client{Timeout: 30 * time.Second},
		token:         token,
		username:      username,
		limiter:       rate.NewLimiter(rate.Limit(5), 10),
		maxRetries:    3,
		retryBaseWait: 500 * time.Millisecond,
		retryMaxWait:  10 * time.Second,
	}
}

// ApplyConfig applies a JCasC YAML configuration to Jenkins via the
// Configuration-as-Code plugin endpoint. It returns an error if the
// response is not 2xx.
func (c *Client) ApplyConfig(ctx context.Context, jcascYAML string) error {
	if c.Logger != nil {
		c.Logger.Debug("jenkins: apply config", "len", len(jcascYAML))
	}
	body := bytes.NewBufferString(jcascYAML)
	resp, err := c.do(ctx, http.MethodPost, "/configuration-as-code/apply/", body, "application/yaml")
	if err != nil {
		return fmt.Errorf("apply config: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("apply config: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	return nil
}

// Reload triggers a JCasC reload by POSTing to /configuration-as-code/reload.
// This re-reads the bundle from the CASC_JENKINS_CONFIG directory without
// requiring a Jenkins restart.  It uses the same operator-JWT auth as
// ApplyConfig and returns the HTTP error to the caller.
func (c *Client) Reload(ctx context.Context) error {
	if c.Logger != nil {
		c.Logger.Debug("jenkins: reload config")
	}
	resp, err := c.do(ctx, http.MethodPost, "/configuration-as-code/reload", nil, "")
	if err != nil {
		return fmt.Errorf("reload config: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("reload config: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	return nil
}

// CheckConfig validates a JCasC document against the live Jenkins instance
// without applying it (dry-run, POST /configuration-as-code/check). Returns a
// non-nil error when the document is rejected.
//
// Endpoint contract (confirmed from plugin source):
//   - HTTP 400 (SC_BAD_REQUEST): ConfiguratorException → error with JSON body
//   - HTTP 200 + non-empty JSON array: validation issues (warnings/errors)
//   - HTTP 200 + empty JSON array: document is valid
//
// The safe default is to treat only non-2xx as failure (status code is sole
// authority). The 200+issues case is informational and should not block apply.
func (c *Client) CheckConfig(ctx context.Context, jcascYAML string) error {
	if c.Logger != nil {
		c.Logger.Debug("jenkins: check config", "len", len(jcascYAML))
	}
	body := bytes.NewBufferString(jcascYAML)
	resp, err := c.do(ctx, http.MethodPost, "/configuration-as-code/check", body, "application/yaml")
	if err != nil {
		return fmt.Errorf("check config: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("check config: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if looksLikeCheckFailure(msg) {
		return fmt.Errorf("check config: %s", strings.TrimSpace(string(msg)))
	}
	return nil
}

// looksLikeCheckFailure returns true when a 200-response body signals a check
// failure. Currently a no-op stub; the sole authority is the status code.
func looksLikeCheckFailure(_ []byte) bool { return false }

// ExportConfig returns the full live JCasC document via POST /configuration-as-code/export.
// The body is read with an 8 MiB cap. Non-2xx status returns a formatted error.
func (c *Client) ExportConfig(ctx context.Context) (string, error) {
	if c.Logger != nil {
		c.Logger.Debug("jenkins: export config")
	}
	resp, err := c.do(ctx, http.MethodPost, "/configuration-as-code/export", nil, "")
	if err != nil {
		return "", fmt.Errorf("export config: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return "", fmt.Errorf("export config: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", fmt.Errorf("export config: read body: %w", err)
	}
	return string(body), nil
}

// WaitForJenkins polls the Jenkins login endpoint at 2-second intervals
// until it responds with HTTP 200. It returns nil when Jenkins is ready,
// or an error if the context is cancelled.
func (c *Client) WaitForJenkins(ctx context.Context) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		resp, err := c.do(ctx, http.MethodGet, "/login", nil, "")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for jenkins: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// SessionID returns the X-Jenkins-Session response header from the login
// endpoint. Jenkins regenerates this value on every JVM boot, so a change
// proves Jenkins restarted since the value was last observed (and therefore
// that init.groovy.d has re-run and reset live configuration).
func (c *Client) SessionID(ctx context.Context) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/login", nil, "")
	if err != nil {
		return "", fmt.Errorf("session id: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	s := resp.Header.Get("X-Jenkins-Session")
	if s == "" {
		return "", fmt.Errorf("session id: no X-Jenkins-Session header (HTTP %d)", resp.StatusCode)
	}
	return s, nil
}

// DoGet sends a GET request to the given path and returns the
// response. It applies the same auth, rate limiting, and retry
// behaviour as the internal do method.
func (c *Client) DoGet(ctx context.Context, path string) (*http.Response, error) {
	return c.do(ctx, http.MethodGet, path, nil, "")
}

// JobSummary holds a bounded snapshot of Jenkins jobs and recent builds.
type JobSummary struct {
	TotalJobs     int               `json:"totalJobs"`
	RunningBuilds int               `json:"runningBuilds"`
	RecentBuilds  []JobBuildSummary `json:"recentBuilds"`
}

// JobBuildSummary holds a single recent build entry.
type JobBuildSummary struct {
	JobName         string `json:"jobName"`
	BuildNumber     int    `json:"buildNumber"`
	Status          string `json:"status"`
	StartedAt       string `json:"startedAt,omitempty"`
	DurationSeconds int    `json:"durationSeconds,omitempty"`
	URL             string `json:"url,omitempty"`
}

// GetJobSummary fetches a bounded job summary from Jenkins.
// Uses a tree query to get job names, statuses, and last build details
// without traversing deep job trees.
func (c *Client) GetJobSummary(ctx context.Context) (*JobSummary, error) {
	resp, err := c.do(ctx, http.MethodGet,
		"/api/json?tree=jobs[name,color,lastBuild[number,result,timestamp,duration,url]]", nil, "")
	if err != nil {
		return nil, fmt.Errorf("get job summary: %w", err)
	}
	defer c.closeResponseBody(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get job summary: HTTP %d", resp.StatusCode)
	}
	var raw struct {
		Jobs []struct {
			Name      string `json:"name"`
			Color     string `json:"color"`
			LastBuild *struct {
				Number    int    `json:"number"`
				Result    string `json:"result"`
				Timestamp int64  `json:"timestamp"`
				Duration  int    `json:"duration"`
				URL       string `json:"url"`
			} `json:"lastBuild"`
		} `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode job summary: %w", err)
	}
	summary := &JobSummary{TotalJobs: len(raw.Jobs)}
	for _, j := range raw.Jobs {
		if j.Color != "" && strings.Contains(j.Color, "anime") {
			summary.RunningBuilds++
		}
		if j.LastBuild != nil {
			status := ""
			switch {
			case j.LastBuild.Result == "SUCCESS":
				status = "SUCCESS"
			case j.LastBuild.Result == "FAILURE":
				status = "FAILURE"
			case j.LastBuild.Result == "ABORTED":
				status = "ABORTED"
			case j.LastBuild.Result != "":
				status = j.LastBuild.Result
			default:
				status = "UNKNOWN"
			}
			startedAt := ""
			if j.LastBuild.Timestamp > 0 {
				startedAt = time.Unix(j.LastBuild.Timestamp/1000, 0).Format(time.RFC3339)
			}
			summary.RecentBuilds = append(summary.RecentBuilds, JobBuildSummary{
				JobName:         j.Name,
				BuildNumber:     j.LastBuild.Number,
				Status:          status,
				StartedAt:       startedAt,
				DurationSeconds: j.LastBuild.Duration / 1000,
				URL:             j.LastBuild.URL,
			})
		}
	}
	return summary, nil
}

// GetPluginManager fetches the installed plugin list from Jenkins.
// It calls /pluginManager/api/json?depth=2, which returns shortName,
// version, enabled, detached, bundled, and dependencies (as objects
// carrying the optional flag). depth=2 is required: at depth=1 each
// dependency renders as a bare string.
func (c *Client) GetPluginManager(ctx context.Context) ([]APIPlugin, error) {
	resp, err := c.do(ctx, http.MethodGet, "/pluginManager/api/json?depth=2", nil, "")
	if err != nil {
		return nil, fmt.Errorf("get plugin manager: %w", err)
	}
	defer c.closeResponseBody(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get plugin manager: HTTP %d", resp.StatusCode)
	}

	var raw struct {
		Plugins []APIPlugin `json:"plugins"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode plugin manager: %w", err)
	}
	return raw.Plugins, nil
}

// APIPlugin is the shape of a single plugin entry from
// /pluginManager/api/json?depth=2. It is also the return element type of
// GetPluginManager. The Dependencies field is []json.RawMessage: at depth=2
// each element is a JSON object {name, version, optional}; at depth=1 it is
// a JSON string (the bare plugin name). Callers must validate the shape.
type APIPlugin struct {
	ShortName    string            `json:"shortName"`
	Version      string            `json:"version"`
	Enabled      bool              `json:"enabled"`
	Detached     bool              `json:"detached"`
	Bundled      bool              `json:"bundled"`
	Dependencies []json.RawMessage `json:"dependencies"`
}

// ScriptConsole runs a Groovy script on the Jenkins script console and
// returns the output. It authenticates using the client's basic auth.
func (c *Client) ScriptConsole(ctx context.Context, script string) (string, error) {
	body := strings.NewReader("script=" + url.QueryEscape(script))
	resp, err := c.do(ctx, http.MethodPost, "/scriptText", body, "application/x-www-form-urlencoded")
	if err != nil {
		return "", fmt.Errorf("script console: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read script output: %w", err)
	}
	return string(b), nil
}

// ScriptConsoleOnce runs a Groovy script on the Jenkins script console and
// returns the output, making exactly one HTTP attempt (no retry, no session
// re-establishment on 401/403, no rate-limiting). It is intended for
// executeGroovy BroodOperations where the caller provides its own context
// deadline and retry/no-retry policy.
//
// The CSRF crumb is still fetched (via ensureCrumb, shared with do()) before
// the single POST, so callers that follow a ScriptConsoleOnce with other
// requests benefit from the cached crumb — but the POST itself uses a
// request-scoped HTTP client (Timeout: 0) so the caller's context deadline
// is the sole timeout authority, not the Client's fixed 30s.
func (c *Client) ScriptConsoleOnce(ctx context.Context, script string) (string, error) {
	if err := c.ensureCrumb(ctx); err != nil {
		return "", fmt.Errorf("script console once: ensure crumb: %w", err)
	}

	body := strings.NewReader("script=" + url.QueryEscape(script))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/scriptText", body)
	if err != nil {
		return "", fmt.Errorf("script console once: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// Auth — mirror do()'s token-shape logic.
	tkn := c.currentToken()
	if len(tkn) > 10 && (tkn[:3] == "eyJ" || c.username == "Bearer") {
		req.Header.Set("Authorization", "Bearer "+tkn)
	} else {
		req.SetBasicAuth(c.username, tkn)
	}

	// CSRF crumb header.
	if c.crumb != "" {
		req.Header.Set("Jenkins-Crumb", c.crumb)
	}

	// Re-send the session cookie if we have one — mirror do() exactly.
	c.sessionCookieMu.Lock()
	sc := c.sessionCookie
	c.sessionCookieMu.Unlock()
	if sc != "" {
		req.Header.Set("Cookie", sc)
	}

	// Request-scoped client with no timeout — the context deadline is the
	// caller's authority (the controller passes a groovyCallTimeout context).
	reqClient := &http.Client{}
	resp, err := reqClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("script console once: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("script console once: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("script console once: read body: %w", err)
	}
	return string(b), nil
}

// SafeRestart triggers a safe restart on the Jenkins instance and then
// waits for it to become ready again.
func (c *Client) SafeRestart(ctx context.Context) error {
	if c.Logger != nil {
		c.Logger.Debug("jenkins: safe restart")
	}
	resp, err := c.do(ctx, http.MethodPost, "/safeRestart", nil, "")
	if err != nil {
		return fmt.Errorf("safe restart: %w", err)
	}
	_ = resp.Body.Close()

	return c.WaitForJenkins(ctx)
}

// QuietDown puts Jenkins into quiet-down mode, preventing new builds from
// starting. Running builds continue to completion.
func (c *Client) QuietDown(ctx context.Context) error {
	if c.Logger != nil {
		c.Logger.Debug("jenkins: quiet down")
	}
	resp, err := c.do(ctx, http.MethodPost, "/quietDown", nil, "")
	if err != nil {
		return fmt.Errorf("quiet down: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("quiet down: HTTP %d", resp.StatusCode)
	}
	return nil
}

// CancelQuietDown cancels the quiet-down mode, allowing new builds to start.
func (c *Client) CancelQuietDown(ctx context.Context) error {
	if c.Logger != nil {
		c.Logger.Debug("jenkins: cancel quiet down")
	}
	resp, err := c.do(ctx, http.MethodPost, "/cancelQuietDown", nil, "")
	if err != nil {
		return fmt.Errorf("cancel quiet down: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("cancel quiet down: HTTP %d", resp.StatusCode)
	}
	return nil
}

// GetInfo retrieves basic Jenkins instance information from the /api/json
// endpoint.
func (c *Client) GetInfo(ctx context.Context) (*Info, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/json", nil, "")
	if err != nil {
		return nil, fmt.Errorf("get info: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get info: HTTP %d", resp.StatusCode)
	}

	var info Info
	info.Version = resp.Header.Get("X-Jenkins")
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode info: %w", err)
	}
	return &info, nil
}

// SetRBAC applies RBAC YAML configuration via the role-strategy plugin
// endpoint.
func (c *Client) SetRBAC(ctx context.Context, yaml string) error {
	if c.Logger != nil {
		c.Logger.Debug("jenkins: set rbac", "len", len(yaml))
	}
	// The generated block is a full JCasC authorizationStrategy.roleBased
	// document. Applying it through the configuration-as-code endpoint makes
	// RoleBasedAuthorizationStrategy the *active* strategy and loads the roles
	// atomically — unlike /role-strategy/update/, which is not a JCasC apply
	// path and does not switch the active authorization strategy.
	body := bytes.NewBufferString(yaml)
	resp, err := c.do(ctx, http.MethodPost, "/configuration-as-code/apply/", body, "application/yaml")
	if err != nil {
		return fmt.Errorf("set rbac: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("set rbac: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	return nil
}

// waitRateLimit blocks until the rate limiter allows a request or the context
// is cancelled.
func (c *Client) waitRateLimit(ctx context.Context) error {
	if c.limiter != nil {
		return c.limiter.Wait(ctx)
	}
	return nil
}

// retryableStatusCode reports whether an HTTP status code is a transient
// failure that should be retried.
func retryableStatusCode(code int) bool {
	switch code {
	case 429, 502, 503, 504:
		return true
	}
	return false
}

// parseRetryAfter parses the Retry-After response header. It supports
// both a delay in seconds ("120") and an HTTP-date.
func parseRetryAfter(resp *http.Response) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// calcBackoff returns the backoff duration for retry attempt n (1-indexed),
// with 25% jitter to spread out retry storms.
func (c *Client) calcBackoff(attempt int) time.Duration {
	backoff := c.retryBaseWait * time.Duration(1<<(attempt-1))
	if backoff > c.retryMaxWait {
		backoff = c.retryMaxWait
	}
	if backoff <= 0 {
		return 0
	}
	// If the backoff is too small to compute meaningful jitter
	// (< 4 ns), return it as-is to avoid rand.Int64N(0) panics.
	if backoff < 4 {
		return backoff
	}
	// Add ±25% jitter.
	jitter := time.Duration(rand.Int64N(int64(backoff) / 4))
	if rand.IntN(2) == 0 {
		return backoff + jitter
	}
	return backoff - jitter
}

// ensureCrumb fetches a CSRF crumb from Jenkins if one is not already
// cached. The crumb is associated with a session cookie (handled by the
// client's cookie jar).
func (c *Client) ensureCrumb(ctx context.Context) error {
	c.crumbMu.Lock()
	defer c.crumbMu.Unlock()

	if c.crumb != "" {
		return nil
	}

	// Rate-limit the crumb fetch to avoid hammering Jenkins.
	if err := c.waitRateLimit(ctx); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/crumbIssuer/api/json", nil)
	if err != nil {
		return fmt.Errorf("crumb request: %w", err)
	}
	tkn := c.currentToken()
	if len(tkn) > 10 && (tkn[:3] == "eyJ" || c.username == "Bearer") {
		req.Header.Set("Authorization", "Bearer "+tkn)
	} else {
		req.SetBasicAuth(c.username, tkn)
	}
	// Do NOT send the session cookie on crumb fetches. A stale
	// JSESSIONID from a previous Jenkins JVM tricks the plugin's
	// hasExistingSession check into skipping JWT validation, causing
	// 403. The crumb endpoint authenticates via Bearer/JWT alone.

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch crumb: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		// CSRF protection is disabled — no crumb needed.
		c.crumb = "disabled"
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch crumb: HTTP %d", resp.StatusCode)
	}

	var result struct {
		Crumb             string `json:"crumb"`
		CrumbRequestField string `json:"crumbRequestField"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode crumb: %w", err)
	}

	c.crumb = result.Crumb
	return nil
}

// do is an internal helper that builds and executes an HTTP request with
// auth, CSRF protection, rate limiting, and retry for transient failures.
// For state-changing methods (POST, PUT, DELETE) it fetches a CSRF crumb
// and includes it in the request headers.
//
// Transient network errors and selected HTTP status codes (429, 502, 503,
// 504) are retried up to maxRetries times with exponential backoff. 401 and
// 403 trigger a session re-establishment: the cached crumb and session cookie
// are cleared, the current Bearer token is re-presented, and the request is
// retried once. The Retry-After response header is honoured when present.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	// Rate-limit before we do anything else.
	if err := c.waitRateLimit(ctx); err != nil {
		return nil, err
	}

	// Buffer request body so it can be replayed across retries.
	var bodyBuf []byte
	if body != nil {
		var err error
		bodyBuf, err = io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
	}

	// Fetch crumb ahead of the retry loop — the crumb is cached so
	// subsequent calls inside the loop are a no-op. This also rate-limits
	// the crumb fetch (inside ensureCrumb).
	if method == http.MethodPost || method == http.MethodPut || method == http.MethodDelete {
		if err := c.ensureCrumb(ctx); err != nil {
			return nil, fmt.Errorf("ensure crumb: %w", err)
		}
	}

	var lastErr error
	var retryAfter time.Duration
	var didSessionRecover bool

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			// Compute backoff, honouring Retry-After from the last response.
			backoff := c.calcBackoff(attempt)
			if retryAfter > 0 {
				// Use the larger of our backoff and the server's Retry-After.
				if retryAfter > backoff {
					backoff = retryAfter
				}
				if backoff > retryAfterCap {
					backoff = retryAfterCap
				}
			}
			if !sleepContext(ctx, backoff) {
				return nil, ctx.Err()
			}

			// Re-rate-limit before retrying.
			if err := c.waitRateLimit(ctx); err != nil {
				return nil, err
			}
		}

		// Build a fresh request (body reader is consumed once per attempt).
		url := c.baseURL + path
		var reqBody io.Reader
		if bodyBuf != nil {
			reqBody = bytes.NewReader(bodyBuf)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}

		// Auth — use the dynamic token function if set.
		tkn := c.currentToken()
		if len(tkn) > 10 && (tkn[:3] == "eyJ" || c.username == "Bearer") {
			req.Header.Set("Authorization", "Bearer "+tkn)
		} else {
			req.SetBasicAuth(c.username, tkn)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}

		// CSRF header for state-changing requests.
		if method == http.MethodPost || method == http.MethodPut || method == http.MethodDelete {
			if c.crumb != "" {
				req.Header.Set("Jenkins-Crumb", c.crumb)
			}
		}

		// Re-send the session cookie if we have one.
		c.sessionCookieMu.Lock()
		sc := c.sessionCookie
		c.sessionCookieMu.Unlock()
		if sc != "" {
			req.Header.Set("Cookie", sc)
			if c.Logger != nil {
				c.Logger.Debug("jenkins: sending session cookie", "cookie", sc[:min(30, len(sc))])
			}
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			retryAfter = 0
			continue
		}

		// Capture a JSESSIONID cookie from the response so it can be re-sent
		// on subsequent requests. Jetty 12 uses `JSESSIONID.<node-hash>` as
		// the cookie name; Go's cookiejar does not reliably carry these.
		for _, ck := range resp.Cookies() {
			if strings.HasPrefix(ck.Name, "JSESSIONID") && ck.Value != "" {
				c.sessionCookieMu.Lock()
				prev := c.sessionCookie
				c.sessionCookie = ck.Name + "=" + ck.Value
				c.sessionCookieMu.Unlock()
				if c.Logger != nil && c.sessionCookie != prev {
					c.Logger.Debug("jenkins: captured session cookie", "name", ck.Name)
				}
				break
			}
		}

		// Handle 401/403: session may have been lost. Clear the crumb and
		// session cookie, re-present the Bearer token to re-establish, and
		// retry once. This path is independent of the main retry counters
		// so a session recovery does not consume a retry budget meant for
		// transient network failures.
		if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && !didSessionRecover {
			didSessionRecover = true
			c.closeResponseBody(resp)

			// If the token may be expired, trigger a refresh before re-establishing.
			if c.OnTokenExpired != nil {
				c.OnTokenExpired()
			}

			// Clear cached crumb and session cookie so the next attempt
			// re-establishes them with the (potentially refreshed) token.
			c.crumbMu.Lock()
			c.crumb = ""
			c.crumbMu.Unlock()
			c.sessionCookieMu.Lock()
			c.sessionCookie = ""
			c.sessionCookieMu.Unlock()

			// For state-changing requests, re-fetch the crumb now (it was
			// cleared above and ensureCrumb runs only before the retry loop).
			// Without this the retried POST/PUT/DELETE would be sent with no
			// crumb and fail CSRF validation again.
			if method == http.MethodPost || method == http.MethodPut || method == http.MethodDelete {
				if err := c.ensureCrumb(ctx); err != nil {
					return nil, fmt.Errorf("ensure crumb after session recovery: %w", err)
				}
			}

			continue
		}

		if !retryableStatusCode(resp.StatusCode) {
			// Success or non-retryable status — return to caller.
			return resp, nil
		}

		// Retryable status: parse Retry-After. On the last attempt,
		// return the response with body intact so callers can read
		// the error message. For intermediate attempts, drain and
		// close so connections are recycled.
		retryAfter = parseRetryAfter(resp)
		if attempt == c.maxRetries {
			return resp, nil
		}
		c.closeResponseBody(resp)
		lastErr = nil
	}

	// Exhausted retries (transport errors only — retryable HTTP
	// statuses return from inside the loop above).
	return nil, fmt.Errorf("request failed after %d retries: %w", c.maxRetries, lastErr)
}

// closeResponseBody drains and closes an HTTP response body, logging any
// error at debug level.
func (c *Client) closeResponseBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)); err != nil && c.Logger != nil {
		c.Logger.Debug("jenkins: error draining response body", "err", err)
	}
	if err := resp.Body.Close(); err != nil && c.Logger != nil {
		c.Logger.Debug("jenkins: error closing response body", "err", err)
	}
}

// sleepContext sleeps for d or until ctx is cancelled, whichever happens
// first. It returns true if the sleep completed, false if ctx was cancelled.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// jobPath returns the URL path segments for a Jenkins item (e.g. "job/foo/job/bar"
// for path "foo/bar"). Callers prepend "/" to form a full API path.
func jobPath(itemPath string) string {
	parts := strings.Split(itemPath, "/")
	segments := make([]string, 0, 2*len(parts))
	for _, p := range parts {
		segments = append(segments, "job", url.PathEscape(p))
	}
	return strings.Join(segments, "/")
}

// CreateItem creates a new Jenkins item at the given path (e.g. "folder/job")
// using the provided config.xml body. The class is the Jenkins Java class name
// (e.g. "com.cloudbees.hudson.plugins.folder.Folder").
func (c *Client) CreateItem(ctx context.Context, itemPath, class, configXML string) error {
	if c.Logger != nil {
		c.Logger.Debug("jenkins: create item", "path", itemPath, "class", class)
	}
	parts := strings.Split(itemPath, "/")
	name := parts[len(parts)-1]
	var apiURL string
	if len(parts) > 1 {
		parentPath := strings.Join(parts[:len(parts)-1], "/")
		apiURL = "/" + jobPath(parentPath) + "/createItem?name=" + url.QueryEscape(name)
	} else {
		apiURL = "/createItem?name=" + url.QueryEscape(name)
	}
	if class != "" {
		apiURL += "&mode=" + url.QueryEscape(class)
	}
	body := bytes.NewBufferString(configXML)
	resp, err := c.do(ctx, http.MethodPost, apiURL, body, "text/xml")
	if err != nil {
		return fmt.Errorf("create item %s: %w", itemPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("create item %s: HTTP %d: %s", itemPath, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	return nil
}

// GetItemConfig retrieves the config.xml for an item at the given path.
// Returns the XML body and a boolean indicating whether the item exists (true)
// or was not found (false, nil error).
func (c *Client) GetItemConfig(ctx context.Context, itemPath string) (string, bool, error) {
	resp, err := c.do(ctx, http.MethodGet, "/"+jobPath(itemPath)+"/config.xml", nil, "")
	if err != nil {
		return "", false, fmt.Errorf("get item config %s: %w", itemPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", false, fmt.Errorf("get item config %s: HTTP %d: %s", itemPath, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false, fmt.Errorf("read item config %s: %w", itemPath, err)
	}
	return string(data), true, nil
}

// UpdateItemConfig updates the config.xml for an existing item.
func (c *Client) UpdateItemConfig(ctx context.Context, itemPath, configXML string) error {
	body := bytes.NewBufferString(configXML)
	resp, err := c.do(ctx, http.MethodPost, "/"+jobPath(itemPath)+"/config.xml", body, "text/xml")
	if err != nil {
		return fmt.Errorf("update item %s: %w", itemPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("update item %s: HTTP %d: %s", itemPath, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	return nil
}

// DeleteItem deletes a Jenkins item at the given path.
func (c *Client) DeleteItem(ctx context.Context, itemPath string) error {
	resp, err := c.do(ctx, http.MethodPost, "/"+jobPath(itemPath)+"/doDelete", nil, "")
	if err != nil {
		return fmt.Errorf("delete item %s: %w", itemPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("delete item %s: %w", itemPath, ErrItemNotFound)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("delete item %s: HTTP %d: %s", itemPath, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	return nil
}

// AssignItemRoles assigns RBAC roles on an item path via the role-strategy
// plugin. Item-level roles are deferred — the role-strategy REST API requires
// XML payloads that vary by plugin version. Groups are parsed from items.yaml
// and available on the Item struct for future wiring.
func (c *Client) AssignItemRoles(ctx context.Context, itemPath string) error {
	return nil
}

// IsItemBuilding reports whether the item at itemPath — or, when it is a
// folder, any descendant job — currently has a build running or queued.
// On any query error it returns (true, err): callers MUST treat uncertainty
// as "busy" and defer destructive actions.
// A definitive 404 (item does not exist) returns (false, nil).
func (c *Client) IsItemBuilding(ctx context.Context, itemPath string) (bool, error) {
	// Recursive tree query: checks the item itself and all descendants for
	// building state. For a non-folder the jobs[...] subtree is empty.
	path := "/" + jobPath(itemPath) + "/api/json?tree=color,inQueue,lastBuild[building],jobs[name,color,inQueue,lastBuild[building],jobs[name,color,inQueue,lastBuild[building]]]"
	resp, err := c.do(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return true, fmt.Errorf("is item building %s: %w", itemPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return true, fmt.Errorf("is item building %s: HTTP %d: %s", itemPath, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	type jobNode struct {
		Name      string `json:"name"`
		Color     string `json:"color"`
		InQueue   bool   `json:"inQueue"`
		LastBuild *struct {
			Building bool `json:"building"`
		} `json:"lastBuild"`
		Jobs []jobNode `json:"jobs"`
	}
	var node jobNode
	if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
		return true, fmt.Errorf("is item building %s: decode: %w", itemPath, err)
	}

	var checkNode func(n jobNode) bool
	checkNode = func(n jobNode) bool {
		if n.InQueue {
			return true
		}
		if strings.Contains(n.Color, "_anime") {
			return true
		}
		if n.LastBuild != nil && n.LastBuild.Building {
			return true
		}
		for _, child := range n.Jobs {
			if checkNode(child) {
				return true
			}
		}
		return false
	}
	return checkNode(node), nil
}
