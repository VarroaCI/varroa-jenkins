package activity

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

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
