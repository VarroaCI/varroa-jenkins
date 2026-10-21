package controller

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// countingProber answers gone for each URL in goneURLs and counts its calls.
type countingProber struct {
	mu       sync.Mutex
	calls    int
	goneURLs map[string]bool
}

func (p *countingProber) RepositoryGone(_ context.Context, _ string, repoURL string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.goneURLs[repoURL]
}

func (p *countingProber) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// inlineProbeReconciler returns a reconciler whose probes run inline.
func inlineProbeReconciler(p RepoGoneProber) *Reconciler {
	r := &Reconciler{}
	r.repoProbe.prober = p
	r.repoProbe.goFn = func(f func()) { f() }
	return r
}

// successResult returns a successful rehydration result with the failed scans.
func successResult(scans ...*mitev1.RehydrationFailedScan) *mitev1.DesiredStateResult {
	return &mitev1.DesiredStateResult{RehydrationSuccess: true, RehydrationFailedScans: scans}
}

func failedScan(folder, repoURL string) *mitev1.RehydrationFailedScan {
	return &mitev1.RehydrationFailedScan{Folder: folder, IndexResult: "FAILURE", DeferredJobs: 1, RepoUrl: repoURL}
}

func TestNoteRepoScansRateBound(t *testing.T) {
	const cell = "ns/cell"
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	prober := &countingProber{}
	r := inlineProbeReconciler(prober)
	d := successResult(failedScan("t1/mb", "https://git.example.org/o/r"))
	matched := map[string]bool{"t1": true}
	tenants := []string{"t1"}

	r.noteRepoScans(cell, d, matched, tenants, now)
	r.noteRepoScans(cell, d, matched, tenants, now.Add(time.Minute))
	if got := prober.count(); got != 1 {
		t.Fatalf("probes inside the interval = %d, want 1", got)
	}
	r.noteRepoScans(cell, d, matched, tenants, now.Add(11*time.Minute))
	if got := prober.count(); got != 2 {
		t.Fatalf("probes after the interval = %d, want 2", got)
	}
}

func TestNoteRepoScansCapsProbesPerTick(t *testing.T) {
	const cell = "ns/cell"
	prober := &countingProber{}
	r := inlineProbeReconciler(prober)
	scans := make([]*mitev1.RehydrationFailedScan, 0, 5)
	matched := map[string]bool{}
	tenants := make([]string, 0, 5)
	for _, tn := range []string{"t1", "t2", "t3", "t4", "t5"} {
		scans = append(scans, failedScan(tn+"/mb", "https://git.example.org/o/"+tn))
		matched[tn] = true
		tenants = append(tenants, tn)
	}
	r.noteRepoScans(cell, successResult(scans...), matched, tenants, time.Now())
	if got := prober.count(); got != maxRepoProbesPerTick {
		t.Fatalf("probes in one tick = %d, want %d", got, maxRepoProbesPerTick)
	}
}

func TestRepoGoneFoldersOnlyGone(t *testing.T) {
	const cell = "ns/cell"
	prober := &countingProber{goneURLs: map[string]bool{"https://git.example.org/o/gone": true}}
	r := inlineProbeReconciler(prober)
	d := successResult(
		failedScan("t/a", "https://git.example.org/o/gone"),
		failedScan("t/b", "https://git.example.org/o/present"),
	)
	r.noteRepoScans(cell, d, map[string]bool{"t": true}, []string{"t"}, time.Now())
	if got, want := r.repoGoneFolders(cell), []string{"t/a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("repoGoneFolders = %v, want %v", got, want)
	}
}

func TestNoteRepoScansClearsTenantWhenScanGone(t *testing.T) {
	const cell = "ns/cell"
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	prober := &countingProber{goneURLs: map[string]bool{"https://git.example.org/o/gone": true}}
	r := inlineProbeReconciler(prober)
	matched := map[string]bool{"t": true}
	tenants := []string{"t"}
	r.noteRepoScans(cell, successResult(failedScan("t/a", "https://git.example.org/o/gone")), matched, tenants, now)
	if got := r.repoGoneFolders(cell); len(got) != 1 {
		t.Fatalf("repoGoneFolders = %v, want one folder", got)
	}
	// The next successful result no longer lists the folder: its verdict leaves too.
	r.noteRepoScans(cell, successResult(), matched, tenants, now.Add(time.Minute))
	if got := r.repoGoneFolders(cell); got != nil {
		t.Fatalf("repoGoneFolders after the scan succeeded = %v, want nil", got)
	}
	if n := len(r.repoProbe.verdicts); n != 0 {
		t.Fatalf("verdicts after the folder left the list = %d, want 0", n)
	}
}

func TestNoteRepoScansSkipsUnknownResult(t *testing.T) {
	const cell = "ns/cell"
	scan := failedScan("t/a", "https://git.example.org/o/gone")
	matched := map[string]bool{"t": true}
	tenants := []string{"t"}
	cases := map[string]*mitev1.DesiredStateResult{
		"unknown branch scans": {RehydrationSuccess: true, RehydrationBranchScansUnknown: true, RehydrationFailedScans: []*mitev1.RehydrationFailedScan{scan}},
		"failed rehydration":   {RehydrationSuccess: false, RehydrationFailedScans: []*mitev1.RehydrationFailedScan{scan}},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			prober := &countingProber{}
			r := inlineProbeReconciler(prober)
			r.noteRepoScans(cell, d, matched, tenants, time.Now())
			if got := prober.count(); got != 0 {
				t.Fatalf("probes = %d, want 0", got)
			}
			if got := r.repoGoneFolders(cell); got != nil {
				t.Fatalf("repoGoneFolders = %v, want nil", got)
			}
		})
	}

	t.Run("nil prober", func(t *testing.T) {
		r := inlineProbeReconciler(nil)
		r.noteRepoScans(cell, successResult(scan), matched, tenants, time.Now())
		if got := r.repoGoneFolders(cell); got != nil {
			t.Fatalf("repoGoneFolders = %v, want nil", got)
		}
	})
}
