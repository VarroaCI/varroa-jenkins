package controller

import (
	"testing"

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
