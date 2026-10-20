package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

func TestBusConfigBroodCellGrantsSubjectAndTimeout(t *testing.T) {
	f, calls := cellPoolConfigBrood(func(string, []byte) ([]byte, error) {
		return json.Marshal(bus.CellGrantsResponse{Tenants: []bus.CellTenantGrants{{Tenant: "acme"}}})
	})
	tenants, err := f.CellGrants(context.Background(), "member-c", []string{"acme", "beta"})
	if err != nil {
		t.Fatalf("grants: %v", err)
	}
	if len(tenants) != 1 || tenants[0].Tenant != "acme" {
		t.Errorf("tenants = %+v, want the member reply", tenants)
	}
	call := (*calls)[0]
	if call.subject != "operator.member-c.cells.grants" || call.timeout != configCellGrantsTimeout {
		t.Errorf("call = %s %v", call.subject, call.timeout)
	}
	var sent bus.CellGrantsRequest
	if err := json.Unmarshal(call.data, &sent); err != nil || len(sent.Tenants) != 2 || sent.Tenants[1] != "beta" {
		t.Errorf("payload = %s, want the tenants in order", call.data)
	}
}

func TestBusConfigBroodCellGrantsRefusesLocal(t *testing.T) {
	f, calls := cellPoolConfigBrood(func(string, []byte) ([]byte, error) {
		t.Fatal("a local-cluster call must send no request")
		return nil, nil
	})
	if _, err := f.CellGrants(context.Background(), "core", []string{"acme"}); err == nil {
		t.Error("grants on the local cluster must fail")
	}
	if len(*calls) != 0 {
		t.Errorf("calls = %d, want 0", len(*calls))
	}
}

func TestBusConfigBroodCellGrantsMapsCode(t *testing.T) {
	f, _ := cellPoolConfigBrood(func(string, []byte) ([]byte, error) {
		return json.Marshal(bus.CellGrantsResponse{Code: bus.CodeNotFound, Error: "no such tenant"})
	})
	_, err := f.CellGrants(context.Background(), "member-c", []string{"acme"})
	var be *BroodError
	if !errors.As(err, &be) || be.Code != bus.CodeNotFound || be.Msg != "no such tenant" {
		t.Errorf("err = %v, want a not-found BroodError", err)
	}
}
