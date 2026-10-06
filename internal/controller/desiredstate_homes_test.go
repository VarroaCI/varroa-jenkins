package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// baseHomesCommand builds a command that carries one tenant home entry.
func baseHomesCommand(homes []*mitev1.TenantHomes) *mitev1.DesiredStateCommand {
	return &mitev1.DesiredStateCommand{
		JcascYaml:   "jcasc: 1",
		RbacYaml:    "rbac: 1",
		ItemsYaml:   "items: 1",
		TenantHomes: homes,
	}
}

// TestComputeDesiredStateHash_CoversTenantHomes asserts field 18 is
// inside the composite hash, so a slot change or a home change pushes a
// new desired state.
func TestComputeDesiredStateHash_CoversTenantHomes(t *testing.T) {
	none := computeDesiredStateHash(baseHomesCommand(nil))
	slotZero := computeDesiredStateHash(baseHomesCommand([]*mitev1.TenantHomes{
		{Tenant: "payments", Slot: 0, Role: "active", JobHomes: map[string]int{"payments/api": 0}},
	}))
	if none == slotZero {
		t.Error("the hash is unchanged when tenant_homes appears")
	}

	homeMoved := computeDesiredStateHash(baseHomesCommand([]*mitev1.TenantHomes{
		{Tenant: "payments", Slot: 0, Role: "active", JobHomes: map[string]int{"payments/api": 1}},
	}))
	if slotZero == homeMoved {
		t.Error("the hash is unchanged when a job's home changes")
	}

	slotMoved := computeDesiredStateHash(baseHomesCommand([]*mitev1.TenantHomes{
		{Tenant: "payments", Slot: 1, Role: "active", JobHomes: map[string]int{"payments/api": 0}},
	}))
	if slotZero == slotMoved {
		t.Error("the hash is unchanged when the cell's own slot changes")
	}
}

// TestComputeDesiredStateHash_TenantHomesOrderDoesNotMatter asserts the
// hash is stable across two assemblies of the same state, whatever order
// the entries and the map keys arrive in.
func TestComputeDesiredStateHash_TenantHomesOrderDoesNotMatter(t *testing.T) {
	first := computeDesiredStateHash(baseHomesCommand([]*mitev1.TenantHomes{
		{Tenant: "payments", Slot: 0, Role: "active", JobHomes: map[string]int{"payments/api": 0, "payments/web": 1}},
		{Tenant: "search", Slot: 1, Role: "standby"},
	}))
	second := computeDesiredStateHash(baseHomesCommand([]*mitev1.TenantHomes{
		{Tenant: "search", Slot: 1, Role: "standby"},
		{Tenant: "payments", Slot: 0, Role: "active", JobHomes: map[string]int{"payments/web": 1, "payments/api": 0}},
	}))
	if first != second {
		t.Errorf("the hash depends on the entry order: %q and %q", first, second)
	}
}

// TestBuildDesiredStateCommand_CellFlag asserts field 19 is true for a
// cell alone. The mite uses it to tell a cell with no tenant from a
// controller that is not a cell.
func TestBuildDesiredStateCommand_CellFlag(t *testing.T) {
	rec := newTestReconciler(newTestClientWithBundle())
	resolved := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hello"}

	if cmd := mustBuildDesiredStateCommand(t, rec, cellController(), resolved); !cmd.Cell {
		t.Error("a cell's desired state has cell false")
	}

	plain := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "ns"}}
	if cmd := mustBuildDesiredStateCommand(t, rec, plain, resolved); cmd.Cell {
		t.Error("the desired state of a controller that is not a cell has cell true")
	}
}

// TestComputeDesiredStateHash_IgnoresCellFlag asserts field 19 stays out
// of the composite hash.
func TestComputeDesiredStateHash_IgnoresCellFlag(t *testing.T) {
	flagged := baseHomesCommand(nil)
	flagged.Cell = true
	if computeDesiredStateHash(baseHomesCommand(nil)) != computeDesiredStateHash(flagged) {
		t.Error("the hash changes when only the cell flag changes")
	}
}

// TestComputeDesiredStateHash_FramesEachSection pins that the same text in
// another section answers another composite hash. An empty section must
// not let the next section take its place.
func TestComputeDesiredStateHash_FramesEachSection(t *testing.T) {
	inJcasc := computeDesiredStateHash(&mitev1.DesiredStateCommand{JcascYaml: "x: 1"})
	inRbac := computeDesiredStateHash(&mitev1.DesiredStateCommand{RbacYaml: "x: 1"})
	inItems := computeDesiredStateHash(&mitev1.DesiredStateCommand{ItemsYaml: "x: 1"})
	if inJcasc == inRbac || inRbac == inItems || inJcasc == inItems {
		t.Fatalf("the same text in two sections answers the same hash: jcasc=%s rbac=%s items=%s",
			inJcasc, inRbac, inItems)
	}
}

// TestComputeDesiredStateHash_SeparatesContentFromHomes pins the boundary
// between the content and the tenant homes. The JCasC text below is the
// byte form that tenantHomesHash hashes for the homes entry. Without the
// frame, the JCasC hash and the homes hash are the same 64 characters in
// the same place, so the two states answer the same composite hash.
func TestComputeDesiredStateHash_SeparatesContentFromHomes(t *testing.T) {
	homes := []*mitev1.TenantHomes{{Tenant: "payments", Slot: 0, Role: "active"}}
	jcasc := "payments\x000\x00active\x00"
	if sha256Hex([]byte(jcasc)) != tenantHomesHash(homes) {
		t.Fatal("the JCasC text is not the byte form of the homes entry")
	}
	contentOnly := computeDesiredStateHash(&mitev1.DesiredStateCommand{JcascYaml: jcasc})
	homesOnly := computeDesiredStateHash(&mitev1.DesiredStateCommand{TenantHomes: homes})
	if contentOnly == homesOnly {
		t.Fatalf("a JCasC section and a homes entry answer the same hash %s", contentOnly)
	}
}

// TestComputeDesiredStateHash_ComposesContentAndHomes pins the rule that
// the manual-mode placement check uses: a home move keeps the content hash
// and changes the composite hash. It guards the composition, not the frame.
func TestComputeDesiredStateHash_ComposesContentAndHomes(t *testing.T) {
	a := baseHomesCommand([]*mitev1.TenantHomes{{Tenant: "t", JobHomes: map[string]int{"j": 0}}})
	b := baseHomesCommand([]*mitev1.TenantHomes{{Tenant: "t", JobHomes: map[string]int{"j": 1}}})
	if computeDesiredContentHash(a) != computeDesiredContentHash(b) {
		t.Fatal("a home move changed the content hash")
	}
	if computeDesiredStateHash(a) == computeDesiredStateHash(b) {
		t.Fatal("a home move did not change the composite hash")
	}
}
