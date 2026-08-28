package updatecenter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrInvalidToken is returned when the update center rejects the import token.
var ErrInvalidToken = errors.New("update-center import: invalid or expired token")

// PostImport POSTs payload to <targetURL>/api/v1/import with a Bearer token.
// Returns ErrInvalidToken (wrapped) on HTTP 401; returns a generic error on
// any other non-2xx response; returns nil on success.
func PostImport(ctx context.Context, targetURL, token string, payload io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL+"/api/v1/import", payload)
	if err != nil {
		return fmt.Errorf("create import request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/gzip")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST import: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%w: HTTP 401", ErrInvalidToken)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("POST import: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}
