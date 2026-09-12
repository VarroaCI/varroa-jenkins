package rbac

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/rbac/jobglob"
)

// --------------------------------------------------------------------------
// Test fixtures shared by the tenant capability resolution tests.
// --------------------------------------------------------------------------

// testTenantResolver builds a Resolver with a Team informer, for tests that
// exercise EffectiveTenantCapabilities.
func testTenantResolver(
	roles []*v1alpha1.VarroaRole,
	bindings []*v1alpha1.VarroaRoleBinding,
	teams []*v1alpha1.Team,
	controllers []*v1alpha1.Controller,
) *Resolver {
	roleIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, r := range roles {
		_ = roleIdx.Add(r)
	}
	bindingIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{BySubjectIndex: SubjectIndexFunc})
	for _, b := range bindings {
		_ = bindingIdx.Add(b)
	}
	teamIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, tm := range teams {
		_ = teamIdx.Add(tm)
	}
	ctrlIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, c := range controllers {
		_ = ctrlIdx.Add(c)
	}

	return NewResolver(
		&fakeInformer{indexer: roleIdx},
		&fakeInformer{indexer: bindingIdx},
		&fakeInformer{indexer: cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})},
		&fakeInformer{indexer: cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{JenkinsBySubjectIndex: JenkinsSubjectIndexFunc})},
		&fakeInformer{indexer: ctrlIdx},
		&fakeInformer{indexer: teamIdx},
		false,
		[]string{"sub", "preferred_username"},
		[]string{"groups"},
	)
}

// developerRole grants pipelines read and write.
func developerRole() *v1alpha1.VarroaRole {
	return &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "developer"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{
				{Resources: []string{"pipelines"}, Verbs: []string{"read", "write"}},
			},
		},
	}
}

// viewerRole grants pipelines read only.
func viewerRole() *v1alpha1.VarroaRole {
	return &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "viewer"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{
				{Resources: []string{"pipelines"}, Verbs: []string{"read"}},
			},
		},
	}
}

// writerRole grants pipelines write only.
func writerRole() *v1alpha1.VarroaRole {
	return &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "writer"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{
				{Resources: []string{"pipelines"}, Verbs: []string{"write"}},
			},
		},
	}
}

func hasVerb(caps APICapabilitySet, resource, verb string) bool {
	return caps[resource] != nil && caps[resource][verb]
}

// --------------------------------------------------------------------------
// TenantTeam paths, each alone.
// --------------------------------------------------------------------------

// TestEffectiveTenantCapabilities_TeamMembershipPathAlone proves path 1:
// a caller whose group claim matches the Team's owned Group, with
// TeamRBACReady true, gets one unnarrowed grant carrying the Team's role.
func TestEffectiveTenantCapabilities_TeamMembershipPathAlone(t *testing.T) {
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TeamSpec{Members: []string{"alice"}, RoleRef: "developer"},
		Status: v1alpha1.TeamStatus{
			GroupRef:   "team-payments",
			Conditions: []v1alpha1.TeamCondition{{Type: v1alpha1.TeamConditionRBACReady, Status: metav1.ConditionTrue}},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, nil, []*v1alpha1.Team{team}, nil)
	claims := &auth.Claims{Groups: []string{"team-payments"}}

	ref, err := ParseTenant("payments")
	if err != nil {
		t.Fatalf("ParseTenant: %v", err)
	}
	grants := r.EffectiveTenantCapabilities(claims, ref)
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant, got %d", len(grants))
	}
	if !hasVerb(grants[0].Capabilities, "pipelines", "read") || !hasVerb(grants[0].Capabilities, "pipelines", "write") {
		t.Errorf("expected pipelines read and write, got %v", grants[0].Capabilities)
	}
	if grants[0].Globs != nil {
		t.Errorf("expected nil Globs, got %v", grants[0].Globs)
	}
}

// TestEffectiveTenantCapabilities_TenantScopedBindingPathAlone proves path 2:
// a tenant-scoped binding contributes one grant carrying its jobs globs.
func TestEffectiveTenantCapabilities_TenantScopedBindingPathAlone(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "payments-release"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "pay-idp-group"}},
				RoleRef:  "developer",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Tenants: []v1alpha1.TenantScope{{Name: "payments", Jobs: []string{"release/*"}}},
				},
			},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, bindings, nil, nil)
	claims := &auth.Claims{Groups: []string{"pay-idp-group"}}

	ref, _ := ParseTenant("payments")
	grants := r.EffectiveTenantCapabilities(claims, ref)
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant, got %d", len(grants))
	}
	if !hasVerb(grants[0].Capabilities, "pipelines", "read") {
		t.Errorf("expected pipelines read, got %v", grants[0].Capabilities)
	}
	if len(grants[0].Globs) != 1 || grants[0].Globs[0] != "release/*" {
		t.Errorf("expected Globs [release/*], got %v", grants[0].Globs)
	}
}

// TestEffectiveTenantCapabilities_ClusterWideBindingPathAlone proves path 3:
// a cluster-wide (nil-scope) binding contributes one unnarrowed grant.
func TestEffectiveTenantCapabilities_ClusterWideBindingPathAlone(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "eng-viewer"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "eng"}},
				RoleRef:  "viewer",
			},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{viewerRole()}, bindings, nil, nil)
	claims := &auth.Claims{Groups: []string{"eng"}}

	ref, _ := ParseTenant("payments")
	grants := r.EffectiveTenantCapabilities(claims, ref)
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant, got %d", len(grants))
	}
	if !hasVerb(grants[0].Capabilities, "pipelines", "read") {
		t.Errorf("expected pipelines read, got %v", grants[0].Capabilities)
	}
	if hasVerb(grants[0].Capabilities, "pipelines", "write") {
		t.Errorf("did not expect pipelines write from the viewer role")
	}
	if grants[0].Globs != nil {
		t.Errorf("expected nil Globs, got %v", grants[0].Globs)
	}
}

// TestEffectiveTenantCapabilities_NoPathWhenSubjectsDoNotMatch proves that a
// caller matching neither the Team's subject set nor any binding gets no grant.
func TestEffectiveTenantCapabilities_NoPathWhenSubjectsDoNotMatch(t *testing.T) {
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TeamSpec{Members: []string{"alice"}, RoleRef: "developer"},
		Status: v1alpha1.TeamStatus{
			GroupRef:   "team-payments",
			Conditions: []v1alpha1.TeamCondition{{Type: v1alpha1.TeamConditionRBACReady, Status: metav1.ConditionTrue}},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "payments-release"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "pay-idp-group"}},
				RoleRef:  "developer",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Tenants: []v1alpha1.TenantScope{{Name: "payments"}},
				},
			},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, bindings, []*v1alpha1.Team{team}, nil)
	claims := &auth.Claims{Groups: []string{"someone-else"}}

	ref, _ := ParseTenant("payments")
	grants := r.EffectiveTenantCapabilities(claims, ref)
	if len(grants) != 0 {
		t.Fatalf("expected no grants, got %v", grants)
	}
}

// --------------------------------------------------------------------------
// TenantController resolution.
// --------------------------------------------------------------------------

// TestEffectiveTenantCapabilities_NamespaceScopedGrantsControllerNotTeam
// proves a namespace-scoped binding grants a synthetic tenant and not a
// pipelines tenant.
func TestEffectiveTenantCapabilities_NamespaceScopedGrantsControllerNotTeam(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "team-pay-devs"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "pay-eng"}},
				RoleRef:  "developer",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-pay"}},
			},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, bindings, nil, nil)
	claims := &auth.Claims{Groups: []string{"pay-eng"}}

	teamRef, _ := ParseTenant("payments")
	if grants := r.EffectiveTenantCapabilities(claims, teamRef); len(grants) != 0 {
		t.Errorf("expected no grants on the pipelines tenant, got %v", grants)
	}

	ctrlRef, _ := ParseTenant("team-pay/ci")
	grants := r.EffectiveTenantCapabilities(claims, ctrlRef)
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant on the synthetic tenant, got %d", len(grants))
	}
	if !hasVerb(grants[0].Capabilities, "pipelines", "read") {
		t.Errorf("expected pipelines read, got %v", grants[0].Capabilities)
	}
}

// TestEffectiveTenantCapabilities_SelectorScopedGrantsNeither proves a
// controllerSelector-scoped binding contributes to no tenant resolution,
// whether or not the Controller it would match exists.
func TestEffectiveTenantCapabilities_SelectorScopedGrantsNeither(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "pay-selector"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "pay-eng"}},
				RoleRef:  "developer",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					ControllerSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "payments"}},
				},
			},
		},
	}
	claims := &auth.Claims{Groups: []string{"pay-eng"}}
	ctrlRef, _ := ParseTenant("team-pay/ci")

	t.Run("controller present", func(t *testing.T) {
		ctrl := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-pay", Labels: map[string]string{"team": "payments"}},
		}
		r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, bindings, nil, []*v1alpha1.Controller{ctrl})
		if grants := r.EffectiveTenantCapabilities(claims, ctrlRef); len(grants) != 0 {
			t.Errorf("expected no grants, got %v", grants)
		}
	})

	t.Run("controller absent", func(t *testing.T) {
		r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, bindings, nil, nil)
		if grants := r.EffectiveTenantCapabilities(claims, ctrlRef); len(grants) != 0 {
			t.Errorf("expected no grants, got %v", grants)
		}
	})
}

// --------------------------------------------------------------------------
// Team-membership guardrails.
// --------------------------------------------------------------------------

// TestEffectiveTenantCapabilities_MemberNameAloneGrantsNothing proves path 1
// never matches spec.members against the caller's user values.
func TestEffectiveTenantCapabilities_MemberNameAloneGrantsNothing(t *testing.T) {
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TeamSpec{Members: []string{"alice"}, RoleRef: "developer"},
		Status: v1alpha1.TeamStatus{
			GroupRef:   "team-payments",
			Conditions: []v1alpha1.TeamCondition{{Type: v1alpha1.TeamConditionRBACReady, Status: metav1.ConditionTrue}},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, nil, []*v1alpha1.Team{team}, nil)
	claims := &auth.Claims{Subject: "alice"}

	ref, _ := ParseTenant("payments")
	if grants := r.EffectiveTenantCapabilities(claims, ref); len(grants) != 0 {
		t.Errorf("expected no grants, got %v", grants)
	}
}

// TestEffectiveTenantCapabilities_SubjectsOnlyTeamNotMatchedByGroupName
// proves a subjects-only Team (no owned Group) is not matched by the group
// value "team-<name>".
func TestEffectiveTenantCapabilities_SubjectsOnlyTeamNotMatchedByGroupName(t *testing.T) {
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TeamSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "pay-idp-group"}},
			RoleRef:  "developer",
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, nil, []*v1alpha1.Team{team}, nil)
	claims := &auth.Claims{Groups: []string{"team-payments"}}

	ref, _ := ParseTenant("payments")
	if grants := r.EffectiveTenantCapabilities(claims, ref); len(grants) != 0 {
		t.Errorf("expected no grants, got %v", grants)
	}
}

// TestEffectiveTenantCapabilities_DirectSubjectOfTeamGrants proves the
// spec.subjects half of the effective subject set works, for contrast with
// the group-name test above.
func TestEffectiveTenantCapabilities_DirectSubjectOfTeamGrants(t *testing.T) {
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TeamSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "pay-idp-group"}},
			RoleRef:  "developer",
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, nil, []*v1alpha1.Team{team}, nil)
	claims := &auth.Claims{Groups: []string{"pay-idp-group"}}

	ref, _ := ParseTenant("payments")
	grants := r.EffectiveTenantCapabilities(claims, ref)
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant, got %d", len(grants))
	}
	if !hasVerb(grants[0].Capabilities, "pipelines", "write") {
		t.Errorf("expected pipelines write, got %v", grants[0].Capabilities)
	}
}

// TestEffectiveTenantCapabilities_RoleRefAdminGrantsNothing proves path 1
// applies the roleRef=="admin" guardrail.
func TestEffectiveTenantCapabilities_RoleRefAdminGrantsNothing(t *testing.T) {
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TeamSpec{Members: []string{"alice"}, RoleRef: "admin"},
		Status: v1alpha1.TeamStatus{
			GroupRef:   "team-payments",
			Conditions: []v1alpha1.TeamCondition{{Type: v1alpha1.TeamConditionRBACReady, Status: metav1.ConditionTrue}},
		},
	}
	r := testTenantResolver(nil, nil, []*v1alpha1.Team{team}, nil)
	claims := &auth.Claims{Groups: []string{"team-payments"}}

	ref, _ := ParseTenant("payments")
	if grants := r.EffectiveTenantCapabilities(claims, ref); len(grants) != 0 {
		t.Errorf("expected no grants, got %v", grants)
	}
}

// TestEffectiveTenantCapabilities_UnknownRoleRefGrantsNothing proves path 1
// applies the "no VarroaRole of that name exists" guardrail.
func TestEffectiveTenantCapabilities_UnknownRoleRefGrantsNothing(t *testing.T) {
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TeamSpec{Members: []string{"alice"}, RoleRef: "ghost-role"},
		Status: v1alpha1.TeamStatus{
			GroupRef:   "team-payments",
			Conditions: []v1alpha1.TeamCondition{{Type: v1alpha1.TeamConditionRBACReady, Status: metav1.ConditionTrue}},
		},
	}
	r := testTenantResolver(nil, nil, []*v1alpha1.Team{team}, nil)
	claims := &auth.Claims{Groups: []string{"team-payments"}}

	ref, _ := ParseTenant("payments")
	if grants := r.EffectiveTenantCapabilities(claims, ref); len(grants) != 0 {
		t.Errorf("expected no grants, got %v", grants)
	}
}

// TestEffectiveTenantCapabilities_EmptyGroupRefGrantsNothing proves that a
// Team with members but no owned Group (an ownership collision, never
// resolved) grants nothing through the membership path.
func TestEffectiveTenantCapabilities_EmptyGroupRefGrantsNothing(t *testing.T) {
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TeamSpec{Members: []string{"alice"}, RoleRef: "developer"},
		// GroupRef left empty: the Group "team-payments" exists but was
		// hand-authored without Team ownership.
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, nil, []*v1alpha1.Team{team}, nil)
	claims := &auth.Claims{Groups: []string{"team-payments"}}

	ref, _ := ParseTenant("payments")
	if grants := r.EffectiveTenantCapabilities(claims, ref); len(grants) != 0 {
		t.Errorf("expected no grants, got %v", grants)
	}
}

// TestEffectiveTenantCapabilities_StaleGroupRefGrantsNothing proves a Team
// that once owned its Group, and now reports TeamRBACReady=False after a
// collision, grants nothing to a member of the replacement Group even
// though status.groupRef still names it.
func TestEffectiveTenantCapabilities_StaleGroupRefGrantsNothing(t *testing.T) {
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec:       v1alpha1.TeamSpec{Members: []string{"alice"}, RoleRef: "developer"},
		Status: v1alpha1.TeamStatus{
			GroupRef: "team-payments",
			Conditions: []v1alpha1.TeamCondition{
				{Type: v1alpha1.TeamConditionRBACReady, Status: metav1.ConditionFalse, Reason: v1alpha1.TeamReasonGroupApplyFailed},
			},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, nil, []*v1alpha1.Team{team}, nil)
	claims := &auth.Claims{Groups: []string{"team-payments"}}

	ref, _ := ParseTenant("payments")
	if grants := r.EffectiveTenantCapabilities(claims, ref); len(grants) != 0 {
		t.Errorf("expected no grants, got %v", grants)
	}
}

// TestEffectiveTenantCapabilities_DeletedTeamLeavesOtherPathsDeciding proves
// that when path 1 has no Team to read, paths 2 and 3 still decide.
func TestEffectiveTenantCapabilities_DeletedTeamLeavesOtherPathsDeciding(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "payments-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "pay-idp-group"}},
				RoleRef:  "developer",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Tenants: []v1alpha1.TenantScope{{Name: "payments"}},
				},
			},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{developerRole()}, bindings, nil, nil)
	claims := &auth.Claims{Groups: []string{"pay-idp-group"}}

	ref, _ := ParseTenant("payments")
	grants := r.EffectiveTenantCapabilities(claims, ref)
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant, got %d", len(grants))
	}
}

// --------------------------------------------------------------------------
// No-merge behaviour.
// --------------------------------------------------------------------------

// TestEffectiveTenantCapabilities_TwoPartialGrantsStaySeparate proves two
// tenant-scoped bindings that narrow differently are never merged: each
// keeps its own Globs, and together they cover the union of job paths.
func TestEffectiveTenantCapabilities_TwoPartialGrantsStaySeparate(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "release-readers"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "pay-a"}},
				RoleRef:  "viewer",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Tenants: []v1alpha1.TenantScope{{Name: "payments", Jobs: []string{"release/*"}}},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "build-readers"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "pay-b"}},
				RoleRef:  "viewer",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Tenants: []v1alpha1.TenantScope{{Name: "payments", Jobs: []string{"build/*"}}},
				},
			},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{viewerRole()}, bindings, nil, nil)
	claims := &auth.Claims{Groups: []string{"pay-a", "pay-b"}}

	ref, _ := ParseTenant("payments")
	grants := r.EffectiveTenantCapabilities(claims, ref)
	if len(grants) != 2 {
		t.Fatalf("expected 2 grants, got %d", len(grants))
	}

	// Together the two grants cover a job in each half; the check that
	// consumes them asks whether SOME grant holds the verb and matches.
	var coversBuild, coversRelease bool
	for _, g := range grants {
		if !hasVerb(g.Capabilities, "pipelines", "read") {
			continue
		}
		for _, glob := range g.Globs {
			if jobglob.Match(glob, "release/eu") {
				coversRelease = true
			}
			if jobglob.Match(glob, "build/nightly") {
				coversBuild = true
			}
		}
	}
	if !coversRelease || !coversBuild {
		t.Errorf("expected the two grants together to cover both job paths: release=%v build=%v", coversRelease, coversBuild)
	}
}

// TestEffectiveTenantCapabilities_UnnarrowedWriteDoesNotWidenNarrowedRead
// proves an unnarrowed grant for one verb is never combined with the globs
// of a narrowed grant for another verb.
func TestEffectiveTenantCapabilities_UnnarrowedWriteDoesNotWidenNarrowedRead(t *testing.T) {
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "release-readers"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "pay-a"}},
				RoleRef:  "viewer",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Tenants: []v1alpha1.TenantScope{{Name: "payments", Jobs: []string{"release/*"}}},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-writers"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "pay-a"}},
				RoleRef:  "writer",
			},
		},
	}
	r := testTenantResolver([]*v1alpha1.VarroaRole{viewerRole(), writerRole()}, bindings, nil, nil)
	claims := &auth.Claims{Groups: []string{"pay-a"}}

	ref, _ := ParseTenant("payments")
	grants := r.EffectiveTenantCapabilities(claims, ref)
	if len(grants) != 2 {
		t.Fatalf("expected 2 grants, got %d", len(grants))
	}

	// No single grant holds "read" with a nil-or-matching glob for
	// "build/nightly": the narrowed read grant does not cover it, and the
	// unnarrowed grant holds only "write".
	for _, g := range grants {
		if !hasVerb(g.Capabilities, "pipelines", "read") {
			continue
		}
		// This is the narrowed read grant; it must not have gained a nil
		// Globs list from the unnarrowed write grant.
		if g.Globs == nil {
			t.Errorf("the read grant must stay narrowed by release/*, got nil Globs")
		}
	}
}
