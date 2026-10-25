package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// JobBadgePattern is the mux pattern of the anonymous badge door. The door
// sits at the host root, outside /api/. The token in the URL is the
// credential, and the handler checks it in constant time.
const JobBadgePattern = "GET /badges/{tenant}/{token}/{file}"

// badgeRecentCompleted bounds the completed runs the badge reads. The badge
// skips TRIGGER_FAILED results, so it reads a few more than one.
const badgeRecentCompleted = 10

// badgeMaxCompletedPages bounds how far back the badge reads for a run that
// decides it. Runs of TRIGGER_FAILED do not decide, so a run of many trigger
// failures can push the deciding run back. Five pages is fifty runs.
const badgeMaxCompletedPages = 5

// badgeNotFoundBody is the one body every refusal of the badge door answers.
// A caller cannot tell an unknown tenant from a wrong token or a switch that
// is off.
const badgeNotFoundBody = "404 not found"

// jobBadgeJSON is the body of GET /pipelines/{tenant}/jobs/{job}/badge. Path
// is the host-root badge URL path, and it is set only when the badge is on.
type jobBadgeJSON struct {
	Enabled bool   `json:"enabled"`
	Path    string `json:"path,omitempty"`
}

// HandleJobBadge serves GET /badges/{tenant}/{token}/{file}. It answers an SVG
// only when the tenant switch is on and the token matches. Every refusal
// answers the same 404 body.
func (s *Server) HandleJobBadge(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		writeBadgeNotFound(w)
		return
	}
	file := r.PathValue("file")
	if !strings.HasSuffix(file, ".svg") {
		writeBadgeNotFound(w)
		return
	}
	rel := strings.TrimSuffix(file, ".svg")
	if !badgeRelPathOK(rel) {
		writeBadgeNotFound(w)
		return
	}

	ctx := r.Context()
	tm, err := crdstore.Get[v1alpha1.Tenant](ctx, s.deps.Store, r.PathValue("tenant"), "")
	if err != nil {
		writeBadgeNotFound(w)
		return
	}
	if p := tm.Spec.Pipelines; p == nil || p.Badge == nil || !p.Badge.Enabled {
		writeBadgeNotFound(w)
		return
	}
	if tm.Status.Pipelines == nil || tm.Status.Pipelines.BadgeToken == "" ||
		!constantTimeEqual(r.PathValue("token"), tm.Status.Pipelines.BadgeToken) {
		writeBadgeNotFound(w)
		return
	}

	label := badgeLabel(rel)
	pl := tm.Status.Placement
	if pl == nil || pl.Cell == "" {
		// A tenant with no placement has no record store rows to read.
		msg, color := badgeStateForRuns(nil, nil)
		s.writeBadge(w, renderBadgeSVG(label, msg, color), true)
		return
	}
	ref := TenantRef{
		Cluster:    pl.Cluster,
		Namespace:  pl.CellNamespace,
		Controller: pl.Cell,
		Tenant:     tm.Name,
		TenantName: tm.Name,
		Folder:     tm.Name,
	}
	// The folder prefix is added here, so the badge reads only jobs of this
	// tenant folder.
	jobPath := tm.Name + "/" + rel

	if r.URL.Query().Has("branch") {
		head, found, herr := s.badgeBranchHead(ctx, ref, jobPath, r.URL.Query().Get("branch"))
		if herr != nil {
			s.deps.Logger.Warn("badge branch lookup failed", "jobPath", jobPath, "error", herr)
			s.writeBadge(w, renderBadgeSVG(label, "unknown", badgeColorGrey), false)
			return
		}
		if !found {
			s.writeBadge(w, renderBadgeSVG(label, "unknown", badgeColorGrey), true)
			return
		}
		jobPath = head.JobPath
		label = badgeLabel(stripFolder(ref.Folder, jobPath))
	}

	msg, color, err := s.badgeStateForJob(ctx, ref, jobPath)
	if err != nil {
		s.deps.Logger.Warn("badge run lookup failed", "jobPath", jobPath, "error", err)
		s.writeBadge(w, renderBadgeSVG(label, "unknown", badgeColorGrey), false)
		return
	}
	s.writeBadge(w, renderBadgeSVG(label, msg, color), true)
}

// badgeStateForJob reads the runs that decide the badge of one job. It pages
// the completed runs, newest first, until one decides or the page bound is
// reached. It reads the newest run of any status only when no completed run
// decides.
func (s *Server) badgeStateForJob(ctx context.Context, ref TenantRef, jobPath string) (string, string, error) {
	var completed []recordstore.Run
	cursor := ""
	for range badgeMaxCompletedPages {
		page, err := s.deps.RecordStore.ListRuns(ctx, recordstore.RunQuery{
			Cluster: ref.Cluster, Tenant: ref.Tenant, JobPath: jobPath,
			Status: "COMPLETED", Limit: badgeRecentCompleted, Cursor: cursor,
		})
		if err != nil {
			return "", "", err
		}
		completed = append(completed, page.Runs...)
		if badgeHasDecidingRun(completed) || page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	var newest []recordstore.Run
	if !badgeHasDecidingRun(completed) {
		page, err := s.deps.RecordStore.ListRuns(ctx, recordstore.RunQuery{
			Cluster: ref.Cluster, Tenant: ref.Tenant, JobPath: jobPath, Limit: 1,
		})
		if err != nil {
			return "", "", err
		}
		newest = page.Runs
	}
	msg, color := badgeStateForRuns(completed, newest)
	return msg, color, nil
}

// badgeHasDecidingRun reports whether a completed run decides the badge.
func badgeHasDecidingRun(completed []recordstore.Run) bool {
	for _, run := range completed {
		if badgeRunDecides(run) {
			return true
		}
	}
	return false
}

// badgeBranchHead resolves ?branch= on a multibranch job through the branch
// inventory. A head of kind changeRequest, tag, or repository does not
// resolve, and neither does a head outside the tenant folder.
func (s *Server) badgeBranchHead(ctx context.Context, ref TenantRef, jobPath, branch string) (recordstore.JobBranchHead, bool, error) {
	project, found, err := s.deps.RecordStore.GetJobBranches(ctx, ref.Cluster, ref.Tenant, jobPath)
	if err != nil || !found {
		return recordstore.JobBranchHead{}, false, err
	}
	for _, head := range project.Heads {
		if head.Name != branch || !badgeBranchKind(head.Kind) {
			continue
		}
		if !strings.HasPrefix(head.JobPath, ref.Folder+"/") {
			continue
		}
		return head, true, nil
	}
	return recordstore.JobBranchHead{}, false, nil
}

// badgeBranchKind reports whether a branch head may show a badge.
func badgeBranchKind(kind string) bool {
	switch kind {
	case "changeRequest", "tag", "repository":
		return false
	default:
		return true
	}
}

// badgeRelPathOK reports whether a folder-relative job path is well formed.
// It refuses an empty path, a leading slash, an empty segment, and a dot
// segment.
func badgeRelPathOK(rel string) bool {
	if rel == "" || strings.HasPrefix(rel, "/") {
		return false
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// writeBadge answers an SVG. A cacheable answer carries a short public
// cache. An answer that could not read the record store is not cacheable.
func (s *Server) writeBadge(w http.ResponseWriter, svg []byte, cacheable bool) {
	h := w.Header()
	h.Set("Content-Type", "image/svg+xml; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	if cacheable {
		h.Set("Cache-Control", "public, max-age=60")
	} else {
		h.Set("Cache-Control", "no-store")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(svg)
}

// writeBadgeNotFound answers the one refusal body of the badge door.
func writeBadgeNotFound(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(badgeNotFoundBody))
}

// handleGetJobBadge serves GET /pipelines/{tenant}/jobs/{job}/badge. It tells
// the job page whether the badge is on and where its URL path is. The
// caller must read the job.
func (s *Server) handleGetJobBadge(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	jobPath := r.PathValue("job")

	refs, serr := s.resolveTenantTenants(ctx, claims, r.PathValue("tenant"))
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	ref, ok := s.tenantForJob(claims, refs, jobPath)
	if !ok {
		s.writeJSONError(w, http.StatusNotFound, "job not found")
		return
	}

	var out jobBadgeJSON
	if token, on := s.badgeTokenOf(ctx, ref); on {
		out.Enabled = true
		out.Path = "/badges/" + url.PathEscape(ref.TenantName) + "/" + token + "/" +
			url.PathEscape(stripFolder(ref.Folder, jobPath)) + ".svg"
	}
	s.writeJSON(w, http.StatusOK, out)
}

// badgeTokenOf returns the badge token of a folder-owned pipelines tenant when
// its switch is on. It answers false for a synthetic tenant, a tenant the
// local API server does not hold, and a tenant with the switch off.
func (s *Server) badgeTokenOf(ctx context.Context, ref TenantRef) (string, bool) {
	if ref.Folder == "" || strings.Contains(ref.Tenant, "/") {
		return "", false
	}
	tm, err := crdstore.Get[v1alpha1.Tenant](ctx, s.deps.Store, ref.TenantName, "")
	if err != nil {
		return "", false
	}
	if p := tm.Spec.Pipelines; p == nil || p.Badge == nil || !p.Badge.Enabled {
		return "", false
	}
	if tm.Status.Pipelines == nil || tm.Status.Pipelines.BadgeToken == "" {
		return "", false
	}
	return tm.Status.Pipelines.BadgeToken, true
}
