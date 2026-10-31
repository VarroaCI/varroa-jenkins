package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/api/sse"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// runStreamTenant holds the read decision for one tenant of a run stream.
// The handler resolves it once, at subscribe.
type runStreamTenant struct {
	folder  string
	grants  []rbac.TenantGrant
	granted bool
}

// handleTenantRunStream streams live run notifications for one tenant. A
// pipelines ticket opens it, so the ticket's claims decide the visible
// tenants. It writes no frame at connect: the page refetches when the
// stream reports itself open.
func (s *Server) handleTenantRunStream(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	if s.deps.RunFanout == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "SSE not available")
		return
	}

	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	refs, serr := s.resolveTenantTenants(ctx, claims, r.PathValue("tenant"))
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}

	keys := runStreamKeys(refs)
	if len(keys) == 0 {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	s.serveRunStream(w, r, keys, s.runStreamFilter(claims, refs))
}

// handleFleetRunStream streams the run notifications of every tenant the
// caller may read. The ticket scope pipelines-runs opens it. The keys are the
// readable scopes of the pipelines walk, and the filter applies the per-tenant
// job check to each notification.
func (s *Server) handleFleetRunStream(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	if s.deps.RunFanout == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "SSE not available")
		return
	}

	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	walk, err := s.readablePipelineTenants(ctx, claims)
	if s.handleReaderError(w, err, "list pipelines tenants") {
		return
	}
	var refs []TenantRef
	for _, t := range walk.Items {
		refs = append(refs, t.Refs...)
	}
	keys := runStreamKeys(refs)
	if len(keys) == 0 {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	s.serveRunStream(w, r, keys, s.runStreamFilter(claims, refs))
}

// serveRunStream writes the run notification frames of keys that pass filter,
// until the client leaves or the fan-out closes the channel. It pings every
// thirty seconds so a proxy keeps the connection open.
func (s *Server) serveRunStream(w http.ResponseWriter, r *http.Request, keys []string, filter func(recordstore.RunNotification) bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeJSONError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	ctx := r.Context()
	ch := s.deps.RunFanout.Subscribe(keys, filter)
	defer s.deps.RunFanout.Unsubscribe(keys, ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case record, open := <-ch:
			if !open {
				return
			}
			writeRunNotificationFrame(w, flusher, record)
		}
	}
}

// runStreamKeys builds one fan-out key per resolvable tenant. Two refs
// that name the same cluster and tenant yield one key.
func runStreamKeys(refs []TenantRef) []string {
	keys := make([]string, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if ref.Cluster == "" || ref.Tenant == "" {
			continue
		}
		key := ref.Cluster + "/" + ref.Tenant
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

// runStreamFilter builds the job filter for refs. It resolves the caller's
// grants once per tenant, and each notification is tested against the
// grants of its own tenant. The filter calls grantsAllow, so no second copy
// of the job rule exists here. Every notification carries the run's job
// path, so the filter drops a notification whose job path is empty. The
// empty-job-path rule admits a whole-tenant request that opens the stream,
// and a notification is never such a request.
func (s *Server) runStreamFilter(claims *auth.Claims, refs []TenantRef) func(recordstore.RunNotification) bool {
	byKey := make(map[string]runStreamTenant, len(refs))
	for _, ref := range refs {
		if ref.Cluster == "" || ref.Tenant == "" {
			continue
		}
		key := ref.Cluster + "/" + ref.Tenant
		if _, dup := byKey[key]; dup {
			continue
		}
		grants, granted := s.deps.Authorizer.tenantGrants(claims, ref.Tenant, ref.Folder)
		byKey[key] = runStreamTenant{folder: ref.Folder, grants: grants, granted: granted}
	}
	return func(n recordstore.RunNotification) bool {
		if n.JobPath == "" {
			return false
		}
		entry, found := byKey[n.Cluster+"/"+n.Tenant]
		if !found || !entry.granted {
			return false
		}
		return grantsAllow(entry.grants, entry.folder, n.JobPath, "read")
	}
}

// writeRunNotificationFrame writes one SSE frame for one record. The event
// name is the notification's own kind: run, stage, lock, input, or wait.
func writeRunNotificationFrame(w http.ResponseWriter, flusher http.Flusher, record sse.Record) {
	payload, err := json.Marshal(record.Data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", record.Event, payload)
	flusher.Flush()
}
