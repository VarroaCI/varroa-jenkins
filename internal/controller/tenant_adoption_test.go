package controller

import (
	"context"
	"log/slog"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestTenantOwnsObject pins the adoption rule: a child is a Tenant's when
// it names the Tenant UID in an owner reference, or when it carries the
// managed-by and tenant-name label pair for that Tenant.
func TestTenantOwnsObject(t *testing.T) {
	t.Parallel()

	tenant := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments", UID: types.UID("uid-new")},
	}
	pair := map[string]string{
		v1alpha1.LabelManagedBy:  "tenant",
		v1alpha1.LabelTenantName: "payments",
	}
	tests := []struct {
		name   string
		owners []metav1.OwnerReference
		labels map[string]string
		want   bool
	}{
		{
			name:   "owner reference with the Tenant UID",
			owners: []metav1.OwnerReference{{UID: types.UID("uid-new")}},
			want:   true,
		},
		{
			name:   "label pair for this Tenant",
			labels: pair,
			want:   true,
		},
		{
			name:   "owner reference of an earlier incarnation and no labels",
			owners: []metav1.OwnerReference{{UID: types.UID("uid-old")}},
			want:   false,
		},
		{
			name:   "managed-by without the tenant name",
			labels: map[string]string{v1alpha1.LabelManagedBy: "tenant"},
			want:   false,
		},
		{
			name:   "tenant name of another Tenant",
			labels: map[string]string{v1alpha1.LabelManagedBy: "tenant", v1alpha1.LabelTenantName: "other"},
			want:   false,
		},
		{
			name: "hand-authored object with neither marker",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			obj := &v1alpha1.Group{ObjectMeta: metav1.ObjectMeta{
				Name:            "tenant-payments",
				OwnerReferences: tt.owners,
				Labels:          tt.labels,
			}}
			if got := tenantOwnsObject(obj, tenant); got != tt.want {
				t.Fatalf("tenantOwnsObject = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestTenantReconciler_ReAddsOwnerReferenceToAnAdoptableGroup seeds a Group
// named tenant-<name> that carries the label pair but no owner reference.
// The reconciler adopts it and writes the owner reference naming the
// current Tenant UID.
func TestTenantReconciler_ReAddsOwnerReferenceToAnAdoptableGroup(t *testing.T) {
	client := newTenantTestClient()
	client.namespaces["team-ns"] = true

	uid := types.UID("00000000-0000-0000-0000-0000000000a1")
	tenantWith(client, "adopt-me", v1alpha1.TenantSpec{
		Members:    []string{"alice"},
		Namespaces: []string{"team-ns"},
		RoleRef:    "developer",
	}, uid)
	crdstore.MustSeed(client.store, &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tenant-adopt-me",
			Labels: map[string]string{
				v1alpha1.LabelManagedBy:  "tenant",
				v1alpha1.LabelTenantName: "adopt-me",
			},
		},
	})

	rec := NewTenantReconciler(client, client.store, tenantTestNS, slog.Default())
	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "adopt-me"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	g, err := crdstore.Get[v1alpha1.Group](context.Background(), client.store, "tenant-adopt-me", "")
	if err != nil {
		t.Fatalf("read Group: %v", err)
	}
	if len(g.OwnerReferences) != 1 || g.OwnerReferences[0].UID != uid {
		t.Fatalf("owner references = %+v, want one reference naming UID %q", g.OwnerReferences, uid)
	}
}
