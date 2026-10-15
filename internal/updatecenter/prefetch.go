package updatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// prefetchMaxBody bounds the POST body of /api/v1/prefetch.
const prefetchMaxBody = 1 << 20

// prefetchMaxEntries bounds how many plugins one prefetch request may name. The
// operator sends at most 20 per pass, so this cap is a server-side guard.
const prefetchMaxEntries = 50

// prefetchWorkers bounds how many pull-through fetches run at once for one request.
const prefetchWorkers = 4

// Outcome codes for one prefetched plugin.
const (
	prefetchOutcomeFetched = "fetched"
	prefetchOutcomeStored  = "stored"
	prefetchOutcomeFailed  = "failed"
)

// Reason codes for a prefetch that failed.
const (
	prefetchReasonNotListed        = "not-listed"
	prefetchReasonSourcesDegraded  = "sources-degraded"
	prefetchReasonChecksumMismatch = "checksum-mismatch"
	prefetchReasonDownloadFailed   = "download-failed"
	prefetchReasonStoreFailed      = "store-failed"
)

// prefetchRequest reuses the servability entry: a name, a version and an optional core.
type prefetchRequest struct {
	Plugins []servabilityPlugin `json:"plugins"`
}

type prefetchResult struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// ResolvedVersion is the concrete version a latest entry resolved to.
	ResolvedVersion string `json:"resolvedVersion,omitempty"`
	Outcome         string `json:"outcome"`
	Reason          string `json:"reason,omitempty"`
}

type prefetchResponse struct {
	Plugins []prefetchResult `json:"plugins"`
}

// handlePrefetch serves POST /api/v1/prefetch. It fetches each posted plugin
// through pull-through and stores it, so a later request finds it in the store.
// It shares one singleflight key per name@version with /download.
func (s *Server) handlePrefetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") || !s.verifyToken(strings.TrimPrefix(authHeader, "Bearer ")) {
		s.logger.Warn("prefetch request with missing or invalid token")
		writeError(w, http.StatusUnauthorized, "unauthorized: missing or invalid Bearer token")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, prefetchMaxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "prefetch request body too large")
		return
	}
	var req prefetchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid prefetch request: "+err.Error())
		return
	}
	if len(req.Plugins) > prefetchMaxEntries {
		writeError(w, http.StatusBadRequest, "prefetch request names too many plugins")
		return
	}
	if !s.pullThroughEnabled || s.resolver == nil {
		writeError(w, http.StatusConflict, "pull-through is disabled")
		return
	}

	packs, _, err := s.listPackInfos(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list the store")
		return
	}
	stored := make(map[string]bool)
	for _, pack := range packs {
		for _, p := range pack.Plugins {
			stored[p.Name+"@"+p.Version] = true
		}
	}

	results := make([]prefetchResult, len(req.Plugins))
	sem := make(chan struct{}, prefetchWorkers)
	var wg sync.WaitGroup
	for i, p := range req.Plugins {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, p servabilityPlugin) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = s.prefetchOne(r.Context(), p, stored)
		}(i, p)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, prefetchResponse{Plugins: results})
}

// prefetchOne resolves one plugin, then fetches it unless the store already holds it.
func (s *Server) prefetchOne(ctx context.Context, p servabilityPlugin, stored map[string]bool) prefetchResult {
	out := prefetchResult{Name: p.Name, Version: p.Version}
	version := p.Version
	if ucmeta.IsLatest(version) {
		var a ucmeta.Availability
		if strings.TrimSpace(p.Core) != "" {
			a = s.resolver.ResolveLatestForCore(ctx, p.Name, p.Core)
		} else {
			a = s.resolver.ResolveAvailable(ctx, p.Name, version)
		}
		if a.Outcome != ucmeta.Resolved {
			out.Outcome = prefetchOutcomeFailed
			out.Reason = prefetchReasonForOutcome(a)
			return out
		}
		version = a.Version
		out.ResolvedVersion = a.Version
	}

	if stored[p.Name+"@"+version] {
		out.Outcome = prefetchOutcomeStored
		return out
	}
	if !ucmeta.IsLatest(p.Version) {
		// A pinned version must be listed before the fetch, so a refusal names its reason.
		a := s.resolver.ResolveAvailable(ctx, p.Name, version)
		if a.Outcome != ucmeta.Resolved {
			out.Outcome = prefetchOutcomeFailed
			out.Reason = prefetchReasonForOutcome(a)
			return out
		}
	}

	res, err, _ := s.dlGroup.Do(p.Name+"@"+version, func() (interface{}, error) {
		return s.doPullThrough(ctx, p.Name, version)
	})
	if err == nil {
		err = res.(pullThroughResult).err
	}
	if err != nil {
		s.logger.Warn("prefetch failed", "name", p.Name, "version", version, "error", err)
		out.Outcome = prefetchOutcomeFailed
		out.Reason = prefetchReasonDownloadFailed
		if errors.Is(err, errChecksumMismatch) {
			out.Reason = prefetchReasonChecksumMismatch
		}
		return out
	}
	// doPullThrough serves the bytes even when the store write fails, so the store
	// is checked before the answer claims the plugin is stored.
	packs, skipped, err := s.listPackInfos(ctx)
	if err != nil {
		out.Outcome = prefetchOutcomeFailed
		out.Reason = prefetchReasonDownloadFailed
		return out
	}
	if !packsHoldPlugin(packs, p.Name, version) {
		// A manifest that could not be read may be the one just written, so an
		// unreadable listing is a read failure, not a write failure.
		out.Outcome = prefetchOutcomeFailed
		out.Reason = prefetchReasonStoreFailed
		if len(skipped) > 0 {
			out.Reason = prefetchReasonDownloadFailed
		}
		return out
	}
	out.Outcome = prefetchOutcomeFetched
	return out
}

// prefetchReasonForOutcome maps a failed resolution onto a prefetch reason.
func prefetchReasonForOutcome(a ucmeta.Availability) string {
	if a.Outcome == ucmeta.NotListed {
		return prefetchReasonNotListed
	}
	return prefetchReasonSourcesDegraded
}

// packsHoldPlugin reports whether one of the packs lists name at version.
func packsHoldPlugin(packs []packInfo, name, version string) bool {
	for _, pack := range packs {
		for _, p := range pack.Plugins {
			if p.Name == name && p.Version == version {
				return true
			}
		}
	}
	return false
}
