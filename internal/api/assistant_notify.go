package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// assistantPublisher publishes one message on a bus subject.
type assistantPublisher func(subject string, data []byte) error

// notifyingStore publishes a content-free notice after each successful write
// to a session. A notice names the session, its kind, its state, and the last
// sequence number. The stream handler reads the transcript itself. Publish
// errors are logged and ignored.
type notifyingStore struct {
	store.Store
	pub    assistantPublisher
	logger *slog.Logger
}

func newNotifyingStore(inner store.Store, pub assistantPublisher, logger *slog.Logger) store.Store {
	if logger == nil {
		logger = slog.Default()
	}
	return notifyingStore{Store: inner, pub: pub, logger: logger}
}

// notice is the body of one agent session notification.
type notice struct {
	SessionID string `json:"session_id"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	Seq       int    `json:"seq"`
}

func (n notifyingStore) notify(ctx context.Context, id uuid.UUID, seq int) {
	if n.pub == nil {
		return
	}
	s, err := n.GetSession(ctx, id)
	if err != nil {
		n.logger.Warn("assistant notify: read session", "error", err.Error())
		return
	}
	body, err := json.Marshal(notice{SessionID: id.String(), Kind: string(s.Kind), State: string(s.State), Seq: seq})
	if err != nil {
		return
	}
	if err := n.pub(bus.AgentSessionSubject(s.Cluster, s.Team), body); err != nil {
		n.logger.Warn("assistant notify: publish", "error", err.Error())
	}
}

func lastSeq(evs []agent.Event) int {
	if len(evs) == 0 {
		return 0
	}
	return evs[len(evs)-1].Seq
}

func (n notifyingStore) CreateSession(ctx context.Context, s agent.Session, events []agent.EventInput) (agent.Session, []agent.Event, error) {
	out, evs, err := n.Store.CreateSession(ctx, s, events)
	if err == nil {
		n.notify(ctx, out.ID, lastSeq(evs))
	}
	return out, evs, err
}

func (n notifyingStore) TakeLease(ctx context.Context, id uuid.UUID, owner string, from []agent.SessionState) (agent.Session, error) {
	out, err := n.Store.TakeLease(ctx, id, owner, from)
	if err == nil {
		n.notify(ctx, id, 0)
	}
	return out, err
}

func (n notifyingStore) AppendEvent(ctx context.Context, id uuid.UUID, g store.Guard, events ...agent.EventInput) ([]agent.Event, error) {
	evs, err := n.Store.AppendEvent(ctx, id, g, events...)
	if err == nil {
		n.notify(ctx, id, lastSeq(evs))
	}
	return evs, err
}

func (n notifyingStore) ClearLease(ctx context.Context, id uuid.UUID, owner string, events []agent.EventInput) ([]agent.Event, error) {
	evs, err := n.Store.ClearLease(ctx, id, owner, events)
	if err == nil {
		n.notify(ctx, id, lastSeq(evs))
	}
	return evs, err
}

func (n notifyingStore) EndSession(ctx context.Context, id uuid.UUID, in store.EndInput) ([]agent.Event, error) {
	evs, err := n.Store.EndSession(ctx, id, in)
	if err == nil {
		n.notify(ctx, id, lastSeq(evs))
	}
	return evs, err
}

func (n notifyingStore) CreateApprovals(ctx context.Context, sessionID uuid.UUID, owner string, approvals []agent.Approval, events []agent.EventInput) ([]agent.Approval, []agent.Event, error) {
	out, evs, err := n.Store.CreateApprovals(ctx, sessionID, owner, approvals, events)
	if err == nil {
		n.notify(ctx, sessionID, lastSeq(evs))
	}
	return out, evs, err
}

func (n notifyingStore) ResolveApproval(ctx context.Context, id uuid.UUID, r store.Resolution) (store.ResolveResult, error) {
	res, err := n.Store.ResolveApproval(ctx, id, r)
	if err == nil {
		n.notify(ctx, res.Session.ID, 0)
	}
	return res, err
}

// publishControl announces a change of the global kill switch.
func publishControl(pub assistantPublisher, paused bool, by string, at time.Time) error {
	if pub == nil {
		return nil
	}
	body, err := json.Marshal(map[string]any{"paused": paused, "updated_by": by, "updated_at": at.UTC()})
	if err != nil {
		return err
	}
	return pub(bus.AssistantControlSubject, body)
}
