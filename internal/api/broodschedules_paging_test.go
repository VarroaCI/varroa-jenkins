package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// replyingBroodSchedules returns a busBroodSchedules whose brood-schedule list
// answers the given replies in order. It records the After key of each request.
func replyingBroodSchedules(replies ...bus.BroodScheduleListResponse) (*busBroodSchedules, *[]string) {
	var afters []string
	i := 0
	f := &busBroodSchedules{
		localCluster: "core",
		logger:       slog.Default(),
		request: func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
			var req bus.BroodScheduleListRequest
			_ = json.Unmarshal(data, &req)
			afters = append(afters, req.After)
			reply := replies[i]
			i++
			return json.Marshal(reply)
		},
	}
	return f, &afters
}

func schedJSON(name string) json.RawMessage {
	return json.RawMessage(`{"namespace":"team-ns","name":"` + name + `"}`)
}

func TestBroodSchedules_ListClusterPages(t *testing.T) {
	f, afters := replyingBroodSchedules(
		bus.BroodScheduleListResponse{Items: []json.RawMessage{schedJSON("a")}, Next: "team-ns/a"},
		bus.BroodScheduleListResponse{Items: []json.RawMessage{schedJSON("b")}},
	)
	items, statuses, err := f.listCluster(context.Background(), "member-b", "team-ns")
	if err != nil {
		t.Fatalf("listCluster: %v", err)
	}
	if len(items) != 2 || items[0].Name != "a" || items[1].Name != "b" {
		t.Fatalf("items = %+v, want a and b", items)
	}
	for _, it := range items {
		if it.Cluster != "member-b" {
			t.Fatalf("Cluster = %q, want member-b", it.Cluster)
		}
	}
	if len(statuses) != 1 || !statuses[0].OK {
		t.Fatalf("statuses = %+v, want one ok row", statuses)
	}
	if len(*afters) != 2 || (*afters)[1] != "team-ns/a" {
		t.Fatalf("after values = %v", *afters)
	}
}

func TestBroodSchedules_ListClusterPageFailureDropsMember(t *testing.T) {
	f, _ := replyingBroodSchedules(
		bus.BroodScheduleListResponse{Items: []json.RawMessage{schedJSON("a")}, Next: "team-ns/a"},
		bus.BroodScheduleListResponse{Error: "boom"},
	)
	items, statuses, err := f.listCluster(context.Background(), "member-b", "team-ns")
	if err == nil {
		t.Fatal("expected an error for the failed member")
	}
	if len(items) != 0 {
		t.Fatalf("items = %+v, want none from member-b", items)
	}
	if len(statuses) != 1 || statuses[0] != (ClusterFanoutStatus{Name: "member-b", OK: false, Error: "boom"}) {
		t.Fatalf("statuses = %+v, want one failed row for member-b with reason boom", statuses)
	}
}

func TestBroodSchedules_ListClusterUnreachable(t *testing.T) {
	f := &busBroodSchedules{
		localCluster: "core",
		logger:       slog.Default(),
		request: func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
			return nil, errors.New("no responders")
		},
	}
	items, statuses, err := f.listCluster(context.Background(), "member-b", "team-ns")
	if err == nil {
		t.Fatal("expected an error for an unreachable member")
	}
	if len(items) != 0 {
		t.Fatalf("items = %+v, want none", items)
	}
	if len(statuses) != 1 || statuses[0] != (ClusterFanoutStatus{Name: "member-b", OK: false, Error: "cluster unreachable"}) {
		t.Fatalf("statuses = %+v, want one cluster unreachable row", statuses)
	}
}

func TestBroodSchedules_ListClusterSkipsUndecodableRow(t *testing.T) {
	f, _ := replyingBroodSchedules(
		bus.BroodScheduleListResponse{Items: []json.RawMessage{
			schedJSON("a"),
			json.RawMessage(`"not-an-object"`),
			schedJSON("c"),
		}},
	)
	items, _, err := f.listCluster(context.Background(), "member-b", "team-ns")
	if err != nil {
		t.Fatalf("listCluster: %v", err)
	}
	if len(items) != 2 || items[0].Name != "a" || items[1].Name != "c" {
		t.Fatalf("items = %+v, want a and c", items)
	}
}

func TestBroodSchedules_ListReturnsStatusRow(t *testing.T) {
	f := &busBroodSchedules{
		localCluster: "core",
		logger:       slog.Default(),
		request: func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
			return json.Marshal(bus.BroodScheduleListResponse{Error: "boom"})
		},
	}
	items, statuses, err := f.List(context.Background(), "team-ns")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %+v, want none", items)
	}
	if len(statuses) != 1 || statuses[0] != (ClusterFanoutStatus{Name: "core", OK: false, Error: "boom"}) {
		t.Fatalf("statuses = %+v, want one failed row for core", statuses)
	}
}
