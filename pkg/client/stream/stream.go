// Package stream provides an SSE (Server-Sent Events) client for the Varroa BFF.
//
// It exports Stream (reconnecting loop for long-lived streams) and ParseSSE
// (one-shot parser for SSE response bodies, reused by task 9.1).
package stream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Event represents a single SSE frame after blank-line dispatch.
type Event struct {
	Name string // SSE "event:" field; "" for unnamed frames
	Data []byte // joined "data:" payload
}

// BackoffConfig controls the reconnection backoff.
type BackoffConfig struct {
	Initial time.Duration // initial delay (default 1s)
	Max     time.Duration // maximum delay (default 30s)
	Factor  float64       // multiplicative factor (default 2)
}

// DefaultBackoff returns a BackoffConfig with sensible defaults.
func DefaultBackoff() BackoffConfig {
	return BackoffConfig{
		Initial: 1 * time.Second,
		Max:     30 * time.Second,
		Factor:  2,
	}
}

// Config configures an SSE stream connection.
type Config struct {
	URL       string                 // absolute stream URL (query included)
	Token     string                 // bearer token
	UserAgent string                 // user agent string
	Client    *http.Client           // HTTP client; default &http.Client{} (no timeout)
	OnConnect func(reconnected bool) // called after each successful open
	Backoff   BackoffConfig          // backoff parameters
}

// HTTPError is returned for terminal HTTP status codes (401, 403, 404).
type HTTPError struct {
	StatusCode int
	Message    string // N1 {"error":...} or truncated body
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("http error %d: %s", e.StatusCode, e.Message)
}

// Stream opens an SSE GET and delivers events to handle until ctx is cancelled.
//
// Behaviour:
//   - 401/403/404 on open → terminal, returned as *HTTPError (no retry).
//   - All other errors (non-2xx, wrong Content-Type, network errors, mid-stream
//     EOF) → logged to stderr once per transition, backoff + retry.
//   - OnConnect called after each successful open — false on first, true on
//     reconnects.
//   - Backoff resets to Initial after every successful open.
//   - Returns nil on ctx cancellation.
func Stream(ctx context.Context, cfg Config, handle func(Event) error) error {
	if cfg.Client == nil {
		cfg.Client = &http.Client{}
	}
	if cfg.Backoff.Initial <= 0 {
		cfg.Backoff.Initial = 1 * time.Second
	}
	if cfg.Backoff.Max <= 0 {
		cfg.Backoff.Max = 30 * time.Second
	}
	if cfg.Backoff.Factor <= 0 {
		cfg.Backoff.Factor = 2
	}
	if cfg.OnConnect == nil {
		cfg.OnConnect = func(bool) {}
	}

	var (
		backoff       time.Duration
		reconnect     bool
		lastLogged    string
		reqErr        error
		resp          *http.Response
		req           *http.Request
		name          string
		data          [][]byte
		sc            *bufio.Scanner
		deadlineTimer *time.Timer
		readDeadline  time.Duration
		ct            string
		watchCh       chan struct{}
		watchDone     <-chan struct{}
	)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// Stop any previous deadline timer
		if deadlineTimer != nil {
			deadlineTimer.Stop()
		}

		// Reset per-connection state
		name = ""
		data = nil

		// --- connection attempt ---
		req, reqErr = http.NewRequestWithContext(ctx, http.MethodGet, cfg.URL, nil)
		if reqErr != nil {
			return reqErr
		}
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
		req.Header.Set("Accept", "text/event-stream")
		if cfg.UserAgent != "" {
			req.Header.Set("User-Agent", cfg.UserAgent)
		}
		req.Header.Set("Cache-Control", "no-cache")

		resp, reqErr = cfg.Client.Do(req)
		if reqErr != nil {
			// network error — retryable
			goto reconnectLabel
		}

		// Terminal status codes — no retry
		if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 {
			errMsg := decodeN1Error(resp.Body)
			_ = resp.Body.Close()
			return &HTTPError{StatusCode: resp.StatusCode, Message: errMsg}
		}

		// Non-200 or wrong Content-Type — retryable
		ct = resp.Header.Get("Content-Type")
		if resp.StatusCode != 200 || !strings.HasPrefix(ct, "text/event-stream") {
			_ = resp.Body.Close()
			reqErr = fmt.Errorf("unexpected response %d content-type %q", resp.StatusCode, ct)
			goto reconnectLabel
		}

		// --- successful open ---
		cfg.OnConnect(reconnect)
		backoff = cfg.Backoff.Initial // reset on successful open

		// 90s read deadline: close resp.Body if no data for 90s
		// (3 missed :ping keepalives at 30s).
		readDeadline = 90 * time.Second
		deadlineTimer = time.AfterFunc(readDeadline, func() {
			_ = resp.Body.Close()
		})

		// Also close resp.Body when ctx is cancelled, so sc.Scan() unblocks.
		watchCh = make(chan struct{}, 1)
		watchDone = ctx.Done()
		go func(done <-chan struct{}, ch chan struct{}, body io.Closer) {
			select {
			case <-done:
				_ = body.Close()
			case <-ch:
			}
		}(watchDone, watchCh, resp.Body)

		sc = bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

		for sc.Scan() {
			// Reset deadline on any received data
			deadlineTimer.Reset(readDeadline)

			line := sc.Text()
			switch {
			case line == "":
				if len(data) > 0 {
					ev := Event{Name: name, Data: bytes.Join(data, []byte("\n"))}
					if err := handle(ev); err != nil {
						deadlineTimer.Stop()
						_ = resp.Body.Close()
						return err
					}
				}
				name = ""
				data = nil
			case strings.HasPrefix(line, ":"):
				// comment/keepalive — liveness only
			case strings.HasPrefix(line, "event:"):
				name = strings.TrimSpace(line[6:])
			case strings.HasPrefix(line, "data:"):
				// Per SSE spec: exactly ONE optional leading space is stripped.
				// Do NOT use TrimSpace (payload-leading whitespace is data).
				payload := strings.TrimPrefix(line, "data:")
				payload = strings.TrimPrefix(payload, " ")
				data = append(data, []byte(payload))
			default:
				// id:/retry:/unknown — ignore
			}
		}

		deadlineTimer.Stop()

		if err := sc.Err(); err != nil {
			_ = resp.Body.Close()
			reqErr = fmt.Errorf("stream error: %w", err)
			goto reconnectLabel
		}
		_ = resp.Body.Close()

		// Clean EOF — check ctx
		select {
		case <-ctx.Done():
			return nil
		default:
			reqErr = fmt.Errorf("stream closed")
			goto reconnectLabel
		}

	reconnectLabel:
		// On first reconnect, start from Initial; otherwise keep accumulated backoff
		if backoff == 0 {
			backoff = cfg.Backoff.Initial
		}

		// Log the disconnect once per transition
		msg := fmt.Sprintf("stream disconnected (%v), reconnecting in %v", reqErr, backoff)
		if msg != lastLogged {
			_, _ = fmt.Fprintln(io.Discard, msg)
			lastLogged = msg
		}

		// Wait with backoff, respecting ctx cancellation
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}

		// Increase backoff (capped at Max)
		next := time.Duration(float64(backoff) * cfg.Backoff.Factor)
		if next > cfg.Backoff.Max || next <= backoff {
			next = cfg.Backoff.Max
		}
		backoff = next

		reconnect = true
	}
}

// ParseSSE parses an SSE response body from r and delivers each event to
// handle. It does NOT reconnect — suitable for one-shot SSE responses.
func ParseSSE(r io.Reader, handle func(Event) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

	name := ""
	data := make([][]byte, 0, 1)

	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if len(data) > 0 {
				ev := Event{Name: name, Data: bytes.Join(data, []byte("\n"))}
				if err := handle(ev); err != nil {
					return err
				}
			}
			name = ""
			data = nil
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimPrefix(payload, " ")
			data = append(data, []byte(payload))
		default:
		}
	}

	return sc.Err()
}

// decodeN1Error reads a response body and extracts the N1 {"error":"..."}
// envelope. If unparseable, returns the raw body truncated to 512 bytes.
func decodeN1Error(r io.Reader) string {
	body, err := io.ReadAll(r)
	if err != nil {
		return "failed to read response body"
	}
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

// AsHTTPError unwraps err to find *HTTPError.
func AsHTTPError(err error, target **HTTPError) bool {
	for {
		if he, ok := err.(*HTTPError); ok {
			*target = he
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
}
