package api

import (
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// UserChange names the kind of User write in an activity event.
type UserChange string

// The kinds of User write that emit an activity event.
const (
	UserCreated UserChange = "created"
	UserUpdated UserChange = "updated"
	UserDeleted UserChange = "deleted"
)

// UserActivityEvent returns the activity event for one User write. Every
// user write surface builds its event here, so the type and the message
// are the same on each surface. The caller sets Actor and Source.
func UserActivityEvent(change UserChange, namespace, name string) activity.Event {
	return activity.Event{
		Type:      "user." + string(change),
		Message:   "User " + name + " " + string(change) + " in " + namespace,
		Namespace: namespace,
	}
}

// notifyUserActivity publishes the activity event for a User write made
// through the REST API.
func (s *Server) notifyUserActivity(claims *auth.Claims, change UserChange, name string) {
	e := UserActivityEvent(change, s.deps.OperatorNamespace, name)
	e.Source = "user"
	e.Actor = ActorFrom(claims)
	e.Severity = "info"
	s.notifyActivity(e)
}
