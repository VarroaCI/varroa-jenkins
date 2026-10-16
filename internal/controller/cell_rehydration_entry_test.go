package controller

import (
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

var rehydrationTestNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// TestMemberSeedReader_AnswersAppliedInstance pins the Team-side read: an
// applied entry answers its cell instance, and a pending or missing entry
// answers none.
func TestMemberSeedReader_AnswersAppliedInstance(t *testing.T) {
	at := metav1.NewTime(rehydrationTestNow)
	entry := v1alpha1.CellRehydrationEntry{Tenant: "team-a", CellInstance: "uid-1", At: &at}
	cell := cellWithSeeds(entry)
	cells := map[string]*v1alpha1.Controller{teamCellKey("ns", "cell"): cell}
	read := memberSeedReader(cells, "team-a")

	if inst, _ := read("ns", "cell"); inst != "" {
		t.Fatalf("pending entry answered instance %q, want none", inst)
	}
	entry.Applied = true
	cell.Status.Cell.Rehydration[0] = entry
	if inst, _ := read("ns", "cell"); inst != "uid-1" {
		t.Fatalf("applied entry answered instance %q, want uid-1", inst)
	}
	entry.Deferred = []string{"team-a/x"}
	cell.Status.Cell.Rehydration[0] = entry
	if inst, jobs := read("ns", "cell"); inst != "uid-1" || !slices.Equal(jobs, []string{"team-a/x"}) {
		t.Fatalf("applied entry with a deferred job answered (%q, %v), want (uid-1, [team-a/x])", inst, jobs)
	}
	if inst, _ := memberSeedReader(cells, "team-b")("ns", "cell"); inst != "" {
		t.Fatalf("missing entry answered instance %q, want none", inst)
	}
}

// cellWithSeeds builds a cell Controller whose status holds the given
// rehydration entries.
func cellWithSeeds(entries ...v1alpha1.CellRehydrationEntry) *v1alpha1.Controller {
	return &v1alpha1.Controller{
		Spec: v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
		Status: v1alpha1.ControllerStatus{Cell: &v1alpha1.CellStatus{
			Rehydration: entries,
		}},
	}
}

// TestCellSeedStateChanged pins the informer wake: only a change of the
// per-tenant seed answer (instance or deferred list) is true.
func TestCellSeedStateChanged(t *testing.T) {
	at := metav1.NewTime(rehydrationTestNow)
	later := metav1.NewTime(rehydrationTestNow.Add(time.Minute))
	pending := func(tenant string) v1alpha1.CellRehydrationEntry {
		return v1alpha1.CellRehydrationEntry{Tenant: tenant, CellInstance: "uid-1", At: &at}
	}
	applied := func(tenant string) v1alpha1.CellRehydrationEntry {
		e := pending(tenant)
		e.Applied = true
		return e
	}
	deferred := func(tenant string, jobs ...string) v1alpha1.CellRehydrationEntry {
		e := pending(tenant)
		e.Deferred = jobs
		return e
	}
	withLaterAt := func(e v1alpha1.CellRehydrationEntry) v1alpha1.CellRehydrationEntry {
		e.At = &later
		return e
	}
	appliedDeferred := func(tenant string, jobs ...string) v1alpha1.CellRehydrationEntry {
		e := applied(tenant)
		e.Deferred = jobs
		return e
	}
	releasedGiveUp := func(tenant string, jobs ...string) v1alpha1.CellRehydrationEntry {
		e := appliedDeferred(tenant, jobs...)
		e.Error = "rehydration deadline exceeded"
		return e
	}
	newInstance := applied("a")
	newInstance.CellInstance = "uid-2"
	notCell := cellWithSeeds(pending("a"))
	notCell.Spec.Cell = nil
	notCellApplied := cellWithSeeds(applied("a"))
	notCellApplied.Spec.Cell = nil
	cases := []struct {
		name     string
		old, new *v1alpha1.Controller
		want     bool
	}{
		{"pending to applied", cellWithSeeds(pending("a")), cellWithSeeds(applied("a")), true},
		{"new cell instance", cellWithSeeds(applied("a")), cellWithSeeds(newInstance), true},
		{"deferred list shrinks", cellWithSeeds(deferred("a", "a/x", "a/y")), cellWithSeeds(deferred("a", "a/x")), true},
		{"identical", cellWithSeeds(applied("a")), cellWithSeeds(applied("a")), false},
		{"only at changes", cellWithSeeds(applied("a")), cellWithSeeds(withLaterAt(applied("a"))), false},
		{"not a cell", notCell, notCellApplied, false},
		{"no old status", &v1alpha1.Controller{Spec: v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}}},
			cellWithSeeds(applied("a")), true},
		{"new pending, old missing", cellWithSeeds(), cellWithSeeds(pending("a")), false},
		{"second tenant applied", cellWithSeeds(pending("a"), pending("b")),
			cellWithSeeds(pending("a"), applied("b")), true},
		{"tenant removed while applied", cellWithSeeds(applied("a")), cellWithSeeds(), true},
		{"pending to applied with deferred job", cellWithSeeds(pending("a")),
			cellWithSeeds(appliedDeferred("a", "a/x")), true},
		{"applied deferred to released give-up", cellWithSeeds(appliedDeferred("a", "a/x")),
			cellWithSeeds(releasedGiveUp("a", "a/x")), true},
		{"applied deferred to carried resend", cellWithSeeds(appliedDeferred("a", "a/x")),
			cellWithSeeds(withLaterAt(appliedDeferred("a", "a/x"))), false},
	}
	for _, tc := range cases {
		if got := CellSeedStateChanged(tc.old, tc.new); got != tc.want {
			t.Errorf("%s: CellSeedStateChanged = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// --- task 6.0a: the empty-selection entry ---

func TestEmptySelectionEntry_AppliedNoRehydrationField(t *testing.T) {
	entry := emptySelectionEntry("team-a", 1, "uid-1", rehydrationTestNow)
	if !entry.Applied || entry.Records != 0 || len(entry.Deferred) != 0 || entry.BundleID != "" {
		t.Fatalf("expected an applied, empty entry with no bundleId, got %+v", entry)
	}
	// The command carries no rehydration field for this tenant: this
	// entry names no CommandID, which the orchestrator never sets on a
	// desired-state command's own bundle for it.
	if entry.CommandID != "" {
		t.Fatalf("expected no CommandID on an empty-selection entry, got %q", entry.CommandID)
	}
}

// TestEmptySelectionEntry_RehydratedBecomesTrue covers 6.0a's second
// scenario end to end through cells.RehydrationPass (task 6.5): the
// condition reaches True/TenantRehydrated on the same tick the empty
// entry is written.
func TestEmptySelectionEntry_RehydratedBecomesTrue(t *testing.T) {
	entry := emptySelectionEntry("team-a", 1, "uid-1", rehydrationTestNow)
	status := &v1alpha1.TeamStatus{
		Placement: &v1alpha1.TeamPlacementStatus{Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
	}
	cells.RehydrationPass(status, cells.RehydrationInputs{CellInstance: "uid-1", Entry: &entry})

	var cond *v1alpha1.TeamCondition
	for i := range status.Conditions {
		if status.Conditions[i].Type == v1alpha1.TeamConditionRehydrated {
			cond = &status.Conditions[i]
		}
	}
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.TeamReasonTenantRehydrated {
		t.Fatalf("expected Rehydrated=True/TenantRehydrated, got %+v", cond)
	}
}

// --- task 6.1: the pre-send entry ---

func TestPreSendEntry_WrittenBeforeSend(t *testing.T) {
	entry := preSendEntry("team-a", "bundle-1", 1, "uid-1", 5, true, 2, rehydrationTestNow)
	if entry.Applied {
		t.Fatal("expected Applied false before any result has arrived")
	}
	if len(entry.Deferred) != 0 {
		t.Fatal("expected Deferred empty before any result has arrived")
	}
	if entry.CommandID != "" || entry.Records != 5 || !entry.Truncated || entry.UnseededJobs != 2 {
		t.Fatalf("expected the selection's own fields and no command ID, got %+v", entry)
	}
}

func TestPreSendEntry_BundleIDMatches(t *testing.T) {
	entry := preSendEntry("team-a", "bundle-xyz", 1, "uid-1", 0, false, 0, rehydrationTestNow)
	if entry.BundleID != "bundle-xyz" {
		t.Fatalf("expected bundleId to match the bundle, got %q", entry.BundleID)
	}
}

// --- task 6.2: applying the arriving result ---

func TestApplyRehydrationResult_OnlyMatchingCommandUpdated(t *testing.T) {
	entries := []v1alpha1.CellRehydrationEntry{
		{Tenant: "team-a", CommandID: "cmd-1"},
		{Tenant: "team-b", CommandID: "cmd-old"},
	}
	out := applyRehydrationResult(entries, "cmd-1", true, "", nil, nil, nil, rehydrationTestNow)
	if !out[0].Applied {
		t.Fatalf("expected the matching entry updated, got %+v", out[0])
	}
	if out[1].CommandID != "cmd-old" || out[1].Applied {
		t.Fatalf("expected the non-matching entry untouched, got %+v", out[1])
	}
}

// TestApplyRehydrationResult_ThreeTenantsOneCommand covers 6.2's named
// scenario: a whole-bundle error applies to every entry of that
// command, across three tenants.
func TestApplyRehydrationResult_ThreeTenantsOneCommand(t *testing.T) {
	entries := []v1alpha1.CellRehydrationEntry{
		{Tenant: "team-a", CommandID: "cmd-1"},
		{Tenant: "team-b", CommandID: "cmd-1"},
		{Tenant: "team-c", CommandID: "cmd-1"},
	}
	out := applyRehydrationResult(entries, "cmd-1", false, "mite rejected a path", nil, nil, nil, rehydrationTestNow)
	for _, e := range out {
		if e.Applied || e.Error != "mite rejected a path" {
			t.Fatalf("expected every entry of the command to carry the whole-bundle error, got %+v", e)
		}
	}
}

func TestApplyRehydrationResult_PerTenantDeferred(t *testing.T) {
	entries := []v1alpha1.CellRehydrationEntry{
		{Tenant: "team-a", CommandID: "cmd-1"},
		{Tenant: "team-b", CommandID: "cmd-1"},
	}
	deferred := map[string][]string{"team-a": {"team-a/job-x"}}
	out := applyRehydrationResult(entries, "cmd-1", true, "", deferred, nil, nil, rehydrationTestNow)
	if len(out[0].Deferred) != 1 || !out[0].Applied {
		t.Fatalf("expected team-a applied with its deferred job, got %+v", out[0])
	}
	if len(out[1].Deferred) != 0 || !out[1].Applied {
		t.Fatalf("expected team-b applied with no deferred job, got %+v", out[1])
	}
}

// TestSeedAnswerOf pins what the Team reconciler reads from one entry.
func TestSeedAnswerOf(t *testing.T) {
	cases := []struct {
		name         string
		entry        v1alpha1.CellRehydrationEntry
		wantInstance string
		wantPending  []string
	}{
		{"pre-send holds the whole tenant", v1alpha1.CellRehydrationEntry{CellInstance: "uid-1"}, "", nil},
		{"applied with deferred job", v1alpha1.CellRehydrationEntry{CellInstance: "uid-1", Applied: true, Deferred: []string{"t/x"}}, "uid-1", []string{"t/x"}},
		{"applied without deferred job", v1alpha1.CellRehydrationEntry{CellInstance: "uid-1", Applied: true}, "uid-1", nil},
		{"released give-up", v1alpha1.CellRehydrationEntry{CellInstance: "uid-1", Applied: true, Deferred: []string{"t/x"}, Error: "rehydration deadline exceeded"}, "uid-1", nil},
		{"never-applied give-up", v1alpha1.CellRehydrationEntry{CellInstance: "uid-1", Deferred: []string{"t/x"}, Error: "rehydration deadline exceeded"}, "", []string{"t/x"}},
		{"applied number-only round holds uncovered jobs", v1alpha1.CellRehydrationEntry{CellInstance: "uid-1", Applied: true, UnseededJobs: 2, Unseeded: []string{"t/b", "t/c"}, Deferred: []string{"t/x"}}, "uid-1", []string{"t/b", "t/c", "t/x"}},
		{"applied with overflowed unseeded list holds the tenant", v1alpha1.CellRehydrationEntry{CellInstance: "uid-1", Applied: true, UnseededJobs: 3}, "", nil},
		{"released give-up with unseeded job", v1alpha1.CellRehydrationEntry{CellInstance: "uid-1", Applied: true, Unseeded: []string{"t/b"}, Error: "rehydration deadline exceeded"}, "uid-1", nil},
	}
	for _, tc := range cases {
		got := seedAnswerOf(&tc.entry)
		if got.instance != tc.wantInstance || !slices.Equal(got.pending, tc.wantPending) {
			t.Errorf("%s: seedAnswerOf = (%q, %v), want (%q, %v)", tc.name, got.instance, got.pending, tc.wantInstance, tc.wantPending)
		}
	}
}

// TestCarryAppliedSeed pins which earlier entries keep their applied seed
// across a resend.
func TestCarryAppliedSeed(t *testing.T) {
	applied := v1alpha1.CellRehydrationEntry{Tenant: "t", Generation: 1, CellInstance: "uid-1", Applied: true, Deferred: []string{"t/x"}, Unseeded: []string{"t/y"}, Error: "old"}
	next := v1alpha1.CellRehydrationEntry{Tenant: "t", Generation: 1, CellInstance: "uid-1"}

	kept := carryAppliedSeed(next, &applied)
	if !kept.Applied || !slices.Equal(kept.Deferred, []string{"t/x"}) || !slices.Equal(kept.Unseeded, []string{"t/y"}) || kept.Error != "" {
		t.Fatalf("same generation and instance: want applied with deferred and unseeded, no error, got %+v", kept)
	}
	otherInstance := applied
	otherInstance.CellInstance = "uid-2"
	otherGen := applied
	otherGen.Generation = 2
	notApplied := applied
	notApplied.Applied = false
	for name, existing := range map[string]*v1alpha1.CellRehydrationEntry{
		"other instance": &otherInstance, "other generation": &otherGen, "not applied": &notApplied, "nil": nil,
	} {
		if got := carryAppliedSeed(next, existing); got.Applied || len(got.Deferred) != 0 {
			t.Errorf("%s: carried a seed, got %+v", name, got)
		}
	}
}

// TestExpiredEntry_ReleasesOnlyAfterAnApply pins the give-up: the deferred
// jobs are released only when the bundle applied earlier on this
// generation and instance.
func TestExpiredEntry_ReleasesOnlyAfterAnApply(t *testing.T) {
	st := rehydrationTenantState{
		team:      &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "t"}},
		placement: rehydrationPlacement{Generation: 1, CellInstance: "uid-1"},
	}
	st.existing = &v1alpha1.CellRehydrationEntry{Tenant: "t", Generation: 1, CellInstance: "uid-1", Applied: true, Deferred: []string{"t/x"}}
	released := expiredEntry(st, "uid-1", rehydrationTestNow)
	if !released.Applied || !slices.Equal(released.Deferred, []string{"t/x"}) || released.Error == "" {
		t.Fatalf("released give-up: want applied, deferred kept in the message, error set; got %+v", released)
	}
	if got := seedAnswerOf(&released); got.instance != "uid-1" || len(got.pending) != 0 {
		t.Fatalf("released give-up answers (%q, %v), want (uid-1, none)", got.instance, got.pending)
	}

	st.existing = &v1alpha1.CellRehydrationEntry{Tenant: "t", Generation: 1, CellInstance: "uid-1", Deferred: []string{"t/x"}}
	held := expiredEntry(st, "uid-1", rehydrationTestNow)
	if held.Applied {
		t.Fatalf("never-applied give-up must keep the tenant held, got %+v", held)
	}
	if got := seedAnswerOf(&held); got.instance != "" {
		t.Fatalf("never-applied give-up answers instance %q, want none", got.instance)
	}
}

// TestCellRehydrationEntryDeferred_GiveUpAnswersNone pins the announced
// deferred list of a released give-up: none.
func TestCellRehydrationEntryDeferred_GiveUpAnswersNone(t *testing.T) {
	cell := cellWithSeeds(v1alpha1.CellRehydrationEntry{Tenant: "team-a", CellInstance: "uid-1", Applied: true, Deferred: []string{"team-a/x"}, Error: "rehydration deadline exceeded"})
	cells := map[string]*v1alpha1.Controller{teamCellKey("ns", "cell"): cell}
	status := &v1alpha1.TeamStatus{Placement: &v1alpha1.TeamPlacementStatus{Cell: "cell", CellNamespace: "ns"}}
	if got := cellRehydrationEntryPending(status, cells, "team-a"); len(got) != 0 {
		t.Fatalf("released give-up announced deferred %v, want none", got)
	}
}

// TestApplyRehydrationResult_DropsCoveredUnseededPaths pins that a
// successful apply removes the covered job paths from the unseeded list,
// and that a failed apply keeps the list.
func TestApplyRehydrationResult_DropsCoveredUnseededPaths(t *testing.T) {
	entries := []v1alpha1.CellRehydrationEntry{
		{Tenant: "team-a", CommandID: "cmd-1", Unseeded: []string{"team-a/b", "team-a/c"}},
	}
	covered := map[string][]string{"team-a": {"team-a/b"}}
	out := applyRehydrationResult(entries, "cmd-1", true, "", nil, covered, nil, rehydrationTestNow)
	if !slices.Equal(out[0].Unseeded, []string{"team-a/c"}) || !out[0].Applied {
		t.Fatalf("successful apply: want unseeded [team-a/c] and applied, got %+v", out[0])
	}
	out = applyRehydrationResult(entries, "cmd-1", false, "mite rejected a path", nil, covered, nil, rehydrationTestNow)
	if !slices.Equal(out[0].Unseeded, []string{"team-a/b", "team-a/c"}) || out[0].Applied {
		t.Fatalf("failed apply: want the unseeded list kept and not applied, got %+v", out[0])
	}
}

// TestBoundUnseeded pins the cap: a list above the limit, or a carried
// list that was incomplete, is dropped and clears Applied.
func TestBoundUnseeded(t *testing.T) {
	applied := v1alpha1.CellRehydrationEntry{Tenant: "t", Applied: true, UnseededJobs: 2}
	got := boundUnseeded(applied, []string{"t/c", "t/a", "t/a"}, true, 2)
	if !slices.Equal(got.Unseeded, []string{"t/a", "t/c"}) || !got.Applied {
		t.Fatalf("within the limit: want sorted unique [t/a t/c] and applied, got %+v", got)
	}
	if got := boundUnseeded(applied, []string{"t/a", "t/b", "t/c"}, true, 2); got.Unseeded != nil || got.Applied {
		t.Fatalf("above the limit: want no list and not applied, got %+v", got)
	}
	if got := boundUnseeded(applied, []string{"t/a"}, false, 2); got.Unseeded != nil || got.Applied {
		t.Fatalf("incomplete carried list: want no list and not applied, got %+v", got)
	}
}

// TestCellSeedStateChanged_UnseededListChange pins that a change of the
// unseeded list alone wakes the Team ticker.
func TestCellSeedStateChanged_UnseededListChange(t *testing.T) {
	at := metav1.NewTime(rehydrationTestNow)
	before := v1alpha1.CellRehydrationEntry{Tenant: "t", CellInstance: "uid-1", Applied: true, At: &at, Unseeded: []string{"t/a"}}
	after := before
	after.Unseeded = []string{"t/a", "t/b"}
	if !CellSeedStateChanged(cellWithSeeds(before), cellWithSeeds(after)) {
		t.Fatalf("an unseeded list change must wake the ticker")
	}
}

// TestCapCellUnseeded pins the per-cell byte budget. Each path counts its
// length plus three bytes. Under the budget nothing changes. Over it, the
// entry with the largest size drops first, ties by tenant name, and a
// dropped entry waits whole.
func TestCapCellUnseeded(t *testing.T) {
	// Ten-byte paths: 13 encoded bytes each. team-a and team-b hold three
	// paths (39 bytes), team-c holds one (13 bytes). Total 91.
	mk := func() []v1alpha1.CellRehydrationEntry {
		return []v1alpha1.CellRehydrationEntry{
			{Tenant: "team-b", Applied: true, Unseeded: []string{"team-b/a", "team-b/b", "team-b/c"}},
			{Tenant: "team-a", Applied: true, Unseeded: []string{"team-a/a", "team-a/b", "team-a/c"}},
			{Tenant: "team-c", Applied: true, Unseeded: []string{"team-c/a"}},
		}
	}

	under := mk()
	capCellUnseeded(under, 91)
	if !slices.Equal(under[0].Unseeded, mk()[0].Unseeded) || !under[1].Applied || len(under[2].Unseeded) != 1 {
		t.Fatalf("under the budget: want entries unchanged, got %+v", under)
	}

	// Total 91, budget 60: team-a and team-b tie at 39 bytes, so team-a
	// drops first, and that alone brings the total to 52.
	over := mk()
	capCellUnseeded(over, 60)
	if over[1].Applied || over[1].Unseeded != nil {
		t.Fatalf("over the budget: want team-a dropped whole, got %+v", over[1])
	}
	if !over[0].Applied || len(over[0].Unseeded) != 3 || len(over[2].Unseeded) != 1 {
		t.Fatalf("over the budget: want team-b and team-c kept, got %+v", over)
	}

	// Few long paths: one 300-byte path is 303 encoded bytes, two short
	// paths are 26. Budget 300 drops the long one even though it has fewer
	// paths.
	long := []string{"team-x/" + strings.Repeat("p", 293)}
	few := []v1alpha1.CellRehydrationEntry{
		{Tenant: "team-y", Applied: true, Unseeded: []string{"team-y/a", "team-y/b"}},
		{Tenant: "team-x", Applied: true, Unseeded: long},
	}
	capCellUnseeded(few, 300)
	if few[1].Applied || few[1].Unseeded != nil || !few[0].Applied || len(few[0].Unseeded) != 2 {
		t.Fatalf("few long paths: want team-x dropped, team-y kept, got %+v", few)
	}

	// Two 140 KiB paths exceed the 256 KiB budget together. The tie drops
	// by name, and one entry alone fits.
	big := []v1alpha1.CellRehydrationEntry{
		{Tenant: "team-q", Applied: true, Unseeded: []string{"team-q/" + strings.Repeat("q", 140<<10)}},
		{Tenant: "team-p", Applied: true, Unseeded: []string{"team-p/" + strings.Repeat("p", 140<<10)}},
	}
	capCellUnseeded(big, maxUnseededBytesPerCell)
	if big[1].Applied || big[1].Unseeded != nil || !big[0].Applied || len(big[0].Unseeded) != 1 {
		t.Fatalf("256 KiB budget: want team-p dropped by name, team-q kept, got %+v", big)
	}

	// Reversed input order gives the same drop.
	rev := mk()
	rev[0], rev[1] = rev[1], rev[0]
	capCellUnseeded(rev, 60)
	if rev[0].Tenant != "team-a" || rev[0].Applied || rev[0].Unseeded != nil || !rev[1].Applied {
		t.Fatalf("reversed order: want team-a dropped, got %+v", rev)
	}
}

// --- partitionJobPathsByTenant ---

func TestPartitionJobPathsByTenant(t *testing.T) {
	out := partitionJobPathsByTenant([]string{"team-a/job-x", "team-b/job-y", "team-a/job-z"}, []string{"team-a", "team-b"})
	if len(out["team-a"]) != 2 || len(out["team-b"]) != 1 {
		t.Fatalf("expected 2 team-a and 1 team-b deferred jobs, got %+v", out)
	}
}

// TestApplyRehydrationResult_ReloadFailureMarksOnlyItsTenant pins that a
// failed reload of one tenant's job sets that tenant's Error alone. The
// tenant keeps Applied, so its seed answers its cell instance.
func TestApplyRehydrationResult_ReloadFailureMarksOnlyItsTenant(t *testing.T) {
	entries := []v1alpha1.CellRehydrationEntry{
		{Tenant: "team-a", CommandID: "cmd-1"},
		{Tenant: "team-b", CommandID: "cmd-1"},
	}
	failed := map[string][]*mitev1.RehydrationReloadFailure{
		"team-a": {{Item: "team-a/mb/main", Error: "HTTP 403"}},
	}
	out := applyRehydrationResult(entries, "cmd-1", true, "", nil, nil, failed, rehydrationTestNow)

	if !out[0].Applied || !strings.Contains(out[0].Error, "team-a/mb/main") {
		t.Errorf("team-a = %+v, want Applied with the reload failure", out[0])
	}
	if !out[1].Applied || out[1].Error != "" {
		t.Errorf("team-b = %+v, want Applied with no error", out[1])
	}
}

// TestPartitionReloadFailuresByTenant pins the prefix attribution of reload
// failures: a tenant owns its own path and the paths under its folder, and
// a path that matches no tenant is dropped.
func TestPartitionReloadFailuresByTenant(t *testing.T) {
	failures := []*mitev1.RehydrationReloadFailure{
		{Item: "team-a", Error: "x"},
		{Item: "team-a/build", Error: "y"},
		{Item: "team-bb/build", Error: "z"},
		{Item: "other/job", Error: "w"},
	}
	out := partitionReloadFailuresByTenant(failures, []string{"team-a", "team-b"})

	items := make([]string, 0, len(out["team-a"]))
	for _, f := range out["team-a"] {
		items = append(items, f.Item)
	}
	if want := []string{"team-a", "team-a/build"}; !slices.Equal(items, want) {
		t.Errorf("team-a items = %v, want %v", items, want)
	}
	if _, ok := out["team-b"]; ok {
		t.Errorf("team-b = %v, want no failures: team-bb is not under team-b", out["team-b"])
	}
	if len(out) != 1 {
		t.Errorf("tenants = %v, want only team-a", out)
	}
}

// TestCarryRehydrationError pins that a resend keeps the Error of the entry
// it replaces, only for the same generation and cell instance.
func TestCarryRehydrationError(t *testing.T) {
	next := v1alpha1.CellRehydrationEntry{Tenant: "team-a", Generation: 1, CellInstance: "i1"}
	existing := &v1alpha1.CellRehydrationEntry{Tenant: "team-a", Generation: 1, CellInstance: "i1", Error: "boom"}

	if got := carryRehydrationError(next, existing); got.Error != "boom" {
		t.Errorf("same placement: Error = %q, want boom", got.Error)
	}
	other := &v1alpha1.CellRehydrationEntry{Tenant: "team-a", Generation: 2, CellInstance: "i1", Error: "boom"}
	if got := carryRehydrationError(next, other); got.Error != "" {
		t.Errorf("another generation: Error = %q, want none", got.Error)
	}
	moved := &v1alpha1.CellRehydrationEntry{Tenant: "team-a", Generation: 1, CellInstance: "i2", Error: "boom"}
	if got := carryRehydrationError(next, moved); got.Error != "" {
		t.Errorf("another cell instance: Error = %q, want none", got.Error)
	}
	if got := carryRehydrationError(next, nil); got.Error != "" {
		t.Errorf("nil existing: Error = %q, want none", got.Error)
	}
}

// TestSeedAnswerOf_ReloadFailureKeepsThePendingJobs pins that a failed reload
// releases the tenant's instance but keeps its still-deferred jobs waiting.
// Only a deadline give-up releases the pending jobs.
func TestSeedAnswerOf_ReloadFailureKeepsThePendingJobs(t *testing.T) {
	e := v1alpha1.CellRehydrationEntry{
		Tenant: "team-a", CellInstance: "i1", Applied: true,
		Error: "reload team-a/build: HTTP 403", Deferred: []string{"team-a/new"},
	}
	got := seedAnswerOf(&e)
	if got.instance != "i1" {
		t.Errorf("instance = %q, want i1", got.instance)
	}
	if !slices.Contains(got.pending, "team-a/new") {
		t.Errorf("pending = %v, want the deferred job to keep waiting", got.pending)
	}

	gaveUp := e
	gaveUp.Error = deadlineExceededMessage([]string{"team-a/new"})
	if got := seedAnswerOf(&gaveUp); len(got.pending) != 0 {
		t.Errorf("released give-up pending = %v, want none", got.pending)
	}
}

// TestApplyRehydrationResult_ReloadFailedJobStaysUnseeded pins that a job
// whose reload failed stays unseeded after a successful apply that covers it.
// A later result therefore resends the job and reloads it.
func TestApplyRehydrationResult_ReloadFailedJobStaysUnseeded(t *testing.T) {
	entries := []v1alpha1.CellRehydrationEntry{
		{Tenant: "team-a", CommandID: "cmd-1", Unseeded: []string{"team-a/mb/main", "team-a/other"}},
	}
	covered := map[string][]string{"team-a": {"team-a/mb/main", "team-a/other"}}
	failed := map[string][]*mitev1.RehydrationReloadFailure{
		"team-a": {{Item: "team-a/mb/main", Error: "HTTP 403"}},
	}
	out := applyRehydrationResult(entries, "cmd-1", true, "", nil, covered, failed, rehydrationTestNow)

	if !slices.Contains(out[0].Unseeded, "team-a/mb/main") {
		t.Errorf("unseeded = %v, want the reload-failed job still listed", out[0].Unseeded)
	}
	if slices.Contains(out[0].Unseeded, "team-a/other") {
		t.Errorf("unseeded = %v, want the successfully reloaded job removed", out[0].Unseeded)
	}
}
