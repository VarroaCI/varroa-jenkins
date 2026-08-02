package activity

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

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
	e.Cluster = p.cluster // publisher identity is authoritative
	subj := p.routeSubject(e)
	data, err := json.Marshal(e)
	if err != nil {
		p.log().Error("marshal activity event failed", "error", err)
		return
	}
	_ = p.conn.Publish(subj, data)
}

// routeSubject computes the subject for the given event per the routing rule.
func (p *Publisher) routeSubject(e Event) string {
	if e.Controller == "" {
		return bus.ActivityGlobal(p.cluster)
	}
	if e.Namespace == "" {
		p.log().Warn("activity event has non-empty Controller but empty Namespace; routing to _global",
			"controller", e.Controller)
		return bus.ActivityGlobal(p.cluster)
	}
	return bus.ActivitySubject(p.cluster, e.Namespace, e.Controller)
}

func (p *Publisher) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}
