package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// replyingBroodOps returns a busBroodOps whose broodops.list answers the
// given replies in order. Each reply is marshalled by the test.
func replyingBroodOps(replies ...bus.BroodOpsListResponse) (*busBroodOps, *[]string) {
	var afters []string
	i := 0
	f := &busBroodOps{
		localCluster: "core",
		logger:       slog.Default(),
		request: func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
			var req bus.BroodOpsListRequest
			_ = json.Unmarshal(data, &req)
			afters = append(afters, req.After)
			reply := replies[i]
			i++
			return json.Marshal(reply)
		},
	}
	return f, &afters
}

func opJSON(name string) json.RawMessage {
	return json.RawMessage(`{"metadata":{"name":"` + name + `","namespace":"team-ns"}}`)
}

func TestBroodOps_ListRemotePages(t *testing.T) {
	f, afters := replyingBroodOps(
		bus.BroodOpsListResponse{Ops: []json.RawMessage{opJSON("a")}, Next: "team-ns/a"},
		bus.BroodOpsListResponse{Ops: []json.RawMessage{opJSON("b")}},
	)
	ops, statuses, err := f.List(context.Background(), "team-ns", "member-b")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ops) != 2 || ops[0].Op.Name != "a" || ops[1].Op.Name != "b" {
		t.Fatalf("ops = %+v, want a and b", ops)
	}
	if len(statuses) != 1 || !statuses[0].OK {
		t.Fatalf("statuses = %+v, want one ok row", statuses)
	}
	if len(*afters) != 2 || (*afters)[1] != "team-ns/a" {
		t.Fatalf("after values = %v", *afters)
	}
}

func TestBroodOps_ListRemotePageFailureDropsMember(t *testing.T) {
	f, _ := replyingBroodOps(
		bus.BroodOpsListResponse{Ops: []json.RawMessage{opJSON("a")}, Next: "team-ns/a"},
		bus.BroodOpsListResponse{Code: bus.CodeInternal, Error: "boom"},
	)
	ops, statuses, err := f.List(context.Background(), "team-ns", "member-b")
	if err == nil {
		t.Fatal("expected an error for the failed member")
	}
	if len(ops) != 0 {
		t.Fatalf("ops = %+v, want none from member-b", ops)
	}
	if len(statuses) != 1 || statuses[0].Name != "member-b" || statuses[0].OK || statuses[0].Error != "boom" {
		t.Fatalf("statuses = %+v, want one failed row for member-b with reason boom", statuses)
	}
}

func TestBroodOps_ListRemoteSkipsUndecodableOp(t *testing.T) {
	f, _ := replyingBroodOps(
		bus.BroodOpsListResponse{Ops: []json.RawMessage{
			opJSON("a"),
			json.RawMessage(`"not-an-object"`),
			opJSON("c"),
		}},
	)
	ops, _, err := f.List(context.Background(), "team-ns", "member-b")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("ops = %d, want 2", len(ops))
	}
	for i, row := range ops {
		if row.Op == nil {
			t.Fatalf("row %d has a nil Op", i)
		}
	}
	if ops[0].Op.Name != "a" || ops[1].Op.Name != "c" {
		t.Fatalf("ops = %s, %s; want a and c", ops[0].Op.Name, ops[1].Op.Name)
	}
}
