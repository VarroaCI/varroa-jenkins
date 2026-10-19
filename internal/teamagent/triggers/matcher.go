package triggers

import (
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/accept"
)

// Outcome is the matcher's answer for one activity row.
type Outcome int

const (
	// OutcomeAck drops the row. No Team agent concerns it.
	OutcomeAck Outcome = iota
	// OutcomeCreate hands the row to the creator.
	OutcomeCreate
)

// maxEventAge bounds how old an activity row may be and still start a session.
// A row older than this comes from a backlog, and the cause is no longer live.
const maxEventAge = time.Hour

// typeToTrigger maps an activity type to the trigger that names it. A type not
// in the map is dropped.
var typeToTrigger = map[string]string{
	"build.failed":                "run.failed",
	"build.interrupted":           "run.interrupted",
	"build.scheduled.backstop":    "cron.missed",
	"cell.signature_refused":      "signature.refused",
	"pipeline.branch_scan_failed": "scan.failed",
	"fleetlock.wait_exceeded":     "lock.held",
	"tenant.placement_moved":      "placement.moved",
	"teamagent.run_requested":     "manual",
}

// ManualTrigger is the trigger of a person's request to run a Team agent.
const ManualTrigger = "manual"

// Match is one activity row that concerns one Team agent.
type Match struct {
	TA      *v1alpha1.TeamAgent
	Trigger string
	Tenant  string
	// ItemKey names the work the row concerns: the job path, or the tenant for
	// a tenant-wide row. A manual request uses its event key, so each request
	// starts a session.
	ItemKey  string
	EventKey string
	Event    activity.Event
}

// PlacementFunc returns the cluster that holds a tenant's live cell.
type PlacementFunc func(tenant string) (cluster string, ok bool)

// Matcher decides which Team agent, if any, an activity row concerns.
type Matcher struct {
	// Accepted returns the acceptance input of the current cache.
	Accepted func() accept.Input
	// Placement answers the cell that holds a tenant.
	Placement PlacementFunc
}

// Match applies the rules in order and returns the match or an ack. now is the
// time of the decision, so a test can fix it.
func (m *Matcher) Match(e activity.Event, now time.Time) (Match, Outcome) {
	trigger, known := typeToTrigger[e.Type]
	if !known || e.ItemPath == "" {
		return Match{}, OutcomeAck
	}
	tenant := strings.SplitN(e.ItemPath, "/", 2)[0]
	if now.Sub(e.Timestamp) > maxEventAge {
		return Match{}, OutcomeAck
	}
	ta, ok := accept.AcceptedFor(m.Accepted(), tenant)
	if !ok || !accept.Enabled(ta) {
		return Match{}, OutcomeAck
	}
	if cluster, ok := m.Placement(tenant); !ok || cluster != e.Cluster {
		return Match{}, OutcomeAck
	}
	if !hasTrigger(ta, trigger) {
		return Match{}, OutcomeAck
	}
	key := e.ItemPath
	if e.ItemPath == tenant {
		key = tenant
	}
	evKey := activity.EventKey(e)
	if trigger == ManualTrigger {
		if e.Reason != ta.Namespace+"/"+ta.Name {
			return Match{}, OutcomeAck
		}
		key = evKey
	}
	return Match{TA: ta, Trigger: trigger, Tenant: tenant, ItemKey: key, EventKey: evKey, Event: e}, OutcomeCreate
}

// hasTrigger reports whether the TeamAgent enables trigger.
func hasTrigger(ta *v1alpha1.TeamAgent, trigger string) bool {
	for _, t := range ta.Spec.Triggers {
		if t.Type == trigger {
			return true
		}
	}
	return false
}
