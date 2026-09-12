package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/tenancy"
)

const teamTestNS = "varroa-system"

// newTeamTestClient returns a testClient pre-populated with a set of VarroaRoles
// needed for the roleRef guardrail tests.
func newTeamTestClient() *testClient {
	tc := newTestClient()
	tc.teams = make(map[string]*v1alpha1.Team)
	tc.namespaces = make(map[string]bool)
	// Pre-populate a "developer" VarroaRole so roleRef guardrail passes.
	tc.varroaRoles["developer"] = &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: "developer",
		},
	}
	crdstore.MustSeed(tc.store, tc.varroaRoles["developer"])
	return tc
}

// teamWith simulates an existing pre-populated team in the test client.
// lastTeamStatus returns the most recent Team status patch from the store,
// or nil when none was recorded.
func lastTeamStatus(client *testClient) *v1alpha1.TeamStatus {
	gvr, err := crdstore.GVRFor[v1alpha1.Team]()
	if err != nil {
		panic(err)
	}
	ps := client.store.StatusPatches(gvr)
	if len(ps) == 0 {
		return nil
	}
	st, _ := ps[len(ps)-1].Status.(*v1alpha1.TeamStatus)
	return st
}

func teamWith(client *testClient, name string, spec v1alpha1.TeamSpec, uid types.UID) {
	client.teams[name] = &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  uid,
		},
		Spec: spec,
	}
	crdstore.MustSeed(client.store, client.teams[name])
}

func TestTeamReconciler_MembersOnly(t *testing.T) {
	// (a) members-only Team → owned Group + binding with Group:team-<t> subject,
	//     both labels, ownerRef, scope namespaces.
	client := newTeamTestClient()
	client.namespaces["team-ns"] = true

	uid := types.UID("00000000-0000-0000-0000-000000000001")
	teamWith(client, "my-team", v1alpha1.TeamSpec{
		DisplayName: "My Team",
		Members:     []string{"alice", "bob"},
		Namespaces:  []string{"team-ns"},
		RoleRef:     "developer",
	}, uid)

	rec := NewTeamReconciler(client, client.store, teamTestNS, slog.Default())
	_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-team"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert Group created.
	var foundGroup *v1alpha1.Group
	for _, g := range client.storeGroups() {
		if g.Name == "team-my-team" {
			foundGroup = g
			break
		}
	}
	if foundGroup == nil {
		t.Fatal("expected Group team-my-team to be created")
	}
	if foundGroup.Spec.DisplayName != "My Team" {
		t.Errorf("expected DisplayName 'My Team', got %q", foundGroup.Spec.DisplayName)
	}
	if len(foundGroup.Spec.Members) != 2 || foundGroup.Spec.Members[0] != "alice" {
		t.Errorf("unexpected members: %v", foundGroup.Spec.Members)
	}
	// Labels
	if foundGroup.Labels[v1alpha1.LabelManagedBy] != "team" {
		t.Errorf("expected managed-by label 'team', got %q", foundGroup.Labels[v1alpha1.LabelManagedBy])
	}
	if foundGroup.Labels[v1alpha1.LabelTeamName] != "my-team" {
		t.Errorf("expected team-name label 'my-team', got %q", foundGroup.Labels[v1alpha1.LabelTeamName])
	}
	// OwnerRef
	if len(foundGroup.OwnerReferences) != 1 || foundGroup.OwnerReferences[0].UID != uid {
		t.Error("expected owner reference with team UID")
	}

	// Assert VarroaRoleBinding created.
	var foundBinding *v1alpha1.VarroaRoleBinding
	for _, b := range client.storeRoleBindings() {
		if b.Name == "team-my-team" {
			foundBinding = b
			break
		}
	}
	if foundBinding == nil {
		t.Fatal("expected VarroaRoleBinding team-my-team to be created")
	}
	// Subject: Group:team-my-team
	if len(foundBinding.Spec.Subjects) != 1 {
		t.Fatalf("expected 1 subject, got %d", len(foundBinding.Spec.Subjects))
	}
	if foundBinding.Spec.Subjects[0].Kind != "Group" || foundBinding.Spec.Subjects[0].Name != "team-my-team" {
		t.Errorf("expected subject Group:team-my-team, got %s:%s",
			foundBinding.Spec.Subjects[0].Kind, foundBinding.Spec.Subjects[0].Name)
	}
	if foundBinding.Spec.RoleRef != "developer" {
		t.Errorf("expected roleRef 'developer', got %q", foundBinding.Spec.RoleRef)
	}
	if foundBinding.Spec.Scope == nil || len(foundBinding.Spec.Scope.Namespaces) != 1 || foundBinding.Spec.Scope.Namespaces[0] != "team-ns" {
		t.Errorf("unexpected scope: %+v", foundBinding.Spec.Scope)
	}
	// Labels + OwnerRef on binding
	if foundBinding.Labels[v1alpha1.LabelManagedBy] != "team" {
		t.Errorf("expected managed-by label on binding")
	}
	if len(foundBinding.OwnerReferences) != 1 || foundBinding.OwnerReferences[0].UID != uid {
		t.Error("expected owner reference on binding")
	}

	// Check status
	if lastTeamStatus(client) == nil {
		t.Fatal("expected at least one status patch")
	}
	lastStatus := lastTeamStatus(client)
	if lastStatus.GroupRef != "team-my-team" {
		t.Errorf("expected groupRef 'team-my-team', got %q", lastStatus.GroupRef)
	}
	if lastStatus.BindingRef != "team-my-team" {
		t.Errorf("expected bindingRef 'team-my-team', got %q", lastStatus.BindingRef)
	}
}

func TestTeamReconciler_SubjectsOnly(t *testing.T) {
	// (b) subjects-only Team → binding with pass-through subjects, no Group.
	client := newTeamTestClient()
	client.namespaces["team-ns"] = true

	uid := types.UID("00000000-0000-0000-0000-000000000002")
	teamWith(client, "sub-team", v1alpha1.TeamSpec{
		Subjects:   []v1alpha1.SubjectRef{{Kind: "Group", Name: "idp-admins"}},
		Namespaces: []string{"team-ns"},
		RoleRef:    "developer",
	}, uid)

	rec := NewTeamReconciler(client, client.store, teamTestNS, slog.Default())
	_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "sub-team"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No group should be created for members-only.
	for _, g := range client.storeGroups() {
		if g.Name == "team-sub-team" {
			t.Fatal("expected NO Group team-sub-team for subjects-only team")
		}
	}

	// Binding should have pass-through subjects (no Group prepended).
	var foundBinding *v1alpha1.VarroaRoleBinding
	for _, b := range client.storeRoleBindings() {
		if b.Name == "team-sub-team" {
			foundBinding = b
			break
		}
	}
	if foundBinding == nil {
		t.Fatal("expected VarroaRoleBinding team-sub-team")
	}
	if len(foundBinding.Spec.Subjects) != 1 {
		t.Fatalf("expected 1 subject, got %d", len(foundBinding.Spec.Subjects))
	}
	if foundBinding.Spec.Subjects[0].Kind != "Group" || foundBinding.Spec.Subjects[0].Name != "idp-admins" {
		t.Errorf("expected subject Group:idp-admins, got %s:%s",
			foundBinding.Spec.Subjects[0].Kind, foundBinding.Spec.Subjects[0].Name)
	}
}

func TestTeamReconciler_AdminRoleRefBlocked(t *testing.T) {
	// (c) roleRef: admin → TeamReady=False/InvalidRoleRef, no binding applied.
	client := newTeamTestClient()
	client.namespaces["team-ns"] = true

	uid := types.UID("00000000-0000-0000-0000-000000000003")
	teamWith(client, "admin-team", v1alpha1.TeamSpec{
		Members:    []string{"admin-wannabe"},
		Namespaces: []string{"team-ns"},
		RoleRef:    "admin",
	}, uid)

	rec := NewTeamReconciler(client, client.store, teamTestNS, slog.Default())
	_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "admin-team"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No binding should be applied.
	for _, b := range client.storeRoleBindings() {
		if b.Name == "team-admin-team" {
			t.Fatal("expected NO binding for admin roleRef")
		}
	}

	// Status should show InvalidRoleRef.
	if lastTeamStatus(client) == nil {
		t.Fatal("expected status patch")
	}
	lastStatus := lastTeamStatus(client)
	found := false
	for _, c := range lastStatus.Conditions {
		if c.Type == v1alpha1.TeamConditionReady && c.Reason == v1alpha1.TeamReasonInvalidRoleRef {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected TeamReady condition with InvalidRoleRef reason")
	}
}

func TestTeamReconciler_CollisionGuard(t *testing.T) {
	// (e) collision: pre-existing unowned team-<t> Group → TeamRBACReady=False/GroupApplyFailed, no overwrite.
	client := newTeamTestClient()
	client.namespaces["team-ns"] = true

	// Pre-create an unowned Group with the team prefix name.
	crdstore.MustSeed(client.store, &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name: "team-collision-team",
		},
		Spec: v1alpha1.GroupSpec{
			Members: []string{"hand-authored"},
		},
	})

	uid := types.UID("00000000-0000-0000-0000-000000000004")
	teamWith(client, "collision-team", v1alpha1.TeamSpec{
		Members:    []string{"new-member"},
		Namespaces: []string{"team-ns"},
		RoleRef:    "developer",
	}, uid)

	// Ensure the group list is as we set it, then count.
	initialGroupCount := len(client.storeGroups())

	rec := NewTeamReconciler(client, client.store, teamTestNS, slog.Default())
	_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "collision-team"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The pre-existing group should NOT have been overwritten.
	if len(client.storeGroups()) != initialGroupCount {
		t.Errorf("expected %d groups (unchanged), got %d", initialGroupCount, len(client.storeGroups()))
	}

	// Status should show GroupApplyFailed.
	lastStatus := lastTeamStatus(client)
	foundRBAC := false
	foundReady := false
	for _, c := range lastStatus.Conditions {
		if c.Type == v1alpha1.TeamConditionRBACReady && c.Reason == v1alpha1.TeamReasonGroupApplyFailed {
			foundRBAC = true
		}
		if c.Type == v1alpha1.TeamConditionReady && c.Reason == v1alpha1.TeamReasonChildApplyFailed {
			foundReady = true
		}
	}
	if !foundRBAC {
		t.Error("expected TeamRBACReady condition with GroupApplyFailed reason")
	}
	if !foundReady {
		t.Error("expected TeamReady condition with ChildApplyFailed reason")
	}
}

func TestTeamReconciler_MembersRemoved(t *testing.T) {
	// (f) members removed on update → owned Group deleted, binding subjects updated.
	client := newTeamTestClient()
	client.namespaces["team-ns"] = true

	uid := types.UID("00000000-0000-0000-0000-000000000005")

	// Simulate a previous reconcile: create the owned Group first.
	crdstore.MustSeed(client.store, &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name: "team-updated-team",
			Labels: map[string]string{
				v1alpha1.LabelManagedBy: "team",
				v1alpha1.LabelTeamName:  "updated-team",
			},
			OwnerReferences: []metav1.OwnerReference{
				{UID: uid, Controller: boolPtr(true)},
			},
		},
		Spec: v1alpha1.GroupSpec{
			Members: []string{"old-member"},
		},
	})

	// Now reconcile with NO members but with subjects.
	teamWith(client, "updated-team", v1alpha1.TeamSpec{
		Subjects:   []v1alpha1.SubjectRef{{Kind: "User", Name: "direct-user"}},
		Namespaces: []string{"team-ns"},
		RoleRef:    "developer",
	}, uid)

	rec := NewTeamReconciler(client, client.store, teamTestNS, slog.Default())
	_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "updated-team"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Group should be deleted.
	for _, g := range client.storeGroups() {
		if g.Name == "team-updated-team" {
			t.Error("expected Group team-updated-team to be deleted after members removed")
		}
	}

	// Binding should have only the direct user subject (no Group subject).
	var foundBinding *v1alpha1.VarroaRoleBinding
	for _, b := range client.storeRoleBindings() {
		if b.Name == "team-updated-team" {
			foundBinding = b
			break
		}
	}
	if foundBinding == nil {
		t.Fatal("expected VarroaRoleBinding team-updated-team")
	}
	if len(foundBinding.Spec.Subjects) != 1 {
		t.Fatalf("expected 1 subject, got %d", len(foundBinding.Spec.Subjects))
	}
	if foundBinding.Spec.Subjects[0].Kind != "User" || foundBinding.Spec.Subjects[0].Name != "direct-user" {
		t.Errorf("expected subject User:direct-user, got %s:%s",
			foundBinding.Spec.Subjects[0].Kind, foundBinding.Spec.Subjects[0].Name)
	}
}

func boolPtr(b bool) *bool {
	return &b
}

func TestTeamReconciler_NamespaceStates(t *testing.T) {
	// (g) stub namespace states → correct TeamNamespacesReady reason + namespaceStates.
	tests := []struct {
		name         string
		ns           string
		nsExists     bool
		provision    bool
		expectState  string
		expectReason string
		expectReady  bool
	}{
		{
			name:         "existing-ns-managed",
			ns:           "managed-ns",
			nsExists:     true,
			provision:    false,
			expectState:  v1alpha1.TeamNamespaceStateManaged,
			expectReason: v1alpha1.TeamReasonNamespacesSatisfied,
			expectReady:  true,
		},
		{
			name:         "missing-ns",
			ns:           "missing-ns",
			nsExists:     false,
			provision:    false,
			expectState:  v1alpha1.TeamNamespaceStateMissing,
			expectReason: v1alpha1.TeamReasonNamespaceMissing,
			expectReady:  false,
		},
		{
			name:         "created-via-provision",
			ns:           "created-ns",
			nsExists:     false,
			provision:    true,
			expectState:  v1alpha1.TeamNamespaceStateCreated,
			expectReason: v1alpha1.TeamReasonNamespacesSatisfied,
			expectReady:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTeamTestClient()
			if tt.nsExists {
				client.namespaces[tt.ns] = true
			}

			uid := types.UID("00000000-0000-0000-0000-000000000006")
			spec := v1alpha1.TeamSpec{
				Members:    []string{"test-user"},
				Namespaces: []string{tt.ns},
				RoleRef:    "developer",
			}
			if tt.provision {
				spec.ProvisionNamespaces = true
			}

			teamWith(client, "ns-team", spec, uid)

			rec := NewTeamReconciler(client, client.store, teamTestNS, slog.Default())
			_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "ns-team"}})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			lastStatus := lastTeamStatus(client)

			// Check namespace state.
			var nsState *v1alpha1.TeamNamespaceState
			for i, s := range lastStatus.NamespaceStates {
				if s.Name == tt.ns {
					nsState = &lastStatus.NamespaceStates[i]
					break
				}
			}
			if nsState == nil {
				t.Fatalf("expected namespace state for %q", tt.ns)
			}
			if nsState.State != tt.expectState {
				t.Errorf("expected state %q, got %q", tt.expectState, nsState.State)
			}

			// Check TeamNamespacesReady condition.
			foundNSCond := false
			foundReadyCond := false
			for _, c := range lastStatus.Conditions {
				if c.Type == v1alpha1.TeamConditionNamespacesReady {
					foundNSCond = true
					if c.Reason != tt.expectReason {
						t.Errorf("expected NamespacesReady reason %q, got %q", tt.expectReason, c.Reason)
					}
					if tt.expectReady && c.Status != metav1.ConditionTrue {
						t.Errorf("expected NamespacesReady=True, got %s", c.Status)
					}
					if !tt.expectReady && c.Status != metav1.ConditionFalse {
						t.Errorf("expected NamespacesReady=False, got %s", c.Status)
					}
				}
				if c.Type == v1alpha1.TeamConditionReady {
					foundReadyCond = true
					if tt.expectReady && c.Status != metav1.ConditionTrue {
						t.Errorf("expected TeamReady=True, got %s", c.Status)
					}
					if !tt.expectReady && c.Status != metav1.ConditionFalse {
						t.Errorf("expected TeamReady=False, got %s", c.Status)
					}
				}
			}
			if !foundNSCond {
				t.Error("expected TeamNamespacesReady condition")
			}
			if !foundReadyCond {
				t.Error("expected TeamReady condition")
			}

			// Precheck path (provision == false) must never sync image pull secrets.
			if !tt.provision && len(client.imagePullSecretCopies) > 0 {
				t.Errorf("expected no imagePullSecretCopies on precheck path, got %d", len(client.imagePullSecretCopies))
			}
		})
	}
}

func TestTeamReconciler_UnmanagedNamespaceScopedMode(t *testing.T) {
	// C1 regression: with a scoped managed set injected, an existing namespace
	// OUTSIDE the set must surface as Unmanaged (NamespacesReady=False), for
	// both the precheck and the provisioning path.
	for _, provision := range []bool{false, true} {
		name := "precheck"
		if provision {
			name = "provision"
		}
		t.Run(name, func(t *testing.T) {
			client := newTeamTestClient()
			client.namespaces["outside-ns"] = true

			uid := types.UID("00000000-0000-0000-0000-000000000007")
			spec := v1alpha1.TeamSpec{
				Members:             []string{"test-user"},
				Namespaces:          []string{"outside-ns"},
				RoleRef:             "developer",
				ProvisionNamespaces: provision,
			}
			teamWith(client, "ns-team-scoped", spec, uid)

			rec := NewTeamReconciler(client, client.store, teamTestNS, slog.Default())
			rec.SetManagedSet(tenancy.NewManagedSet("managed-a,managed-b", teamTestNS))
			_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "ns-team-scoped"}})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			lastStatus := lastTeamStatus(client)
			var nsState *v1alpha1.TeamNamespaceState
			for i, s := range lastStatus.NamespaceStates {
				if s.Name == "outside-ns" {
					nsState = &lastStatus.NamespaceStates[i]
					break
				}
			}
			if nsState == nil {
				t.Fatal("expected namespace state for outside-ns")
			}
			if nsState.State != v1alpha1.TeamNamespaceStateUnmanaged {
				t.Errorf("expected state %q, got %q", v1alpha1.TeamNamespaceStateUnmanaged, nsState.State)
			}
			for _, c := range lastStatus.Conditions {
				if c.Type == v1alpha1.TeamConditionNamespacesReady {
					if c.Status != metav1.ConditionFalse {
						t.Errorf("expected NamespacesReady=False, got %s", c.Status)
					}
					if c.Reason != v1alpha1.TeamReasonNamespaceUnmanaged {
						t.Errorf("expected reason %q, got %q", v1alpha1.TeamReasonNamespaceUnmanaged, c.Reason)
					}
				}
			}
		})
	}
}

func TestTeamReconciler_SyncsImagePullSecretsOnProvision(t *testing.T) {
	tests := []struct {
		name       string
		nsExists   bool
		provision  bool
		defaults   *v1alpha1.ProvisioningDefaults
		injectErr  error
		wantCopies int
		wantReady  bool
	}{
		{
			name:       "created-via-provision-creates-secret",
			nsExists:   false,
			provision:  true,
			defaults:   &v1alpha1.ProvisioningDefaults{Spec: v1alpha1.ProvisioningDefaultsSpec{ImagePullSecrets: []string{"registry-creds"}}},
			wantCopies: 1,
			wantReady:  true,
		},
		{
			name:       "already-managed-still-syncs",
			nsExists:   true,
			provision:  true,
			defaults:   &v1alpha1.ProvisioningDefaults{Spec: v1alpha1.ProvisioningDefaultsSpec{ImagePullSecrets: []string{"registry-creds"}}},
			wantCopies: 1,
			wantReady:  true,
		},
		{
			name:       "no-defaults-no-copy",
			nsExists:   false,
			provision:  true,
			defaults:   nil,
			wantCopies: 0,
			wantReady:  true,
		},
		{
			name:       "copy-error-does-not-flip-ready",
			nsExists:   false,
			provision:  true,
			defaults:   &v1alpha1.ProvisioningDefaults{Spec: v1alpha1.ProvisioningDefaultsSpec{ImagePullSecrets: []string{"registry-creds"}}},
			injectErr:  fmt.Errorf("injected error"),
			wantCopies: 1,
			wantReady:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTeamTestClient()
			if tt.nsExists {
				client.namespaces["created-ns"] = true
			}
			client.provisioningDefaults = tt.defaults
			if tt.defaults != nil {
				d := tt.defaults.DeepCopy()
				if d.Name == "" {
					d.Name = "varroa-defaults"
				}
				crdstore.MustSeed(client.store, d)
			}
			client.imagePullSecretCopyErr = tt.injectErr

			uid := types.UID("00000000-0000-0000-0000-000000000008")
			spec := v1alpha1.TeamSpec{
				Members:             []string{"test-user"},
				Namespaces:          []string{"created-ns"},
				RoleRef:             "developer",
				ProvisionNamespaces: tt.provision,
			}
			teamWith(client, "pull-secret-team", spec, uid)

			rec := NewTeamReconciler(client, client.store, teamTestNS, slog.Default())
			_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "pull-secret-team"}})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(client.imagePullSecretCopies) != tt.wantCopies {
				t.Errorf("expected %d imagePullSecretCopies, got %d", tt.wantCopies, len(client.imagePullSecretCopies))
			}
			if tt.wantCopies > 0 {
				cp := client.imagePullSecretCopies[0]
				if cp.src != teamTestNS {
					t.Errorf("expected src=%q, got %q", teamTestNS, cp.src)
				}
				if cp.dst != "created-ns" {
					t.Errorf("expected dst=%q, got %q", "created-ns", cp.dst)
				}
				if cp.name != "registry-creds" {
					t.Errorf("expected name=%q, got %q", "registry-creds", cp.name)
				}
			}

			lastStatus := lastTeamStatus(client)
			var ready bool
			for _, c := range lastStatus.Conditions {
				if c.Type == v1alpha1.TeamConditionReady {
					ready = c.Status == metav1.ConditionTrue
				}
			}
			if ready != tt.wantReady {
				t.Errorf("expected TeamReady=%v, got %v", tt.wantReady, ready)
			}
		})
	}
}

func TestTeamReconciler_MissingRoleRef(t *testing.T) {
	// (d) missing roleRef VarroaRole → InvalidRoleRef.
	client := newTeamTestClient()
	client.namespaces["team-ns"] = true

	uid := types.UID("00000000-0000-0000-0000-000000000007")
	teamWith(client, "badrole-team", v1alpha1.TeamSpec{
		Members:    []string{"user"},
		Namespaces: []string{"team-ns"},
		RoleRef:    "nonexistent-role",
	}, uid)

	rec := NewTeamReconciler(client, client.store, teamTestNS, slog.Default())
	_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "badrole-team"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	lastStatus := lastTeamStatus(client)
	found := false
	for _, c := range lastStatus.Conditions {
		if c.Type == v1alpha1.TeamConditionReady && c.Reason == v1alpha1.TeamReasonInvalidRoleRef {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected TeamReady condition with InvalidRoleRef reason for missing VarroaRole")
	}
}

// --- pipelines placement pass integration (design D4) ---
//
// recordingSink (the activity.EventSink test double) is shared with
// upgradetracker_controller_test.go — this file adds no second definition.

// succeedingPurgeFunc answers every C11 job_schedules purge request with
// success.
func succeedingPurgeFunc(context.Context, cells.PurgeRequest) (bool, error) {
	return true, nil
}

// succeedingAnnounceFunc answers every C6 tenant.sync "announce" request
// with success.
func succeedingAnnounceFunc(context.Context, cells.AnnounceRequest) (cells.AnnounceResult, error) {
	return cells.AnnounceResult{OK: true}, nil
}

// seedCellController seeds a Ready cell Controller named "cell-a" in
// namespace "cells-ns", labelled for the "jenkins-lts" profile, in
// client.store: a candidate the placement pass can select.
func seedCellController(client *testClient, tenants []string) {
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cell-a",
			Namespace: "cells-ns",
			Labels:    map[string]string{cells.LabelCellProfile: "jenkins-lts"},
		},
		Spec: v1alpha1.ControllerSpec{
			Cell: &v1alpha1.CellSpec{Enabled: true, MaxTenants: 10},
		},
		Status: v1alpha1.ControllerStatus{
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionCellReady, Status: metav1.ConditionTrue}},
			// AppliedTenants mirrors Tenants: the default fixture represents
			// a cell whose last successful apply matches its current
			// tenant list. A test exercising the mid-drain state mutates
			// AppliedTenants after seeding.
			Cell: &v1alpha1.CellStatus{Tenants: tenants, AppliedTenants: append([]string(nil), tenants...)},
		},
	}
	crdstore.MustSeed(client.store, cr)
}

// seedJenkinsVersionProfile seeds a minimal "jenkins-lts" JenkinsVersionProfile
// so gate 3 (ProfileNotFound) passes.
func seedJenkinsVersionProfile(client *testClient) {
	crdstore.MustSeed(client.store, &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "jenkins-lts"},
	})
}

// newPipelinesTeamReconciler builds a TeamReconciler with every pipelines
// setter wired, so refusal gates 1 and 2 pass and a purge always succeeds
// unless the test overrides it.
func newPipelinesTeamReconciler(client *testClient, sink *recordingSink) *TeamReconciler {
	rec := NewTeamReconciler(client, client.store, teamTestNS, slog.Default())
	rec.SetCellsConfig(CellsConfig{MaxTenantsPerCell: 10})
	rec.SetPipelinesGates("1", "s3://bucket/prefix")
	rec.SetClusterIdentity("core")
	rec.SetPurgeRequester(succeedingPurgeFunc)
	rec.SetSyncAnnouncer(succeedingAnnounceFunc)
	if sink != nil {
		rec.SetActivityPublisher(sink)
	}
	return rec
}

func TestTeamReconciler_Pipelines_PlacesTenantAndAppliesRoleBinding(t *testing.T) {
	client := newTeamTestClient()
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)
	seedCellController(client, nil)

	uid := types.UID("10000000-0000-0000-0000-000000000001")
	teamWith(client, "payments", v1alpha1.TeamSpec{
		Members:    []string{"alice"},
		Namespaces: []string{"payments-ns"},
		Pipelines: &v1alpha1.TeamPipelinesSpec{
			Profile:        "jenkins-lts",
			AgentNamespace: "payments-ns",
		},
	}, uid)

	sink := &recordingSink{}
	rec := newPipelinesTeamReconciler(client, sink)
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	status := lastTeamStatus(client)
	if status == nil || status.Placement == nil || status.Placement.Cell != "cell-a" || status.Placement.CellNamespace != "cells-ns" || status.Placement.Cluster != "core" {
		t.Fatalf("expected a placement on cell-a, got %+v", status)
	}

	// task 6.8: exactly one PatchStatus[Team] call per tick.
	gvr, err := crdstore.GVRFor[v1alpha1.Team]()
	if err != nil {
		t.Fatalf("GVRFor failed: %v", err)
	}
	if n := len(client.store.StatusPatches(gvr)); n != 1 {
		t.Errorf("expected exactly one Team status patch, got %d", n)
	}

	// task 6.6: the cell patch is applied after the status patch.
	if len(client.cellStatusPatches) != 1 {
		t.Fatalf("expected one cell status patch, got %d", len(client.cellStatusPatches))
	}
	if got := client.cellStatusPatches[0]; got.name != "cell-a" || len(got.cell.Tenants) != 1 || got.cell.Tenants[0] != "payments" {
		t.Errorf("unexpected cell status patch: %+v", got)
	}

	// task 9.2: the team-<team>-pipelines JenkinsRoleBinding exists with the
	// Team's roleRef and subjects, Folder+Subtree scope, and the owning
	// cell's ControllerScope.
	rb, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](context.Background(), client.store, "team-payments-pipelines", "")
	if err != nil {
		t.Fatalf("expected the pipelines JenkinsRoleBinding to exist: %v", err)
	}
	if rb.Spec.RoleRef != "developer" {
		t.Errorf("roleRef = %q, want developer", rb.Spec.RoleRef)
	}
	if rb.Spec.JenkinsScope == nil || rb.Spec.JenkinsScope.Type != "Folder" || rb.Spec.JenkinsScope.Folder != "payments" || rb.Spec.JenkinsScope.Propagate != "Subtree" {
		t.Errorf("unexpected JenkinsScope: %+v", rb.Spec.JenkinsScope)
	}
	if rb.Spec.ControllerScope == nil || rb.Spec.ControllerScope.ControllerSelector == nil ||
		rb.Spec.ControllerScope.ControllerSelector.MatchLabels[v1alpha1.LabelControllerName] != "cell-a" {
		t.Errorf("unexpected ControllerScope: %+v", rb.Spec.ControllerScope)
	}

	// task 6.12: tenant.placed is published.
	if len(sink.events) != 1 || sink.events[0].Type != "tenant.placed" {
		t.Errorf("expected one tenant.placed event, got %+v", sink.events)
	}
}

// TestTeamReconciler_Pipelines_ManuallyCreatedCellResolvesProfileFromVersion
// asserts buildCellCandidate's fallback: a ready cell with no warm-pool
// label (cells.LabelCellProfile) still resolves a profile, through
// ResolveProfile against the cell's spec.version, and becomes a placement
// candidate.
func TestTeamReconciler_Pipelines_ManuallyCreatedCellResolvesProfileFromVersion(t *testing.T) {
	client := newTeamTestClient()
	client.namespaces["payments-ns"] = true
	crdstore.MustSeed(client.store, &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "jenkins-lts"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.479.3"},
	})
	// A hand-made cell: ready, matching version, but no
	// varroa.dev/cell-profile label — the warm pool never created it.
	crdstore.MustSeed(client.store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell-manual", Namespace: "cells-ns"},
		Spec: v1alpha1.ControllerSpec{
			Version: "2.479.3",
			Cell:    &v1alpha1.CellSpec{Enabled: true, MaxTenants: 10},
		},
		Status: v1alpha1.ControllerStatus{
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionCellReady, Status: metav1.ConditionTrue}},
			Cell:       &v1alpha1.CellStatus{},
		},
	})

	uid := types.UID("10000000-0000-0000-0000-000000000004")
	teamWith(client, "payments", v1alpha1.TeamSpec{
		Members:    []string{"alice"},
		Namespaces: []string{"payments-ns"},
		Pipelines: &v1alpha1.TeamPipelinesSpec{
			Profile:        "jenkins-lts",
			AgentNamespace: "payments-ns",
		},
	}, uid)

	rec := newPipelinesTeamReconciler(client, nil)
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	status := lastTeamStatus(client)
	if status == nil || status.Placement == nil || status.Placement.Cell != "cell-manual" {
		t.Fatalf("expected the label-less cell to be selected as a candidate, got %+v", status)
	}
}

func TestTeamReconciler_Pipelines_TeardownDeletesChildrenAndHoldsFinalizer(t *testing.T) {
	client := newTeamTestClient()
	client.namespaces["payments-ns"] = true
	// The cell still applies the tenant's folder: the drain has not
	// finished, so the finalizer must hold.
	seedCellController(client, []string{"payments"})

	uid := types.UID("10000000-0000-0000-0000-000000000002")
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "payments",
			UID:        uid,
			Finalizers: []string{v1alpha1.TeamFinalizerTenantPlacement},
		},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"payments-ns"},
			// spec.pipelines removed: teardown starts.
		},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{Cluster: "core", Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
			Pipelines: &v1alpha1.TeamPipelinesStatus{WebhookToken: "tok", WebhookSecret: "sec", AgentNamespace: "payments-ns"},
		},
	}
	client.teams["payments"] = team
	crdstore.MustSeed(client.store, team)
	crdstore.MustSeed(client.store, &v1alpha1.JenkinsRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "team-payments-pipelines"}})

	rec := newPipelinesTeamReconciler(client, nil)
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	status := lastTeamStatus(client)
	if status == nil || status.Placement == nil {
		t.Fatal("the finalizer holds before the cell applies, so placement must stay set")
	}
	placedCond := false
	for _, c := range status.Conditions {
		if c.Type == v1alpha1.TeamConditionPlaced && c.Reason == v1alpha1.TeamReasonTenantTeardownPending {
			placedCond = true
		}
	}
	if !placedCond {
		t.Errorf("expected Placed=False/TenantTeardownPending, got %+v", status.Conditions)
	}

	// The four teardown-stage-one deletions: the tenant dropped from the
	// cell's tenant list, the JenkinsRoleBinding, the ServiceAccount, and
	// its token Secret.
	if len(client.cellStatusPatches) != 1 || len(client.cellStatusPatches[0].cell.Tenants) != 0 {
		t.Errorf("expected the tenant dropped from the cell's tenant list: %+v", client.cellStatusPatches)
	}
	if _, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](context.Background(), client.store, "team-payments-pipelines", ""); err == nil {
		t.Error("expected the pipelines JenkinsRoleBinding to be deleted")
	}
	var deletedSA bool
	for _, d := range client.deleted {
		if d.kind == "ServiceAccount" && d.name == "varroa-agent-payments" {
			deletedSA = true
		}
	}
	if !deletedSA {
		t.Errorf("expected the tenant agent ServiceAccount deleted, got %+v", client.deleted)
	}
	var deletedSecret bool
	for _, s := range client.deletedSecrets {
		if s == "varroa-agent-payments-token" {
			deletedSecret = true
		}
	}
	if !deletedSecret {
		t.Errorf("expected the tenant agent token Secret deleted, got %+v", client.deletedSecrets)
	}
}

// TestTeamReconciler_Pipelines_PurgeAttemptsPersistAcrossReconcilerRestarts
// is task 6.10's persistence acceptance: status.pipelines.purgeAttempts
// round-trips through the fake store, not just through an in-memory
// *TeamStatus reused across calls. Two Reconcile calls, each on a fresh
// TeamReconciler instance reading the Team back from the store, stand in
// for an operator restart between purge retries. A restart must not lose
// the count: cells.Place bounds the retry at ten failed attempts, and a
// count reset by every restart would hold the finalizer indefinitely on a
// recorder that stays down.
func TestTeamReconciler_Pipelines_PurgeAttemptsPersistAcrossReconcilerRestarts(t *testing.T) {
	client := newTeamTestClient()
	client.namespaces["payments-ns"] = true
	// A drained cell: the cell no longer applies the tenant's folder, so
	// teardown reaches the purge step on every tick instead of holding on
	// TenantTeardownPending for the drain.
	seedCellController(client, nil)

	uid := types.UID("10000000-0000-0000-0000-000000000005")
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "payments",
			UID:        uid,
			Finalizers: []string{v1alpha1.TeamFinalizerTenantPlacement},
		},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"payments-ns"},
			// spec.pipelines removed: teardown is in progress.
		},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{Cluster: "core", Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
			Pipelines: &v1alpha1.TeamPipelinesStatus{WebhookToken: "tok", WebhookSecret: "sec", AgentNamespace: "payments-ns"},
		},
	}
	client.teams["payments"] = team
	crdstore.MustSeed(client.store, team)

	failingPurge := func(context.Context, cells.PurgeRequest) (bool, error) {
		return false, fmt.Errorf("recorder unreachable")
	}

	rec1 := newPipelinesTeamReconciler(client, nil)
	rec1.SetPurgeRequester(failingPurge)
	if _, err := rec1.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	status1 := lastTeamStatus(client)
	if status1 == nil || status1.Pipelines == nil || status1.Pipelines.PurgeAttempts != 1 {
		t.Fatalf("after first Reconcile: purgeAttempts = %+v, want 1", status1)
	}
	if status1.Placement == nil {
		t.Fatal("the finalizer must still hold after one failed purge attempt")
	}

	// A fresh TeamReconciler instance reading the Team back through the
	// store, standing in for an operator restart between retries.
	rec2 := newPipelinesTeamReconciler(client, nil)
	rec2.SetPurgeRequester(failingPurge)
	if _, err := rec2.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	status2 := lastTeamStatus(client)
	if status2 == nil || status2.Pipelines == nil || status2.Pipelines.PurgeAttempts != 2 {
		t.Fatalf("after second Reconcile (fresh reconciler instance): purgeAttempts = %+v, want 2 — the count must persist across a restart, not reset to 1", status2)
	}
}

func TestTeamReconciler_Pipelines_TeardownReleasesFinalizerWhenDrained(t *testing.T) {
	client := newTeamTestClient()
	client.namespaces["payments-ns"] = true
	// The cell no longer applies the tenant's folder: the drain finished.
	seedCellController(client, nil)

	uid := types.UID("10000000-0000-0000-0000-000000000003")
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "payments",
			UID:        uid,
			Finalizers: []string{v1alpha1.TeamFinalizerTenantPlacement},
		},
		Spec: v1alpha1.TeamSpec{Namespaces: []string{"payments-ns"}},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{Cluster: "core", Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
			Pipelines: &v1alpha1.TeamPipelinesStatus{WebhookToken: "tok", WebhookSecret: "sec", AgentNamespace: "payments-ns"},
		},
	}
	client.teams["payments"] = team
	crdstore.MustSeed(client.store, team)

	rec := newPipelinesTeamReconciler(client, nil)
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	status := lastTeamStatus(client)
	if status == nil || status.Placement != nil {
		t.Fatalf("expected placement cleared once drained, got %+v", status)
	}
	teamGVR, err := crdstore.GVRFor[v1alpha1.Team]()
	if err != nil {
		t.Fatalf("GVRFor failed: %v", err)
	}
	metaPatches := client.store.MetaPatches(teamGVR)
	released := false
	for _, mp := range metaPatches {
		if mp.Name != "payments" {
			continue
		}
		if finalizers, ok := mp.Meta["finalizers"].([]string); ok {
			released = true
			for _, f := range finalizers {
				if f == v1alpha1.TeamFinalizerTenantPlacement {
					released = false
				}
			}
		}
	}
	if !released {
		t.Errorf("expected the finalizer released, meta patches: %+v", metaPatches)
	}
}

func TestTeamReconciler_Pipelines_TenantWarningsConditionFolded(t *testing.T) {
	client := newTeamTestClient()
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)
	seedCellController(client, []string{"payments"})
	// Fold in a warning the controller reconciler recorded for this tenant.
	cell, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "cell-a", "cells-ns")
	if err != nil {
		t.Fatalf("get cell: %v", err)
	}
	cell.Status.Cell.TenantWarnings = []v1alpha1.TenantWarning{
		{Tenant: "payments", Reason: v1alpha1.TeamReasonCredentialSkipped, Message: "duplicate credential id"},
		{Tenant: "other-team", Reason: v1alpha1.TeamReasonBundleKeysIgnored, Message: "not this tenant"},
	}
	crdstore.MustSeed(client.store, cell)

	uid := types.UID("10000000-0000-0000-0000-000000000004")
	teamWith(client, "payments", v1alpha1.TeamSpec{
		Members:    []string{"alice"},
		Namespaces: []string{"payments-ns"},
		Pipelines: &v1alpha1.TeamPipelinesSpec{
			Profile:        "jenkins-lts",
			AgentNamespace: "payments-ns",
		},
	}, uid)

	rec := newPipelinesTeamReconciler(client, nil)
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	status := lastTeamStatus(client)
	var found *v1alpha1.TeamCondition
	for i := range status.Conditions {
		if status.Conditions[i].Type == v1alpha1.TeamConditionTenantWarnings {
			found = &status.Conditions[i]
		}
	}
	if found == nil {
		t.Fatal("expected a TenantWarnings condition")
	}
	if found.Status != metav1.ConditionTrue || found.Reason != v1alpha1.TeamReasonCredentialSkipped {
		t.Errorf("unexpected TenantWarnings condition: %+v", found)
	}
	if strings.Contains(found.Message, "other-team") {
		t.Errorf("expected only this Team's own warnings folded in, got message %q", found.Message)
	}
}

// TestTeamReconciler_Pipelines_CreatesTenantAgentIdentity is task 8.9's
// acceptance: the placement pass creates the ServiceAccount, its token
// Secret, and the Role and RoleBinding CreateAgentRBAC builds, in
// agentNamespace, all named varroa-agent-<team>[-token].
func TestTeamReconciler_Pipelines_CreatesTenantAgentIdentity(t *testing.T) {
	client := newTeamTestClient()
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)
	seedCellController(client, nil)

	uid := types.UID("10000000-0000-0000-0000-000000000005")
	teamWith(client, "payments", v1alpha1.TeamSpec{
		Namespaces: []string{"payments-ns"},
		Pipelines: &v1alpha1.TeamPipelinesSpec{
			Profile:        "jenkins-lts",
			AgentNamespace: "payments-ns",
		},
	}, uid)

	rec := newPipelinesTeamReconciler(client, nil)
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	foundSA := false
	for _, sa := range client.serviceAccounts {
		if sa == "varroa-agent-payments" {
			foundSA = true
		}
	}
	if !foundSA {
		t.Errorf("expected ServiceAccount varroa-agent-payments created, got %v", client.serviceAccounts)
	}

	foundToken := false
	for _, s := range client.saTokenSecrets {
		if s.name == "varroa-agent-payments-token" && s.namespace == "payments-ns" && s.serviceAccount == "varroa-agent-payments" {
			foundToken = true
		}
	}
	if !foundToken {
		t.Errorf("expected token Secret varroa-agent-payments-token bound to varroa-agent-payments in payments-ns, got %v", client.saTokenSecrets)
	}

	foundRBAC := false
	for _, c := range client.agentRBACCalls {
		if c.name == "varroa-agent-payments" && c.namespace == "payments-ns" {
			foundRBAC = true
		}
	}
	if !foundRBAC {
		t.Errorf("expected CreateAgentRBAC(varroa-agent-payments, payments-ns), got %v", client.agentRBACCalls)
	}
}

// TestTeamReconciler_Pipelines_TeardownWaitCreatesNothing is task 8.9's
// second acceptance clause: a tick between the two teardown stages (the
// tenant already dropped from the cell's list, the finalizer still held)
// creates none of the tenant agent identity objects — the placement pass is
// the one owner, and it never recreates what teardown removed.
func TestTeamReconciler_Pipelines_TeardownWaitCreatesNothing(t *testing.T) {
	client := newTeamTestClient()
	client.namespaces["payments-ns"] = true
	// The cell still applies the tenant's folder: the drain has not
	// finished, so teardown stays in the between-stages wait.
	seedCellController(client, []string{"payments"})

	uid := types.UID("10000000-0000-0000-0000-000000000006")
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "payments",
			UID:        uid,
			Finalizers: []string{v1alpha1.TeamFinalizerTenantPlacement},
		},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"payments-ns"},
			// spec.pipelines removed: teardown started on an earlier tick.
		},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{Cluster: "core", Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
			Pipelines: &v1alpha1.TeamPipelinesStatus{WebhookToken: "tok", WebhookSecret: "sec", AgentNamespace: "payments-ns"},
		},
	}
	client.teams["payments"] = team
	crdstore.MustSeed(client.store, team)

	rec := newPipelinesTeamReconciler(client, nil)
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(client.serviceAccounts) != 0 {
		t.Errorf("expected no ServiceAccount created during the teardown wait, got %v", client.serviceAccounts)
	}
	if len(client.saTokenSecrets) != 0 {
		t.Errorf("expected no token Secret created during the teardown wait, got %v", client.saTokenSecrets)
	}
	if len(client.agentRBACCalls) != 0 {
		t.Errorf("expected no agent RBAC created during the teardown wait, got %v", client.agentRBACCalls)
	}
}

// callOrderBackend wraps a crdstore.Backend and appends to one shared,
// ordered call log every time it patches a Team's status or writes a
// JenkinsRoleBinding (task 9.2's pipelines child). It shares the log with
// callOrderClient below, so the two together record one call order across
// the fake store and the fake client.
type callOrderBackend struct {
	crdstore.Backend
	calls *[]string
}

func (b *callOrderBackend) PatchObjectStatus(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string, status any) error {
	if teamGVR, _ := crdstore.GVRFor[v1alpha1.Team](); gvr == teamGVR {
		*b.calls = append(*b.calls, "team-status-patch")
	}
	return b.Backend.PatchObjectStatus(ctx, gvr, namespace, name, status)
}

func (b *callOrderBackend) CreateObject(ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if jrbGVR, _ := crdstore.GVRFor[v1alpha1.JenkinsRoleBinding](); gvr == jrbGVR {
		*b.calls = append(*b.calls, "child-jenkinsrolebinding")
	}
	return b.Backend.CreateObject(ctx, gvr, namespace, obj)
}

func (b *callOrderBackend) UpdateObject(ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if jrbGVR, _ := crdstore.GVRFor[v1alpha1.JenkinsRoleBinding](); gvr == jrbGVR {
		*b.calls = append(*b.calls, "child-jenkinsrolebinding")
	}
	return b.Backend.UpdateObject(ctx, gvr, namespace, obj)
}

// callOrderClient wraps a *testClient and appends to the same shared,
// ordered call log every time it patches a cell's tenant list or creates
// part of the tenant agent identity (design D7/D8/D11's other pipelines
// children).
type callOrderClient struct {
	*testClient
	calls *[]string
}

func (c *callOrderClient) PatchControllerCellStatus(ctx context.Context, name, namespace string, cell *v1alpha1.CellStatus) error {
	*c.calls = append(*c.calls, "cell-tenant-patch")
	return c.testClient.PatchControllerCellStatus(ctx, name, namespace, cell)
}

func (c *callOrderClient) CreateServiceAccount(ctx context.Context, name, namespace string) error {
	*c.calls = append(*c.calls, "child-agent-serviceaccount")
	return c.testClient.CreateServiceAccount(ctx, name, namespace)
}

// TestTeamReconciler_Pipelines_StatusPatchPrecedesCellPatchAndChildren is
// task 6.8's ordering acceptance: the Team status patch is the durable
// record of intent, so it must precede the cell tenant-list patch and every
// pipelines child write (design D4). Counting each call independently, as
// an earlier test did, cannot catch a regression that reorders them; only
// one shared, ordered log across the fake store and the fake client can.
func TestTeamReconciler_Pipelines_StatusPatchPrecedesCellPatchAndChildren(t *testing.T) {
	client := newTeamTestClient()
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)
	seedCellController(client, nil)

	uid := types.UID("10000000-0000-0000-0000-000000000007")
	teamWith(client, "payments", v1alpha1.TeamSpec{
		Namespaces: []string{"payments-ns"},
		Pipelines: &v1alpha1.TeamPipelinesSpec{
			Profile:        "jenkins-lts",
			AgentNamespace: "payments-ns",
		},
	}, uid)

	var calls []string
	orderedStore := &callOrderBackend{Backend: client.store, calls: &calls}
	orderedClient := &callOrderClient{testClient: client, calls: &calls}

	rec := NewTeamReconciler(orderedClient, orderedStore, teamTestNS, slog.Default())
	rec.SetCellsConfig(CellsConfig{MaxTenantsPerCell: 10})
	rec.SetPipelinesGates("1", "s3://bucket/prefix")
	rec.SetClusterIdentity("core")
	rec.SetPurgeRequester(succeedingPurgeFunc)
	rec.SetSyncAnnouncer(succeedingAnnounceFunc)

	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(calls) < 3 {
		t.Fatalf("expected at least 3 recorded calls (status patch, cell patch, a child write), got %v", calls)
	}
	// The status patch is the durable record of intent, and it must be the
	// very first write: every cell patch and child write below it in
	// TeamReconciler.Reconcile only runs once that patch has succeeded.
	if calls[0] != "team-status-patch" {
		t.Fatalf("expected the Team status patch first, got order %v", calls)
	}
	sawCellPatch, sawChildWrite := false, false
	for _, c := range calls[1:] {
		switch c {
		case "cell-tenant-patch":
			sawCellPatch = true
		case "child-jenkinsrolebinding", "child-agent-serviceaccount":
			sawChildWrite = true
		case "team-status-patch":
			t.Fatalf("expected exactly one Team status patch, got a second one in order %v", calls)
		}
	}
	if !sawCellPatch {
		t.Fatalf("expected the cell tenant-list patch after the status patch, got order %v", calls)
	}
	if !sawChildWrite {
		t.Fatalf("expected at least one pipelines child write after the status patch, got order %v", calls)
	}
}

// TestTeamReconciler_Rehydration_NoPipelinesWritesNothing asserts that a
// Team with no spec.pipelines writes no rehydration status and sets no
// Rehydrated condition.
func TestTeamReconciler_Rehydration_NoPipelinesWritesNothing(t *testing.T) {
	client := newTeamTestClient()
	client.namespaces["team-ns"] = true

	uid := types.UID("00000000-0000-0000-0000-0000000000aa")
	teamWith(client, "no-pipelines", v1alpha1.TeamSpec{
		DisplayName: "No Pipelines",
		Members:     []string{"alice"},
		Namespaces:  []string{"team-ns"},
	}, uid)

	rec := newPipelinesTeamReconciler(client, nil)
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "no-pipelines"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	status := lastTeamStatus(client)
	if status == nil {
		t.Fatal("expected a Team status patch")
	}
	if status.Rehydration != nil {
		t.Errorf("expected no rehydration status, got %+v", status.Rehydration)
	}
	for _, c := range status.Conditions {
		if c.Type == v1alpha1.TeamConditionRehydrated {
			t.Errorf("expected no Rehydrated condition, got %+v", c)
		}
	}
}
