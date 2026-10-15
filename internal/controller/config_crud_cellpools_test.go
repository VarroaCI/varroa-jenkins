package controller

import (
	"context"
	"encoding/json"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func cellPoolCRUD(t *testing.T) (*ConfigCRUD, *crdstore.Fake) {
	t.Helper()
	store := crdstore.NewFake()
	crdstore.MustSeed(store,
		&v1alpha1.CellPool{
			ObjectMeta: metav1.ObjectMeta{Name: "member-2-570"},
			Spec:       v1alpha1.CellPoolSpec{ProfileRef: "lts-2-570", Warm: 1, MaxCells: 4, MaxTenantsPerCell: 4},
		},
		&v1alpha1.CellPool{
			ObjectMeta: metav1.ObjectMeta{Name: "other"},
			Spec:       v1alpha1.CellPoolSpec{ProfileRef: "lts-2-570", MaxTenantsPerCell: 10},
		},
	)
	return &ConfigCRUD{Store: store, Logger: testLogger()}, store
}

func TestHandleCellPoolsList(t *testing.T) {
	c, _ := cellPoolCRUD(t)
	var resp bus.ConfigListResponse
	if err := json.Unmarshal(c.HandleCellPoolsList([]byte(`{}`)), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Code != "" || len(resp.Items) != 2 {
		t.Fatalf("resp = %+v, want two items and no code", resp)
	}
	var pool v1alpha1.CellPool
	if err := json.Unmarshal(resp.Items[0], &pool); err != nil {
		t.Fatalf("item is not a CellPool: %v", err)
	}
}

func TestHandleCellPoolsGet(t *testing.T) {
	c, _ := cellPoolCRUD(t)
	var resp bus.ConfigGetResponse
	_ = json.Unmarshal(c.HandleCellPoolsGet(mustMarshal(t, bus.ConfigGetRequest{Name: "member-2-570"})), &resp)
	var pool v1alpha1.CellPool
	if err := json.Unmarshal(resp.Item, &pool); err != nil || pool.Name != "member-2-570" {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}

	resp = bus.ConfigGetResponse{}
	_ = json.Unmarshal(c.HandleCellPoolsGet(mustMarshal(t, bus.ConfigGetRequest{Name: "nope"})), &resp)
	if resp.Code != bus.CodeNotFound {
		t.Errorf("missing pool code = %q, want %q", resp.Code, bus.CodeNotFound)
	}
}

func TestHandleCellPoolsScale_WritesTheDial(t *testing.T) {
	c, store := cellPoolCRUD(t)
	warm := int32(2)
	var resp bus.ConfigGetResponse
	_ = json.Unmarshal(c.HandleCellPoolsScale(mustMarshal(t, bus.CellPoolScaleRequest{Name: "member-2-570", Warm: &warm})), &resp)
	if resp.Code != "" {
		t.Fatalf("code = %q (%s), want none", resp.Code, resp.Error)
	}
	var pool v1alpha1.CellPool
	if err := json.Unmarshal(resp.Item, &pool); err != nil || pool.Spec.Warm != 2 {
		t.Fatalf("reply pool = %+v, err = %v", pool.Spec, err)
	}
	stored, _ := crdstore.Get[v1alpha1.CellPool](context.Background(), store, "member-2-570", "")
	if stored.Spec.Warm != 2 || stored.Spec.MaxCells != 4 || stored.Spec.ProfileRef != "lts-2-570" {
		t.Errorf("stored spec = %+v", stored.Spec)
	}
}

func TestHandleCellPoolsScale_Codes(t *testing.T) {
	neg := int32(-1)
	one := int32(1)
	tests := []struct {
		name string
		body []byte
		want string
	}{
		{"undecodable body", []byte(`{`), bus.CodeInvalid},
		{"no dial", []byte(`{"name":"member-2-570"}`), bus.CodeInvalid},
		{"structural key only", []byte(`{"name":"member-2-570","profileRef":"x"}`), bus.CodeInvalid},
		{"out of bounds", mustMarshal(t, bus.CellPoolScaleRequest{Name: "member-2-570", Warm: &neg}), bus.CodeInvalid},
		{"missing pool", mustMarshal(t, bus.CellPoolScaleRequest{Name: "nope", Warm: &one}), bus.CodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := cellPoolCRUD(t)
			var resp bus.ConfigGetResponse
			_ = json.Unmarshal(c.HandleCellPoolsScale(tt.body), &resp)
			if resp.Code != tt.want {
				t.Errorf("code = %q (%s), want %q", resp.Code, resp.Error, tt.want)
			}
		})
	}
}

func TestHandleCellPoolsScale_Conflict(t *testing.T) {
	c, store := cellPoolCRUD(t)
	gvr := schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "cellpools"}
	store.FailAlways("update", gvr, apierrors.NewConflict(
		schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}, "member-2-570", nil))
	warm := int32(2)
	var resp bus.ConfigGetResponse
	_ = json.Unmarshal(c.HandleCellPoolsScale(mustMarshal(t, bus.CellPoolScaleRequest{Name: "member-2-570", Warm: &warm})), &resp)
	if resp.Code != bus.CodeConflict {
		t.Errorf("code = %q, want %q", resp.Code, bus.CodeConflict)
	}
}
