package activity

import (
	"slices"
	"strings"
	"time"
)

// Event represents a single activity feed entry.
//
// Cluster names the cluster the event is *about*, which is not always the one
// publishing it: the BFF serves every cluster in the brood. Leave it empty and
// Publisher.Publish defaults it to the publisher's own identity.
type Event struct {
	Timestamp   time.Time `json:"timestamp"`
	Type        string    `json:"type"`
	Source      string    `json:"source"`          // "mite", "operator", "user", "jenkins", "api", "mcp", "assistant"
	Actor       string    `json:"actor,omitempty"` // username, else email, else subject (api.ActorFrom); "operator"; or empty
	Controller  string    `json:"controller,omitempty"`
	Namespace   string    `json:"namespace,omitempty"`
	Cluster     string    `json:"cluster"`
	Message     string    `json:"message"`
	Phase       string    `json:"phase,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	Severity    string    `json:"severity"`
	ItemPath    string    `json:"itemPath,omitempty"`
	BuildNumber int64     `json:"buildNumber,omitempty"`
	Result      string    `json:"result,omitempty"`
	URL         string    `json:"url,omitempty"`
}

var errorSeverityTokens = []string{"failed", "failure", "error", "denied", "unhealthy", "disconnected"}
var warningSeverityTokens = []string{"warning", "retry", "drift", "pending", "paused", "hibernated"}

// Normalize applies the canonical wire representation to producer and retained events.
func (e *Event) Normalize() {
	e.Actor = strings.TrimSpace(e.Actor)
	e.Severity = strings.ToLower(strings.TrimSpace(e.Severity))
	if e.Severity == "info" || e.Severity == "warning" || e.Severity == "error" {
		return
	}
	text := strings.ToLower(e.Phase + " " + e.Reason + " " + e.Type)
	e.Severity = "info"
	for _, token := range warningSeverityTokens {
		if strings.Contains(text, token) {
			e.Severity = "warning"
			break
		}
	}
	for _, token := range errorSeverityTokens {
		if strings.Contains(text, token) {
			e.Severity = "error"
			break
		}
	}
}

// LocalSignInType is the activity type of a successful local sign-in.
const LocalSignInType = "auth.local.login"

// IsRoutineSignIn reports whether e is a routine local sign-in: a sign-in
// below warning severity. A break-glass sign-in carries warning severity, so
// it is never routine. The SQL form in recordstore.ListActivity must match
// this rule. Call it after Normalize, which leaves only info, warning, or
// error, so "not warning and not error" means info.
func IsRoutineSignIn(e Event) bool {
	return e.Type == LocalSignInType && e.Severity != "warning" && e.Severity != "error"
}

// Build event types. Each one repeats a run row, so a run feed leaves it out.
// The Jenkins plugin writes BuildStartedType and BuildCompletedType. The
// record store writes BuildFailedType after each failed run.
const (
	BuildStartedType   = "build.started"
	BuildCompletedType = "build.completed"
	BuildFailedType    = "build.failed"
)

// buildRepeatTypes lists the build event types that repeat a run row. The SQL
// clause of recordstore ListActivity reads this same list.
var buildRepeatTypes = []string{BuildStartedType, BuildCompletedType, BuildFailedType}

// BuildRepeatTypes returns a copy of the build event types that repeat a run
// row. A feed of runs leaves out exactly these types. Every other build.* type
// reports something that no run row shows, so it stays in the feed.
func BuildRepeatTypes() []string {
	return append([]string(nil), buildRepeatTypes...)
}

// IsBuildEvent reports whether e is a build event that repeats a run row. It
// is true for build.started, build.completed, and build.failed only.
func IsBuildEvent(e Event) bool {
	for _, t := range buildRepeatTypes {
		if e.Type == t {
			return true
		}
	}
	return false
}

// routineTypes lists the routine event types. A controller writes each one on
// every start and restart. A transition to Failed also writes provisioning.failed,
// which is not in this list.
var routineTypes = []string{"connected", "disconnected", "phase", "provisioning.started", "provisioning.completed"}

// routineItemTypes lists the Jenkins item events. One is routine only when a machine actor made the change.
var routineItemTypes = []string{"item.created", "item.updated", "item.deleted", "item.moved", "config.changed"}

// machineActors lists the actors of an item change that no person made. The plugin
// reports the Jenkins system user as "". The mite calls Jenkins as varroa-mite or varroa-mite-system.
var machineActors = []string{"", "varroa-mite", "varroa-mite-system"}

// RoutineTypes returns a copy of the routine event types. The SQL clause of
// recordstore ListActivity reads these lists.
func RoutineTypes() []string { return append([]string(nil), routineTypes...) }

// RoutineItemTypes returns a copy of the item event types that are routine
// only for a machine actor.
func RoutineItemTypes() []string { return append([]string(nil), routineItemTypes...) }

// MachineActors returns a copy of the actors that mark an item change as machine work.
func MachineActors() []string { return append([]string(nil), machineActors...) }

// IsRoutineEvent reports whether e is a routine event. The SQL form in
// recordstore ListActivity must match this rule. Call it after Normalize.
func IsRoutineEvent(e Event) bool {
	if slices.Contains(routineTypes, e.Type) {
		return true
	}
	return slices.Contains(routineItemTypes, e.Type) && slices.Contains(machineActors, e.Actor)
}
