package main

import (
	"encoding/json"
	"net/http"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// homesEntry is one hosted tenant's answer on GET /v1/homes: the slot
// this cell holds for the tenant, the role, and the tenant's job-path to
// slot map.
type homesEntry struct {
	Tenant   string         `json:"tenant"`
	Slot     int            `json:"slot"`
	Role     string         `json:"role"`
	JobHomes map[string]int `json:"job_homes,omitempty"`
}

// homesResponse is the body GET /v1/homes answers.
type homesResponse struct {
	Entries []homesEntry `json:"entries"`
}

// cacheTenantHomes stores the tenant home map a desired-state command
// carries. The apply calls it before the convergence gate, so a converged
// push still delivers a fresh map to the loopback route.
//
// A command that carries no entry leaves the cache unchanged, so a push
// the operator assembled without the field never empties a map the plugin
// still needs.
func (a *Agent) cacheTenantHomes(cmd *mitev1.DesiredStateCommand) {
	if cmd == nil || cmd.TenantHomes == nil {
		return
	}
	homes := cmd.TenantHomes
	a.tenantHomes.Store(&homes)
}

// handleHomes serves the tenant home map the newest applied desired state
// carried. The mite computes no home: it serves the field it received and
// nothing else.
//
// The route is read-only. It accepts GET alone and answers 405 to any
// other method. It answers an empty entry list before the mite has
// applied a desired state that carries the field, and a caller reads that
// as "no opinion".
func (a *Agent) handleHomes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.authenticated(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	resp := homesResponse{Entries: homesEntries(a.tenantHomes.Load())}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		a.Logger.Warn("write the homes response", "error", err)
	}
}

// homesEntries turns the cached proto entries into the wire shape. A nil
// cache answers an empty, non-nil list, so the body always carries an
// entries array.
func homesEntries(cached *[]*mitev1.TenantHomes) []homesEntry {
	out := []homesEntry{}
	if cached == nil {
		return out
	}
	for _, entry := range *cached {
		if entry == nil {
			continue
		}
		out = append(out, homesEntry{
			Tenant:   entry.Tenant,
			Slot:     entry.Slot,
			Role:     entry.Role,
			JobHomes: entry.JobHomes,
		})
	}
	return out
}
