package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/tenancy"
)

const tenantTestNS = "varroa-system"

// newTenantTestClient returns a testClient pre-populated with a set of VarroaRoles
// needed for the roleRef guardrail tests.
func newTenantTestClient() *testClient {
	tc := newTestClient()
	tc.tenants = make(map[string]*v1alpha1.Tenant)
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

// tenantWith simulates an existing pre-populated tenant in the test client.
// lastTenantStatus returns the most recent Tenant status patch from the store,
// or nil when none was recorded.
func lastTenantStatus(client *testClient) *v1alpha1.TenantStatus {
	gvr, err := crdstore.GVRFor[v1alpha1.Tenant]()
	if err != nil {
		panic(err)
	}
	ps := client.store.StatusPatches(gvr)
	if len(ps) == 0 {
		return nil
	}
	st, _ := ps[len(ps)-1].Status.(*v1alpha1.TenantStatus)
	return st
}

func tenantWith(client *testClient, name string, spec v1alpha1.TenantSpec, uid types.UID) {
	client.tenants[name] = &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  uid,
		},
		Spec: spec,
	}
	crdstore.MustSeed(client.store, client.tenants[name])
}

func TestTenantReconciler_MembersOnly(t *testing.T) {
	// (a) members-only Tenant → owned Group + binding with Group:tenant-<t> subject,
	//     both labels, ownerRef, scope namespaces.
	client := newTenantTestClient()
	client.namespaces["team-ns"] = true

	uid := types.UID("00000000-0000-0000-0000-000000000001")
	tenantWith(client, "my-tenant", v1alpha1.TenantSpec{
		DisplayName: "My Tenant",
		Members:     []string{"alice", "bob"},
		Namespaces:  []string{"team-ns"},
		RoleRef:     "developer",
	}, uid)

	rec := NewTenantReconciler(client, client.store, tenantTestNS, slog.Default())
	_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-tenant"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert Group created.
	var foundGroup *v1alpha1.Group
	for _, g := range client.storeGroups() {
		if g.Name == "tenant-my-tenant" {
			foundGroup = g
			break
		}
	}
	if foundGroup == nil {
		t.Fatal("expected Group tenant-my-tenant to be created")
	}
	if foundGroup.Spec.DisplayName != "My Tenant" {
		t.Errorf("expected DisplayName 'My Tenant', got %q", foundGroup.Spec.DisplayName)
	}
	if len(foundGroup.Spec.Members) != 2 || foundGroup.Spec.Members[0] != "alice" {
		t.Errorf("unexpected members: %v", foundGroup.Spec.Members)
	}
	// Labels
	if foundGroup.Labels[v1alpha1.LabelManagedBy] != "tenant" {
		t.Errorf("expected managed-by label 'tenant', got %q", foundGroup.Labels[v1alpha1.LabelManagedBy])
	}
	if foundGroup.Labels[v1alpha1.LabelTenantName] != "my-tenant" {
		t.Errorf("expected tenant-name label 'my-tenant', got %q", foundGroup.Labels[v1alpha1.LabelTenantName])
	}
	// OwnerRef
	if len(foundGroup.OwnerReferences) != 1 || foundGroup.OwnerReferences[0].UID != uid {
		t.Error("expected owner reference with tenant UID")
	}

	// Assert VarroaRoleBinding created.
	var foundBinding *v1alpha1.VarroaRoleBinding
	for _, b := range client.storeRoleBindings() {
		if b.Name == "tenant-my-tenant" {
			foundBinding = b
			break
		}
	}
	if foundBinding == nil {
		t.Fatal("expected VarroaRoleBinding tenant-my-tenant to be created")
	}
	// Subject: Group:tenant-my-tenant
	if len(foundBinding.Spec.Subjects) != 1 {
		t.Fatalf("expected 1 subject, got %d", len(foundBinding.Spec.Subjects))
	}
	if foundBinding.Spec.Subjects[0].Kind != "Group" || foundBinding.Spec.Subjects[0].Name != "tenant-my-tenant" {
		t.Errorf("expected subject Group:tenant-my-tenant, got %s:%s",
			foundBinding.Spec.Subjects[0].Kind, foundBinding.Spec.Subjects[0].Name)
	}
	if foundBinding.Spec.RoleRef != "developer" {
		t.Errorf("expected roleRef 'developer', got %q", foundBinding.Spec.RoleRef)
	}
	if foundBinding.Spec.Scope == nil || len(foundBinding.Spec.Scope.Namespaces) != 1 || foundBinding.Spec.Scope.Namespaces[0] != "team-ns" {
		t.Errorf("unexpected scope: %+v", foundBinding.Spec.Scope)
	}
	// Labels + OwnerRef on binding
	if foundBinding.Labels[v1alpha1.LabelManagedBy] != "tenant" {
		t.Errorf("expected managed-by label on binding")
	}
	if len(foundBinding.OwnerReferences) != 1 || foundBinding.OwnerReferences[0].UID != uid {
		t.Error("expected owner reference on binding")
	}

	// Check status
	if lastTenantStatus(client) == nil {
		t.Fatal("expected at least one status patch")
	}
	lastStatus := lastTenantStatus(client)
	if lastStatus.GroupRef != "tenant-my-tenant" {
		t.Errorf("expected groupRef 'tenant-my-tenant', got %q", lastStatus.GroupRef)
	}
	if lastStatus.BindingRef != "tenant-my-tenant" {
		t.Errorf("expected bindingRef 'tenant-my-tenant', got %q", lastStatus.BindingRef)
	}
}

func TestTenantReconciler_SubjectsOnly(t *testing.T) {
	// (b) subjects-only Tenant → binding with pass-through subjects, no Group.
	client := newTenantTestClient()
	client.namespaces["team-ns"] = true

	uid := types.UID("00000000-0000-0000-0000-000000000002")
	tenantWith(client, "sub-tenant", v1alpha1.TenantSpec{
		Subjects:   []v1alpha1.SubjectRef{{Kind: "Group", Name: "idp-admins"}},
		Namespaces: []string{"team-ns"},
		RoleRef:    "developer",
	}, uid)

	rec := NewTenantReconciler(client, client.store, tenantTestNS, slog.Default())
	_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "sub-tenant"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No group should be created for members-only.
	for _, g := range client.storeGroups() {
		if g.Name == "tenant-sub-tenant" {
			t.Fatal("expected NO Group tenant-sub-tenant for subjects-only tenant")
		}
	}

	// Binding should have pass-through subjects (no Group prepended).
	var foundBinding *v1alpha1.VarroaRoleBinding
	for _, b := range client.storeRoleBindings() {
		if b.Name == "tenant-sub-tenant" {
			foundBinding = b
			break
		}
	}
	if foundBinding == nil {
		t.Fatal("expected VarroaRoleBinding tenant-sub-tenant")
	}
	if len(foundBinding.Spec.Subjects) != 1 {
		t.Fatalf("expected 1 subject, got %d", len(foundBinding.Spec.Subjects))
	}
	if foundBinding.Spec.Subjects[0].Kind != "Group" || foundBinding.Spec.Subjects[0].Name != "idp-admins" {
		t.Errorf("expected subject Group:idp-admins, got %s:%s",
			foundBinding.Spec.Subjects[0].Kind, foundBinding.Spec.Subjects[0].Name)
	}
}

func TestTenantReconciler_AdminRoleRefBlocked(t *testing.T) {
	// (c) roleRef: admin → TenantReady=False/InvalidRoleRef, no binding applied.
	client := newTenantTestClient()
	client.namespaces["team-ns"] = true

	uid := types.UID("00000000-0000-0000-0000-000000000003")
	tenantWith(client, "admin-tenant", v1alpha1.TenantSpec{
		Members:    []string{"admin-wannabe"},
		Namespaces: []string{"team-ns"},
		RoleRef:    "admin",
	}, uid)

	rec := NewTenantReconciler(client, client.store, tenantTestNS, slog.Default())
	_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "admin-tenant"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No binding should be applied.
	for _, b := range client.storeRoleBindings() {
		if b.Name == "team-admin-tenant" {
			t.Fatal("expected NO binding for admin roleRef")
		}
	}

	// Status should show InvalidRoleRef.
	if lastTenantStatus(client) == nil {
		t.Fatal("expected status patch")
	}
	lastStatus := lastTenantStatus(client)
	found := false
	for _, c := range lastStatus.Conditions {
		if c.Type == v1alpha1.TenantConditionReady && c.Reason == v1alpha1.TenantReasonInvalidRoleRef {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected TenantReady condition with InvalidRoleRef reason")
	}
}

func TestTenantReconciler_CollisionGuard(t *testing.T) {
	// (e) collision: pre-existing unowned tenant-<t> Group → TenantRBACReady=False/GroupApplyFailed, no overwrite.
	client := newTenantTestClient()
	client.namespaces["team-ns"] = true

	// Pre-create an unowned Group with the tenant prefix name.
	crdstore.MustSeed(client.store, &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tenant-collision-tenant",
		},
		Spec: v1alpha1.GroupSpec{
			Members: []string{"hand-authored"},
		},
	})

	uid := types.UID("00000000-0000-0000-0000-000000000004")
	tenantWith(client, "collision-tenant", v1alpha1.TenantSpec{
		Members:    []string{"new-member"},
		Namespaces: []string{"team-ns"},
		RoleRef:    "developer",
	}, uid)

	// Ensure the group list is as we set it, then count.
	initialGroupCount := len(client.storeGroups())

	rec := NewTenantReconciler(client, client.store, tenantTestNS, slog.Default())
	_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "collision-tenant"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The pre-existing group should NOT have been overwritten.
	if len(client.storeGroups()) != initialGroupCount {
		t.Errorf("expected %d groups (unchanged), got %d", initialGroupCount, len(client.storeGroups()))
	}

	// Status should show GroupApplyFailed.
	lastStatus := lastTenantStatus(client)
	foundRBAC := false
	foundReady := false
	for _, c := range lastStatus.Conditions {
		if c.Type == v1alpha1.TenantConditionRBACReady && c.Reason == v1alpha1.TenantReasonGroupApplyFailed {
			foundRBAC = true
		}
		if c.Type == v1alpha1.TenantConditionReady && c.Reason == v1alpha1.TenantReasonChildApplyFailed {
			foundReady = true
		}
	}
	if !foundRBAC {
		t.Error("expected TenantRBACReady condition with GroupApplyFailed reason")
	}
	if !foundReady {
		t.Error("expected TenantReady condition with ChildApplyFailed reason")
	}
}

func TestTenantReconciler_MembersRemoved(t *testing.T) {
	// (f) members removed on update → owned Group deleted, binding subjects updated.
	client := newTenantTestClient()
	client.namespaces["team-ns"] = true

	uid := types.UID("00000000-0000-0000-0000-000000000005")

	// Simulate a previous reconcile: create the owned Group first.
	crdstore.MustSeed(client.store, &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tenant-updated-tenant",
			Labels: map[string]string{
				v1alpha1.LabelManagedBy:  "tenant",
				v1alpha1.LabelTenantName: "updated-tenant",
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
	tenantWith(client, "updated-tenant", v1alpha1.TenantSpec{
		Subjects:   []v1alpha1.SubjectRef{{Kind: "User", Name: "direct-user"}},
		Namespaces: []string{"team-ns"},
		RoleRef:    "developer",
	}, uid)

	rec := NewTenantReconciler(client, client.store, tenantTestNS, slog.Default())
	_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "updated-tenant"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Group should be deleted.
	for _, g := range client.storeGroups() {
		if g.Name == "tenant-updated-tenant" {
			t.Error("expected Group tenant-updated-tenant to be deleted after members removed")
		}
	}

	// Binding should have only the direct user subject (no Group subject).
	var foundBinding *v1alpha1.VarroaRoleBinding
	for _, b := range client.storeRoleBindings() {
		if b.Name == "tenant-updated-tenant" {
			foundBinding = b
			break
		}
	}
	if foundBinding == nil {
		t.Fatal("expected VarroaRoleBinding tenant-updated-tenant")
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

func TestTenantReconciler_NamespaceStates(t *testing.T) {
	// (g) stub namespace states → correct TenantNamespacesReady reason + namespaceStates.
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
			expectState:  v1alpha1.TenantNamespaceStateManaged,
			expectReason: v1alpha1.TenantReasonNamespacesSatisfied,
			expectReady:  true,
		},
		{
			name:         "missing-ns",
			ns:           "missing-ns",
			nsExists:     false,
			provision:    false,
			expectState:  v1alpha1.TenantNamespaceStateMissing,
			expectReason: v1alpha1.TenantReasonNamespaceMissing,
			expectReady:  false,
		},
		{
			name:         "created-via-provision",
			ns:           "created-ns",
			nsExists:     false,
			provision:    true,
			expectState:  v1alpha1.TenantNamespaceStateCreated,
			expectReason: v1alpha1.TenantReasonNamespacesSatisfied,
			expectReady:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTenantTestClient()
			if tt.nsExists {
				client.namespaces[tt.ns] = true
			}

			uid := types.UID("00000000-0000-0000-0000-000000000006")
			spec := v1alpha1.TenantSpec{
				Members:    []string{"test-user"},
				Namespaces: []string{tt.ns},
				RoleRef:    "developer",
			}
			if tt.provision {
				spec.ProvisionNamespaces = true
			}

			tenantWith(client, "ns-tenant", spec, uid)

			rec := NewTenantReconciler(client, client.store, tenantTestNS, slog.Default())
			_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "ns-tenant"}})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			lastStatus := lastTenantStatus(client)

			// Check namespace state.
			var nsState *v1alpha1.TenantNamespaceState
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

			// Check TenantNamespacesReady condition.
			foundNSCond := false
			foundReadyCond := false
			for _, c := range lastStatus.Conditions {
				if c.Type == v1alpha1.TenantConditionNamespacesReady {
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
				if c.Type == v1alpha1.TenantConditionReady {
					foundReadyCond = true
					if tt.expectReady && c.Status != metav1.ConditionTrue {
						t.Errorf("expected TenantReady=True, got %s", c.Status)
					}
					if !tt.expectReady && c.Status != metav1.ConditionFalse {
						t.Errorf("expected TenantReady=False, got %s", c.Status)
					}
				}
			}
			if !foundNSCond {
				t.Error("expected TenantNamespacesReady condition")
			}
			if !foundReadyCond {
				t.Error("expected TenantReady condition")
			}

			// Precheck path (provision == false) must never sync image pull secrets.
			if !tt.provision && len(client.imagePullSecretCopies) > 0 {
				t.Errorf("expected no imagePullSecretCopies on precheck path, got %d", len(client.imagePullSecretCopies))
			}
		})
	}
}

func TestTenantReconciler_UnmanagedNamespaceScopedMode(t *testing.T) {
	// C1 regression: with a scoped managed set injected, an existing namespace
	// OUTSIDE the set must surface as Unmanaged (NamespacesReady=False), for
	// both the precheck and the provisioning path.
	for _, provision := range []bool{false, true} {
		name := "precheck"
		if provision {
			name = "provision"
		}
		t.Run(name, func(t *testing.T) {
			client := newTenantTestClient()
			client.namespaces["outside-ns"] = true

			uid := types.UID("00000000-0000-0000-0000-000000000007")
			spec := v1alpha1.TenantSpec{
				Members:             []string{"test-user"},
				Namespaces:          []string{"outside-ns"},
				RoleRef:             "developer",
				ProvisionNamespaces: provision,
			}
			tenantWith(client, "ns-tenant-scoped", spec, uid)

			rec := NewTenantReconciler(client, client.store, tenantTestNS, slog.Default())
			rec.SetManagedSet(tenancy.NewManagedSet("managed-a,managed-b", tenantTestNS))
			_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "ns-tenant-scoped"}})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			lastStatus := lastTenantStatus(client)
			var nsState *v1alpha1.TenantNamespaceState
			for i, s := range lastStatus.NamespaceStates {
				if s.Name == "outside-ns" {
					nsState = &lastStatus.NamespaceStates[i]
					break
				}
			}
			if nsState == nil {
				t.Fatal("expected namespace state for outside-ns")
			}
			if nsState.State != v1alpha1.TenantNamespaceStateUnmanaged {
				t.Errorf("expected state %q, got %q", v1alpha1.TenantNamespaceStateUnmanaged, nsState.State)
			}
			for _, c := range lastStatus.Conditions {
				if c.Type == v1alpha1.TenantConditionNamespacesReady {
					if c.Status != metav1.ConditionFalse {
						t.Errorf("expected NamespacesReady=False, got %s", c.Status)
					}
					if c.Reason != v1alpha1.TenantReasonNamespaceUnmanaged {
						t.Errorf("expected reason %q, got %q", v1alpha1.TenantReasonNamespaceUnmanaged, c.Reason)
					}
				}
			}
		})
	}
}

func TestTenantReconciler_SyncsImagePullSecretsOnProvision(t *testing.T) {
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
			client := newTenantTestClient()
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
			spec := v1alpha1.TenantSpec{
				Members:             []string{"test-user"},
				Namespaces:          []string{"created-ns"},
				RoleRef:             "developer",
				ProvisionNamespaces: tt.provision,
			}
			tenantWith(client, "pull-secret-tenant", spec, uid)

			rec := NewTenantReconciler(client, client.store, tenantTestNS, slog.Default())
			_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "pull-secret-tenant"}})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(client.imagePullSecretCopies) != tt.wantCopies {
				t.Errorf("expected %d imagePullSecretCopies, got %d", tt.wantCopies, len(client.imagePullSecretCopies))
			}
			if tt.wantCopies > 0 {
				cp := client.imagePullSecretCopies[0]
				if cp.src != tenantTestNS {
					t.Errorf("expected src=%q, got %q", tenantTestNS, cp.src)
				}
				if cp.dst != "created-ns" {
					t.Errorf("expected dst=%q, got %q", "created-ns", cp.dst)
				}
				if cp.name != "registry-creds" {
					t.Errorf("expected name=%q, got %q", "registry-creds", cp.name)
				}
			}

			lastStatus := lastTenantStatus(client)
			var ready bool
			for _, c := range lastStatus.Conditions {
				if c.Type == v1alpha1.TenantConditionReady {
					ready = c.Status == metav1.ConditionTrue
				}
			}
			if ready != tt.wantReady {
				t.Errorf("expected TenantReady=%v, got %v", tt.wantReady, ready)
			}
		})
	}
}

func TestTenantReconciler_MissingRoleRef(t *testing.T) {
	// (d) missing roleRef VarroaRole → InvalidRoleRef.
	client := newTenantTestClient()
	client.namespaces["team-ns"] = true

	uid := types.UID("00000000-0000-0000-0000-000000000007")
	tenantWith(client, "badrole-tenant", v1alpha1.TenantSpec{
		Members:    []string{"user"},
		Namespaces: []string{"team-ns"},
		RoleRef:    "nonexistent-role",
	}, uid)

	rec := NewTenantReconciler(client, client.store, tenantTestNS, slog.Default())
	_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "badrole-tenant"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	lastStatus := lastTenantStatus(client)
	found := false
	for _, c := range lastStatus.Conditions {
		if c.Type == v1alpha1.TenantConditionReady && c.Reason == v1alpha1.TenantReasonInvalidRoleRef {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected TenantReady condition with InvalidRoleRef reason for missing VarroaRole")
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

// newPipelinesTenantReconciler builds a TenantReconciler with every pipelines
// setter wired, so refusal gates 1 and 2 pass and a purge always succeeds
// unless the test overrides it.
func newPipelinesTenantReconciler(client *testClient, sink *recordingSink) *TenantReconciler {
	rec := NewTenantReconciler(client, client.store, tenantTestNS, slog.Default())
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

func TestTenantReconciler_Pipelines_PlacesTenantWithoutPipelinesRoleBinding(t *testing.T) {
	client := newTenantTestClient()
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)
	seedCellController(client, nil)

	uid := types.UID("10000000-0000-0000-0000-000000000001")
	tenantWith(client, "payments", v1alpha1.TenantSpec{
		Members:    []string{"alice"},
		Namespaces: []string{"payments-ns"},
		Pipelines: &v1alpha1.TenantPipelinesSpec{
			Profile:        "jenkins-lts",
			AgentNamespace: "payments-ns",
		},
	}, uid)

	sink := &recordingSink{}
	rec := newPipelinesTenantReconciler(client, sink)
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	status := lastTenantStatus(client)
	if status == nil || status.Placement == nil || status.Placement.Cell != "cell-a" || status.Placement.CellNamespace != "cells-ns" || status.Placement.Cluster != "core" {
		t.Fatalf("expected a placement on cell-a, got %+v", status)
	}

	// task 6.8: exactly one PatchStatus[Tenant] call per tick.
	gvr, err := crdstore.GVRFor[v1alpha1.Tenant]()
	if err != nil {
		t.Fatalf("GVRFor failed: %v", err)
	}
	if n := len(client.store.StatusPatches(gvr)); n != 1 {
		t.Errorf("expected exactly one Tenant status patch, got %d", n)
	}

	// task 6.6: the cell patch is applied after the status patch.
	if len(client.cellStatusPatches) != 1 {
		t.Fatalf("expected one cell status patch, got %d", len(client.cellStatusPatches))
	}
	if got := client.cellStatusPatches[0]; got.name != "cell-a" || len(got.cell.Tenants) != 1 || got.cell.Tenants[0] != "payments" {
		t.Errorf("unexpected cell status patch: %+v", got)
	}

	// The placement pass writes no pipelines JenkinsRoleBinding. Human
	// access comes from the generated member role.
	_, err = crdstore.Get[v1alpha1.JenkinsRoleBinding](context.Background(), client.store, "team-payments-pipelines", "")
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected no pipelines JenkinsRoleBinding, got err %v", err)
	}

	// task 6.12: tenant.placed is published.
	if len(sink.events) != 1 || sink.events[0].Type != "tenant.placed" {
		t.Errorf("expected one tenant.placed event, got %+v", sink.events)
	}
}

// TestTenantReconciler_Pipelines_PoolListFailureFailsClosed asserts the pass
// fails closed when the CellPool list fails. An empty pool set makes every
// effective-owner lookup answer nil, so a cell owned by a live single-tenant
// pool would look unowned and admit a default-pool tenant. The tick must
// therefore write no placement and return the error for a requeue.
func TestTenantReconciler_Pipelines_PoolListFailureFailsClosed(t *testing.T) {
	client := newTenantTestClient()
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)

	// A free cell owned by a single-tenant pool. With the pool set intact it
	// is no candidate for a tenant that names no pool.
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cell-solo-0",
			Namespace: "cells-ns",
			Labels: map[string]string{
				cells.LabelCellProfile: "jenkins-lts",
				cells.LabelCellPool:    "solo",
			},
		},
		Spec: v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
		Status: v1alpha1.ControllerStatus{
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionCellReady, Status: metav1.ConditionTrue}},
		},
	}
	crdstore.MustSeed(client.store, cr)
	crdstore.MustSeed(client.store, &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "solo"},
		Spec:       v1alpha1.CellPoolSpec{ProfileRef: "jenkins-lts", MaxTenantsPerCell: 1},
	})

	uid := types.UID("10000000-0000-0000-0000-00000000000f")
	tenantWith(client, "payments", v1alpha1.TenantSpec{
		Members:    []string{"alice"},
		Namespaces: []string{"payments-ns"},
		Pipelines: &v1alpha1.TenantPipelinesSpec{
			Profile:        "jenkins-lts",
			AgentNamespace: "payments-ns",
		},
	}, uid)

	poolGVR, err := crdstore.GVRFor[v1alpha1.CellPool]()
	if err != nil {
		t.Fatalf("GVRFor[CellPool]: %v", err)
	}
	client.store.FailAlways("list", poolGVR, fmt.Errorf("apiserver unavailable"))

	rec := newPipelinesTenantReconciler(client, nil)
	_, err = rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}})
	if err == nil {
		t.Fatal("a CellPool list failure must return an error so the tick requeues")
	}
	if !strings.Contains(err.Error(), "apiserver unavailable") {
		t.Errorf("error = %v, want the list failure", err)
	}

	if status := lastTenantStatus(client); status != nil && status.Placement != nil {
		t.Fatalf("a failed pool list must write no placement, got %+v", status.Placement)
	}
	if len(client.cellStatusPatches) != 0 {
		t.Errorf("a failed pool list must patch no cell, got %+v", client.cellStatusPatches)
	}
}

// TestTenantReconciler_Pipelines_ManuallyCreatedCellResolvesProfileFromVersion
// asserts buildCellCandidate's fallback: a ready cell with no warm-pool
// label (cells.LabelCellProfile) still resolves a profile, through
// ResolveProfile against the cell's spec.version, and becomes a placement
// candidate.
func TestTenantReconciler_Pipelines_ManuallyCreatedCellResolvesProfileFromVersion(t *testing.T) {
	client := newTenantTestClient()
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
	tenantWith(client, "payments", v1alpha1.TenantSpec{
		Members:    []string{"alice"},
		Namespaces: []string{"payments-ns"},
		Pipelines: &v1alpha1.TenantPipelinesSpec{
			Profile:        "jenkins-lts",
			AgentNamespace: "payments-ns",
		},
	}, uid)

	rec := newPipelinesTenantReconciler(client, nil)
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	status := lastTenantStatus(client)
	if status == nil || status.Placement == nil || status.Placement.Cell != "cell-manual" {
		t.Fatalf("expected the label-less cell to be selected as a candidate, got %+v", status)
	}
}

func TestTenantReconciler_Pipelines_TeardownDeletesChildrenAndHoldsFinalizer(t *testing.T) {
	client := newTenantTestClient()
	client.namespaces["payments-ns"] = true
	// The cell still applies the tenant's folder: the drain has not
	// finished, so the finalizer must hold.
	seedCellController(client, []string{"payments"})

	uid := types.UID("10000000-0000-0000-0000-000000000002")
	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "payments",
			UID:        uid,
			Finalizers: []string{v1alpha1.TenantFinalizerTenantPlacement},
		},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"payments-ns"},
			// spec.pipelines removed: teardown starts.
		},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{Cluster: "core", Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
			Pipelines: &v1alpha1.TenantPipelinesStatus{WebhookToken: "tok", WebhookSecret: "sec", AgentNamespace: "payments-ns"},
		},
	}
	client.tenants["payments"] = tenant
	crdstore.MustSeed(client.store, tenant)

	rec := newPipelinesTenantReconciler(client, nil)
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	status := lastTenantStatus(client)
	if status == nil || status.Placement == nil {
		t.Fatal("the finalizer holds before the cell applies, so placement must stay set")
	}
	placedCond := false
	for _, c := range status.Conditions {
		if c.Type == v1alpha1.TenantConditionPlaced && c.Reason == v1alpha1.TenantReasonTenantTeardownPending {
			placedCond = true
		}
	}
	if !placedCond {
		t.Errorf("expected Placed=False/TenantTeardownPending, got %+v", status.Conditions)
	}

	// The three teardown-stage-one deletions that remain: the tenant dropped
	// from the cell's tenant list, the ServiceAccount, and its token Secret.
	if len(client.cellStatusPatches) != 1 || len(client.cellStatusPatches[0].cell.Tenants) != 0 {
		t.Errorf("expected the tenant dropped from the cell's tenant list: %+v", client.cellStatusPatches)
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

// TestTenantReconciler_Pipelines_PurgeAttemptsPersistAcrossReconcilerRestarts
// is task 6.10's persistence acceptance: status.pipelines.purgeAttempts
// round-trips through the fake store, not just through an in-memory
// *TenantStatus reused across calls. Two Reconcile calls, each on a fresh
// TenantReconciler instance reading the Tenant back from the store, stand in
// for an operator restart between purge retries. A restart must not lose
// the count: cells.Place bounds the retry at ten failed attempts, and a
// count reset by every restart would hold the finalizer indefinitely on a
// recorder that stays down.
func TestTenantReconciler_Pipelines_PurgeAttemptsPersistAcrossReconcilerRestarts(t *testing.T) {
	client := newTenantTestClient()
	client.namespaces["payments-ns"] = true
	// A drained cell: the cell no longer applies the tenant's folder, so
	// teardown reaches the purge step on every tick instead of holding on
	// TenantTeardownPending for the drain.
	seedCellController(client, nil)

	uid := types.UID("10000000-0000-0000-0000-000000000005")
	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "payments",
			UID:        uid,
			Finalizers: []string{v1alpha1.TenantFinalizerTenantPlacement},
		},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"payments-ns"},
			// spec.pipelines removed: teardown is in progress.
		},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{Cluster: "core", Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
			Pipelines: &v1alpha1.TenantPipelinesStatus{WebhookToken: "tok", WebhookSecret: "sec", AgentNamespace: "payments-ns"},
		},
	}
	client.tenants["payments"] = tenant
	crdstore.MustSeed(client.store, tenant)

	failingPurge := func(context.Context, cells.PurgeRequest) (bool, error) {
		return false, fmt.Errorf("recorder unreachable")
	}

	rec1 := newPipelinesTenantReconciler(client, nil)
	rec1.SetPurgeRequester(failingPurge)
	if _, err := rec1.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	status1 := lastTenantStatus(client)
	if status1 == nil || status1.Pipelines == nil || status1.Pipelines.PurgeAttempts != 1 {
		t.Fatalf("after first Reconcile: purgeAttempts = %+v, want 1", status1)
	}
	if status1.Placement == nil {
		t.Fatal("the finalizer must still hold after one failed purge attempt")
	}

	// A fresh TenantReconciler instance reading the Tenant back through the
	// store, standing in for an operator restart between retries.
	rec2 := newPipelinesTenantReconciler(client, nil)
	rec2.SetPurgeRequester(failingPurge)
	if _, err := rec2.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	status2 := lastTenantStatus(client)
	if status2 == nil || status2.Pipelines == nil || status2.Pipelines.PurgeAttempts != 2 {
		t.Fatalf("after second Reconcile (fresh reconciler instance): purgeAttempts = %+v, want 2 — the count must persist across a restart, not reset to 1", status2)
	}
}

func TestTenantReconciler_Pipelines_TeardownReleasesFinalizerWhenDrained(t *testing.T) {
	client := newTenantTestClient()
	client.namespaces["payments-ns"] = true
	// The cell no longer applies the tenant's folder: the drain finished.
	seedCellController(client, nil)

	uid := types.UID("10000000-0000-0000-0000-000000000003")
	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "payments",
			UID:        uid,
			Finalizers: []string{v1alpha1.TenantFinalizerTenantPlacement},
		},
		Spec: v1alpha1.TenantSpec{Namespaces: []string{"payments-ns"}},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{Cluster: "core", Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
			Pipelines: &v1alpha1.TenantPipelinesStatus{WebhookToken: "tok", WebhookSecret: "sec", AgentNamespace: "payments-ns"},
		},
	}
	client.tenants["payments"] = tenant
	crdstore.MustSeed(client.store, tenant)

	rec := newPipelinesTenantReconciler(client, nil)
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	status := lastTenantStatus(client)
	if status == nil || status.Placement != nil {
		t.Fatalf("expected placement cleared once drained, got %+v", status)
	}
	tenantGVR, err := crdstore.GVRFor[v1alpha1.Tenant]()
	if err != nil {
		t.Fatalf("GVRFor failed: %v", err)
	}
	metaPatches := client.store.MetaPatches(tenantGVR)
	released := false
	for _, mp := range metaPatches {
		if mp.Name != "payments" {
			continue
		}
		if finalizers, ok := mp.Meta["finalizers"].([]string); ok {
			released = true
			for _, f := range finalizers {
				if f == v1alpha1.TenantFinalizerTenantPlacement {
					released = false
				}
			}
		}
	}
	if !released {
		t.Errorf("expected the finalizer released, meta patches: %+v", metaPatches)
	}
}

func TestTenantReconciler_Pipelines_TenantWarningsConditionFolded(t *testing.T) {
	client := newTenantTestClient()
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)
	seedCellController(client, []string{"payments"})
	// Fold in a warning the controller reconciler recorded for this tenant.
	cell, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store, "cell-a", "cells-ns")
	if err != nil {
		t.Fatalf("get cell: %v", err)
	}
	cell.Status.Cell.TenantWarnings = []v1alpha1.TenantWarning{
		{Tenant: "payments", Reason: v1alpha1.TenantReasonCredentialSkipped, Message: "duplicate credential id"},
		{Tenant: "other-team", Reason: v1alpha1.TenantReasonBundleKeysIgnored, Message: "not this tenant"},
	}
	crdstore.MustSeed(client.store, cell)

	uid := types.UID("10000000-0000-0000-0000-000000000004")
	tenantWith(client, "payments", v1alpha1.TenantSpec{
		Members:    []string{"alice"},
		Namespaces: []string{"payments-ns"},
		Pipelines: &v1alpha1.TenantPipelinesSpec{
			Profile:        "jenkins-lts",
			AgentNamespace: "payments-ns",
		},
	}, uid)

	rec := newPipelinesTenantReconciler(client, nil)
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	status := lastTenantStatus(client)
	var found *v1alpha1.TenantCondition
	for i := range status.Conditions {
		if status.Conditions[i].Type == v1alpha1.TenantConditionTenantWarnings {
			found = &status.Conditions[i]
		}
	}
	if found == nil {
		t.Fatal("expected a TenantWarnings condition")
	}
	if found.Status != metav1.ConditionTrue || found.Reason != v1alpha1.TenantReasonCredentialSkipped {
		t.Errorf("unexpected TenantWarnings condition: %+v", found)
	}
	if strings.Contains(found.Message, "other-team") {
		t.Errorf("expected only this Tenant's own warnings folded in, got message %q", found.Message)
	}
}

// TestTenantReconciler_Pipelines_CreatesTenantAgentIdentity is task 8.9's
// acceptance: the placement pass creates the ServiceAccount, its token
// Secret, and the Role and RoleBinding CreateAgentRBAC builds, in
// agentNamespace, all named varroa-agent-<tenant>[-token].
func TestTenantReconciler_Pipelines_CreatesTenantAgentIdentity(t *testing.T) {
	client := newTenantTestClient()
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)
	seedCellController(client, nil)

	uid := types.UID("10000000-0000-0000-0000-000000000005")
	tenantWith(client, "payments", v1alpha1.TenantSpec{
		Namespaces: []string{"payments-ns"},
		Pipelines: &v1alpha1.TenantPipelinesSpec{
			Profile:        "jenkins-lts",
			AgentNamespace: "payments-ns",
		},
	}, uid)

	rec := newPipelinesTenantReconciler(client, nil)
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

// TestTenantReconciler_Pipelines_TeardownWaitCreatesNothing is task 8.9's
// second acceptance clause: a tick between the two teardown stages (the
// tenant already dropped from the cell's list, the finalizer still held)
// creates none of the tenant agent identity objects — the placement pass is
// the one owner, and it never recreates what teardown removed.
func TestTenantReconciler_Pipelines_TeardownWaitCreatesNothing(t *testing.T) {
	client := newTenantTestClient()
	client.namespaces["payments-ns"] = true
	// The cell still applies the tenant's folder: the drain has not
	// finished, so teardown stays in the between-stages wait.
	seedCellController(client, []string{"payments"})

	uid := types.UID("10000000-0000-0000-0000-000000000006")
	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "payments",
			UID:        uid,
			Finalizers: []string{v1alpha1.TenantFinalizerTenantPlacement},
		},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"payments-ns"},
			// spec.pipelines removed: teardown started on an earlier tick.
		},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{Cluster: "core", Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
			Pipelines: &v1alpha1.TenantPipelinesStatus{WebhookToken: "tok", WebhookSecret: "sec", AgentNamespace: "payments-ns"},
		},
	}
	client.tenants["payments"] = tenant
	crdstore.MustSeed(client.store, tenant)

	rec := newPipelinesTenantReconciler(client, nil)
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
// ordered call log every time it patches a Tenant's status or a cell's status. It shares the log with
// callOrderClient below, so the two together record one call order across
// the fake store and the fake client.
type callOrderBackend struct {
	crdstore.Backend
	calls *[]string
}

func (b *callOrderBackend) PatchObjectStatus(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string, status any) error {
	if tenantGVR, _ := crdstore.GVRFor[v1alpha1.Tenant](); gvr == tenantGVR {
		*b.calls = append(*b.calls, "tenant-status-patch")
	}
	if ctrlGVR, _ := crdstore.GVRFor[v1alpha1.Controller](); gvr == ctrlGVR {
		*b.calls = append(*b.calls, "cell-claim-patch")
	}
	return b.Backend.PatchObjectStatus(ctx, gvr, namespace, name, status)
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

// TestTenantReconciler_Pipelines_StatusPatchPrecedesCellPatchAndChildren is
// task 6.8's ordering acceptance: the Tenant status patch is the durable
// record of intent, so it must precede the cell tenant-list patch and every
// pipelines child write (design D4). Counting each call independently, as
// an earlier test did, cannot catch a regression that reorders them; only
// one shared, ordered log across the fake store and the fake client can.
func TestTenantReconciler_Pipelines_StatusPatchPrecedesCellPatchAndChildren(t *testing.T) {
	client := newTenantTestClient()
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)
	seedCellController(client, nil)

	uid := types.UID("10000000-0000-0000-0000-000000000007")
	tenantWith(client, "payments", v1alpha1.TenantSpec{
		Namespaces: []string{"payments-ns"},
		Pipelines: &v1alpha1.TenantPipelinesSpec{
			Profile:        "jenkins-lts",
			AgentNamespace: "payments-ns",
		},
	}, uid)

	var calls []string
	orderedStore := &callOrderBackend{Backend: client.store, calls: &calls}
	orderedClient := &callOrderClient{testClient: client, calls: &calls}

	rec := NewTenantReconciler(orderedClient, orderedStore, tenantTestNS, slog.Default())
	rec.SetCellsConfig(CellsConfig{MaxTenantsPerCell: 10})
	rec.SetPipelinesGates("1", "s3://bucket/prefix")
	rec.SetClusterIdentity("core")
	rec.SetPurgeRequester(succeedingPurgeFunc)
	rec.SetSyncAnnouncer(succeedingAnnounceFunc)

	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(calls) < 4 {
		t.Fatalf("expected at least 4 recorded calls (claim, status patch, cell patch, a child write), got %v", calls)
	}
	// The claim is the first write of a new placement. It must precede the
	// Tenant status patch, or a pool drain can delete the chosen cell in the
	// window between the two.
	if calls[0] != "cell-claim-patch" {
		t.Fatalf("expected the cell claim first, got order %v", calls)
	}
	// The status patch is the durable record of intent, and every cell
	// tenant patch and child write below it in TenantReconciler.Reconcile
	// only runs once that patch has succeeded.
	if calls[1] != "tenant-status-patch" {
		t.Fatalf("expected the Tenant status patch after the claim, got order %v", calls)
	}
	sawCellPatch, sawChildWrite := false, false
	for _, c := range calls[2:] {
		switch c {
		case "cell-tenant-patch":
			sawCellPatch = true
		case "child-agent-serviceaccount":
			sawChildWrite = true
		case "tenant-status-patch":
			t.Fatalf("expected exactly one Tenant status patch, got a second one in order %v", calls)
		}
	}
	if !sawCellPatch {
		t.Fatalf("expected the cell tenant-list patch after the status patch, got order %v", calls)
	}
	if !sawChildWrite {
		t.Fatalf("expected at least one pipelines child write after the status patch, got order %v", calls)
	}
}

// failingCellPatchClient refuses every cell tenant-list patch, the shape of
// a cell whose status patch stays refused over several ticks.
// failingCellPatchClient refuses the first refusals cell status patches
// and records every later one on the embedded test client.
type failingCellPatchClient struct {
	*testClient
	refusals int
	calls    int
}

func (c *failingCellPatchClient) PatchControllerCellStatus(ctx context.Context, name, namespace string, cell *v1alpha1.CellStatus) error {
	c.calls++
	if c.calls <= c.refusals {
		return fmt.Errorf("the cell status patch is refused")
	}
	return c.testClient.PatchControllerCellStatus(ctx, name, namespace, cell)
}

// countClaimPatches answers how many Controller status patches carried a
// non-empty status.cell.pendingTenants list.
func countClaimPatches(t *testing.T, client *testClient) int {
	t.Helper()
	gvr, err := crdstore.GVRFor[v1alpha1.Controller]()
	if err != nil {
		t.Fatalf("GVRFor[Controller]: %v", err)
	}
	n := 0
	for _, patch := range client.store.StatusPatches(gvr) {
		body, ok := patch.Status.(map[string]any)
		if !ok {
			continue
		}
		cell, ok := body["cell"].(map[string]any)
		if !ok {
			continue
		}
		claims, ok := cell["pendingTenants"].([]v1alpha1.CellPendingTenant)
		if ok && len(claims) > 0 {
			n++
		}
	}
	return n
}

// TestTenantReconciler_Pipelines_ClaimSurvivesAFailingCellPatch covers the
// second interleaving. A tenant whose cell tenant-list patch stays refused
// holds a fence and no tenant entry. Every tick must write its claim again
// and send the tenant patch again, or the claim expires and the pool drain
// deletes the cell the tenant is fenced to. The tick whose patch lands
// retires the claim.
func TestTenantReconciler_Pipelines_ClaimSurvivesAFailingCellPatch(t *testing.T) {
	base := newTenantTestClient()
	base.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(base)
	seedCellController(base, nil)

	uid := types.UID("10000000-0000-0000-0000-000000000009")
	tenantWith(base, "payments", v1alpha1.TenantSpec{
		Namespaces: []string{"payments-ns"},
		Pipelines: &v1alpha1.TenantPipelinesSpec{
			Profile:        "jenkins-lts",
			AgentNamespace: "payments-ns",
		},
	}, uid)

	client := &failingCellPatchClient{testClient: base, refusals: 2}
	rec := NewTenantReconciler(client, base.store, tenantTestNS, slog.Default())
	rec.SetCellsConfig(CellsConfig{MaxTenantsPerCell: 10})
	rec.SetPipelinesGates("1", "s3://bucket/prefix")
	rec.SetClusterIdentity("core")
	rec.SetPurgeRequester(succeedingPurgeFunc)
	rec.SetSyncAnnouncer(succeedingAnnounceFunc)

	ctx := context.Background()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}
	for tick := 1; tick <= 2; tick++ {
		if _, err := rec.Reconcile(ctx, req); err != nil {
			t.Fatalf("tick %d: unexpected error: %v", tick, err)
		}
	}

	if n := countClaimPatches(t, base); n < 2 {
		t.Fatalf("the pass wrote %d claims over two ticks, want one per tick", n)
	}
	cell, err := crdstore.Get[v1alpha1.Controller](ctx, base.store, "cell-a", "cells-ns")
	if err != nil {
		t.Fatalf("get cell: %v", err)
	}
	if !cells.HasLiveCellClaim(cell, time.Now()) {
		t.Fatalf("the cell carries no live claim: %+v", cell.Status.Cell)
	}
	if client.calls != 2 {
		t.Fatalf("the pass sent %d tenant patches over two ticks, want one per tick", client.calls)
	}

	// The third tick's patch lands: the tenant list carries the tenant and
	// the claim retires.
	if _, err := rec.Reconcile(ctx, req); err != nil {
		t.Fatalf("tick 3: unexpected error: %v", err)
	}
	if len(base.cellStatusPatches) != 1 {
		t.Fatalf("tick 3 recorded %d cell patches, want 1", len(base.cellStatusPatches))
	}
	landed := base.cellStatusPatches[0]
	if landed.name != "cell-a" || !containsString(landed.cell.Tenants, "payments") {
		t.Fatalf("tick 3 patched %s with %v, want cell-a listing payments", landed.name, landed.cell.Tenants)
	}
	cell, err = crdstore.Get[v1alpha1.Controller](ctx, base.store, "cell-a", "cells-ns")
	if err != nil {
		t.Fatalf("get cell: %v", err)
	}
	if cells.HasLiveCellClaim(cell, time.Now()) {
		t.Fatalf("the landed patch must retire the claim: %+v", cell.Status.Cell)
	}
}

// TestTenantReconciler_Rehydration_NoPipelinesWritesNothing asserts that a
// Tenant with no spec.pipelines writes no rehydration status and sets no
// Rehydrated condition.
func TestTenantReconciler_Rehydration_NoPipelinesWritesNothing(t *testing.T) {
	client := newTenantTestClient()
	client.namespaces["team-ns"] = true

	uid := types.UID("00000000-0000-0000-0000-0000000000aa")
	tenantWith(client, "no-pipelines", v1alpha1.TenantSpec{
		DisplayName: "No Pipelines",
		Members:     []string{"alice"},
		Namespaces:  []string{"team-ns"},
	}, uid)

	rec := newPipelinesTenantReconciler(client, nil)
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "no-pipelines"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	status := lastTenantStatus(client)
	if status == nil {
		t.Fatal("expected a Tenant status patch")
	}
	if status.Rehydration != nil {
		t.Errorf("expected no rehydration status, got %+v", status.Rehydration)
	}
	for _, c := range status.Conditions {
		if c.Type == v1alpha1.TenantConditionRehydrated {
			t.Errorf("expected no Rehydrated condition, got %+v", c)
		}
	}
}

// ---------------------------------------------------------------------------
// The slot 0 agent Service (design D7, tasks 4.2 to 4.6)
// ---------------------------------------------------------------------------

// seedAgentServiceTenant seeds the cell, the profile, and a placed pipelines
// Tenant named "payments", the shape every agent Service test needs.
func seedAgentServiceTenant(client *testClient) {
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)
	seedCellController(client, nil)
	tenantWith(client, "payments", v1alpha1.TenantSpec{
		Members:    []string{"alice"},
		Namespaces: []string{"payments-ns"},
		Pipelines: &v1alpha1.TenantPipelinesSpec{
			Profile:        "jenkins-lts",
			AgentNamespace: "payments-ns",
		},
	}, types.UID("10000000-0000-0000-0000-000000000001"))
}

// reconcileTenant runs one tick for the Tenant every agent Service test seeds.
func reconcileTenant(t *testing.T, rec *TenantReconciler) {
	t.Helper()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}
	if _, err := rec.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// TestTenantReconciler_AgentService_CreatedOnFirstPlacement asserts the name,
// the namespace, and the selector of the Service a first placement writes.
func TestTenantReconciler_AgentService_CreatedOnFirstPlacement(t *testing.T) {
	client := newTenantTestClient()
	seedAgentServiceTenant(client)

	rec := newPipelinesTenantReconciler(client, nil)
	reconcileTenant(t, rec)

	if len(client.tenantServices) != 1 {
		t.Fatalf("expected one agent Service write, got %+v", client.tenantServices)
	}
	got := client.tenantServices[0]
	if got.name != "varroa-agents-payments-0" {
		t.Errorf("name = %q, want varroa-agents-payments-0", got.name)
	}
	if got.namespace != "cells-ns" {
		t.Errorf("namespace = %q, want cells-ns", got.namespace)
	}
	if got.selector != "cell-a-" {
		t.Errorf("selector = %q, want the pod label of cell-a", got.selector)
	}
}

// TestTenantReconciler_AgentService_ConvergesEveryTick asserts that a second
// tick writes the Service again, so a deleted Service and a drifted
// selector both repair themselves.
func TestTenantReconciler_AgentService_ConvergesEveryTick(t *testing.T) {
	client := newTenantTestClient()
	seedAgentServiceTenant(client)

	rec := newPipelinesTenantReconciler(client, nil)
	reconcileTenant(t, rec)
	seedCellController(client, []string{"payments"})
	reconcileTenant(t, rec)

	if len(client.tenantServices) != 2 {
		t.Fatalf("expected the Service to converge every tick, got %+v", client.tenantServices)
	}
	for _, call := range client.tenantServices {
		if call.name != "varroa-agents-payments-0" || call.namespace != "cells-ns" {
			t.Errorf("unexpected write %+v", call)
		}
	}
}

// TestTenantReconciler_AgentService_FollowsTheSlotHolder asserts that the
// selector names the cell the tenant is placed on, so a move repoints it.
func TestTenantReconciler_AgentService_FollowsTheSlotHolder(t *testing.T) {
	client := newTenantTestClient()
	seedAgentServiceTenant(client)
	// A second cell whose pod label differs from the first one's.
	crdstore.MustSeed(client.store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cell-b",
			Namespace: "cells-ns",
			UID:       types.UID("20000000-0000-0000-0000-000000000002"),
			Labels:    map[string]string{cells.LabelCellProfile: "jenkins-lts"},
		},
		Spec: v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true, MaxTenants: 10}},
		Status: v1alpha1.ControllerStatus{
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionCellReady, Status: metav1.ConditionTrue}},
			Cell:       &v1alpha1.CellStatus{Tenants: []string{"payments"}, AppliedTenants: []string{"payments"}},
		},
	})

	rec := newPipelinesTenantReconciler(client, nil)
	reconcileTenant(t, rec)

	if len(client.tenantServices) == 0 {
		t.Fatal("expected an agent Service write")
	}
	status := lastTenantStatus(client)
	if status == nil || status.Placement == nil {
		t.Fatal("expected a placement")
	}
	holder, err := crdstore.Get[v1alpha1.Controller](context.Background(), client.store,
		status.Placement.Cell, status.Placement.CellNamespace)
	if err != nil {
		t.Fatalf("read the placement cell: %v", err)
	}
	last := client.tenantServices[len(client.tenantServices)-1]
	if last.selector != controllerPrefix(holder) {
		t.Errorf("selector = %q, want %q", last.selector, controllerPrefix(holder))
	}

	// A move repoints the selector and keeps the name. The placement pass
	// hands the new cell to the same writer.
	_, cellsByKey, err := rec.listCellCandidates(context.Background())
	if err != nil {
		t.Fatalf("listCellCandidates: %v", err)
	}
	other := "cell-a"
	if status.Placement.Cell == "cell-a" {
		other = "cell-b"
	}
	if err := rec.upsertTenantAgentService(context.Background(), "payments", "cells-ns", other, cellsByKey, tenantSlotZero); err != nil {
		t.Fatalf("upsertTenantAgentService: %v", err)
	}
	after := client.tenantServices[len(client.tenantServices)-1]
	if after.name != last.name {
		t.Errorf("the move changed the Service name to %q", after.name)
	}
	if after.selector == last.selector {
		t.Errorf("the move did not repoint the selector, still %q", after.selector)
	}
	if after.selector != controllerPrefix(cellsByKey[tenantCellKey("cells-ns", other)]) {
		t.Errorf("selector = %q, want the pod label of %q", after.selector, other)
	}
}

// TestTenantReconciler_AgentService_DeletedAtTeardown asserts the delete a
// teardown performs, beside the ServiceAccount and the token Secret.
func TestTenantReconciler_AgentService_DeletedAtTeardown(t *testing.T) {
	client := newTenantTestClient()
	seedAgentServiceTenant(client)

	rec := newPipelinesTenantReconciler(client, nil)
	reconcileTenant(t, rec)

	// Drop spec.pipelines, which is what starts a teardown.
	tenant, err := crdstore.Get[v1alpha1.Tenant](context.Background(), client.store, "payments", "")
	if err != nil {
		t.Fatalf("read the Tenant: %v", err)
	}
	tenant.Spec.Pipelines = nil
	if err := crdstore.Update(context.Background(), client.store, tenant); err != nil {
		t.Fatalf("update the Tenant: %v", err)
	}
	reconcileTenant(t, rec)

	deleted := false
	for _, d := range client.deleted {
		if d.kind == "Service" && d.name == "varroa-agents-payments-0" {
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("expected the agent Service to be deleted, got %+v", client.deleted)
	}
}

// TestTenantReconciler_AgentService_FailureReportsAndKeepsThePlacement asserts
// the failure path: Placed=False with AgentServiceFailed, a second status
// patch in the same tick, and an unchanged placement.
func TestTenantReconciler_AgentService_FailureReportsAndKeepsThePlacement(t *testing.T) {
	client := newTenantTestClient()
	seedAgentServiceTenant(client)
	client.tenantServiceErr = errors.New("apiserver unavailable")

	rec := newPipelinesTenantReconciler(client, nil)
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}
	if _, err := rec.Reconcile(context.Background(), req); err == nil {
		t.Fatal("expected the tick to return the write failure, so it retries")
	}

	status := lastTenantStatus(client)
	if status == nil || status.Placement == nil || status.Placement.Cell != "cell-a" {
		t.Fatalf("expected the placement to survive, got %+v", status)
	}
	var placed *v1alpha1.TenantCondition
	for i := range status.Conditions {
		if status.Conditions[i].Type == v1alpha1.TenantConditionPlaced {
			placed = &status.Conditions[i]
		}
	}
	if placed == nil || placed.Status != metav1.ConditionFalse || placed.Reason != v1alpha1.TenantReasonAgentServiceFailed {
		t.Fatalf("Placed = %+v, want False/AgentServiceFailed", placed)
	}
	gvr, err := crdstore.GVRFor[v1alpha1.Tenant]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	if n := len(client.store.StatusPatches(gvr)); n != 2 {
		t.Errorf("expected a second status patch on the failure path, got %d", n)
	}
}

// staleMarkers answers the stale-agent-Service namespaces of the last
// patched status.
func staleMarkers(status *v1alpha1.TenantStatus) []string {
	if status == nil {
		return nil
	}
	return status.StaleAgentServiceNamespaces
}

// placedReasonOf answers the Placed condition reason of a status.
func placedReasonOf(status *v1alpha1.TenantStatus) string {
	if status == nil {
		return ""
	}
	for _, c := range status.Conditions {
		if c.Type == v1alpha1.TenantConditionPlaced {
			return c.Reason
		}
	}
	return ""
}

// serviceDeletedIn reports whether the agent Service of the tenant every
// test of this file names was deleted from a namespace.
func serviceDeletedIn(client *testClient, namespace string) bool {
	name := tenantAgentServiceName("payments", tenantSlotZero)
	for _, d := range client.deletedWithNamespace {
		if d.kind == "Service" && d.name == name && d.namespace == namespace {
			return true
		}
	}
	return false
}

// seedEvacuatingMove seeds the two cells and the placed Tenant a
// cross-namespace move needs: cell-a in cells-ns holds the tenant and
// reports evacuate, and cell-b in other-cells-ns is free and ready.
func seedEvacuatingMove(client *testClient) {
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)
	crdstore.MustSeed(client.store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cell-a",
			Namespace: "cells-ns",
			UID:       types.UID("20000000-0000-0000-0000-000000000001"),
			Labels:    map[string]string{cells.LabelCellProfile: "jenkins-lts"},
		},
		Spec: v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true, MaxTenants: 10}},
		Status: v1alpha1.ControllerStatus{
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionCellReady, Status: metav1.ConditionTrue}},
			Cell: &v1alpha1.CellStatus{
				Tenants:        []string{"payments"},
				AppliedTenants: []string{"payments"},
				Evacuate:       true,
			},
		},
	})
	crdstore.MustSeed(client.store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cell-b",
			Namespace: "other-cells-ns",
			UID:       types.UID("20000000-0000-0000-0000-000000000002"),
			Labels:    map[string]string{cells.LabelCellProfile: "jenkins-lts"},
		},
		Spec: v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true, MaxTenants: 10}},
		Status: v1alpha1.ControllerStatus{
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionCellReady, Status: metav1.ConditionTrue}},
		},
	})
	seedPlacedPipelinesTenant(client, "cell-a", "cells-ns")
}

// seedPlacedPipelinesTenant seeds the tenant "payments", already placed on the
// named cell.
func seedPlacedPipelinesTenant(client *testClient, cell, cellNamespace string) {
	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "payments",
			UID:        types.UID("10000000-0000-0000-0000-000000000003"),
			Finalizers: []string{v1alpha1.TenantFinalizerTenantPlacement},
		},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"payments-ns"},
			Pipelines: &v1alpha1.TenantPipelinesSpec{
				Profile:        "jenkins-lts",
				AgentNamespace: "payments-ns",
			},
		},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{
				Cluster: "core", Cell: cell, CellNamespace: cellNamespace, Generation: 1,
			},
			Pipelines: &v1alpha1.TenantPipelinesStatus{
				WebhookToken: "tok", WebhookSecret: "sec", AgentNamespace: "payments-ns",
			},
		},
	}
	client.tenants["payments"] = tenant
	crdstore.MustSeed(client.store, tenant)
}

// TestTenantReconciler_AgentService_FailedMoveDeleteRetriesOnTheNextTick
// asserts the durable marker. A move that crosses a namespace commits, and
// the namespace it left is recorded. A refused delete keeps the marker and
// reports AgentServiceFailed; a later tick deletes and clears it. The move
// is never rolled back: the placement pass has already announced the new
// generation to the recorder and requested the purge of the old one.
func TestTenantReconciler_AgentService_FailedMoveDeleteRetriesOnTheNextTick(t *testing.T) {
	client := newTenantTestClient()
	seedEvacuatingMove(client)
	client.deleteResourceErr = errors.New("apiserver unavailable")
	client.deleteResourceErrKind = "Service"

	rec := newPipelinesTenantReconciler(client, nil)
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}

	// Tick one: the move lands, the marker records the namespace it left,
	// and the refused delete stops the new Service write.
	if _, err := rec.Reconcile(context.Background(), req); err == nil {
		t.Fatal("expected the tick to return the refused delete")
	}
	status := lastTenantStatus(client)
	if status == nil || status.Placement == nil ||
		status.Placement.Cell != "cell-b" || status.Placement.CellNamespace != "other-cells-ns" {
		t.Fatalf("the move did not commit: %+v", status)
	}
	if got := staleMarkers(status); len(got) != 1 || got[0] != "cells-ns" {
		t.Fatalf("the stale namespaces are %v, want [cells-ns]", got)
	}
	if got := placedReasonOf(status); got != v1alpha1.TenantReasonAgentServiceFailed {
		t.Fatalf("Placed reason = %q, want AgentServiceFailed", got)
	}
	if len(client.tenantServices) != 0 {
		t.Errorf("the failed tick wrote an agent Service: %+v", client.tenantServices)
	}

	// Tick two: the delete succeeds, the marker clears, and the new Service
	// is written in the new namespace.
	client.deleteResourceErr = nil
	client.deleteResourceErrKind = ""
	if _, err := rec.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if !serviceDeletedIn(client, "cells-ns") {
		t.Fatalf("the retry did not delete the old Service, got %+v", client.deletedWithNamespace)
	}
	status = lastTenantStatus(client)
	if got := staleMarkers(status); len(got) != 0 {
		t.Errorf("the stale namespaces are %v, want none", got)
	}
	if got := placedReasonOf(status); got != v1alpha1.TenantReasonTenantPlaced {
		t.Errorf("Placed reason = %q, want TenantPlaced", got)
	}
	name := tenantAgentServiceName("payments", tenantSlotZero)
	if len(client.tenantServices) != 1 {
		t.Fatalf("expected one agent Service write, got %+v", client.tenantServices)
	}
	if got := client.tenantServices[0]; got.name != name || got.namespace != "other-cells-ns" {
		t.Errorf("the write is %+v, want %q in other-cells-ns", got, name)
	}
}

// TestTenantReconciler_AgentService_MoveInsideOneNamespaceSetsNoMarker asserts
// that a move between two cells of one namespace only repoints the selector.
func TestTenantReconciler_AgentService_MoveInsideOneNamespaceSetsNoMarker(t *testing.T) {
	client := newTenantTestClient()
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)
	crdstore.MustSeed(client.store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cell-a",
			Namespace: "cells-ns",
			UID:       types.UID("20000000-0000-0000-0000-000000000001"),
			Labels:    map[string]string{cells.LabelCellProfile: "jenkins-lts"},
		},
		Spec: v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true, MaxTenants: 10}},
		Status: v1alpha1.ControllerStatus{
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionCellReady, Status: metav1.ConditionTrue}},
			Cell: &v1alpha1.CellStatus{
				Tenants:        []string{"payments"},
				AppliedTenants: []string{"payments"},
				Evacuate:       true,
			},
		},
	})
	// The cell the tenant moves to shares the namespace of the one it leaves.
	crdstore.MustSeed(client.store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cell-b",
			Namespace: "cells-ns",
			UID:       types.UID("20000000-0000-0000-0000-000000000002"),
			Labels:    map[string]string{cells.LabelCellProfile: "jenkins-lts"},
		},
		Spec: v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true, MaxTenants: 10}},
		Status: v1alpha1.ControllerStatus{
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionCellReady, Status: metav1.ConditionTrue}},
		},
	})
	seedPlacedPipelinesTenant(client, "cell-a", "cells-ns")

	rec := newPipelinesTenantReconciler(client, nil)
	reconcileTenant(t, rec)

	status := lastTenantStatus(client)
	if status == nil || status.Placement == nil || status.Placement.Cell != "cell-b" {
		t.Fatalf("the move did not land on cell-b: %+v", status)
	}
	if got := staleMarkers(status); len(got) != 0 {
		t.Errorf("the stale namespaces are %v, want none", got)
	}
	for _, d := range client.deletedWithNamespace {
		if d.kind == "Service" {
			t.Errorf("the pass deleted a Service: %+v", d)
		}
	}
}

// TestTenantReconciler_AgentService_TeardownDrainsTheMarker asserts that a
// teardown tick drains a marker a move left, so the Service never outlives
// the tenant.
func TestTenantReconciler_AgentService_TeardownDrainsTheMarker(t *testing.T) {
	client := newTenantTestClient()
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)
	seedCellController(client, []string{"payments"})

	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "payments",
			UID:        types.UID("10000000-0000-0000-0000-000000000004"),
			Finalizers: []string{v1alpha1.TenantFinalizerTenantPlacement},
		},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"payments-ns"},
			// spec.pipelines removed: teardown starts.
		},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{
				Cluster: "core", Cell: "cell-a", CellNamespace: "cells-ns", Generation: 2,
			},
			Pipelines: &v1alpha1.TenantPipelinesStatus{
				WebhookToken: "tok", WebhookSecret: "sec", AgentNamespace: "payments-ns",
			},
			StaleAgentServiceNamespaces: []string{"old-cells-ns"},
		},
	}
	client.tenants["payments"] = tenant
	crdstore.MustSeed(client.store, tenant)

	rec := newPipelinesTenantReconciler(client, nil)
	reconcileTenant(t, rec)

	if !serviceDeletedIn(client, "old-cells-ns") {
		t.Fatalf("teardown did not drain the marker, got %+v", client.deletedWithNamespace)
	}
	if !serviceDeletedIn(client, "cells-ns") {
		t.Fatalf("teardown did not delete the current namespace's Service, got %+v", client.deletedWithNamespace)
	}
}

// seedEvacuatingCell seeds one evacuating cell that holds the tenant.
func seedEvacuatingCell(client *testClient, name, namespace, uid string) {
	crdstore.MustSeed(client.store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(uid),
			Labels:    map[string]string{cells.LabelCellProfile: "jenkins-lts"},
		},
		Spec: v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true, MaxTenants: 10}},
		Status: v1alpha1.ControllerStatus{
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionCellReady, Status: metav1.ConditionTrue}},
			Cell: &v1alpha1.CellStatus{
				Tenants:        []string{"payments"},
				AppliedTenants: []string{"payments"},
				Evacuate:       true,
			},
		},
	})
}

// seedFreeCell seeds one free, ready cell.
func seedFreeCell(client *testClient, name, namespace, uid string) {
	crdstore.MustSeed(client.store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(uid),
			Labels:    map[string]string{cells.LabelCellProfile: "jenkins-lts"},
		},
		Spec: v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true, MaxTenants: 10}},
		Status: v1alpha1.ControllerStatus{
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionCellReady, Status: metav1.ConditionTrue}},
		},
	})
}

// TestTenantReconciler_AgentService_TwoMovesKeepBothNamespaces asserts that a
// second move across namespaces, before the first namespace drains, keeps
// both namespaces and drains both once the deletes succeed. A single-valued
// marker would forget the first one.
func TestTenantReconciler_AgentService_TwoMovesKeepBothNamespaces(t *testing.T) {
	client := newTenantTestClient()
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)
	seedEvacuatingCell(client, "cell-a", "ns-a", "20000000-0000-0000-0000-00000000000a")
	seedFreeCell(client, "cell-b", "ns-b", "20000000-0000-0000-0000-00000000000b")
	seedPlacedPipelinesTenant(client, "cell-a", "ns-a")
	client.deleteResourceErr = errors.New("apiserver unavailable")
	client.deleteResourceErrKind = "Service"

	rec := newPipelinesTenantReconciler(client, nil)
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}

	// Move one: a to b. The delete of ns-a is refused.
	if _, err := rec.Reconcile(context.Background(), req); err == nil {
		t.Fatal("expected the tick to return the refused delete")
	}
	status := lastTenantStatus(client)
	if status.Placement == nil || status.Placement.CellNamespace != "ns-b" {
		t.Fatalf("move one did not land: %+v", status.Placement)
	}
	if got := staleMarkers(status); len(got) != 1 || got[0] != "ns-a" {
		t.Fatalf("the stale namespaces are %v, want [ns-a]", got)
	}

	// Move two: b to c, while ns-a still owes a delete.
	seedEvacuatingCell(client, "cell-b", "ns-b", "20000000-0000-0000-0000-00000000000b")
	seedFreeCell(client, "cell-c", "ns-c", "20000000-0000-0000-0000-00000000000c")
	if _, err := rec.Reconcile(context.Background(), req); err == nil {
		t.Fatal("expected the tick to return the refused delete")
	}
	status = lastTenantStatus(client)
	if status.Placement == nil || status.Placement.CellNamespace != "ns-c" {
		t.Fatalf("move two did not land: %+v", status.Placement)
	}
	if got := staleMarkers(status); len(got) != 2 || got[0] != "ns-a" || got[1] != "ns-b" {
		t.Fatalf("the stale namespaces are %v, want [ns-a ns-b]", got)
	}

	// Both deletes now succeed, and the list empties.
	client.deleteResourceErr = nil
	client.deleteResourceErrKind = ""
	if _, err := rec.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("the draining tick failed: %v", err)
	}
	for _, namespace := range []string{"ns-a", "ns-b"} {
		if !serviceDeletedIn(client, namespace) {
			t.Errorf("the drain did not delete the Service in %q, got %+v", namespace, client.deletedWithNamespace)
		}
	}
	if got := staleMarkers(lastTenantStatus(client)); len(got) != 0 {
		t.Errorf("the stale namespaces are %v, want none", got)
	}
}

// TestTenantReconciler_AgentService_TeardownWithNoCellDrainsTheMarker asserts
// the teardown path that completes in one tick. An absent cell short-
// circuits to the final teardown stage, which clears status.pipelines; the
// list therefore lives on the status root, where that stage never reaches
// it.
func TestTenantReconciler_AgentService_TeardownWithNoCellDrainsTheMarker(t *testing.T) {
	client := newTenantTestClient()
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)

	// No cell object exists, so teardown finishes on this tick.
	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "payments",
			UID:        types.UID("10000000-0000-0000-0000-000000000005"),
			Finalizers: []string{v1alpha1.TenantFinalizerTenantPlacement},
		},
		Spec: v1alpha1.TenantSpec{Namespaces: []string{"payments-ns"}},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{
				Cluster: "core", Cell: "cell-gone", CellNamespace: "cells-ns", Generation: 3,
			},
			Pipelines: &v1alpha1.TenantPipelinesStatus{
				WebhookToken: "tok", WebhookSecret: "sec", AgentNamespace: "payments-ns",
			},
			StaleAgentServiceNamespaces: []string{"old-cells-ns"},
		},
	}
	client.tenants["payments"] = tenant
	crdstore.MustSeed(client.store, tenant)

	rec := newPipelinesTenantReconciler(client, nil)
	reconcileTenant(t, rec)

	status := lastTenantStatus(client)
	if status == nil || status.Placement != nil {
		t.Fatalf("expected teardown to complete on this tick, got %+v", status)
	}
	if !serviceDeletedIn(client, "old-cells-ns") {
		t.Fatalf("teardown did not drain the list, got %+v", client.deletedWithNamespace)
	}
	if got := staleMarkers(status); len(got) != 0 {
		t.Errorf("the stale namespaces are %v, want none", got)
	}
}

// tenantFinalizers answers the placement finalizer state of the stored Tenant.
func tenantHoldsPlacementFinalizer(t *testing.T, client *testClient) bool {
	t.Helper()
	tenant, err := crdstore.Get[v1alpha1.Tenant](context.Background(), client.store, "payments", "")
	if err != nil {
		t.Fatalf("read the Tenant: %v", err)
	}
	return containsString(tenant.Finalizers, v1alpha1.TenantFinalizerTenantPlacement)
}

// TestTenantReconciler_AgentService_TeardownHoldsFinalizerUntilTheDrain
// asserts that the placement finalizer releases only once every agent
// Service of the tenant is gone. The finalizer is what keeps the pipelines
// block reachable, so releasing it over an owed delete would strand the
// Service for good.
func TestTenantReconciler_AgentService_TeardownHoldsFinalizerUntilTheDrain(t *testing.T) {
	client := newTenantTestClient()
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)

	// No cell object exists, so teardown asks to release the finalizer on
	// the first tick.
	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "payments",
			UID:        types.UID("10000000-0000-0000-0000-000000000006"),
			Finalizers: []string{v1alpha1.TenantFinalizerTenantPlacement},
		},
		Spec: v1alpha1.TenantSpec{Namespaces: []string{"payments-ns"}},
		Status: v1alpha1.TenantStatus{
			Placement: &v1alpha1.TenantPlacementStatus{
				Cluster: "core", Cell: "cell-gone", CellNamespace: "cells-ns", Generation: 3,
			},
			Pipelines: &v1alpha1.TenantPipelinesStatus{
				WebhookToken: "tok", WebhookSecret: "sec", AgentNamespace: "payments-ns",
			},
		},
	}
	client.tenants["payments"] = tenant
	crdstore.MustSeed(client.store, tenant)
	client.deleteResourceErr = errors.New("apiserver unavailable")
	client.deleteResourceErrKind = "Service"

	rec := newPipelinesTenantReconciler(client, nil)
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}

	// Tick one: the drain is refused, so the finalizer stays and the list
	// names the namespace still owed.
	if _, err := rec.Reconcile(context.Background(), req); err == nil {
		t.Fatal("expected the tick to return the refused delete")
	}
	if !tenantHoldsPlacementFinalizer(t, client) {
		t.Fatal("the finalizer was released over an owed agent Service delete")
	}
	status := lastTenantStatus(client)
	if got := staleMarkers(status); len(got) != 1 || got[0] != "cells-ns" {
		t.Fatalf("the stale namespaces are %v, want [cells-ns]", got)
	}
	if got := placedReasonOf(status); got != v1alpha1.TenantReasonAgentServiceFailed {
		t.Fatalf("Placed reason = %q, want AgentServiceFailed", got)
	}

	// Tick two: the delete succeeds, the list empties, and the finalizer
	// releases. The stored Tenant still carries the list, and spec.pipelines
	// and the placement are both gone, so this tick proves the guard lets a
	// finalizer-only tenant reach the drain.
	client.deleteResourceErr = nil
	client.deleteResourceErrKind = ""
	if _, err := rec.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if !serviceDeletedIn(client, "cells-ns") {
		t.Fatalf("the retry did not delete the Service, got %+v", client.deletedWithNamespace)
	}
	if tenantHoldsPlacementFinalizer(t, client) {
		t.Fatal("the finalizer was not released once the drain succeeded")
	}
	if got := staleMarkers(lastTenantStatus(client)); len(got) != 0 {
		t.Errorf("the stale namespaces are %v, want none", got)
	}
}

// foldTenantWarnings runs setTenantWarningsCondition for the Tenant payments
// placed on cell-a, whose tenantWarnings list is entries, and returns the
// TenantWarnings condition.
func foldTenantWarnings(t *testing.T, entries []v1alpha1.TenantWarning) v1alpha1.TenantCondition {
	t.Helper()
	cell := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "cells-ns"},
		Status:     v1alpha1.ControllerStatus{Cell: &v1alpha1.CellStatus{TenantWarnings: entries}},
	}
	tenant := &v1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "payments"}}
	status := &v1alpha1.TenantStatus{Placement: &v1alpha1.TenantPlacementStatus{Cell: "cell-a", CellNamespace: "cells-ns"}}
	rec := &TenantReconciler{}
	rec.setTenantWarningsCondition(status, tenant, map[string]*v1alpha1.Controller{tenantCellKey("cells-ns", "cell-a"): cell})
	for _, c := range status.Conditions {
		if c.Type == v1alpha1.TenantConditionTenantWarnings {
			return c
		}
	}
	t.Fatal("expected a TenantWarnings condition")
	return v1alpha1.TenantCondition{}
}

func TestSetTenantWarningsCondition_BundleKeysIgnoredOutranksPodTemplateSkipped(t *testing.T) {
	cond := foldTenantWarnings(t, []v1alpha1.TenantWarning{
		{Tenant: "payments", Reason: v1alpha1.TenantReasonPodTemplateSkipped, Message: `pod template "default": the name "default" is reserved`},
		{Tenant: "payments", Reason: v1alpha1.TenantReasonBundleKeysIgnored, Message: `tenant bundle key "plugins.yaml" is ignored on a cell`},
	})
	if cond.Reason != v1alpha1.TenantReasonBundleKeysIgnored {
		t.Errorf("expected reason BundleKeysIgnored, got %q", cond.Reason)
	}
	if !strings.Contains(cond.Message, "BundleKeysIgnored: ") || !strings.Contains(cond.Message, "PodTemplateSkipped: ") {
		t.Errorf("expected both entries in the message, got %q", cond.Message)
	}
}

func TestSetTenantWarningsCondition_PodTemplateSkippedOutranksCredentialSkipped(t *testing.T) {
	cond := foldTenantWarnings(t, []v1alpha1.TenantWarning{
		{Tenant: "payments", Reason: v1alpha1.TenantReasonCredentialSkipped, Message: "duplicate credential id"},
		{Tenant: "payments", Reason: v1alpha1.TenantReasonPodTemplateSkipped, Message: `pod template "golang": label is not a string`},
	})
	if cond.Reason != v1alpha1.TenantReasonPodTemplateSkipped {
		t.Errorf("expected reason PodTemplateSkipped, got %q", cond.Reason)
	}
}

func TestSetTenantWarningsCondition_ItemsHeldOutranksBundleKeysIgnored(t *testing.T) {
	cond := foldTenantWarnings(t, []v1alpha1.TenantWarning{
		{Tenant: "payments", Reason: v1alpha1.TenantReasonBundleKeysIgnored, Message: `tenant bundle key "plugins.yaml" is ignored on a cell`},
		{Tenant: "payments", Reason: v1alpha1.TenantReasonItemsHeld, Message: "tenant folder held: items.yaml holds unresolved variables: x"},
	})
	if cond.Reason != v1alpha1.TenantReasonItemsHeld {
		t.Errorf("expected reason ItemsHeld, got %q", cond.Reason)
	}
}

// foldTenantWarningsMembers runs setTenantWarningsCondition for the Tenant
// payments over placement and cellsByKey, and returns the TenantWarnings
// condition.
func foldTenantWarningsMembers(t *testing.T, placement *v1alpha1.TenantPlacementStatus, cellsByKey map[string]*v1alpha1.Controller) v1alpha1.TenantCondition {
	t.Helper()
	tenant := &v1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "payments"}}
	status := &v1alpha1.TenantStatus{Placement: placement}
	rec := &TenantReconciler{}
	rec.setTenantWarningsCondition(status, tenant, cellsByKey)
	for _, c := range status.Conditions {
		if c.Type == v1alpha1.TenantConditionTenantWarnings {
			return c
		}
	}
	t.Fatal("expected a TenantWarnings condition")
	return v1alpha1.TenantCondition{}
}

// warnCell builds a cell Controller in namespace cells-ns that reports
// entries.
func warnCell(name string, entries ...v1alpha1.TenantWarning) *v1alpha1.Controller {
	return &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "cells-ns"},
		Status:     v1alpha1.ControllerStatus{Cell: &v1alpha1.CellStatus{TenantWarnings: entries}},
	}
}

func TestSetTenantWarningsCondition_ReadsNonSlotZeroMember(t *testing.T) {
	placement := &v1alpha1.TenantPlacementStatus{
		Cell: "cell-a", CellNamespace: "cells-ns",
		Members: []v1alpha1.TenantMemberStatus{
			{Cell: "cell-a", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive, Slot: int32Ptr(0)},
			{Cell: "cell-b", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive, Slot: int32Ptr(1)},
		},
	}
	cells := map[string]*v1alpha1.Controller{
		tenantCellKey("cells-ns", "cell-a"): warnCell("cell-a"),
		tenantCellKey("cells-ns", "cell-b"): warnCell("cell-b",
			v1alpha1.TenantWarning{Tenant: "payments", Reason: v1alpha1.TenantReasonCredentialSkipped, Message: `credential "big" does not fit the cell budget`},
			v1alpha1.TenantWarning{Tenant: "other-team", Reason: v1alpha1.TenantReasonItemsHeld, Message: "other tenant warning"},
		),
	}
	cond := foldTenantWarningsMembers(t, placement, cells)
	if cond.Status != metav1.ConditionTrue {
		t.Fatalf("status = %v, want True", cond.Status)
	}
	if cond.Reason != v1alpha1.TenantReasonCredentialSkipped {
		t.Errorf("reason = %q, want CredentialSkipped", cond.Reason)
	}
	want := `CredentialSkipped: credential "big" does not fit the cell budget`
	if cond.Message != want {
		t.Errorf("message = %q, want %q", cond.Message, want)
	}
}

func TestSetTenantWarningsCondition_ReadsStandbyMember(t *testing.T) {
	placement := &v1alpha1.TenantPlacementStatus{
		Cell: "cell-a", CellNamespace: "cells-ns",
		Members: []v1alpha1.TenantMemberStatus{
			{Cell: "cell-a", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive, Slot: int32Ptr(0)},
			{Cell: "cell-s", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleStandby},
		},
	}
	cells := map[string]*v1alpha1.Controller{
		tenantCellKey("cells-ns", "cell-a"): warnCell("cell-a"),
		tenantCellKey("cells-ns", "cell-s"): warnCell("cell-s",
			v1alpha1.TenantWarning{Tenant: "payments", Reason: v1alpha1.TenantReasonCredentialSkipped, Message: `credential "big" does not fit the cell budget`},
		),
	}
	cond := foldTenantWarningsMembers(t, placement, cells)
	if cond.Status != metav1.ConditionTrue {
		t.Fatalf("status = %v, want True", cond.Status)
	}
	want := `CredentialSkipped: credential "big" does not fit the cell budget`
	if cond.Message != want {
		t.Errorf("message = %q, want %q", cond.Message, want)
	}
}

func TestSetTenantWarningsCondition_NamesCellsWhenTwoContribute(t *testing.T) {
	placement := &v1alpha1.TenantPlacementStatus{
		Cell: "cell-a", CellNamespace: "cells-ns",
		Members: []v1alpha1.TenantMemberStatus{
			{Cell: "cell-a", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive, Slot: int32Ptr(0)},
			{Cell: "cell-b", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive, Slot: int32Ptr(1)},
		},
	}
	cells := map[string]*v1alpha1.Controller{
		tenantCellKey("cells-ns", "cell-a"): warnCell("cell-a",
			v1alpha1.TenantWarning{Tenant: "payments", Reason: v1alpha1.TenantReasonPodTemplateSkipped, Message: "pod template skipped"},
		),
		tenantCellKey("cells-ns", "cell-b"): warnCell("cell-b",
			v1alpha1.TenantWarning{Tenant: "payments", Reason: v1alpha1.TenantReasonItemsHeld, Message: "items held"},
		),
	}
	cond := foldTenantWarningsMembers(t, placement, cells)
	if cond.Reason != v1alpha1.TenantReasonItemsHeld {
		t.Errorf("reason = %q, want ItemsHeld", cond.Reason)
	}
	want := "cell-b: ItemsHeld: items held; cell-a: PodTemplateSkipped: pod template skipped"
	if cond.Message != want {
		t.Errorf("message = %q, want %q", cond.Message, want)
	}
}

func TestSetTenantWarningsCondition_SameWarningOnTwoCellsListedOnce(t *testing.T) {
	placement := &v1alpha1.TenantPlacementStatus{
		Cell: "cell-a", CellNamespace: "cells-ns",
		Members: []v1alpha1.TenantMemberStatus{
			{Cell: "cell-a", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive, Slot: int32Ptr(0)},
			{Cell: "cell-b", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive, Slot: int32Ptr(1)},
		},
	}
	held := v1alpha1.TenantWarning{Tenant: "payments", Reason: v1alpha1.TenantReasonItemsHeld, Message: "items held"}
	cells := map[string]*v1alpha1.Controller{
		tenantCellKey("cells-ns", "cell-a"): warnCell("cell-a", held),
		tenantCellKey("cells-ns", "cell-b"): warnCell("cell-b", held,
			v1alpha1.TenantWarning{Tenant: "payments", Reason: v1alpha1.TenantReasonCredentialSkipped, Message: "credential skipped"},
		),
	}
	cond := foldTenantWarningsMembers(t, placement, cells)
	if cond.Reason != v1alpha1.TenantReasonItemsHeld {
		t.Errorf("reason = %q, want ItemsHeld", cond.Reason)
	}
	// Lines sort by reason, so CredentialSkipped precedes ItemsHeld.
	want := "cell-b: CredentialSkipped: credential skipped; cell-a, cell-b: ItemsHeld: items held"
	if cond.Message != want {
		t.Errorf("message = %q, want %q", cond.Message, want)
	}
}

func TestSetTenantWarningsCondition_SkipsMissingMemberCell(t *testing.T) {
	placement := &v1alpha1.TenantPlacementStatus{
		Cell: "cell-a", CellNamespace: "cells-ns",
		Members: []v1alpha1.TenantMemberStatus{
			{Cell: "cell-a", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive, Slot: int32Ptr(0)},
			{Cell: "cell-gone", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleStandby},
		},
	}
	cells := map[string]*v1alpha1.Controller{
		tenantCellKey("cells-ns", "cell-a"): warnCell("cell-a",
			v1alpha1.TenantWarning{Tenant: "payments", Reason: v1alpha1.TenantReasonItemsHeld, Message: "items held"},
		),
	}
	cond := foldTenantWarningsMembers(t, placement, cells)
	if cond.Status != metav1.ConditionTrue {
		t.Fatalf("status = %v, want True", cond.Status)
	}
	if cond.Message != "ItemsHeld: items held" {
		t.Errorf("message = %q, want %q", cond.Message, "ItemsHeld: items held")
	}
}

func TestSetTenantWarningsCondition_HibernationIgnoredOutranksBranchScanFailed(t *testing.T) {
	cond := foldTenantWarnings(t, []v1alpha1.TenantWarning{
		{Tenant: "payments", Reason: v1alpha1.TenantReasonBranchScanFailed, Message: "the last branch scan of mb ended with FAILURE"},
		{Tenant: "payments", Reason: v1alpha1.TenantReasonHibernationIgnored, Message: "hibernation is ignored on a cell"},
	})
	if cond.Reason != v1alpha1.TenantReasonHibernationIgnored {
		t.Errorf("expected reason HibernationIgnored, got %q", cond.Reason)
	}
}

func TestSetTenantWarningsCondition_BranchScanFailedAlone(t *testing.T) {
	cond := foldTenantWarnings(t, []v1alpha1.TenantWarning{
		{Tenant: "payments", Reason: v1alpha1.TenantReasonBranchScanFailed, Message: "the last branch scan of mb ended with FAILURE"},
	})
	if cond.Reason != v1alpha1.TenantReasonBranchScanFailed {
		t.Errorf("expected reason BranchScanFailed, got %q", cond.Reason)
	}
}
