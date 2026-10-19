package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// mkItemsFailureDSR builds a desired-state result whose items section
// refused each item path.
func mkItemsFailureDSR(itemPaths ...string) *mitev1.CommandResult {
	failures := make([]*mitev1.ItemFailure, 0, len(itemPaths))
	msgs := make([]string, 0, len(itemPaths))
	for _, p := range itemPaths {
		text := "create " + p + ": HTTP 500: Jenkins"
		failures = append(failures, &mitev1.ItemFailure{Path: p, Error: text})
		msgs = append(msgs, text)
	}
	res := mkDSR(true, true, true, false, "", "", "", strings.Join(msgs, "; "), "hash-1")
	res.GetDesiredState().ItemFailures = failures
	return res
}

func TestItemsApplyFailureWarningsAttributesTenant(t *testing.T) {
	cell := &v1alpha1.CellStatus{Tenants: []string{"payments", "search"}}
	d := mkItemsFailureDSR("payments/repos").GetDesiredState()

	got := itemsApplyFailureWarnings(cell, d)
	if len(got) != 1 {
		t.Fatalf("warnings = %+v, want one entry", got)
	}
	if got[0].Tenant != "payments" || got[0].Reason != v1alpha1.TeamReasonItemsApplyFailed {
		t.Errorf("warning = %+v, want payments/ItemsApplyFailed", got[0])
	}
	if got[0].Message == "" {
		t.Error("warning message is empty, want the Jenkins error")
	}
}

func TestItemsApplyFailureWarningsNoneOnSuccess(t *testing.T) {
	cell := &v1alpha1.CellStatus{Tenants: []string{"payments"}}
	d := mkDSR(true, true, true, true, "", "", "", "", "hash-1").GetDesiredState()
	d.ItemFailures = []*mitev1.ItemFailure{{Path: "payments/repos", Error: "create payments/repos: HTTP 500"}}
	if got := itemsApplyFailureWarnings(cell, d); got != nil {
		t.Fatalf("warnings = %+v, want none on a successful items section", got)
	}
}

func TestItemsApplyFailureWarningsOneEntryPerTenant(t *testing.T) {
	cell := &v1alpha1.CellStatus{Tenants: []string{"payments", "search"}}
	d := mkItemsFailureDSR("payments/repos", "payments/x", "search/y").GetDesiredState()

	got := itemsApplyFailureWarnings(cell, d)
	if len(got) != 2 {
		t.Fatalf("warnings = %+v, want one entry per tenant", got)
	}
	byTenant := map[string]string{}
	for _, w := range got {
		byTenant[w.Tenant] = w.Message
	}
	payments, ok := byTenant["payments"]
	if !ok || !strings.Contains(payments, "payments/repos") || !strings.Contains(payments, "payments/x") {
		t.Fatalf("payments message = %q, want both payments items", payments)
	}
	if strings.Contains(payments, "search/y") {
		t.Errorf("payments message = %q, must not name another tenant's item", payments)
	}
	search, ok := byTenant["search"]
	if !ok || !strings.Contains(search, "search/y") {
		t.Fatalf("search message = %q, want its own item", search)
	}
	if strings.Contains(search, "payments") {
		t.Errorf("search message = %q, must not name a payments item", search)
	}
}

func TestItemsApplyFailureWarningsNoneWithoutPath(t *testing.T) {
	cell := &v1alpha1.CellStatus{Tenants: []string{"payments"}}
	d := mkDSR(true, true, true, false, "", "", "", "parse items: bad yaml", "hash-1").GetDesiredState()
	if got := itemsApplyFailureWarnings(cell, d); got != nil {
		t.Fatalf("warnings = %+v, want none when the failure names no item", got)
	}
}

func TestItemsApplyFailureWarningsNoneForUnknownTenant(t *testing.T) {
	cell := &v1alpha1.CellStatus{Tenants: []string{"payments"}}
	d := mkItemsFailureDSR("ghost/repos").GetDesiredState()
	if got := itemsApplyFailureWarnings(cell, d); got != nil {
		t.Fatalf("warnings = %+v, want none for a path no tenant owns", got)
	}
}

// newItemsFailureCell returns a cell controller whose tenants are payments
// and search, ready for a drain of desired-state results.
func newItemsFailureCell(hash string) *v1alpha1.Controller {
	return &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(time.Now())},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"},
			Cell:              &v1alpha1.CellSpec{Enabled: true},
		},
		Status: v1alpha1.ControllerStatus{
			Phase:            v1alpha1.ControllerPhaseConnected,
			DesiredStateHash: hash,
			Conditions:       []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
			Cell:             &v1alpha1.CellStatus{Tenants: []string{"payments", "search"}},
		},
	}
}

func itemsFailureCount(cr *v1alpha1.Controller, tenant string) int {
	n := 0
	if cr.Status.Cell == nil {
		return n
	}
	for _, w := range cr.Status.Cell.TenantWarnings {
		if w.Tenant == tenant && w.Reason == v1alpha1.TeamReasonItemsApplyFailed {
			n++
		}
	}
	return n
}

func TestCellItemsApplyFailureRecordsTenantWarning(t *testing.T) {
	rec := newTestReconcilerWithTokenSigner(newTestClientWithBundle())
	transport := &captureTransport{}
	rec.miteTransport = transport
	cr := newItemsFailureCell("hash-1")

	transport.drainResults = []*mitev1.CommandResult{mkItemsFailureDSR("payments/repos")}
	_ = rec.reconcileController(context.Background(), cr)

	if n := itemsFailureCount(cr, "payments"); n != 1 {
		t.Fatalf("payments ItemsApplyFailed warnings = %d, want 1 (%+v)", n, cr.Status.Cell.TenantWarnings)
	}
	if n := itemsFailureCount(cr, "search"); n != 0 {
		t.Fatalf("search ItemsApplyFailed warnings = %d, want 0", n)
	}
}

func TestCellItemsApplySuccessClearsTenantWarning(t *testing.T) {
	rec := newTestReconcilerWithTokenSigner(newTestClientWithBundle())
	transport := &captureTransport{}
	rec.miteTransport = transport
	cr := newItemsFailureCell("hash-1")

	transport.drainResults = []*mitev1.CommandResult{mkItemsFailureDSR("payments/repos")}
	_ = rec.reconcileController(context.Background(), cr)

	// The push of the first reconcile replaced the tracked hash. The
	// result below answers that desired state, so the hash is set back.
	cr.Status.DesiredStateHash = "hash-1"
	transport.drainResults = []*mitev1.CommandResult{
		mkDSR(true, true, true, true, "", "", "", "", "hash-1"),
	}
	_ = rec.reconcileController(context.Background(), cr)

	if n := itemsFailureCount(cr, "payments"); n != 0 {
		t.Fatalf("payments ItemsApplyFailed warnings = %d after a successful apply, want 0", n)
	}
}

func TestCellItemsApplyPathlessFailureKeepsTenantWarning(t *testing.T) {
	rec := newTestReconcilerWithTokenSigner(newTestClientWithBundle())
	transport := &captureTransport{}
	rec.miteTransport = transport
	cr := newItemsFailureCell("hash-1")
	cr.Status.Cell.TenantWarnings = []v1alpha1.TenantWarning{{
		Tenant: "payments", Reason: v1alpha1.TeamReasonItemsApplyFailed, Message: "create payments/repos: boom",
	}}

	// A parse error names no item. The refused item is still refused, so
	// the entry must stay.
	transport.drainResults = []*mitev1.CommandResult{
		mkDSR(true, true, true, false, "", "", "", "parse items: bad yaml", "hash-1"),
	}
	_ = rec.reconcileController(context.Background(), cr)

	if n := itemsFailureCount(cr, "payments"); n != 1 {
		t.Fatalf("payments ItemsApplyFailed warnings = %d, want the earlier one kept (%+v)", n, cr.Status.Cell.TenantWarnings)
	}
}

func TestCellItemsApplyStaleHashKeepsTenantWarning(t *testing.T) {
	rec := newTestReconcilerWithTokenSigner(newTestClientWithBundle())
	transport := &captureTransport{}
	rec.miteTransport = transport
	cr := newItemsFailureCell("hash-2")
	cr.Status.Cell.TenantWarnings = []v1alpha1.TenantWarning{{
		Tenant: "payments", Reason: v1alpha1.TeamReasonItemsApplyFailed, Message: "earlier failure",
	}}

	// The result answers an earlier desired state. Its hash no longer
	// matches the tracked one, so the warning list must not change.
	transport.drainResults = []*mitev1.CommandResult{mkDSR(true, true, true, true, "", "", "", "", "hash-1")}
	_ = rec.reconcileController(context.Background(), cr)

	if n := itemsFailureCount(cr, "payments"); n != 1 {
		t.Fatalf("payments ItemsApplyFailed warnings = %d, want the earlier one kept (%+v)", n, cr.Status.Cell.TenantWarnings)
	}
}

func TestCellItemsApplyFailureReplacesEveryTenantWarning(t *testing.T) {
	rec := newTestReconcilerWithTokenSigner(newTestClientWithBundle())
	transport := &captureTransport{}
	rec.miteTransport = transport
	cr := newItemsFailureCell("hash-1")
	cr.Status.Cell.TenantWarnings = []v1alpha1.TenantWarning{{
		Tenant: "payments", Reason: v1alpha1.TeamReasonItemsApplyFailed, Message: "create payments/repos: boom",
	}}

	// The walk covers every tenant. A result that refuses only a search item
	// replaces the whole set, so the payments entry clears.
	transport.drainResults = []*mitev1.CommandResult{mkItemsFailureDSR("search/repos")}
	_ = rec.reconcileController(context.Background(), cr)

	if n := itemsFailureCount(cr, "payments"); n != 0 {
		t.Fatalf("payments warnings = %d, want 0 once its items apply (%+v)", n, cr.Status.Cell.TenantWarnings)
	}
	if n := itemsFailureCount(cr, "search"); n != 1 {
		t.Fatalf("search warnings = %d, want 1 (%+v)", n, cr.Status.Cell.TenantWarnings)
	}

	// A successful items apply clears every ItemsApplyFailed entry.
	cr.Status.DesiredStateHash = "hash-1"
	transport.drainResults = []*mitev1.CommandResult{mkDSR(true, true, true, true, "", "", "", "", "hash-1")}
	_ = rec.reconcileController(context.Background(), cr)

	if n := itemsFailureCount(cr, "payments") + itemsFailureCount(cr, "search"); n != 0 {
		t.Fatalf("warnings after success = %d, want 0 (%+v)", n, cr.Status.Cell.TenantWarnings)
	}
}

func TestCellItemsApplyFailureAdvancesHealthyTenant(t *testing.T) {
	rec := newTestReconcilerWithTokenSigner(newTestClientWithBundle())
	transport := &captureTransport{}
	rec.miteTransport = transport
	cr := newItemsFailureCell("hash-1")
	cr.Status.Cell.AppliedTenants = nil

	transport.drainResults = []*mitev1.CommandResult{mkItemsFailureDSR("payments/repos")}
	_ = rec.reconcileController(context.Background(), cr)

	if got := strings.Join(cr.Status.Cell.AppliedTenants, ","); got != "search" {
		t.Fatalf("appliedTenants = %q, want %q", got, "search")
	}
}

func TestCellItemsApplyFailureKeepsRefusedAppliedTenant(t *testing.T) {
	rec := newTestReconcilerWithTokenSigner(newTestClientWithBundle())
	transport := &captureTransport{}
	rec.miteTransport = transport
	cr := newItemsFailureCell("hash-1")
	cr.Status.Cell.AppliedTenants = []string{"payments"}

	transport.drainResults = []*mitev1.CommandResult{mkItemsFailureDSR("payments/repos")}
	_ = rec.reconcileController(context.Background(), cr)

	if got := strings.Join(cr.Status.Cell.AppliedTenants, ","); got != "payments,search" {
		t.Fatalf("appliedTenants = %q, want %q", got, "payments,search")
	}
}

func TestCellItemsApplyFailureStaleHashKeepsAppliedTenants(t *testing.T) {
	rec := newTestReconcilerWithTokenSigner(newTestClientWithBundle())
	transport := &captureTransport{}
	rec.miteTransport = transport
	cr := newItemsFailureCell("hash-2")
	cr.Status.Cell.AppliedTenants = nil

	transport.drainResults = []*mitev1.CommandResult{mkItemsFailureDSR("payments/repos")}
	_ = rec.reconcileController(context.Background(), cr)

	if len(cr.Status.Cell.AppliedTenants) != 0 {
		t.Fatalf("appliedTenants = %v, want none for a result that answers another hash", cr.Status.Cell.AppliedTenants)
	}
}

func TestCellItemsApplyPathlessFailureKeepsAppliedTenants(t *testing.T) {
	rec := newTestReconcilerWithTokenSigner(newTestClientWithBundle())
	transport := &captureTransport{}
	rec.miteTransport = transport
	cr := newItemsFailureCell("hash-1")
	cr.Status.Cell.AppliedTenants = nil

	transport.drainResults = []*mitev1.CommandResult{
		mkDSR(true, true, true, false, "", "", "", "parse items: bad yaml", "hash-1"),
	}
	_ = rec.reconcileController(context.Background(), cr)

	if len(cr.Status.Cell.AppliedTenants) != 0 {
		t.Fatalf("appliedTenants = %v, want none for a result with no item_failures", cr.Status.Cell.AppliedTenants)
	}
}
