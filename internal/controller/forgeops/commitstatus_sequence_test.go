package forgeops

import (
	"context"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// latestForge is a forge that keeps every status write of one commit in
// order. CommitStatuses answers the latest status of each context, as the
// combined status endpoint of GitHub and of Gitea does. A plugin write goes
// through plugin and is not counted. A write of Ops goes through
// SetCommitStatus and is counted in statusWrites.
type latestForge struct {
	*fakeClient
	history []forge.CommitStatus
}

func newLatestForge() *latestForge {
	return &latestForge{fakeClient: pushable()}
}

// plugin records a status write of the branch-source plugin.
func (f *latestForge) plugin(s forge.CommitStatus) {
	f.history = append(f.history, s)
}

// CommitStatuses answers the latest status of each context, in the order
// of the first write of each context.
func (f *latestForge) CommitStatuses(ctx context.Context, r forge.Repo, sha string) ([]forge.CommitStatus, error) {
	if _, err := f.fakeClient.CommitStatuses(ctx, r, sha); err != nil {
		return nil, err
	}
	index := map[string]int{}
	var latest []forge.CommitStatus
	for _, s := range f.history {
		if i, ok := index[s.Context]; ok {
			latest[i] = s
			continue
		}
		index[s.Context] = len(latest)
		latest = append(latest, s)
	}
	return latest, nil
}

// SetCommitStatus records the write of Ops and adds it to the history.
func (f *latestForge) SetCommitStatus(ctx context.Context, r forge.Repo, sha string, s forge.CommitStatus) error {
	if err := f.fakeClient.SetCommitStatus(ctx, r, sha, s); err != nil {
		return err
	}
	f.history = append(f.history, s)
	return nil
}

// latest answers the latest status of context name.
func (f *latestForge) latest(t *testing.T, name string) forge.CommitStatus {
	t.Helper()
	statuses, err := f.CommitStatuses(context.Background(), forge.Repo{}, testSHA)
	if err != nil {
		t.Fatalf("read statuses: %v", err)
	}
	for _, s := range statuses {
		if s.Context == name {
			return s
		}
	}
	t.Fatalf("no status for context %q", name)
	return forge.CommitStatus{}
}

// oneLatestWriter sets a single pushing credential "a" that answers with f.
func oneLatestWriter(fx *fixture, f *latestForge) {
	fx.secrets.addCred("sa", "a", "tok-a")
	fx.clients[forge.NewToken([]byte("tok-a"))] = f
}

// sequenceContexts are the default branch-source contexts of a branch build
// of payments/app/main on each forge kind.
var sequenceContexts = map[forge.Kind]string{
	forge.KindGitHub: "continuous-integration/jenkins/branch",
	forge.KindGitea:  giteaHeadContext,
}

// interruptedOlderReq returns the end request of the recorder for build 7,
// which it marked cell-rolled.
func interruptedOlderReq() forgeapi.CommitStatusRequest {
	return forgeapi.CommitStatusRequest{
		Tenant:      "payments",
		Repo:        testRepoURL,
		SHA:         testSHA,
		JobPath:     "payments/app/main",
		BuildNumber: 7,
		Reason:      "cell-rolled",
		JenkinsURL:  statusRoot,
	}
}

// TestStatusSequence_EndOfOlderBuildKeepsNewestResult builds the sequence of
// two builds of one job and one commit. Build 7 starts, build 8 starts, and
// build 8 completes. Then the recorder ends build 7. Both builds write one
// context, so the latest status of that context names build 8. The end write
// changes only a pending status of build 7, so it writes nothing, and the
// result of build 8 stays on the commit.
func TestStatusSequence_EndOfOlderBuildKeepsNewestResult(t *testing.T) {
	for kind, name := range sequenceContexts {
		t.Run(string(kind), func(t *testing.T) {
			fx := newFixture(t, "agents-payments")
			fx.kind = kind
			f := newLatestForge()
			oneLatestWriter(fx, f)
			f.plugin(forge.CommitStatus{Context: name, State: forge.StatePending, TargetURL: statusJobURL, Description: "queued"})
			f.plugin(forge.CommitStatus{Context: name, State: forge.StatePending, TargetURL: statusOlderURL, Description: "build 7 runs"})
			f.plugin(forge.CommitStatus{Context: name, State: forge.StatePending, TargetURL: statusJobURL, Description: "queued"})
			f.plugin(forge.CommitStatus{Context: name, State: forge.StatePending, TargetURL: statusNewestURL, Description: "build 8 runs"})
			f.plugin(forge.CommitStatus{Context: name, State: forge.StateSuccess, TargetURL: statusNewestURL, Description: "build 8 passed"})

			n, err := fx.ops.EndPendingStatus(context.Background(), interruptedOlderReq())
			if err != nil || n != 0 || len(f.statusWrites) != 0 {
				t.Fatalf("written %d, writes %+v, err %v; want none", n, f.statusWrites, err)
			}
			if got := f.latest(t, name); got.State != forge.StateSuccess || got.TargetURL != statusNewestURL {
				t.Fatalf("latest status = %+v, want the success of build 8", got)
			}
		})
	}
}

// TestStatusSequence_LatePendingOfOlderBuildNeedsRestore builds the one
// sequence in which the end of build 7 does write. Build 8 completes first.
// Build 7 then writes its pending status, for example at a late checkout,
// and that status replaces the result of build 8. The end write sets that
// pending status of build 7 to error. Only a restore of build 8 after the
// end puts the result of build 8 back. The restore and the end can arrive
// in each order, and both orders leave the result of build 8.
func TestStatusSequence_LatePendingOfOlderBuildNeedsRestore(t *testing.T) {
	for kind, name := range sequenceContexts {
		late := func(t *testing.T) (*fixture, *latestForge) {
			t.Helper()
			fx := newFixture(t, "agents-payments")
			fx.kind = kind
			f := newLatestForge()
			oneLatestWriter(fx, f)
			f.plugin(forge.CommitStatus{Context: name, State: forge.StatePending, TargetURL: statusNewestURL, Description: "build 8 runs"})
			f.plugin(forge.CommitStatus{Context: name, State: forge.StateSuccess, TargetURL: statusNewestURL, Description: "build 8 passed"})
			f.plugin(forge.CommitStatus{Context: name, State: forge.StatePending, TargetURL: statusOlderURL, Description: "build 7 runs"})
			return fx, f
		}
		wantNewest := func(t *testing.T, f *latestForge) {
			t.Helper()
			got := f.latest(t, name)
			if got.State != forge.StateSuccess || got.TargetURL != statusNewestURL {
				t.Fatalf("latest status = %+v, want the success of build 8", got)
			}
		}

		t.Run(string(kind)+" end alone", func(t *testing.T) {
			fx, f := late(t)
			n, err := fx.ops.EndPendingStatus(context.Background(), interruptedOlderReq())
			if err != nil || n != 1 {
				t.Fatalf("written %d, err %v; want 1, nil", n, err)
			}
			if got := f.latest(t, name); got.State != forge.StateError || got.TargetURL != statusOlderURL {
				t.Fatalf("latest status = %+v, want the error of build 7", got)
			}
		})

		t.Run(string(kind)+" end then restore", func(t *testing.T) {
			fx, f := late(t)
			if _, err := fx.ops.EndPendingStatus(context.Background(), interruptedOlderReq()); err != nil {
				t.Fatalf("end: %v", err)
			}
			n, err := fx.ops.RestoreNewestStatus(context.Background(), newestReq("SUCCESS"))
			if err != nil || n != 1 {
				t.Fatalf("restore written %d, err %v; want 1, nil", n, err)
			}
			wantNewest(t, f)
		})

		t.Run(string(kind)+" restore then end", func(t *testing.T) {
			fx, f := late(t)
			if _, err := fx.ops.RestoreNewestStatus(context.Background(), newestReq("SUCCESS")); err != nil {
				t.Fatalf("restore: %v", err)
			}
			n, err := fx.ops.EndPendingStatus(context.Background(), interruptedOlderReq())
			if err != nil || n != 0 {
				t.Fatalf("end written %d, err %v; want 0, nil", n, err)
			}
			wantNewest(t, f)
		})
	}
}
