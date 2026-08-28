// Package client provides a hand-written Go client for the Varroa BFF API.
//
// The generated code (gen.go) provides the raw HTTP wrappers produced
// by oapi-codegen. This package wraps them with:
//   - Bearer-token injection via RequestEditorFn
//   - User-agent headers
//   - Structured error decoding (APIError)
//
// Usage:
//
//	c, err := client.New("https://varroa.example.com", "vk_xxx...")
//	if err != nil { ... }
//	resp, err := c.ListControllers(ctx, &ListControllersParams{})
//
// The generated GetControllerLogs method is JSON-only; DO NOT call it with
// follow=true (the response would be text/event-stream and JSON parsing
// fails). For SSE streams use pkg/client/stream (C3 territory).
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// New creates a new Varroa BFF API client wrapping the generated
// ClientWithResponses. baseURL is the host root URL (e.g.
// "https://varroa.example.com"). token is a session JWT or vk_ API key.
func New(baseURL, token string, opts ...ClientOption) (*ClientWithResponses, error) {
	// Combine default opts with caller opts.
	allOpts := make([]ClientOption, 0, 2+len(opts))
	allOpts = append(allOpts,
		WithHTTPClient(http.DefaultClient),
		WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("User-Agent", "varroa-client/1.0")
			return nil
		}),
	)
	allOpts = append(allOpts, opts...)

	return NewClientWithResponses(baseURL, allOpts...)
}

// WithUserAgent sets a custom User-Agent header on every request.
func WithUserAgent(ua string) ClientOption {
	return WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
		req.Header.Set("User-Agent", ua)
		return nil
	})
}

// APIError represents a non-2xx API response with a structured error body.
type APIError struct {
	StatusCode int
	Message    string // The "error" field from the N1 envelope.
	Body       []byte // Raw response body.
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api error %d: %s", e.StatusCode, e.Message)
}

// DecodeError parses a non-2xx response into an *APIError by reading the
// N1 {"error":"..."} envelope. Returns nil if the response is nil or 2xx.
func DecodeError(resp *http.Response) *APIError {
	if resp == nil || resp.StatusCode < 400 {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return &APIError{StatusCode: resp.StatusCode, Message: "failed to read response body"}
	}
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return &APIError{StatusCode: resp.StatusCode, Message: string(body), Body: body}
	}
	return &APIError{StatusCode: resp.StatusCode, Message: envelope.Error, Body: body}
}
