package mite

import (
	"context"
	"encoding/json"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// kindJobBranches is the ingest kind a job.branches envelope carries. The
// literal value must match internal/recordstore.KindJobBranches.
const kindJobBranches = "job.branches"

// jobBranchProjectInput is one multibranch project of the payload. Each
// project carries its own tenant and placement generation, because one
// envelope describes a whole cell.
type jobBranchProjectInput struct {
	Tenant              string               `json:"tenant"`
	PlacementGeneration int64                `json:"placement_generation"`
	JobPath             string               `json:"job_path"`
	IndexedAt           string               `json:"indexed_at"`
	IndexResult         string               `json:"index_result"`
	Heads               []jobBranchHeadInput `json:"heads"`
	HeadsTruncated      bool                 `json:"heads_truncated"`
	LastScan            *branchScanInput     `json:"last_scan"`
}

// jobBranchHeadInput is one head of a project in the payload.
type jobBranchHeadInput struct {
	Name          string                    `json:"name"`
	JobPath       string                    `json:"job_path"`
	Kind          string                    `json:"kind"`
	Orphaned      bool                      `json:"orphaned"`
	Revision      string                    `json:"revision"`
	ChangeRequest *changeRequestBranchInput `json:"change_request,omitempty"`
}

// changeRequestBranchInput is the change request of a head.
type changeRequestBranchInput struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	URL          string `json:"url"`
}

// branchScanInput is the last scan of a project. The payload carries it
// as null until the scan path fills it.
type branchScanInput struct {
	ScanID        string   `json:"scan_id"`
	State         string   `json:"state"`
	Actor         string   `json:"actor"`
	RequestedAt   string   `json:"requested_at"`
	EndedAt       string   `json:"ended_at"`
	Result        string   `json:"result"`
	NewHeads      []string `json:"new_heads"`
	ChangedHeads  []string `json:"changed_heads"`
	OrphanedHeads []string `json:"orphaned_heads"`
	Message       string   `json:"message"`
}

// handleJobBranches is OnSnapshot's post-strip hook for the branch
// inventory. It follows the same retain-and-retry rule as
// handleJobSchedules: a later snapshot's set replaces an earlier retained
// one, and a set clears its retained state only after a durable publish.
func (h *BusHandler) handleJobBranches(ns, name string, set *mitev1.JobBranchSet) {
	key := ns + "/" + name

	if set != nil {
		h.scheduleMu.Lock()
		if h.branchRetained == nil {
			h.branchRetained = make(map[string]*mitev1.JobBranchSet)
		}
		h.branchRetained[key] = set
		h.scheduleMu.Unlock()
	}

	h.scheduleMu.Lock()
	current := h.branchRetained[key]
	h.scheduleMu.Unlock()
	if current == nil {
		return
	}

	if h.jobBranchStreamMissing(key) {
		return
	}

	if h.publishJobBranches(ns, name, current) {
		h.scheduleMu.Lock()
		delete(h.branchRetained, key)
		h.scheduleMu.Unlock()
	}
}

// jobBranchStreamMissing reports whether key's negative cache is still
// armed, so the caller skips the publish attempt.
func (h *BusHandler) jobBranchStreamMissing(key string) bool {
	h.scheduleMu.Lock()
	defer h.scheduleMu.Unlock()
	until, ok := h.branchStreamMissingUntil[key]
	return ok && time.Now().Before(until)
}

// armJobBranchStreamMissing arms key's negative cache after a failed
// publish attempt. It reuses the job-schedule TTL.
func (h *BusHandler) armJobBranchStreamMissing(key string) {
	h.scheduleMu.Lock()
	if h.branchStreamMissingUntil == nil {
		h.branchStreamMissingUntil = make(map[string]time.Time)
	}
	h.branchStreamMissingUntil[key] = time.Now().Add(h.jobScheduleStreamMissingTTL())
	h.scheduleMu.Unlock()
}

// clearJobBranchStreamMissing disarms key's negative cache after a
// successful publish.
func (h *BusHandler) clearJobBranchStreamMissing(key string) {
	h.scheduleMu.Lock()
	delete(h.branchStreamMissingUntil, key)
	h.scheduleMu.Unlock()
}

// publishJobBranches resolves the tenant of every project and, only when
// every project resolves, publishes one job.branches envelope with a
// JetStream acknowledgement. One unresolved project publishes nothing.
// It reports whether the publish succeeded.
func (h *BusHandler) publishJobBranches(ns, name string, set *mitev1.JobBranchSet) bool {
	key := ns + "/" + name

	projects := make([]jobBranchProjectInput, 0, len(set.Items))
	for _, item := range set.Items {
		if item == nil {
			// All or nothing: a nil project rejects the whole envelope.
			h.metrics.buildEventsDropped.Add(context.Background(), 1)
			h.log().Warn("job branch project nil, publishing no envelope", "controller", key)
			return false
		}
		tenant, generation := h.resolver().ResolveJobTenant(h.cluster, ns, name, item.JobPath)
		if tenant == "" {
			// All or nothing: one unresolved project rejects the envelope.
			h.metrics.buildEventsDropped.Add(context.Background(), 1)
			h.log().Warn("job branch project unresolved, publishing no envelope",
				"controller", key, "job_path", item.JobPath)
			return false
		}
		heads := make([]jobBranchHeadInput, 0, len(item.Heads))
		for _, head := range item.Heads {
			if head == nil {
				continue
			}
			heads = append(heads, jobBranchHeadInputOf(head))
		}
		projects = append(projects, jobBranchProjectInput{
			Tenant:              tenant,
			PlacementGeneration: generation,
			JobPath:             item.JobPath,
			IndexedAt:           item.IndexedAt,
			IndexResult:         item.IndexResult,
			Heads:               heads,
			HeadsTruncated:      item.HeadsTruncated,
			LastScan:            branchScanInputOf(item.LastScan),
		})
	}

	env := ingestEnvelope{
		SchemaVersion: 1,
		Kind:          kindJobBranches,
		Cluster:       h.cluster,
		Namespace:     ns,
		Cell:          name,
		CellInstance:  h.lookupCellInstance(ns, name),
		OccurredAt:    time.Now().UTC(),
		Payload: map[string]interface{}{
			"hash":      set.Hash,
			"truncated": set.Truncated,
			"total":     set.Total,
			"projects":  projects,
		},
	}

	data, err := json.Marshal(env)
	if err != nil {
		h.log().Error("marshal job branch envelope failed", "controller", key, "error", err)
		return false
	}

	if _, err := h.publisher.PublishJetStream(bus.BuildSubject(h.cluster, ns, name), data); err != nil {
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("job branch publish failed, retaining for retry",
			"controller", key, "error", err)
		h.armJobBranchStreamMissing(key)
		return false
	}

	h.clearJobBranchStreamMissing(key)
	return true
}

// jobBranchHeadInputOf converts one proto head into its payload form.
func jobBranchHeadInputOf(head *mitev1.JobBranchHead) jobBranchHeadInput {
	out := jobBranchHeadInput{
		Name:     head.Name,
		JobPath:  head.JobPath,
		Kind:     head.Kind,
		Orphaned: head.Orphaned,
		Revision: head.Revision,
	}
	if cr := head.ChangeRequest; cr != nil {
		out.ChangeRequest = &changeRequestBranchInput{
			ID:           cr.Id,
			Title:        cr.Title,
			SourceBranch: cr.SourceBranch,
			TargetBranch: cr.TargetBranch,
			URL:          cr.URL,
		}
	}
	return out
}

// branchScanInputOf converts the last scan of a project. A nil scan
// answers nil, which the payload carries as null.
func branchScanInputOf(scan *mitev1.BranchScanResult) *branchScanInput {
	if scan == nil {
		return nil
	}
	return &branchScanInput{
		ScanID:        scan.ScanId,
		State:         scan.State,
		Actor:         scan.Actor,
		RequestedAt:   scan.RequestedAt,
		EndedAt:       scan.EndedAt,
		Result:        scan.Result,
		NewHeads:      scan.NewHeads,
		ChangedHeads:  scan.ChangedHeads,
		OrphanedHeads: scan.OrphanedHeads,
		Message:       scan.Message,
	}
}
