package updatecenter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"

	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// servabilityMaxBody bounds the POST body of /api/v1/servability.
const servabilityMaxBody = 1 << 20

// servabilityMaxEntries bounds how many plugins one servability request may name.
const servabilityMaxEntries = 2000

// servabilityWorkers bounds how many upstream lookups run at once for one request.
const servabilityWorkers = 8

// Reason codes for a plugin the update center cannot serve.
const (
	servabilityReasonNotListed           = "not-listed"
	servabilityReasonArchiveDisabled     = "archive-disabled"
	servabilityReasonSourcesDegraded     = "sources-degraded"
	servabilityReasonPullThroughDisabled = "pull-through-disabled"
)

// availabilityResolver is the part of ucmeta.Resolver that servability needs. It is
// an interface so tests can drive concurrency and outcomes directly.
type availabilityResolver interface {
	ResolveAvailable(ctx context.Context, name, version string) ucmeta.Availability
}

type servabilityPlugin struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type servabilityRequest struct {
	Plugins []servabilityPlugin `json:"plugins"`
}

type servabilityResult struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Servable bool   `json:"servable"`
	Via      string `json:"via,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// ResolvedVersion is the concrete version a latest request resolved to.
	ResolvedVersion string `json:"resolvedVersion,omitempty"`
}

type servabilityResponse struct {
	Plugins []servabilityResult `json:"plugins"`
}

// handleServability serves POST /api/v1/servability. It answers, for each posted
// plugin, whether pull-through would serve it, using the same resolution path as
// /download. It reads no store; the caller supplies the plugin list.
func (s *Server) handleServability(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, servabilityMaxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "servability request body too large")
		return
	}
	var req servabilityRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid servability request: "+err.Error())
		return
	}
	if len(req.Plugins) > servabilityMaxEntries {
		writeError(w, http.StatusBadRequest, "servability request names too many plugins")
		return
	}

	results := make([]servabilityResult, len(req.Plugins))
	if !s.pullThroughEnabled || s.resolver == nil {
		for i, p := range req.Plugins {
			results[i] = servabilityResult{
				Name: p.Name, Version: p.Version,
				Reason: servabilityReasonPullThroughDisabled,
			}
		}
	} else {
		s.answerServability(r.Context(), s.resolver, req.Plugins, results)
	}

	writeJSON(w, http.StatusOK, servabilityResponse{Plugins: results})
}

// answerServability fills results in request order, running at most
// servabilityWorkers lookups at a time.
func (s *Server) answerServability(ctx context.Context, res availabilityResolver, plugins []servabilityPlugin, results []servabilityResult) {
	sem := make(chan struct{}, servabilityWorkers)
	var wg sync.WaitGroup
	for i, p := range plugins {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, p servabilityPlugin) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = servabilityFor(ctx, res, p)
		}(i, p)
	}
	wg.Wait()
}

// servabilityFor maps one lookup onto the wire shape.
func servabilityFor(ctx context.Context, res availabilityResolver, p servabilityPlugin) servabilityResult {
	out := servabilityResult{Name: p.Name, Version: p.Version}
	a := res.ResolveAvailable(ctx, p.Name, p.Version)
	switch a.Outcome {
	case ucmeta.Resolved:
		out.Servable = true
		out.Via = string(a.Via)
		if ucmeta.IsLatest(p.Version) {
			out.ResolvedVersion = a.Version
		}
	case ucmeta.NotListed:
		out.Reason = servabilityReasonNotListed
		if a.ArchiveDisabled {
			out.Reason = servabilityReasonArchiveDisabled
		}
	default:
		out.Reason = servabilityReasonSourcesDegraded
	}
	return out
}
