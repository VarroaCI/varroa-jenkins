package controller

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
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
