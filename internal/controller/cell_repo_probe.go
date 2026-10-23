package controller

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/forgeurl"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

const (
	// repoProbeInterval is the least time between two probes of one
	// tenant and repository.
	repoProbeInterval = 10 * time.Minute
	// maxRepoProbesPerTick bounds the new probes that one cell result starts.
	maxRepoProbesPerTick = 4
	// repoProbeTimeout bounds one probe. A probe runs on its own context,
	// never on the reconcile context.
	repoProbeTimeout = 45 * time.Second
)

// RepoGoneProber answers whether the forge confirms that a repository is
// gone. Only a true answer releases the deferred branch jobs of the
// repository. The operator's forge answerer implements it.
type RepoGoneProber interface {
	RepositoryGone(ctx context.Context, tenant, repoURL string) bool
}

// repoScan is one failed-scan folder of a tenant, with its repository URL.
type repoScan struct {
	Folder  string
	RepoURL string
}

// repoVerdict is the last probe answer of one tenant and repository.
type repoVerdict struct {
	gone     bool
	probedAt time.Time
	running  bool
}

// repoProbeState holds the probe cache of one reconciler. Every field is
// guarded by mu. The zero value is ready to use, and a nil prober keeps
// every answer unknown.
type repoProbeState struct {
	mu     sync.Mutex
	prober RepoGoneProber
	// goFn starts one probe. A nil goFn starts a goroutine. A test sets it
	// to run the probe inline.
	goFn     func(func())
	scans    map[string]map[string][]repoScan // cell key -> tenant -> failed scans
	verdicts map[string]*repoVerdict          // tenant NUL repo URL -> verdict
}

// SetRepoGoneProber installs the forge answerer. It runs before the manager
// starts, so the probe state needs no further setup.
func (r *Reconciler) SetRepoGoneProber(p RepoGoneProber) {
	r.repoProbe.mu.Lock()
	defer r.repoProbe.mu.Unlock()
	r.repoProbe.prober = p
}

// repoVerdictKey names the verdict of one tenant and repository. Two spellings
// of one repository share the key, because the probe normalizes the URL the same way.
func repoVerdictKey(tenant, repoURL string) string {
	if repo, err := forgeurl.NormalizeRepoURL(repoURL); err == nil {
		repoURL = repo.Host + "/" + repo.Owner + "/" + repo.Name
	}
	return tenant + "\x00" + repoURL
}

// noteRepoScans records the failed scans of each matched tenant of one
// cell result and starts the probes that are due. A result that is not
// successful, or whose branch inventory is unknown, changes nothing. The
// tenants that the result matched take the scans that they own. A scan
// without a repository URL cannot be probed, so it is not recorded.
func (r *Reconciler) noteRepoScans(cellKey string, d *mitev1.DesiredStateResult, matched map[string]bool, tenants []string, now time.Time) {
	st := &r.repoProbe
	st.mu.Lock()
	if st.prober == nil || d == nil || !d.RehydrationSuccess || d.RehydrationBranchScansUnknown || len(matched) == 0 {
		st.mu.Unlock()
		return
	}
	if st.scans == nil {
		st.scans = make(map[string]map[string][]repoScan)
	}
	if st.verdicts == nil {
		st.verdicts = make(map[string]*repoVerdict)
	}
	byTenant := st.scans[cellKey]
	if byTenant == nil {
		byTenant = make(map[string][]repoScan)
		st.scans[cellKey] = byTenant
	}
	byFolder := make(map[string]*mitev1.RehydrationFailedScan, len(d.RehydrationFailedScans))
	folders := make([]string, 0, len(d.RehydrationFailedScans))
	for _, s := range d.RehydrationFailedScans {
		if s == nil || s.RepoUrl == "" {
			continue
		}
		byFolder[s.Folder] = s
		folders = append(folders, s.Folder)
	}
	owned := partitionJobPathsByTenant(folders, tenants)
	for tenant := range matched {
		old := byTenant[tenant]
		var list []repoScan
		for _, folder := range owned[tenant] {
			s := byFolder[folder]
			list = append(list, repoScan{Folder: folder, RepoURL: s.RepoUrl})
		}
		if len(list) == 0 {
			delete(byTenant, tenant)
		} else {
			byTenant[tenant] = list
		}
		// A verdict leaves with its last folder: a folder that no longer
		// fails must not keep a gone verdict for a later failure. A probe
		// still running for a dropped verdict writes into that detached
		// verdict only, so its answer cannot outlive the folder.
		for _, o := range old {
			if !st.tracked(tenant, o.RepoURL) {
				delete(st.verdicts, repoVerdictKey(tenant, o.RepoURL))
			}
		}
	}

	type probe struct {
		tenant, repoURL string
		verdict         *repoVerdict
	}
	var due []probe
	for tenant, list := range byTenant {
		for _, s := range list {
			if len(due) >= maxRepoProbesPerTick {
				break
			}
			key := repoVerdictKey(tenant, s.RepoURL)
			v := st.verdicts[key]
			if v == nil {
				v = &repoVerdict{}
				st.verdicts[key] = v
			}
			if v.running || (!v.probedAt.IsZero() && now.Sub(v.probedAt) < repoProbeInterval) {
				continue
			}
			v.running = true
			due = append(due, probe{tenant: tenant, repoURL: s.RepoURL, verdict: v})
		}
	}
	prober := st.prober
	goFn := st.goFn
	st.mu.Unlock()

	for _, p := range due {
		run := func() {
			ctx, cancel := context.WithTimeout(context.Background(), repoProbeTimeout)
			defer cancel()
			gone := prober.RepositoryGone(ctx, p.tenant, p.repoURL)
			st.mu.Lock()
			defer st.mu.Unlock()
			p.verdict.running = false
			// Store the answer only in the verdict that the map still holds.
			if st.verdicts[repoVerdictKey(p.tenant, p.repoURL)] == p.verdict {
				p.verdict.gone = gone
				p.verdict.probedAt = now
			}
		}
		if goFn != nil {
			goFn(run)
		} else {
			go run()
		}
	}
}

// repoGoneFolders returns the sorted failed-scan folders of cellKey whose
// repository a probe confirmed gone. It answers nil when none is.
func (r *Reconciler) repoGoneFolders(cellKey string) []string {
	st := &r.repoProbe
	st.mu.Lock()
	defer st.mu.Unlock()
	seen := make(map[string]bool)
	var out []string
	for tenant, list := range st.scans[cellKey] {
		for _, s := range list {
			v := st.verdicts[repoVerdictKey(tenant, s.RepoURL)]
			if v != nil && v.gone && !seen[s.Folder] {
				seen[s.Folder] = true
				out = append(out, s.Folder)
			}
		}
	}
	sort.Strings(out)
	return out
}

// dropRepoScans forgets the failed scans of cellKey.
func (r *Reconciler) dropRepoScans(cellKey string) {
	st := &r.repoProbe
	st.mu.Lock()
	defer st.mu.Unlock()
	// The verdicts that no other cell still tracks leave with the cell's
	// scans, so a cell placed again reprobes instead of reusing an answer
	// it cannot account for.
	cellScans := st.scans[cellKey]
	delete(st.scans, cellKey)
	for tenant, list := range cellScans {
		for _, s := range list {
			if !st.tracked(tenant, s.RepoURL) {
				delete(st.verdicts, repoVerdictKey(tenant, s.RepoURL))
			}
		}
	}
}

// tracked reports whether any cell still holds a failed scan of repoURL for
// tenant. The verdict of a repository is shared by every cell of the tenant.
func (st *repoProbeState) tracked(tenant, repoURL string) bool {
	for _, byTenant := range st.scans {
		for _, s := range byTenant[tenant] {
			if repoVerdictKey(tenant, s.RepoURL) == repoVerdictKey(tenant, repoURL) {
				return true
			}
		}
	}
	return false
}
