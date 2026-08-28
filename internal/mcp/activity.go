package mcp

import (
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// mcpSource marks an event as originating from an MCP tool call rather than
// the dashboard, so an agent-initiated mutation is distinguishable from a
// human one in the activity feed.
const mcpSource = "mcp"

// emitActivity publishes an activity event for an MCP-originated mutation.
//
// Callers supply Type, Message, Namespace and (for controller-scoped tools
// only) Controller. Actor, Source and a default Severity are stamped here, and
// Timestamp by activity.Publisher.Publish.
//
// Cluster is left empty here, which makes Publish default it to the publishing
// BFF's own identity — correct only for a mutation on the local cluster.
// Cluster-aware tools must go through emitActivityForCluster instead, or a
// remote mutation is recorded against the wrong cluster.
//
// Leaving Controller empty routes the event to activity.<cluster>._global,
// which is where a role, user, group, bundle or catalog-source event belongs.
func emitActivity(deps *api.Dependencies, claims *auth.Claims, e activity.Event) {
	if deps == nil || deps.ActivityPublisher == nil {
		return
	}
	e.Actor = api.ActorFrom(claims)
	e.Source = mcpSource
	if e.Severity == "" {
		e.Severity = "info"
	}
	deps.ActivityPublisher.Publish(e)
}

// emitActivityForCluster is emitActivity for mutations that actually reached a
// named cluster.
//
// The BFF serves every cluster in the brood, so a mutation may target a cluster
// other than the one the BFF calls its own. Stamping the target here is what
// makes the record truthful and keeps it visible to cluster-filtered queries;
// Publisher.Publish falls back to its own identity only when this is empty.
//
// Use it ONLY where the write was genuinely routed to that cluster — behind a
// brood or reconciler call. The store-only fallback paths write to the local
// crdstore no matter what `cluster` argument the caller passed, so stamping it
// there would invent a mutation on a cluster that was never touched. Those call
// emitActivity and let the publisher stamp its own identity.
func emitActivityForCluster(deps *api.Dependencies, claims *auth.Claims, cluster string, e activity.Event) {
	e.Cluster = cluster
	emitActivity(deps, claims, e)
}
