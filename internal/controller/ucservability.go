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

// Every operator call to the update center's servability endpoint goes through
// postServability. Every operator read of the update center's inventory goes
// through getUCInventory. The UpdateCenter and ProfileCandidate reconcilers
// both use these two helpers, so their answers cannot drift apart.

// servabilityTimeout bounds one servability round trip, including the
// upstream lookups the update center runs for the posted plugins.
const servabilityTimeout = 45 * time.Second

// servabilityChunk matches the update center's per-request entry limit.
const servabilityChunk = 2000

// errServabilityUnsupported marks an update center that does not route the
// servability endpoint, which happens when the image predates it.
var errServabilityUnsupported = errors.New("update center has no servability endpoint")

// ucHTTPDoer is the HTTP client seam shared by the update-center helpers.
type ucHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// ucServabilityPlugin is one entry of the servability request.
type ucServabilityPlugin struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ucServabilityRequest is the POST body of /api/v1/servability.
type ucServabilityRequest struct {
	Plugins []ucServabilityPlugin `json:"plugins"`
}

// ucServabilityResult is one answer of the servability response.
type ucServabilityResult struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	ResolvedVersion string `json:"resolvedVersion,omitempty"`
	Servable        bool   `json:"servable"`
	Via             string `json:"via,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// ucServabilityResponse is the response of /api/v1/servability.
type ucServabilityResponse struct {
	Plugins []ucServabilityResult `json:"plugins"`
}

// postServability POSTs the plugins to the update center's servability endpoint
// in chunks and returns the answers in request order. A 404 or 405 answer
// returns errServabilityUnsupported.
func postServability(ctx context.Context, doer ucHTTPDoer, baseURL string, plugins []ucServabilityPlugin) ([]ucServabilityResult, error) {
	ctx, cancel := context.WithTimeout(ctx, servabilityTimeout)
	defer cancel()

	url := baseURL + "/api/v1/servability"
	out := make([]ucServabilityResult, 0, len(plugins))
	for start := 0; start < len(plugins); start += servabilityChunk {
		end := min(start+servabilityChunk, len(plugins))
		body, err := json.Marshal(ucServabilityRequest{Plugins: plugins[start:end]})
		if err != nil {
			return nil, fmt.Errorf("encode servability request: %w", err)
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create servability request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := doer.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("POST servability: %w", err)
		}
		var payload ucServabilityResponse
		decodeErr := func() error {
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
				return fmt.Errorf("%w: HTTP %d", errServabilityUnsupported, resp.StatusCode)
			}
			if resp.StatusCode != http.StatusOK {
				msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				return fmt.Errorf("servability HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
			}
			if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
				return fmt.Errorf("parse servability JSON: %w", err)
			}
			return nil
		}()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if len(payload.Plugins) != end-start {
			return nil, fmt.Errorf("servability answered %d of %d plugins", len(payload.Plugins), end-start)
		}
		out = append(out, payload.Plugins...)
	}
	return out, nil
}

// classifyServability maps one servability answer to a CRD-facing unservable
// reason. It returns ("", false) for a servable plugin, and ("", true) for an
// undecided answer, such as sources-degraded: the negative cannot be proven,
// so the plugin is neither servable nor listed as unservable.
func classifyServability(res ucServabilityResult) (reason string, undecided bool) {
	if res.Servable {
		return "", false
	}
	switch res.Reason {
	case "pull-through-disabled":
		return unservablePullThroughDisabled, false
	case "not-listed":
		return unservableNotListed, false
	case "archive-disabled":
		return unservableArchiveDisabled, false
	default:
		return "", true
	}
}

// getUCInventory GETs the update center's /api/v1/inventory. The second result
// names every plugin-pack manifest the update center could not read (see
// ucInventoryResponse); it is empty on a fully complete scan.
func getUCInventory(ctx context.Context, doer ucHTTPDoer, baseURL string) ([]inventoryEntry, []ucSkippedPack, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/inventory", nil)
	if err != nil {
		return nil, nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := doer.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("GET inventory: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, nil, fmt.Errorf("inventory HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload ucInventoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, nil, fmt.Errorf("parse inventory JSON: %w", err)
	}
	return payload.Plugins, payload.SkippedPacks, nil
}
