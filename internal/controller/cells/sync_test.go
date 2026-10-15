package cells

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// --- task 4.2: the announcement pre-step for a first placement and for an
// involuntary move ---

func TestPlace_FirstPlacement_AnnouncesBeforePlacementWrite(t *testing.T) {
	var sawPlacementAtCall bool
	var calls int
	in := testInputs()
	in.Cells = []CellCandidate{readyCell("cell-a")}
	team := testTeam()
	status := &v1alpha1.TeamStatus{}

	// Place mutates status only after a successful announce, so the
	// announce closure observing status.Placement == nil proves the call
	// happened first.
	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		calls++
		if status.Placement != nil {
			sawPlacementAtCall = true
		}
		return AnnounceResult{OK: true}, nil
	}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one announce call, got %d", calls)
	}
	if sawPlacementAtCall {
		t.Fatal("announce must be called before status.Placement is written")
	}
	if status.Placement == nil || status.Placement.Cell != "cell-a" || status.Placement.Generation != 1 {
		t.Fatalf("expected the placement to be written after a successful announce, got %+v", status.Placement)
	}
}

func TestPlace_InvoluntaryMove_AnnouncesBeforePlacementWrite(t *testing.T) {
	var sawNewPlacementAtCall bool
	var calls int
	in := testInputs()
	evacuating := readyCell("cell-a")
	evacuating.Evacuate = true
	fresh := readyCell("cell-b")
	in.Cells = []CellCandidate{evacuating, fresh}
	team := testTeam()
	status := placedStatus(1)

	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		calls++
		if req.Cell != status.Placement.Cell {
			// status.Placement still names the OLD cell/generation at
			// call time: the announce request already names the NEW one.
			sawNewPlacementAtCall = true
		}
		return AnnounceResult{OK: true}, nil
	}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one announce call, got %d", calls)
	}
	if !sawNewPlacementAtCall {
		t.Fatal("announce must be called (naming the new cell) before status.Placement is overwritten")
	}
	if status.Placement.Cell != "cell-b" || status.Placement.Generation != 2 {
		t.Fatalf("expected the move to complete, got %+v", status.Placement)
	}
}

// TestPlace_Move_FailedAnnouncementStopsTheMove pins the tenant-placement
// spec's failure rule: a failed announcement leaves the placement and the
// Placed reason untouched, and Place retries on the next tick.
func TestPlace_Move_FailedAnnouncementStopsTheMove(t *testing.T) {
	in := testInputs()
	in.Announce = failingAnnounce()
	in.Cells = []CellCandidate{readyCell("cell-a")}
	team := testTeam()
	status := &v1alpha1.TeamStatus{}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if status.Placement != nil {
		t.Fatalf("expected no placement after a failed announcement, got %+v", status.Placement)
	}
	if cond := condition(status); cond != nil {
		t.Fatalf("expected no Placed condition change after a failed announcement, got %+v", cond)
	}
}

// --- task 4.3: the announcement pre-step for a voluntary move ---

func TestPlace_VoluntaryMove_AnnouncesAfterDrain(t *testing.T) {
	in := testInputs()
	mismatched := readyCell("cell-a")
	mismatched.ResolvedProfile = "other-profile"
	other := readyCell("cell-b")
	in.Cells = []CellCandidate{mismatched, other}
	team := testTeam()
	status := placedStatus(1)
	status.Conditions = []v1alpha1.TeamCondition{{
		Type: v1alpha1.TeamConditionPlaced, Status: metav1.ConditionFalse,
		Reason: v1alpha1.TeamReasonTenantMoving, Message: tenantMovingMessage,
	}}

	var calls int
	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		calls++
		if req.Cell != "cell-b" || req.Generation != 2 {
			t.Errorf("expected the announce to already name the new cell and generation, got %+v", req)
		}
		if status.Placement.Cell != "cell-a" {
			t.Error("status.Placement must still name the old cell at call time")
		}
		return AnnounceResult{OK: true}, nil
	}

	// Tick: the drain has already completed (mismatched.AppliedTenants
	// does not name the tenant, and there is no pending deletion), so this
	// call performs the move.
	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one announce call, got %d", calls)
	}
	if status.Placement.Cell != "cell-b" || status.Placement.Generation != 2 {
		t.Fatalf("expected the drained move to complete, got %+v", status.Placement)
	}
}

// --- task 4.4: policy, presence, and dead_instances ---

func TestPlace_Announce_CarriesDefaultsAndPresence(t *testing.T) {
	in := testInputs()
	t1 := fixedNow.Add(-time.Minute)
	cell := readyCell("cell-a")
	cell.Instance = "uid-1"
	cell.MiteConnected = true
	cell.MiteLastSeen = &t1
	in.Cells = []CellCandidate{cell}
	team := testTeam() // spec.pipelines sets none of the seven settings.
	status := &v1alpha1.TeamStatus{}

	var got AnnounceRequest
	var calls int
	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		calls++
		got = req
		return AnnounceResult{OK: true}, nil
	}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("Place returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one announce call, got %d", calls)
	}
	wantPolicy := SyncPolicyFromPipelines(team.Spec.Pipelines)
	if !got.Policy.Equal(wantPolicy) {
		t.Errorf("Policy = %+v, want every default: %+v", got.Policy, wantPolicy)
	}
	if got.CellInstance != "uid-1" {
		t.Errorf("CellInstance = %q, want uid-1", got.CellInstance)
	}
	if !got.CellAlive {
		t.Error("CellAlive = false, want true")
	}
	if got.CellAliveAt == nil {
		t.Error("CellAliveAt is nil")
	}
	if got.CellLastSeenAt == nil || !got.CellLastSeenAt.Equal(t1) {
		t.Errorf("CellLastSeenAt = %v, want %v", got.CellLastSeenAt, t1)
	}
	// A first placement raises the generation, so the seed state is
	// forced empty regardless of any input.
	if got.RehydratedInstance != "" || len(got.PendingJobs) != 0 {
		t.Errorf("expected an empty seed state on a generation raise, got instance=%q jobs=%v", got.RehydratedInstance, got.PendingJobs)
	}
}

func TestPlace_Announce_RolledPodAddsDeadInstance(t *testing.T) {
	in := testInputs()
	t1 := fixedNow.Add(-2 * time.Minute)
	cellTick1 := readyCell("cell-a")
	cellTick1.Instance = "uid-1"
	cellTick1.MiteConnected = true
	cellTick1.MiteLastSeen = &t1
	in.Cells = []CellCandidate{cellTick1}
	team := testTeam()
	status := &v1alpha1.TeamStatus{}

	out1, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("tick 1: Place returned error: %v", err)
	}
	if out1.SyncCacheUpdate == nil {
		t.Fatal("tick 1: expected a SyncCacheUpdate after a successful announce")
	}

	// Tick 2: the same cell rolled to a new pod.
	t2 := fixedNow.Add(time.Minute)
	cellTick2 := readyCell("cell-a")
	cellTick2.Instance = "uid-2"
	cellTick2.MiteConnected = true
	cellTick2.MiteLastSeen = &t2
	in.Cells = []CellCandidate{cellTick2}
	in.LastSync = out1.SyncCacheUpdate
	in.Now = fixedClock(fixedNow.Add(time.Minute)) // well inside the resync window

	var got AnnounceRequest
	var calls int
	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		calls++
		got = req
		return AnnounceResult{OK: true}, nil
	}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("tick 2: Place returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one reannounce, got %d", calls)
	}
	if len(got.DeadInstances) != 1 {
		t.Fatalf("expected one dead instance, got %+v", got.DeadInstances)
	}
	if got.DeadInstances[0].Instance != "uid-1" || !got.DeadInstances[0].LastSeenAt.Equal(t1) {
		t.Errorf("DeadInstances[0] = %+v, want instance uid-1 last seen at %v", got.DeadInstances[0], t1)
	}
}

// --- task 4.4a: the same-generation reannounce triggers ---

func TestPlace_Reannounce_PodRoll(t *testing.T) {
	in := testInputs()
	cellTick1 := readyCell("cell-a")
	cellTick1.Instance = "uid-1"
	in.Cells = []CellCandidate{cellTick1}
	team := testTeam()
	status := &v1alpha1.TeamStatus{}
	out1, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}

	cellTick2 := readyCell("cell-a")
	cellTick2.Instance = "uid-2"
	in.Cells = []CellCandidate{cellTick2}
	in.LastSync = out1.SyncCacheUpdate
	in.Now = fixedClock(fixedNow.Add(time.Minute))

	var calls int
	var got AnnounceRequest
	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		calls++
		got = req
		return AnnounceResult{OK: true}, nil
	}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected one reannounce for the pod roll, got %d", calls)
	}
	if got.Generation != status.Placement.Generation {
		t.Errorf("expected the reannounce to stay at generation %d, got %d", status.Placement.Generation, got.Generation)
	}
	if got.CellInstance != "uid-2" {
		t.Errorf("CellInstance = %q, want uid-2", got.CellInstance)
	}
}

func TestPlace_Reannounce_PolicyEdit(t *testing.T) {
	in := testInputs()
	cell := readyCell("cell-a")
	cell.Instance = "uid-1"
	in.Cells = []CellCandidate{cell}
	team := testTeam()
	status := &v1alpha1.TeamStatus{}
	out1, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}

	team.Spec.Pipelines.Concurrency = "cancelInProgress"
	in.LastSync = out1.SyncCacheUpdate
	in.Now = fixedClock(fixedNow.Add(time.Minute))

	var calls int
	var got AnnounceRequest
	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		calls++
		got = req
		return AnnounceResult{OK: true}, nil
	}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected one reannounce for the policy edit, got %d", calls)
	}
	if got.Generation != status.Placement.Generation {
		t.Errorf("expected the reannounce to stay at generation %d, got %d", status.Placement.Generation, got.Generation)
	}
	if got.Policy.Concurrency != "cancelInProgress" {
		t.Errorf("Policy.Concurrency = %q, want cancelInProgress", got.Policy.Concurrency)
	}
}

// TestPlace_Reannounce_ExportsEdit covers task 7.3: an edited
// spec.pipelines.exports reaches the announce body on the next tick,
// and the edit alone never raises the generation — evaluateKeep tests
// neither Profile drift nor Exports, so an exports-only edit always
// stays on the moveNone/reannounce path.
func TestPlace_Reannounce_ExportsEdit(t *testing.T) {
	in := testInputs()
	cell := readyCell("cell-a")
	cell.Instance = "uid-1"
	in.Cells = []CellCandidate{cell}
	team := testTeam()
	status := &v1alpha1.TeamStatus{}
	out1, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	generationBefore := status.Placement.Generation

	team.Spec.Pipelines.Exports = []v1alpha1.TeamExport{
		{Job: "release/*", Callers: []string{"payments", "search"}},
	}
	in.LastSync = out1.SyncCacheUpdate
	in.Now = fixedClock(fixedNow.Add(time.Minute))

	var calls int
	var got AnnounceRequest
	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		calls++
		got = req
		return AnnounceResult{OK: true}, nil
	}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected one reannounce for the exports edit, got %d", calls)
	}
	if status.Placement.Generation != generationBefore {
		t.Errorf("expected the generation to stay at %d for an exports-only edit, got %d", generationBefore, status.Placement.Generation)
	}
	if got.Generation != generationBefore {
		t.Errorf("expected the reannounce to carry generation %d, got %d", generationBefore, got.Generation)
	}
	if len(got.Policy.Exports) != 1 || got.Policy.Exports[0].Job != "release/*" ||
		len(got.Policy.Exports[0].Callers) != 2 {
		t.Errorf("Policy.Exports = %+v, want one release/* export for payments and search", got.Policy.Exports)
	}
}

// TestMaybeReannounceOnAgentNamespaceChange asserts the first
// announcement carries the tenant's agent namespace, and an edited
// agent namespace alone triggers a same-generation reannounce that
// carries the new value.
func TestMaybeReannounceOnAgentNamespaceChange(t *testing.T) {
	in := testInputs()
	cell := readyCell("cell-a")
	cell.Instance = "uid-1"
	in.Cells = []CellCandidate{cell}
	team := testTeam()
	team.Spec.Namespaces = append(team.Spec.Namespaces, "payments-agents")
	status := &v1alpha1.TeamStatus{}

	var first AnnounceRequest
	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		first = req
		return AnnounceResult{OK: true}, nil
	}
	out1, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if first.AgentNamespace != "payments-ns" {
		t.Fatalf("first AgentNamespace = %q, want payments-ns", first.AgentNamespace)
	}
	if out1.SyncCacheUpdate == nil || out1.SyncCacheUpdate.AgentNamespace != "payments-ns" {
		t.Fatalf("SyncCacheUpdate = %+v, want the announced agent namespace", out1.SyncCacheUpdate)
	}
	generationBefore := status.Placement.Generation

	team.Spec.Pipelines.AgentNamespace = "payments-agents"
	in.LastSync = out1.SyncCacheUpdate
	in.Now = fixedClock(fixedNow.Add(time.Minute))

	var calls int
	var got AnnounceRequest
	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		calls++
		got = req
		return AnnounceResult{OK: true}, nil
	}
	out2, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected one reannounce for the agent namespace edit, got %d", calls)
	}
	if got.AgentNamespace != "payments-agents" {
		t.Errorf("AgentNamespace = %q, want payments-agents", got.AgentNamespace)
	}
	if got.Generation != generationBefore {
		t.Errorf("expected the reannounce to stay at generation %d, got %d", generationBefore, got.Generation)
	}
	if out2.SyncCacheUpdate == nil || out2.SyncCacheUpdate.AgentNamespace != "payments-agents" {
		t.Errorf("SyncCacheUpdate = %+v, want the new agent namespace", out2.SyncCacheUpdate)
	}
}

// TestSyncPolicyFromPipelines_NilSpec_NoExports covers task 7.3: a Team
// with no spec.pipelines contributes no export policy.
func TestSyncPolicyFromPipelines_NilSpec_NoExports(t *testing.T) {
	policy := SyncPolicyFromPipelines(nil)
	if policy.Exports != nil {
		t.Errorf("Exports = %+v, want nil for a nil spec.pipelines", policy.Exports)
	}
}

func TestPlace_Reannounce_UnchangedSendsNothingInWindow(t *testing.T) {
	in := testInputs()
	cell := readyCell("cell-a")
	cell.Instance = "uid-1"
	in.Cells = []CellCandidate{cell}
	team := testTeam()
	status := &v1alpha1.TeamStatus{}
	out1, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if out1.SyncCacheUpdate == nil {
		t.Fatal("expected a SyncCacheUpdate after tick 1")
	}

	in.LastSync = out1.SyncCacheUpdate
	in.Now = fixedClock(fixedNow.Add(time.Minute)) // well inside the 10-minute resync window

	var calls int
	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		calls++
		return AnnounceResult{OK: true}, nil
	}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected no reannounce for an unchanged tenant inside the resync window, got %d", calls)
	}
}

func TestPlace_Reannounce_TenMinuteResync(t *testing.T) {
	in := testInputs()
	cell := readyCell("cell-a")
	cell.Instance = "uid-1"
	in.Cells = []CellCandidate{cell}
	team := testTeam()
	status := &v1alpha1.TeamStatus{}
	out1, err := Place(context.Background(), in, team, status)
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}

	in.LastSync = out1.SyncCacheUpdate
	in.Now = fixedClock(fixedNow.Add(syncResyncInterval)) // exactly at the resync boundary

	var calls int
	in.Announce = func(_ context.Context, req AnnounceRequest) (AnnounceResult, error) {
		calls++
		return AnnounceResult{OK: true}, nil
	}

	if _, err := Place(context.Background(), in, team, status); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected the ten-minute resync to fire, got %d calls", calls)
	}
}

func TestSyncPolicyFromPipelinesCarriesRetention(t *testing.T) {
	spec := &v1alpha1.TeamPipelinesSpec{
		Retention: &v1alpha1.TeamPipelinesRetention{
			Runs: "30d",
			Jobs: []v1alpha1.TeamJobRetention{{Job: "pr-*", Runs: "7d"}},
		},
	}
	got := SyncPolicyFromPipelines(spec)
	if got.Retention == nil || got.Retention.Runs != "30d" || len(got.Retention.Jobs) != 1 {
		t.Fatalf("Retention = %+v, want runs 30d and one pr-* rule", got.Retention)
	}
}

func TestSyncPolicyEqualDetectsRetentionChange(t *testing.T) {
	base := SyncPolicyFromPipelines(&v1alpha1.TeamPipelinesSpec{
		Retention: &v1alpha1.TeamPipelinesRetention{Runs: "30d"},
	})
	changed := SyncPolicyFromPipelines(&v1alpha1.TeamPipelinesSpec{
		Retention: &v1alpha1.TeamPipelinesRetention{Runs: "14d"},
	})
	if base.Equal(changed) {
		t.Fatalf("Equal = true for policies whose retention differs")
	}
	if !base.Equal(base) {
		t.Fatalf("Equal = false for an identical policy")
	}
}
