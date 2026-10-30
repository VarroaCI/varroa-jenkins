package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// ListTenantPlacements answers no placement row, so a tenant the fake does not
// hold is a plain refusal. memberBadgeReader overrides it for the member cases.
func (f *fakeRecordReader) ListTenantPlacements(context.Context, string) ([]recordstore.TenantPlacement, error) {
	return nil, nil
}

// TenantBadgeToken answers no stored token, so a member lookup without a
// placement row refuses.
func (f *fakeRecordReader) TenantBadgeToken(context.Context, string, string) (string, error) {
	return "", nil
}

// memberBadgeReader serves the placement rows and the stored badge token of a
// tenant that a member cluster holds. The embedded fake answers the runs.
type memberBadgeReader struct {
	*fakeRecordReader
	rows  []recordstore.TenantPlacement
	token string
}

func (m *memberBadgeReader) ListTenantPlacements(context.Context, string) ([]recordstore.TenantPlacement, error) {
	return m.rows, nil
}

func (m *memberBadgeReader) TenantBadgeToken(context.Context, string, string) (string, error) {
	return m.token, nil
}

// eastRow is one live placement row of payments on the member cluster east.
func eastRow(generation int64) recordstore.TenantPlacement {
	return recordstore.TenantPlacement{
		Cluster: "east", Tenant: "payments", Namespace: "cells", Cell: "ctrl-e", Generation: generation,
	}
}

// memberBadgeReaderWith builds a reader with one run that fails, and the given
// rows and stored token.
func memberBadgeReaderWith(rows []recordstore.TenantPlacement, token string) *memberBadgeReader {
	fake := newFakeRecordReader()
	run := badgeRun("payments/checkout-api", "COMPLETED", badgeStrPtr("FAILURE"))
	run.Cluster = "east"
	fake.addRun(run)
	return &memberBadgeReader{fakeRecordReader: fake, rows: rows, token: token}
}

// unknownBadgeBody is the body that every refusal answers. A test compares a
// refusal to it.
func unknownBadgeBody(t *testing.T) []byte {
	t.Helper()
	mux := badgeDoor(t, nil, memberBadgeReaderWith(nil, ""))
	rec := badgeGet(mux, "/badges/payments/"+badgeTestToken+"/checkout-api.svg")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown tenant status = %d, want 404", rec.Code)
	}
	return rec.Body.Bytes()
}

func TestJobBadge_MemberPlacedServesFromPlacementRow(t *testing.T) {
	reader := memberBadgeReaderWith([]recordstore.TenantPlacement{eastRow(3)}, badgeTestToken)
	mux := badgeDoor(t, nil, reader)

	rec := badgeGet(mux, "/badges/payments/"+badgeTestToken+"/checkout-api.svg")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, ">failing<") {
		t.Errorf("badge does not show the failing result: %s", body)
	}
}

func TestJobBadge_MemberPlacedWrongTokenIdentical404(t *testing.T) {
	want := unknownBadgeBody(t)
	reader := memberBadgeReaderWith([]recordstore.TenantPlacement{eastRow(3)}, badgeTestToken)
	mux := badgeDoor(t, nil, reader)

	rec := badgeGet(mux, "/badges/payments/fedcba9876543210fedcba9876543210/checkout-api.svg")
	if rec.Code != http.StatusNotFound || rec.Body.String() != string(want) {
		t.Errorf("status = %d, body = %q, want the identical 404", rec.Code, rec.Body.String())
	}
}

func TestJobBadge_MemberPlacedSwitchOffIs404(t *testing.T) {
	want := unknownBadgeBody(t)
	// The record store holds no token while the switch is off.
	reader := memberBadgeReaderWith([]recordstore.TenantPlacement{eastRow(3)}, "")
	mux := badgeDoor(t, nil, reader)

	rec := badgeGet(mux, "/badges/payments/"+badgeTestToken+"/checkout-api.svg")
	if rec.Code != http.StatusNotFound || rec.Body.String() != string(want) {
		t.Errorf("status = %d, want the identical 404", rec.Code)
	}
}

func TestJobBadge_MemberPlacedGenerationZeroIs404(t *testing.T) {
	want := unknownBadgeBody(t)
	reader := memberBadgeReaderWith([]recordstore.TenantPlacement{eastRow(0)}, badgeTestToken)
	mux := badgeDoor(t, nil, reader)

	rec := badgeGet(mux, "/badges/payments/"+badgeTestToken+"/checkout-api.svg")
	if rec.Code != http.StatusNotFound || rec.Body.String() != string(want) {
		t.Errorf("status = %d, want the identical 404", rec.Code)
	}
}

func TestJobBadge_MemberPlacedOnTwoClustersIs404(t *testing.T) {
	want := unknownBadgeBody(t)
	second := eastRow(2)
	second.Cluster = "west"
	reader := memberBadgeReaderWith([]recordstore.TenantPlacement{eastRow(3), second}, badgeTestToken)
	mux := badgeDoor(t, nil, reader)

	rec := badgeGet(mux, "/badges/payments/"+badgeTestToken+"/checkout-api.svg")
	if rec.Code != http.StatusNotFound || rec.Body.String() != string(want) {
		t.Errorf("status = %d, want the identical 404", rec.Code)
	}
}

// TestGetJobBadge_MemberPlacedReturnsPath asserts the job-page card of a tenant
// on a member cluster reads the stored token and returns the badge path.
func TestGetJobBadge_MemberPlacedReturnsPath(t *testing.T) {
	client := newFakeResourceClient() // no local Tenant: the tenant is member-placed
	authz := NewAuthorizer(testResolver(pipelinesRoles(), []*v1alpha1.VarroaRoleBinding{
		pipelineBinding("badge", "payments"),
	}), false)
	reader := memberBadgeReaderWith([]recordstore.TenantPlacement{eastRow(3)}, badgeTestToken)
	srv := pipelinesTestServer(client, authz, reader, nil)
	// The resolver reads the member placement row through the same reader.
	srv.deps.TenantResolver = NewTenantTenantResolver(storeFromFake(client), newFakeBrood(client), authz,
		WithMemberPlacements(reader))

	rec := doPipelinesRequest(t, srv.handleGetJobBadge, "/pipelines/payments/jobs/payments%2Fcheckout-api/badge",
		map[string]string{"tenant": "payments", "job": "payments/checkout-api"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out jobBadgeJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := "/badges/payments/" + badgeTestToken + "/checkout-api.svg"
	if !out.Enabled || out.Path != want {
		t.Errorf("got %+v, want enabled with path %q", out, want)
	}
}
