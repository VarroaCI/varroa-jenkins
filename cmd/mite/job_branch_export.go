package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// jobBranchesPath is the plugin route the mite polls for the multibranch
// inventory.
const jobBranchesPath = "/varroa-activity/branches"

// jobBranchHeadRow is one head of a multibranch project in the plugin's
// branch-inventory response.
type jobBranchHeadRow struct {
	Name          string                     `json:"name"`
	JobPath       string                     `json:"jobPath"`
	Kind          string                     `json:"kind"`
	Orphaned      bool                       `json:"orphaned"`
	Revision      string                     `json:"revision"`
	ChangeRequest *jobBranchChangeRequestRow `json:"changeRequest,omitempty"`
}

// jobBranchChangeRequestRow is the change request of a head.
type jobBranchChangeRequestRow struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	SourceBranch string `json:"sourceBranch"`
	TargetBranch string `json:"targetBranch"`
	URL          string `json:"url"`
}

// jobBranchProjectRow is one multibranch project of the plugin's
// branch-inventory response. The plugin sends lastScan as null until the
// scan path fills it, so the mite does not read that key yet.
type jobBranchProjectRow struct {
	JobPath        string             `json:"jobPath"`
	IndexedAt      string             `json:"indexedAt"`
	IndexResult    string             `json:"indexResult"`
	Heads          []jobBranchHeadRow `json:"heads"`
	HeadsTruncated bool               `json:"headsTruncated"`
}

// jobBranchResponse is the JSON envelope the branches endpoint returns.
type jobBranchResponse struct {
	Projects  []jobBranchProjectRow `json:"projects"`
	Total     int                   `json:"total"`
	Truncated bool                  `json:"truncated"`
}

// pollJobBranchesOnce performs one branch-inventory poll and replaces the
// cached set on success. A failed poll logs and keeps the last set. It
// never fails the schedule poll that calls it.
func (a *Agent) pollJobBranchesOnce(ctx context.Context, logger *slog.Logger) {
	set, err := a.pollJobBranches(ctx)
	if err != nil {
		logger.Warn("job-branch poll failed, keeping last known set", "error", err)
		return
	}
	a.jobScheduleMu.Lock()
	a.jobBranchSet = set
	a.jobScheduleMu.Unlock()
}

// pollJobBranches performs one GET /varroa-activity/branches request and
// converts the response into a JobBranchSet.
func (a *Agent) pollJobBranches(ctx context.Context) (*mitev1.JobBranchSet, error) {
	client := a.getJenkinsClient()

	httpResp, err := client.DoGet(ctx, jobBranchesPath)
	if err != nil {
		return nil, fmt.Errorf("do get: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status %d", httpResp.StatusCode)
	}

	var resp jobBranchResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	projects := normalizeJobBranchProjects(resp.Projects)
	items := make([]*mitev1.JobBranchProject, 0, len(projects))
	for _, p := range projects {
		heads := make([]*mitev1.JobBranchHead, 0, len(p.Heads))
		for _, h := range p.Heads {
			head := &mitev1.JobBranchHead{
				Name:     h.Name,
				JobPath:  h.JobPath,
				Kind:     h.Kind,
				Orphaned: h.Orphaned,
				Revision: h.Revision,
			}
			if h.ChangeRequest != nil {
				head.ChangeRequest = &mitev1.ChangeRequestInfo{
					Id:           h.ChangeRequest.ID,
					Title:        h.ChangeRequest.Title,
					SourceBranch: h.ChangeRequest.SourceBranch,
					TargetBranch: h.ChangeRequest.TargetBranch,
					URL:          h.ChangeRequest.URL,
				}
			}
			heads = append(heads, head)
		}
		items = append(items, &mitev1.JobBranchProject{
			JobPath:        p.JobPath,
			IndexedAt:      p.IndexedAt,
			IndexResult:    p.IndexResult,
			Heads:          heads,
			HeadsTruncated: p.HeadsTruncated,
		})
	}

	return &mitev1.JobBranchSet{
		Hash:      computeJobBranchHash(projects),
		Items:     items,
		Truncated: resp.Truncated,
		Total:     resp.Total,
	}, nil
}

// normalizeJobBranchProjects returns a copy of the projects sorted by job
// path, with each project's heads sorted by job path then name. The hash
// and the set use this order, so a plugin that lists items in another
// order sends the same set.
func normalizeJobBranchProjects(in []jobBranchProjectRow) []jobBranchProjectRow {
	out := make([]jobBranchProjectRow, len(in))
	copy(out, in)
	for i := range out {
		heads := make([]jobBranchHeadRow, len(out[i].Heads))
		copy(heads, out[i].Heads)
		sort.Slice(heads, func(a, b int) bool {
			if heads[a].JobPath != heads[b].JobPath {
				return heads[a].JobPath < heads[b].JobPath
			}
			return heads[a].Name < heads[b].Name
		})
		out[i].Heads = heads
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JobPath < out[j].JobPath })
	return out
}

// computeJobBranchHash hashes the normalized projects. The hash covers the
// whole row, so a changed head, revision, or change request changes it.
func computeJobBranchHash(projects []jobBranchProjectRow) string {
	h := sha256.New()
	for _, p := range normalizeJobBranchProjects(projects) {
		// A project row holds only strings, bools, and slices of those, so
		// json.Marshal cannot fail on it.
		b, _ := json.Marshal(p)
		h.Write(b)
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// maybeAttachJobBranches fills snap.JobBranches when the export gate is
// open and one of three conditions holds: the cached set's hash differs
// from the hash of the last set the mite sent, the stream reconnected
// since the last send, or the resend interval has passed since the last
// send. The set never contributes to any state hash.
func (a *Agent) maybeAttachJobBranches(snap *mitev1.StateSnapshot) {
	if !a.scheduleExportOpen() {
		return
	}

	a.jobScheduleMu.Lock()
	defer a.jobScheduleMu.Unlock()

	set := a.jobBranchSet
	if set == nil {
		return
	}

	resendAfter := a.jobScheduleResendAfter
	if resendAfter <= 0 {
		resendAfter = jobScheduleResendInterval
	}

	now := time.Now()
	changed := set.Hash != a.jobBranchLastSentHash
	stale := now.Sub(a.jobBranchLastSendTime) >= resendAfter
	if !changed && !a.jobBranchForceSend && !stale {
		return
	}

	snap.JobBranches = set
	a.jobBranchLastSentHash = set.Hash
	a.jobBranchLastSendTime = now
	a.jobBranchForceSend = false
}
