package activity

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// recordingSink captures published events for assertions.
type recordingSink struct{ events []Event }

func (r *recordingSink) Publish(e Event) { r.events = append(r.events, e) }

func TestPublisherSatisfiesEventSink(t *testing.T) {
	var _ EventSink = (*Publisher)(nil)
	var _ EventSink = (*recordingSink)(nil)

	var sink EventSink = &recordingSink{}
	sink.Publish(Event{Type: "controller.created", Message: "hi"})

	rec := sink.(*recordingSink)
	if len(rec.events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.events))
	}
	if rec.events[0].Type != "controller.created" {
		t.Errorf("Type = %q, want controller.created", rec.events[0].Type)
	}
}

func TestRouteSubject(t *testing.T) {
	p := &Publisher{cluster: bus.DefaultCluster} // no conn needed for routeSubject

	tests := []struct {
		name  string
		event Event
		want  string
	}{
		{
			name:  "controller-scoped event",
			event: Event{Controller: "foo", Namespace: "team-a"},
			want:  "activity.core.team-a.foo",
		},
		{
			name:  "empty controller routes to global",
			event: Event{Controller: "", Namespace: "team-a"},
			want:  "activity.core._global",
		},
		{
			name:  "empty namespace with controller routes to global (programming error)",
			event: Event{Controller: "foo", Namespace: ""},
			want:  "activity.core._global",
		},
		{
			name:  "both empty routes to global",
			event: Event{},
			want:  "activity.core._global",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.routeSubject(tt.event)
			if got != tt.want {
				t.Errorf("routeSubject(%+v) = %q, want %q", tt.event, got, tt.want)
			}
		})
	}
}

func TestRouteSubject_RespectsCallerSuppliedCluster(t *testing.T) {
	// A caller-supplied cluster names the cluster the event is about; the
	// publisher's own identity is only the default when none is given.
	e := Event{Type: "controller.created", Cluster: "hive", Namespace: "team-a", Controller: "c1"}
	if got := (&Publisher{cluster: "core"}).routeSubject(e); got != "activity.hive.team-a.c1" {
		t.Errorf("routeSubject = %q, want activity.hive.team-a.c1", got)
	}
	e.Cluster = ""
	if got := (&Publisher{cluster: "core"}).routeSubject(e); got != "activity.core.team-a.c1" {
		t.Errorf("routeSubject with empty cluster = %q, want activity.core.team-a.c1", got)
	}
}
