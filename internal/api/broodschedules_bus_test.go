package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

func TestNewBusBroodSchedules(t *testing.T) {
	svc := NewBusBroodSchedules("core", nil, nil, slog.Default())
	if svc == nil {
		t.Fatal("NewBusBroodSchedules returned nil")
	}
}

func TestBusBroodSchedules_Create(t *testing.T) {
	spec := v1alpha1.BroodScheduleSpec{
		Schedule: "*/5 * * * *",
		Template: v1alpha1.BroodScheduleTemplate{
			Targets: v1alpha1.BroodTargets{Names: []string{"ctrl-1"}},
			Action:  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
		},
	}
	specRaw, _ := json.Marshal(spec)
	resp := bus.BroodScheduleResponse{
		Namespace: "test-ns",
		Name:      "test-sched",
		Spec:      specRaw,
	}
	respData, _ := json.Marshal(resp)

	f := &busBroodSchedules{
		localCluster: "core",
		logger:       slog.Default(),
		request: func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
			if timeout != broodSchedulesCreateTimeout {
				t.Errorf("timeout = %v, want %v", timeout, broodSchedulesCreateTimeout)
			}
			return respData, nil
		},
	}

	got, err := f.Create(context.Background(), "", "test-ns", "test-sched", spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Namespace != "test-ns" || got.Name != "test-sched" {
		t.Errorf("got Namespace=%q Name=%q, want test-ns/test-sched", got.Namespace, got.Name)
	}
	if got.Cluster != "core" {
		t.Errorf("Cluster = %q, want core", got.Cluster)
	}
}

func TestBusBroodSchedules_Get(t *testing.T) {
	resp := bus.BroodScheduleResponse{
		Namespace: "ns1",
		Name:      "sched1",
	}
	respData, _ := json.Marshal(resp)

	f := &busBroodSchedules{
		localCluster: "core",
		logger:       slog.Default(),
		request: func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
			if timeout != broodSchedulesGetTimeout {
				t.Errorf("timeout = %v, want %v", timeout, broodSchedulesGetTimeout)
			}
			return respData, nil
		},
	}

	got, err := f.Get(context.Background(), "", "ns1", "sched1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Namespace != "ns1" || got.Name != "sched1" {
		t.Errorf("got Namespace=%q Name=%q", got.Namespace, got.Name)
	}
}

func TestBusBroodSchedules_GetWithError(t *testing.T) {
	resp := bus.BroodScheduleResponse{Error: "not found"}
	respData, _ := json.Marshal(resp)

	f := &busBroodSchedules{
		localCluster: "core",
		logger:       slog.Default(),
		request: func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
			return respData, nil
		},
	}

	_, err := f.Get(context.Background(), "", "ns1", "nonexistent")
	if err == nil {
		t.Fatal("expected error for not found")
	}
}

func TestBusBroodSchedules_List(t *testing.T) {
	items := []bus.BroodScheduleResponse{
		{Namespace: "ns1", Name: "sched-a"},
		{Namespace: "ns1", Name: "sched-b"},
	}
	resp := bus.BroodScheduleListResponse{Items: items}
	respData, _ := json.Marshal(resp)

	f := &busBroodSchedules{
		localCluster: "core",
		logger:       slog.Default(),
		request: func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
			if timeout != broodSchedulesListTimeout {
				t.Errorf("timeout = %v, want %v", timeout, broodSchedulesListTimeout)
			}
			return respData, nil
		},
		membership: nil,
	}

	got, err := f.List(context.Background(), "ns1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2", len(got))
	}
	if got[0].Name != "sched-a" || got[1].Name != "sched-b" {
		t.Errorf("items = %+v", got)
	}
	// Cluster field should be filled by listCluster.
	for _, item := range got {
		if item.Cluster != "core" {
			t.Errorf("Cluster = %q, want core", item.Cluster)
		}
	}
}

func TestBusBroodSchedules_Delete(t *testing.T) {
	resp := bus.BroodScheduleResponse{}
	respData, _ := json.Marshal(resp)

	f := &busBroodSchedules{
		localCluster: "core",
		logger:       slog.Default(),
		request: func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
			if timeout != broodSchedulesDeleteTimeout {
				t.Errorf("timeout = %v, want %v", timeout, broodSchedulesDeleteTimeout)
			}
			return respData, nil
		},
	}

	if err := f.Delete(context.Background(), "", "ns1", "sched1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestBusBroodSchedules_Suspend(t *testing.T) {
	resp := bus.BroodScheduleResponse{}
	respData, _ := json.Marshal(resp)

	f := &busBroodSchedules{
		localCluster: "core",
		logger:       slog.Default(),
		request: func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
			if timeout != broodSchedulesSuspendTimeout {
				t.Errorf("timeout = %v, want %v", timeout, broodSchedulesSuspendTimeout)
			}
			var req bus.BroodScheduleSuspendRequest
			if err := json.Unmarshal(data, &req); err != nil {
				t.Fatalf("unmarshal request: %v", err)
			}
			if !req.Suspend {
				t.Errorf("Suspend = false, want true")
			}
			return respData, nil
		},
	}

	if err := f.Suspend(context.Background(), "", "ns1", "sched1", true); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
}

func TestBusBroodSchedules_SuspendResume(t *testing.T) {
	resp := bus.BroodScheduleResponse{}
	respData, _ := json.Marshal(resp)

	f := &busBroodSchedules{
		localCluster: "core",
		logger:       slog.Default(),
		request: func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
			var req bus.BroodScheduleSuspendRequest
			if err := json.Unmarshal(data, &req); err != nil {
				t.Fatalf("unmarshal request: %v", err)
			}
			if req.Suspend {
				t.Errorf("Suspend = true, want false")
			}
			return respData, nil
		},
	}

	if err := f.Suspend(context.Background(), "", "ns1", "sched1", false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
}
