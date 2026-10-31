package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/openapi"
	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// memberGrantsBrood records the cells.grants calls and answers a canned
// reply, so a test can tell which resolver answered a member cell.
type memberGrantsBrood struct {
	ConfigBrood
	cluster string
	tenants []string
	calls   int
	reply   []bus.CellTenantGrants
	err     error
}

func (f *memberGrantsBrood) CellLint(context.Context, string, bus.CellLintRequest) (bus.CellLintResponse, error) {
	return bus.CellLintResponse{}, errNoCellPools
}

func (f *memberGrantsBrood) CellGrants(_ context.Context, cluster string, tenants []string) ([]bus.CellTenantGrants, error) {
	f.calls++
	f.cluster = cluster
	f.tenants = tenants
	return f.reply, f.err
}

// memberCellGrantsServer wires the cell grants fixture with a cluster-wide
// binding on the core. A member cell must not report that binding.
func memberCellGrantsServer(brood ConfigBrood) *Server {
	fleet := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-writers"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "everyone"}},
			RoleRef:  "pipelines-developer",
		},
	}
	srv := cellGrantsServer(cellGrantsBindings(fleet))
	srv.deps.ConfigBrood = brood
	return srv
}

func TestCellGrantsAsksTheMemberOperatorForAMemberCell(t *testing.T) {
	canned := []bus.CellTenantGrants{{
		Tenant:   "acme",
		ItemRole: "varroa:tenant:acme",
		Grants: []bus.CellGrant{{
			Source:        "federatedBinding",
			Binding:       "varroa-fed-tenant-acme",
			RoleRef:       "pipelines-developer",
			Subjects:      []bus.CellGrantSubject{{Kind: "User", Name: "alice"}},
			Verbs:         []string{"write"},
			FederatedFrom: []string{"acme-writers"},
		}},
		MemberRoles: []bus.CellMemberRole{},
	}}
	brood := &memberGrantsBrood{reply: canned}
	srv := memberCellGrantsServer(brood)

	out, serr := srv.CellGrants(context.Background(), cellOperatorClaims, "member-c", "ns-a", "cell-a")
	if serr != nil {
		t.Fatalf("CellGrants: %+v", serr)
	}
	if brood.calls != 1 || brood.cluster != "member-c" {
		t.Fatalf("calls = %d cluster = %q, want one call to member-c", brood.calls, brood.cluster)
	}
	if len(brood.tenants) != 2 || brood.tenants[0] != "acme" || brood.tenants[1] != "beta" {
		t.Errorf("tenants = %v, want the cell's tenants in order", brood.tenants)
	}
	if len(out.Tenants) != 1 || out.Tenants[0].Tenant != "acme" {
		t.Fatalf("tenants = %+v, want the member reply verbatim", out.Tenants)
	}
	for _, tenant := range out.Tenants {
		for _, g := range tenant.Grants {
			if g.Binding == "fleet-writers" {
				t.Errorf("the core cluster-wide binding leaked into a member cell answer: %+v", g)
			}
		}
	}
	if got := out.Tenants[0].Grants[0].FederatedFrom; len(got) != 1 || got[0] != "acme-writers" {
		t.Errorf("federatedFrom = %v, want acme-writers", got)
	}
}

func TestCellGrantsMemberUnreachableAnswers502(t *testing.T) {
	brood := &memberGrantsBrood{err: &ErrClusterUnreachable{Cluster: "member-c", Err: errors.New("no responders")}}
	srv := memberCellGrantsServer(brood)

	rec := doCellRequest(t, srv.handleCellGrants, "/cells/ns-a/cell-a/grants?cluster=member-c",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellOperatorClaims)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCellGrantsKeepsTheMemberAllTenantsFlag pins the member flag on a
// federated all-tenants copy. The reply must validate against the
// CellGrant schema, so the flag must be in the spec.
func TestCellGrantsKeepsTheMemberAllTenantsFlag(t *testing.T) {
	allTenantsCopy := bus.CellGrant{
		Source:        "federatedBinding",
		Binding:       "varroa-fed-tenant-all-abc",
		RoleRef:       "pipelines-writer",
		Subjects:      []bus.CellGrantSubject{{Kind: "User", Name: "alice"}},
		Verbs:         []string{"write"},
		FederatedFrom: []string{"fleet-writers"},
		AllTenants:    true,
	}
	canned := []bus.CellTenantGrants{
		{Tenant: "acme", ItemRole: "varroa:tenant:acme", Grants: []bus.CellGrant{allTenantsCopy}, MemberRoles: []bus.CellMemberRole{}},
		{Tenant: "beta", ItemRole: "varroa:tenant:beta", Grants: []bus.CellGrant{allTenantsCopy}, MemberRoles: []bus.CellMemberRole{}},
	}
	brood := &memberGrantsBrood{reply: canned}
	srv := memberCellGrantsServer(brood)

	rec := doCellRequest(t, srv.handleCellGrants, "/cells/ns-a/cell-a/grants?cluster=member-c",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellOperatorClaims)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"allTenants":true`) {
		t.Fatalf("body %s lacks allTenants:true", rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if err := cellGrantsSchema(t).VisitJSON(body); err != nil {
		t.Fatalf("body does not match the spec: %v", err)
	}
}

// cellGrantsSchema answers the 200 body schema of the cell grants route
// from the embedded spec.
func cellGrantsSchema(t *testing.T) *openapi3.Schema {
	t.Helper()
	doc, err := (&openapi3.Loader{IsExternalRefsAllowed: false}).LoadFromData(openapi.SpecJSON)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	item := doc.Paths.Find("/api/v1/cells/{namespace}/{name}/grants")
	if item == nil || item.Get == nil {
		t.Fatal("spec has no cell grants operation")
	}
	resp := item.Get.Responses.Status(http.StatusOK)
	if resp == nil || resp.Value == nil {
		t.Fatal("cell grants operation declares no 200 response")
	}
	return resp.Value.Content.Get("application/json").Schema.Value
}

func TestCellGrantsMemberWithoutConfigBroodAnswers502(t *testing.T) {
	srv := memberCellGrantsServer(nil)

	rec := doCellRequest(t, srv.handleCellGrants, "/cells/ns-a/cell-a/grants?cluster=member-c",
		map[string]string{"namespace": "ns-a", "name": "cell-a"}, cellOperatorClaims)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}
}
