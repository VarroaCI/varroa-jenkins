package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// UpdateCenterInventoryEntry represents a single plugin in the update center inventory.
type UpdateCenterInventoryEntry struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

// updateCenterSkippedPack names one plugin-pack manifest the update-center
// service could not read, as disclosed in its /api/v1/inventory response.
type updateCenterSkippedPack struct {
	Ref   string `json:"ref"`
	Error string `json:"error"`
}

// updateCenterInventoryResponse is the wire shape of GET /api/v1/inventory:
// the plugin listing built from every readable pack, plus any pack the update
// center could not read. List() logs SkippedPacks rather than failing on it —
// this consumer only ever displays the inventory, it never prunes against it.
type updateCenterInventoryResponse struct {
	Plugins      []UpdateCenterInventoryEntry `json:"plugins"`
	SkippedPacks []updateCenterSkippedPack    `json:"skippedPacks,omitempty"`
}

// UpdateCenterInventory provides access to the update center's plugin inventory.
type UpdateCenterInventory interface {
	List(ctx context.Context) ([]UpdateCenterInventoryEntry, error)
}

// updatecenterHTTPClient is an HTTP-backed implementation of UpdateCenterInventory.
type updatecenterHTTPClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewUpdateCenterInventoryClient creates a new HTTP-backed UpdateCenterInventory.
func NewUpdateCenterInventoryClient(baseURL string, httpClient *http.Client) UpdateCenterInventory {
	return &updatecenterHTTPClient{
		baseURL:    baseURL,
		httpClient: httpClient,
	}
}

// List fetches the plugin inventory from the update center service.
//
// The update center's /api/v1/inventory now degrades gracefully instead of
// failing closed on one unreadable pack: it serves every readable plugin and
// discloses unreadable packs in "skippedPacks". This consumer only ever
// displays the inventory (it never prunes against it), so a non-empty
// skippedPacks is logged rather than surfaced as an error — the caller still
// gets the readable subset.
func (c *updatecenterHTTPClient) List(ctx context.Context) ([]UpdateCenterInventoryEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/inventory", nil)
	if err != nil {
		return nil, fmt.Errorf("create inventory request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("inventory request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("inventory request returned status %d: %s", resp.StatusCode, string(body))
	}

	var payload updateCenterInventoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode inventory response: %w", err)
	}

	if len(payload.SkippedPacks) > 0 {
		refs := make([]string, len(payload.SkippedPacks))
		for i, sp := range payload.SkippedPacks {
			refs[i] = sp.Ref
		}
		slog.Default().Warn("update center inventory is partial: some plugin packs could not be read",
			"unreadableManifests", len(payload.SkippedPacks), "refs", refs)
	}

	return payload.Plugins, nil
}
