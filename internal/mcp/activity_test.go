package mcp

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

type recordingSink struct{ events []activity.Event }

func (r *recordingSink) Publish(e activity.Event) { r.events = append(r.events, e) }

func TestEmitActivity_StampsActorSourceSeverity(t *testing.T) {
	sink := &recordingSink{}
	deps := &api.Dependencies{ActivityPublisher: sink}
	claims := &auth.Claims{PreferredUsername: "alice", Subject: "uuid-1"}

	emitActivity(deps, claims, activity.Event{
		Type:       "controller.updated",
		Message:    "Controller ctrl1 updated in team-a",
		Namespace:  "team-a",
		Controller: "ctrl1",
	})

	if len(sink.events) != 1 {
		t.Fatalf("got %d events, want 1", len(sink.events))
	}
	e := sink.events[0]
	if e.Actor != "alice" {
		t.Errorf("Actor = %q, want alice", e.Actor)
	}
	if e.Source != "mcp" {
		t.Errorf("Source = %q, want mcp", e.Source)
	}
	if e.Severity != "info" {
		t.Errorf("Severity = %q, want info", e.Severity)
	}
	if e.Type != "controller.updated" {
		t.Errorf("Type = %q, want controller.updated", e.Type)
	}
}

func TestEmitActivity_NilPublisherIsNoop(t *testing.T) {
	deps := &api.Dependencies{ActivityPublisher: nil}
	emitActivity(deps, &auth.Claims{Subject: "s"}, activity.Event{Type: "x.y"})
	emitActivity(nil, nil, activity.Event{Type: "x.y"})
	// Reaching here without panicking is the assertion.
}

func TestEmitActivity_ExplicitSeverityWins(t *testing.T) {
	sink := &recordingSink{}
	deps := &api.Dependencies{ActivityPublisher: sink}

	emitActivity(deps, &auth.Claims{Subject: "s"}, activity.Event{
		Type: "controller.deleted", Message: "gone", Severity: "warning",
	})

	if sink.events[0].Severity != "warning" {
		t.Errorf("Severity = %q, want warning", sink.events[0].Severity)
	}
}

// TestEmitActivityForCluster_StampsTarget covers the cross-cluster audit case:
// the BFF serves every cluster in the brood, so a mutation targeting a remote
// cluster must be recorded under that cluster or the record is false and
// cluster-filtered queries miss it.
func TestEmitActivityForCluster_StampsTarget(t *testing.T) {
	for _, tc := range []struct{ name, cluster string }{
		{"remote target", "hive"},
		{"local target", "local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordingSink{}
			deps := &api.Dependencies{ActivityPublisher: sink, Brood: &recordingBrood{}}

			emitActivityForCluster(deps, &auth.Claims{Subject: "s"}, tc.cluster, activity.Event{
				Type:    "controller.created",
				Message: "Controller c1 created in team-a",
			})

			if len(sink.events) != 1 {
				t.Fatalf("got %d events, want 1", len(sink.events))
			}
			if got := sink.events[0].Cluster; got != tc.cluster {
				t.Errorf("Cluster = %q, want %q", got, tc.cluster)
			}
		})
	}
}

// TestStoreOnlyFallback_DoesNotStampCallerCluster guards the inverse of the
// cross-cluster fix. The store-only path writes to the local crdstore whatever
// `cluster` the caller passed, so stamping that argument would invent a
// mutation on a cluster never touched — a false audit record in the other
// direction.
func TestStoreOnlyFallback_DoesNotStampCallerCluster(t *testing.T) {
	sink := &recordingSink{}
	deps := &api.Dependencies{ActivityPublisher: sink} // Brood nil => store-only

	emitActivity(deps, &auth.Claims{Subject: "s"}, activity.Event{
		Type:    "controller.created",
		Message: "Controller c1 created in team-a",
	})

	if len(sink.events) != 1 {
		t.Fatalf("got %d events, want 1", len(sink.events))
	}
	if got := sink.events[0].Cluster; got != "" {
		t.Errorf("Cluster = %q, want empty so Publish stamps the local identity", got)
	}
}
