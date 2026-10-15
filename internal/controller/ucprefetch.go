package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Every operator call to the update center's prefetch endpoint goes through
// postPrefetch. The endpoint fetches and stores plugins through pull-through.

// prefetchTimeout bounds one prefetch round trip. The reconcile that sends it
// runs on the leader, so the bound keeps one slow upstream from stalling it.
const prefetchTimeout = 90 * time.Second

// Sentinel errors for the prefetch answers that stop the batch.
var (
	errPrefetchUnsupported    = errors.New("update center has no prefetch endpoint")
	errPrefetchUnauthorized   = errors.New("update center rejected the import token")
	errPrefetchPullThroughOff = errors.New("update center pull-through is disabled")
)

// ucPrefetchResult is one answer of the prefetch response.
type ucPrefetchResult struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	ResolvedVersion string `json:"resolvedVersion,omitempty"`
	Outcome         string `json:"outcome"`
	Reason          string `json:"reason,omitempty"`
}

// ucPrefetchRequest is the POST body of /api/v1/prefetch.
type ucPrefetchRequest struct {
	Plugins []ucServabilityPlugin `json:"plugins"`
}

// ucPrefetchResponse is the response of /api/v1/prefetch.
type ucPrefetchResponse struct {
	Plugins []ucPrefetchResult `json:"plugins"`
}

// postPrefetch POSTs one batch to the update center's prefetch endpoint with the
// import token as Bearer credential. It returns the answers in request order.
func postPrefetch(ctx context.Context, doer ucHTTPDoer, baseURL, token string, plugins []ucServabilityPlugin) ([]ucPrefetchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, prefetchTimeout)
	defer cancel()

	body, err := json.Marshal(ucPrefetchRequest{Plugins: plugins})
	if err != nil {
		return nil, fmt.Errorf("encode prefetch request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/prefetch", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create prefetch request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := doer.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("POST prefetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, errPrefetchUnauthorized
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return nil, fmt.Errorf("%w: HTTP %d", errPrefetchUnsupported, resp.StatusCode)
	case http.StatusConflict:
		return nil, errPrefetchPullThroughOff
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("prefetch HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var payload ucPrefetchResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("parse prefetch JSON: %w", err)
	}
	if len(payload.Plugins) != len(plugins) {
		return nil, fmt.Errorf("prefetch answered %d of %d plugins", len(payload.Plugins), len(plugins))
	}
	return payload.Plugins, nil
}
