package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// jenkinsLinkJSON is the body of GET .../jenkins-link: the Varroa page a
// Jenkins item path under a headless controller resolves to.
type jenkinsLinkJSON struct {
	Link string `json:"link"`
}

// parseJenkinsItemPath reads the Jenkins item path that follows the
// headless door prefix. It returns the job path (job names joined with "/",
// each decoded once) and the build number, or ("", 0) when the path names no
// job or a segment does not decode. A view segment is skipped. A build number
// is recognized only directly after the last job segment. Any other trailing
// segment, such as lastBuild or display, leaves the build number at zero.
func parseJenkinsItemPath(raw string) (string, int64) {
	segs := strings.Split(strings.Trim(raw, "/"), "/")
	var jobs []string
	var build int64
	for i := 0; i < len(segs); i++ {
		seg := segs[i]
		if seg == "view" && i+1 < len(segs) {
			i++
			continue
		}
		if seg == "job" && i+1 < len(segs) {
			name, err := url.PathUnescape(segs[i+1])
			if err != nil || name == "" {
				return "", 0
			}
			jobs = append(jobs, name)
			i++
			continue
		}
		if len(jobs) > 0 {
			if n, err := strconv.ParseInt(seg, 10, 64); err == nil && n > 0 && isAllDigits(seg) {
				build = n
			}
		}
		break
	}
	if len(jobs) == 0 {
		return "", 0
	}
	return strings.Join(jobs, "/"), build
}

// isAllDigits reports whether s holds only ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// handleControllerJenkinsLink answers GET .../jenkins-link?path=<item path>.
// It finds the run that the item path names, authorizes it with the run read
// check, and answers the Varroa build page, or the job page when the path has
// no build number. Every failure to match or read a run answers 404.
func (s *Server) handleControllerJenkinsLink(w http.ResponseWriter, r *http.Request, cluster, namespace, name string) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	raw := r.URL.Query().Get("path")
	if raw == "" {
		s.writeJSONError(w, http.StatusBadRequest, "path is required")
		return
	}
	jobPath, build := parseJenkinsItemPath(raw)
	if jobPath == "" {
		s.writeJSONError(w, http.StatusNotFound, "no job at path")
		return
	}

	// A cell hosts Teams as top-level folders, so the first job segment can
	// name the tenant. A dedicated controller names its tenant namespace/name.
	var candidates []string
	if i := strings.Index(jobPath, "/"); i > 0 {
		candidates = append(candidates, jobPath[:i])
	}
	candidates = append(candidates, namespace+"/"+name)

	ctx := r.Context()
	var run recordstore.Run
	found := false
	for _, tenant := range candidates {
		// The query keeps only this controller's runs, so the first hit is the
		// run this door names, whatever other controllers hold.
		result, err := s.deps.RecordStore.ListRuns(ctx, recordstore.RunQuery{
			Cluster: cluster, Tenant: tenant, JobPath: jobPath, BuildNumber: build,
			Namespace: namespace, Cell: name, Limit: 1,
		})
		if s.handleReaderError(w, err, "resolve jenkins link") {
			return
		}
		if len(result.Runs) > 0 {
			run = result.Runs[0]
			found = true
			break
		}
	}
	if !found {
		s.writeJSONError(w, http.StatusNotFound, "no run matches the path")
		return
	}

	claims := auth.ClaimsFromContext(ctx)
	if _, _, ok := s.resolveRunOrNotFound(w, r, claims, run.RunID); !ok {
		return
	}
	ref, err := TenantRefFromRow(run.Cluster, run.Namespace, run.Cell, run.Tenant)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "resolve tenant")
		return
	}
	link := searchJobLink(ref.Team, run.JobPath)
	if build > 0 {
		link = searchRunLink(ref.Team, run.JobPath, run.RunID)
	}
	s.writeJSON(w, http.StatusOK, jenkinsLinkJSON{Link: link})
}
