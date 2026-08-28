package updatecenter

import (
	"context"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/hpi"
	"github.com/varroaci/varroa-jenkins/internal/pluginver"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeStore implements storeLookup. versions maps name -> versions held;
// deps maps "name@version" -> dependency list; legacy names a version whose
// pack predates the annotation contract (authoritative=false).
type fakeStore struct {
	versions map[string][]string
	deps     map[string][]hpi.Dependency
	legacy   map[string]bool
}

func (f *fakeStore) Versions(_ context.Context, name string) []string { return f.versions[name] }

func (f *fakeStore) Dependencies(_ context.Context, name, version string) ([]hpi.Dependency, bool) {
	key := name + "@" + version
	if f.legacy[key] {
		return nil, false
	}
	if _, held := f.deps[key]; !held {
		// Not in the store at all.
		found := false
		for _, v := range f.versions[name] {
			if v == version {
				found = true
			}
		}
		if !found {
			return nil, false
		}
	}
	return f.deps[key], true
}

// fakeResolver implements metaResolver over a static upstream index plus a set
// of names whose sources are degraded.
type fakeResolver struct {
	// listed maps name -> the single version upstream lists, with its metadata.
	listed   map[string]ucmeta.PluginMeta
	degraded map[string]bool
}

func (f *fakeResolver) lookup(name string, accept func(ucmeta.PluginMeta) bool) ucmeta.Resolution {
	if f.degraded[name] {
		return ucmeta.Resolution{Outcome: ucmeta.SourcesDegraded}
	}
	m, ok := f.listed[name]
	if !ok {
		return ucmeta.Resolution{Outcome: ucmeta.NotListed}
	}
	best := m
	if !accept(m) {
		return ucmeta.Resolution{Outcome: ucmeta.NotListed, Best: &best}
	}
	return ucmeta.Resolution{Outcome: ucmeta.Resolved, Meta: m, Best: &best}
}

func (f *fakeResolver) ResolveExact(_ context.Context, name, version string) ucmeta.Resolution {
	return f.lookup(name, func(m ucmeta.PluginMeta) bool { return m.Version == version })
}

func (f *fakeResolver) ResolveSatisfying(_ context.Context, name, minVersion string) ucmeta.Resolution {
	return f.lookup(name, func(m ucmeta.PluginMeta) bool { return atLeastForTest(m.Version, minVersion) })
}

func atLeastForTest(have, want string) bool {
	// Delegate to the real comparator so the fake cannot disagree with the code
	// under test about what "satisfies" means.
	return pluginver.AtLeast(have, want)
}

func newPlanner(store *fakeStore, declared DeclaredSet, res *fakeResolver, pullThrough bool) *closurePlanner {
	if store == nil {
		store = &fakeStore{}
	}
	if store.versions == nil {
		store.versions = map[string][]string{}
	}
	if store.deps == nil {
		store.deps = map[string][]hpi.Dependency{}
	}
	if store.legacy == nil {
		store.legacy = map[string]bool{}
	}
	if declared == nil {
		declared = DeclaredSet{}
	}
	if res == nil {
		res = &fakeResolver{}
	}
	if res.listed == nil {
		res.listed = map[string]ucmeta.PluginMeta{}
	}
	if res.degraded == nil {
		res.degraded = map[string]bool{}
	}
	return &closurePlanner{store: store, declared: declared, resolver: res, pullThrough: pullThrough}
}

func root(deps ...hpi.Dependency) hpi.PluginManifest {
	return hpi.PluginManifest{ShortName: "root", Version: "1.0", Dependencies: deps}
}

func dep(name, minVersion string) hpi.Dependency { return hpi.Dependency{Name: name, Min: minVersion} }

func statusOf(plan Plan, name string) string {
	for _, e := range plan.Closure {
		if e.Name == name {
			return e.Status
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Decision tree (§5.3)
// ---------------------------------------------------------------------------

func TestPlanClosure_DecisionTree(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name        string
		store       *fakeStore
		declared    DeclaredSet
		resolver    *fakeResolver
		pullThrough bool
		min         string
		want        string
	}{
		{
			name:  "row a — store satisfies",
			store: &fakeStore{versions: map[string][]string{"dep": {"2.0"}}, deps: map[string][]hpi.Dependency{"dep@2.0": nil}},
			min:   "1.0",
			want:  StatusSatisfiedStore,
		},
		{
			name:     "row b — declared pin cannot satisfy, warn only",
			declared: DeclaredSet{"dep": {"1.0"}},
			min:      "2.0",
			want:     StatusLockTooOld,
		},
		{
			name:     "row e2 — declared, pull-through off, air gap",
			declared: DeclaredSet{"dep": {"2.0"}},
			min:      "1.0",
			want:     StatusNotInStore,
		},
		{
			name:        "row c — declared pin resolves upstream",
			declared:    DeclaredSet{"dep": {"2.0"}},
			resolver:    &fakeResolver{listed: map[string]ucmeta.PluginMeta{"dep": {Name: "dep", Version: "2.0"}}},
			pullThrough: true,
			min:         "1.0",
			want:        StatusDeclaredNotYetStored,
		},
		{
			name:        "row c2 — declared pin listed nowhere (aged pin)",
			declared:    DeclaredSet{"dep": {"2.0"}},
			resolver:    &fakeResolver{listed: map[string]ucmeta.PluginMeta{"dep": {Name: "dep", Version: "9.9"}}},
			pullThrough: true,
			min:         "1.0",
			want:        StatusUnreachable,
		},
		{
			name:        "row c3 — declared pin, sources degraded",
			declared:    DeclaredSet{"dep": {"2.0"}},
			resolver:    &fakeResolver{degraded: map[string]bool{"dep": true}},
			pullThrough: true,
			min:         "1.0",
			want:        StatusMetadataUnavailable,
		},
		{
			name: "row e — undeclared, pull-through off",
			min:  "1.0",
			want: StatusNotInStore,
		},
		{
			name:        "row f — undeclared, upstream satisfies",
			resolver:    &fakeResolver{listed: map[string]ucmeta.PluginMeta{"dep": {Name: "dep", Version: "1.9"}}},
			pullThrough: true,
			min:         "1.2",
			want:        StatusPlannedFetch,
		},
		{
			name:        "row g — undeclared, sources degraded",
			resolver:    &fakeResolver{degraded: map[string]bool{"dep": true}},
			pullThrough: true,
			min:         "1.2",
			want:        StatusMetadataUnavailable,
		},
		{
			name:        "row h — undeclared, upstream too old",
			resolver:    &fakeResolver{listed: map[string]ucmeta.PluginMeta{"dep": {Name: "dep", Version: "3.1"}}},
			pullThrough: true,
			min:         "9.0",
			want:        StatusUnreachable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlanner(tc.store, tc.declared, tc.resolver, tc.pullThrough)
			plan := p.planClosure(ctx, root(dep("dep", tc.min)))
			if got := statusOf(plan, "dep"); got != tc.want {
				t.Fatalf("status = %q, want %q (closure %+v)", got, tc.want, plan.Closure)
			}
		})
	}
}

// TestPlanClosure_StaleStoredCopyDoesNotTerminate covers the property the flat
// table did not have: an old pull-through pack holding X@1.0 against an X>=2.0
// requirement falls through step 1 and is resolved by step 3.
func TestPlanClosure_StaleStoredCopyDoesNotTerminate(t *testing.T) {
	p := newPlanner(
		&fakeStore{versions: map[string][]string{"x": {"1.0"}}},
		nil,
		&fakeResolver{listed: map[string]ucmeta.PluginMeta{"x": {Name: "x", Version: "2.5"}}},
		true,
	)
	plan := p.planClosure(context.Background(), root(dep("x", "2.0")))
	if got := statusOf(plan, "x"); got != StatusPlannedFetch {
		t.Fatalf("status = %q, want planned-fetch", got)
	}
}

// TestPlanClosure_UniversalDescent: a stored dependency with an absent nested
// dependency must be reported, not silently accepted.
func TestPlanClosure_UniversalDescent(t *testing.T) {
	p := newPlanner(
		&fakeStore{
			versions: map[string][]string{"a": {"1.0"}},
			deps:     map[string][]hpi.Dependency{"a@1.0": {dep("nested", "1.0")}},
		},
		nil, nil, false,
	)
	plan := p.planClosure(context.Background(), root(dep("a", "1.0")))
	if got := statusOf(plan, "a"); got != StatusSatisfiedStore {
		t.Fatalf("a status = %q, want satisfied-store", got)
	}
	if got := statusOf(plan, "nested"); got != StatusNotInStore {
		t.Fatalf("nested status = %q, want not-in-store — descent must be universal", got)
	}
}

// TestPlanClosure_RequirementEscalation: resolving B>=1 first must not validate
// away a later B>=2.
func TestPlanClosure_RequirementEscalation(t *testing.T) {
	p := newPlanner(
		&fakeStore{
			versions: map[string][]string{"b": {"1.0"}, "a": {"1.0"}},
			deps: map[string][]hpi.Dependency{
				"b@1.0": nil,
				"a@1.0": {dep("b", "2.0")}, // raises b's minimum on a second path
			},
		},
		nil, nil, false,
	)
	// root depends on b>=1 (satisfied by the store) and on a, which needs b>=2.
	plan := p.planClosure(context.Background(), root(dep("b", "1.0"), dep("a", "1.0")))
	if got := statusOf(plan, "b"); got != StatusNotInStore {
		t.Fatalf("b status = %q, want not-in-store after escalation to >=2.0", got)
	}
	for _, e := range plan.Closure {
		if e.Name == "b" && e.Min != "2.0" {
			t.Fatalf("b min = %q, want the escalated 2.0", e.Min)
		}
	}
}

// TestPlanClosure_StaleUndeclaredCachedCopy: a stale UNDECLARED cached copy
// resolves upstream instead of warning.
func TestPlanClosure_StaleUndeclaredResolvesUpstream(t *testing.T) {
	p := newPlanner(
		&fakeStore{versions: map[string][]string{"x": {"1.0"}}},
		DeclaredSet{}, // explicitly not declared
		&fakeResolver{listed: map[string]ucmeta.PluginMeta{"x": {Name: "x", Version: "4.0"}}},
		true,
	)
	plan := p.planClosure(context.Background(), root(dep("x", "3.0")))
	if got := statusOf(plan, "x"); got != StatusPlannedFetch {
		t.Fatalf("status = %q, want planned-fetch (no warning tier for an undeclared plugin)", got)
	}
	if len(plan.Warnings) != 0 {
		t.Fatalf("warnings = %+v, want none", plan.Warnings)
	}
}

// TestPlanClosure_MultiVersionDeclarationCountsAsDeclared
func TestPlanClosure_MultiVersionDeclaration(t *testing.T) {
	p := newPlanner(nil, DeclaredSet{"x": {"1.0", "3.0", "2.0"}}, nil, false)
	plan := p.planClosure(context.Background(), root(dep("x", "2.5")))
	// Highest declared is 3.0, which satisfies 2.5 → air-gap not-in-store, not
	// lock-too-old.
	if got := statusOf(plan, "x"); got != StatusNotInStore {
		t.Fatalf("status = %q, want not-in-store (highest declared 3.0 satisfies)", got)
	}
}

func TestPlanClosure_CycleGuard(t *testing.T) {
	p := newPlanner(
		&fakeStore{
			versions: map[string][]string{"a": {"1.0"}, "b": {"1.0"}},
			deps: map[string][]hpi.Dependency{
				"a@1.0": {dep("b", "1.0")},
				"b@1.0": {dep("a", "1.0")},
			},
		},
		nil, nil, false,
	)
	plan := p.planClosure(context.Background(), root(dep("a", "1.0")))
	if plan.TooDeep {
		t.Fatal("a two-node cycle must not trip the depth cap")
	}
	if len(plan.Closure) != 2 {
		t.Fatalf("closure = %+v, want exactly a and b", plan.Closure)
	}
}

func TestPlanClosure_DepthCap(t *testing.T) {
	versions := map[string][]string{}
	deps := map[string][]hpi.Dependency{}
	const chain = maxClosureDepth + 5
	for i := 0; i < chain; i++ {
		name := chainName(i)
		versions[name] = []string{"1.0"}
		if i+1 < chain {
			deps[name+"@1.0"] = []hpi.Dependency{dep(chainName(i+1), "1.0")}
		}
	}
	p := newPlanner(&fakeStore{versions: versions, deps: deps}, nil, nil, false)
	plan := p.planClosure(context.Background(), root(dep(chainName(0), "1.0")))
	if !plan.TooDeep {
		t.Fatal("a chain past the depth cap must set TooDeep")
	}
	code, status, _ := plan.envelope()
	if code != ErrClosureTooDeep || status != 422 {
		t.Fatalf("envelope = (%q,%d), want (closure-too-deep,422)", code, status)
	}
}

func chainName(i int) string { return "c" + string(rune('a'+i%26)) + string(rune('a'+i/26)) }

func TestPlanClosure_OptionalNeverResolved(t *testing.T) {
	p := newPlanner(nil, nil, nil, false)
	plan := p.planClosure(context.Background(), hpi.PluginManifest{
		ShortName: "root", Version: "1.0",
		Dependencies: []hpi.Dependency{{Name: "junit", Min: "1.0", Optional: true}},
	})
	if len(plan.Closure) != 0 {
		t.Fatalf("closure = %+v, want empty — optional dependencies are never resolved", plan.Closure)
	}
	if len(plan.Optional) != 1 || plan.Optional[0].Name != "junit" {
		t.Fatalf("optional = %+v, want junit recorded", plan.Optional)
	}
}

// TestPlanClosure_OptionalNestedNeverResolved: an optional dependency OF a
// resolved dependency is also never resolved.
func TestPlanClosure_OptionalNestedNeverResolved(t *testing.T) {
	p := newPlanner(
		&fakeStore{
			versions: map[string][]string{"a": {"1.0"}},
			deps:     map[string][]hpi.Dependency{"a@1.0": {{Name: "opt", Min: "1.0", Optional: true}}},
		},
		nil, nil, false,
	)
	plan := p.planClosure(context.Background(), root(dep("a", "1.0")))
	if statusOf(plan, "opt") != "" {
		t.Fatalf("closure = %+v, want no entry for the nested optional dependency", plan.Closure)
	}
}

// TestPlanClosure_UnverifiableLegacyPack: a stored pack with no kind proves
// nothing about its dependencies, and upstream no longer lists that version.
func TestPlanClosure_UnverifiableLegacyPack(t *testing.T) {
	p := newPlanner(
		&fakeStore{
			versions: map[string][]string{"a": {"1.0"}},
			legacy:   map[string]bool{"a@1.0": true},
		},
		nil,
		&fakeResolver{listed: map[string]ucmeta.PluginMeta{"a": {Name: "a", Version: "9.9"}}},
		true,
	)
	plan := p.planClosure(context.Background(), root(dep("a", "1.0")))
	if got := statusOf(plan, "a"); got != StatusClosureUnverifiable {
		t.Fatalf("status = %q, want closure-unverifiable", got)
	}
	code, status, _ := plan.envelope()
	if code != ErrClosureUnverifiable || status != 422 {
		t.Fatalf("envelope = (%q,%d), want (closure-unverifiable,422)", code, status)
	}
}

// TestPlanClosure_UnverifiableRecoveredFromUpstream: the same legacy pack is
// verifiable when upstream still lists that exact version.
func TestPlanClosure_UnverifiableRecoveredFromUpstream(t *testing.T) {
	p := newPlanner(
		&fakeStore{
			versions: map[string][]string{"a": {"1.0"}},
			legacy:   map[string]bool{"a@1.0": true},
		},
		nil,
		&fakeResolver{listed: map[string]ucmeta.PluginMeta{"a": {Name: "a", Version: "1.0"}}},
		true,
	)
	plan := p.planClosure(context.Background(), root(dep("a", "1.0")))
	if got := statusOf(plan, "a"); got != StatusSatisfiedStore {
		t.Fatalf("status = %q, want satisfied-store", got)
	}
}

// TestPlanClosure_DegradedDependencyListIsRetryable: an undeterminable list
// caused by a degraded source is retryable, not a permanent rejection.
func TestPlanClosure_DegradedDependencyListIsRetryable(t *testing.T) {
	p := newPlanner(
		&fakeStore{
			versions: map[string][]string{"a": {"1.0"}},
			legacy:   map[string]bool{"a@1.0": true},
		},
		nil,
		&fakeResolver{degraded: map[string]bool{"a": true}},
		true,
	)
	plan := p.planClosure(context.Background(), root(dep("a", "1.0")))
	if got := statusOf(plan, "a"); got != StatusMetadataUnavailable {
		t.Fatalf("status = %q, want metadata-unavailable", got)
	}
	code, status, _ := plan.envelope()
	if code != ErrMetadataUnavailable || status != 503 {
		t.Fatalf("envelope = (%q,%d), want (metadata-unavailable,503)", code, status)
	}
}

// ---------------------------------------------------------------------------
// Envelope precedence (§5.4)
// ---------------------------------------------------------------------------

func TestEnvelopePrecedence(t *testing.T) {
	cases := []struct {
		name       string
		plan       Plan
		wantCode   string
		wantStatus int
	}{
		{"clean", Plan{Closure: []ClosureEntry{{Status: StatusSatisfiedStore}}}, "", 0},
		{
			"too-deep outranks everything",
			Plan{TooDeep: true, Closure: []ClosureEntry{
				{Status: StatusClosureUnverifiable}, {Status: StatusNotInStore}, {Status: StatusMetadataUnavailable},
			}},
			ErrClosureTooDeep, 422,
		},
		{
			"unverifiable outranks unresolved and degraded",
			Plan{Closure: []ClosureEntry{
				{Status: StatusClosureUnverifiable}, {Status: StatusUnreachable}, {Status: StatusMetadataUnavailable},
			}},
			ErrClosureUnverifiable, 422,
		},
		{
			"permanent before retryable",
			Plan{Closure: []ClosureEntry{{Status: StatusNotInStore}, {Status: StatusMetadataUnavailable}}},
			ErrUnresolvedDependencies, 422,
		},
		{
			"degraded only",
			Plan{Closure: []ClosureEntry{{Status: StatusMetadataUnavailable}, {Status: StatusSatisfiedStore}}},
			ErrMetadataUnavailable, 503,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, status, _ := tc.plan.envelope()
			if code != tc.wantCode || status != tc.wantStatus {
				t.Fatalf("envelope = (%q,%d), want (%q,%d)", code, status, tc.wantCode, tc.wantStatus)
			}
		})
	}
}

// TestPlanClosure_DiffListsEveryRejectingDependency: whichever envelope is
// chosen, every rejecting dependency appears in the diff.
func TestPlanClosure_DiffListsEveryRejectingDependency(t *testing.T) {
	p := newPlanner(
		&fakeStore{versions: map[string][]string{"old": {"1.0"}}},
		DeclaredSet{},
		&fakeResolver{
			listed:   map[string]ucmeta.PluginMeta{"old": {Name: "old", Version: "3.1"}},
			degraded: map[string]bool{"flaky": true},
		},
		true,
	)
	plan := p.planClosure(context.Background(), root(dep("old", "9.0"), dep("flaky", "1.0")))
	if len(plan.Unresolved) != 2 {
		t.Fatalf("unresolved = %+v, want both rows", plan.Unresolved)
	}
	byName := map[string]UnresolvedDependency{}
	for _, u := range plan.Unresolved {
		byName[u.Name] = u
	}
	if got := byName["old"]; got.Reason != StatusUnreachable ||
		got.FoundInStore == nil || *got.FoundInStore != "1.0" ||
		got.FoundUpstream == nil || *got.FoundUpstream != "3.1" {
		t.Fatalf("old row = %+v, want unreachable with store 1.0 / upstream 3.1", got)
	}
	if got := byName["flaky"]; got.Reason != StatusMetadataUnavailable || got.Remediation == "" {
		t.Fatalf("flaky row = %+v, want metadata-unavailable with a remediation", got)
	}
	// Permanent before retryable.
	code, status, _ := plan.envelope()
	if code != ErrUnresolvedDependencies || status != 422 {
		t.Fatalf("envelope = (%q,%d), want (unresolved-dependencies,422)", code, status)
	}
}
