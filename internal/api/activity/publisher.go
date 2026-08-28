package activity

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// EventSink accepts activity events. *Publisher is the production
// implementation; tests substitute a recorder. It exists so the MCP and REST
// surfaces can be tested for emission without standing up a bus.
type EventSink interface {
	Publish(Event)
}

// Publisher publishes activity events to the NATS bus on the hierarchical
// activity subject family. It is mode-agnostic: it always uses core NATS
// publish (fire-and-forget), and the JetStream stream passively captures
// matching subjects when retention is enabled.
type Publisher struct {
	cluster string
	conn    *bus.Conn
	Logger  *slog.Logger
}

// NewPublisher creates a new activity Publisher.
func NewPublisher(cluster string, conn *bus.Conn) *Publisher {
	return &Publisher{cluster: cluster, conn: conn}
}

// Publish serializes the event as full JSON and publishes it to the
// appropriate activity subject determined by the routing rule:
//   - Event with Controller == "" → activity.<cluster>._global
//   - Event with Controller != "" and Namespace == "" → activity.<cluster>._global (error, logged)
//   - Event with Controller != "" and Namespace != "" → activity.<cluster>.<ns>.<ctrl>
func (p *Publisher) Publish(e Event) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	// A caller-supplied cluster names the cluster the event is *about*, which
	// is not always the one publishing it: the BFF serves every cluster in the
	// brood, so a mutation targeting a remote cluster must be recorded — and
	// routed — under that cluster or it is a false audit record and invisible
	// to cluster-filtered queries. The publisher's own identity is only a
	// default for events that name no target.
	if e.Cluster == "" {
		e.Cluster = p.cluster
	}
	subj := p.routeSubject(e)
	data, err := json.Marshal(e)
	if err != nil {
		p.log().Error("marshal activity event failed", "error", err)
		return
	}
	// Do not swallow this. Cluster now affects the subject, so a NATS ACL that
	// does not cover a remote cluster drops audit events — and an audit trail
	// that loses records silently is worse than one that never existed.
	if err := p.conn.Publish(subj, data); err != nil {
		p.log().Error("publish activity event failed",
			"subject", subj, "type", e.Type, "cluster", e.Cluster, "error", err)
	}
}

// routeSubject computes the subject for the given event per the routing rule.
// routeSubject routes on e.Cluster, which Publish has already defaulted to the
// publisher's own identity when the caller named no target. Routing on
// p.cluster instead would send an event about a remote cluster to the local
// cluster's subject, hiding it from that cluster's consumers.
func (p *Publisher) routeSubject(e Event) string {
	cluster := e.Cluster
	if cluster == "" {
		cluster = p.cluster
	}
	if e.Controller == "" {
		return bus.ActivityGlobal(cluster)
	}
	if e.Namespace == "" {
		p.log().Warn("activity event has non-empty Controller but empty Namespace; routing to _global",
			"controller", e.Controller)
		return bus.ActivityGlobal(cluster)
	}
	return bus.ActivitySubject(cluster, e.Namespace, e.Controller)
}

func (p *Publisher) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}
