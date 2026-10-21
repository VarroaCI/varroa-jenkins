package api

import (
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// AccessKind names the kind of access object in an activity event.
type AccessKind string

// The kinds of access object that emit an activity event.
const (
	AccessGroup              AccessKind = "group"
	AccessVarroaRole         AccessKind = "varroarole"
	AccessVarroaRoleBinding  AccessKind = "varroarolebinding"
	AccessJenkinsRole        AccessKind = "jenkinsrole"
	AccessJenkinsRoleBinding AccessKind = "jenkinsrolebinding"
)

// AccessChange names the kind of write in an access activity event.
type AccessChange string

// The kinds of write that emit an access activity event.
const (
	AccessCreated AccessChange = "created"
	AccessUpdated AccessChange = "updated"
	AccessDeleted AccessChange = "deleted"
)

// accessKindLabel maps each kind to the label in the message.
var accessKindLabel = map[AccessKind]string{
	AccessGroup:              "Group",
	AccessVarroaRole:         "VarroaRole",
	AccessVarroaRoleBinding:  "VarroaRoleBinding",
	AccessJenkinsRole:        "JenkinsRole",
	AccessJenkinsRoleBinding: "JenkinsRoleBinding",
}

// AccessActivityEvent returns the activity event for one write to a
// cluster-scoped access object. Every write surface builds its event here,
// so the type and the message are the same on each surface. The message
// holds no actor, because the feed shows Actor beside it. The caller sets
// Actor, Source, Severity, and Cluster.
func AccessActivityEvent(kind AccessKind, change AccessChange, name string) activity.Event {
	return activity.Event{
		Type:    string(kind) + "." + string(change),
		Message: accessKindLabel[kind] + " " + name + " " + string(change),
	}
}

// notifyAccessActivity publishes the activity event for an access-object
// write made through the REST API. An empty cluster lets the publisher
// stamp its own identity.
func (s *Server) notifyAccessActivity(claims *auth.Claims, cluster string, kind AccessKind, change AccessChange, name string) {
	e := AccessActivityEvent(kind, change, name)
	e.Source = "user"
	e.Actor = ActorFrom(claims)
	e.Severity = "info"
	e.Cluster = cluster
	s.notifyActivity(e)
}
