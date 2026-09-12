package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// webhookReplayConsumer is the durable JetStream consumer name the operator
// leader binds to drain queued webhooks for its cluster.
const webhookReplayConsumer = "varroa-webhook-replay"

// replayResultTimeout bounds how long a single replay waits for the mite's
// result before the message is nak'd for later redelivery. Kept comfortably
// under the consumer AckWait (60s) so a slow replay is redelivered rather than
// silently expiring its ack deadline.
const replayResultTimeout = 30 * time.Second

// webhookEnvelope is the queued webhook shape the BFF writes to the
// varroa_webhooks stream (see internal/api/handlers_hibernation.go). It
// carries either the controller shape (Namespace and Controller set) or
// the tenant shape (Team set, Namespace and Controller both empty) —
// design.md D13. isTenantEnvelope decides which, from these fields, never
// from the subject.
type webhookEnvelope struct {
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	Query      string            `json:"query"`
	Headers    map[string]string `json:"headers"`
	BodyB64    string            `json:"bodyB64"`
	ReceivedAt string            `json:"receivedAt"`
	Cluster    string            `json:"cluster"`
	Namespace  string            `json:"namespace"`
	Controller string            `json:"controller"`

	// Team, Forge, and DeliveryID are set only on a tenant envelope. Team
	// is the Team name; Forge and DeliveryID are the door's own forge
	// identification and derived delivery id, joined into
	// ReplayWebhookPayload.delivery_id as "<forge>:<delivery-id>".
	Team       string `json:"team,omitempty"`
	Forge      string `json:"forge,omitempty"`
	DeliveryID string `json:"deliveryId,omitempty"`
}

// isTenantEnvelope reports whether env is the tenant shape. The decision
// reads only the envelope's own fields, never the subject, so a
// Kubernetes namespace literally named "tenant" is never mistaken for the
// tenant shape (design.md D13).
func (env webhookEnvelope) isTenantEnvelope() bool {
	return env.Controller == "" && env.Team != ""
}

// RunWebhookReplay drains the cluster's queued webhooks and replays them to
// awake controllers, blocking until ctx is cancelled. It is leader-gated: only
// the operator leader runs it (JetStream durable consumer state survives leader
// failover). Each message is delivered to the controller's mite as a
// REPLAY_WEBHOOK imperative command; the mite's result — routed to the
// dedicated whreply.* subject so it never enters the reconciler's shared
// command-result buffer — drives the ack/nak/term decision (design D8):
//
//   - controller Connected and mite returns 2xx  → Ack
//   - controller not Connected, 5xx, 429, timeout → Nak (redeliver after delay)
//   - controller CR deleted, or Jenkins 4xx≠429   → Term + rejected activity event
func (r *Reconciler) RunWebhookReplay(ctx context.Context, conn *bus.Conn, cluster string, replicas int) error {
	if err := conn.EnsureStream(bus.WebhookStreamConfig(bus.WebhookStreamName, replicas)); err != nil {
		return fmt.Errorf("ensure webhook stream: %w", err)
	}
	// The varroa_webhooks stream is shared across clusters, so filter to this
	// cluster's subjects — a bare webhook.> would drain (and replay) webhooks
	// destined for other clusters. Durable name is cluster-scoped for the same
	// reason (one durable per cluster on the shared stream).
	clusterWildcard := bus.WebhookClusterWildcard(cluster)
	durable := webhookReplayConsumer + "-" + cluster
	if err := conn.EnsureConsumer(bus.WebhookStreamName, durable, &nats.ConsumerConfig{
		Durable:       durable,
		FilterSubject: clusterWildcard, // webhook.<cluster>.>
		AckPolicy:     nats.AckExplicitPolicy,
		AckWait:       60 * time.Second,
		MaxDeliver:    -1, // MaxAge on the stream (1h) is the real bound
	}); err != nil {
		return fmt.Errorf("ensure webhook replay consumer: %w", err)
	}
	sub, err := conn.PullSubscribe(clusterWildcard, bus.WebhookStreamName, durable)
	if err != nil {
		return fmt.Errorf("pull-subscribe webhook replay: %w", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	// Correlate mite replay results (delivered on whreply.<cluster>.>) back to
	// the goroutine awaiting each command_id.
	waiters := &replayWaiters{chans: make(map[string]chan *mitev1.CommandResult)}
	resultSub, err := conn.Subscribe(bus.WebhookResultWildcard(cluster), func(msg *nats.Msg) {
		var res mitev1.CommandResult
		if err := json.Unmarshal(msg.Data, &res); err != nil {
			r.Logger.Warn("unmarshal replay result failed", "error", err)
			return
		}
		waiters.deliver(&res)
	})
	if err != nil {
		return fmt.Errorf("subscribe webhook results: %w", err)
	}
	defer func() { _ = resultSub.Unsubscribe() }()

	r.Logger.Info("webhook replay consumer started", "cluster", cluster)
	for {
		if ctx.Err() != nil {
			return nil
		}
		msgs, err := conn.DrainConsumer(sub, 16)
		if err != nil {
			// A fetch timeout with no messages is normal; anything else, back off.
			if errors.Is(err, nats.ErrTimeout) {
				continue
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
				continue
			}
		}
		for _, msg := range msgs {
			r.handleQueuedWebhook(ctx, cluster, msg, waiters)
		}
	}
}

// handleQueuedWebhook decodes the envelope and dispatches on its shape.
// The shape comes from the envelope's own fields, never the subject
// (design.md D13).
func (r *Reconciler) handleQueuedWebhook(ctx context.Context, cluster string, msg *nats.Msg, waiters *replayWaiters) {
	var env webhookEnvelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		// Unparseable payload will never succeed — drop it rather than loop.
		r.Logger.Warn("dropping unparseable queued webhook", "error", err)
		_ = msg.Term()
		return
	}

	// Defense-in-depth: the consumer's FilterSubject already scopes to this
	// cluster, but never replay a webhook whose envelope names another cluster.
	if env.Cluster != cluster {
		r.Logger.Warn("dropping cross-cluster webhook", "envelopeCluster", env.Cluster, "cluster", cluster)
		_ = msg.Term()
		return
	}

	if env.isTenantEnvelope() {
		r.handleQueuedTenantWebhook(ctx, env, msg, waiters)
		return
	}
	r.handleQueuedControllerWebhook(ctx, env, msg, waiters)
}

// handleQueuedControllerWebhook applies the decision table to a
// controller-shape queued webhook. Unchanged by the tenant shape.
func (r *Reconciler) handleQueuedControllerWebhook(ctx context.Context, env webhookEnvelope, msg *nats.Msg, waiters *replayWaiters) {
	cr, err := crdstore.Get[v1alpha1.Controller](ctx, r.store, env.Controller, env.Namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.Logger.Info("dropping webhook for deleted controller",
				"controller", env.Namespace+"/"+env.Controller)
			r.publishReplayRejected(env, "controller no longer exists")
			_ = msg.Term()
			return
		}
		// Transient read error: retry later.
		_ = msg.NakWithDelay(replayResultTimeout)
		return
	}

	if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
		// Not ready yet (hibernated/waking/provisioning) — redeliver later.
		_ = msg.NakWithDelay(replayResultTimeout)
		return
	}

	body, err := base64.StdEncoding.DecodeString(env.BodyB64)
	if err != nil {
		r.Logger.Warn("dropping webhook with bad body encoding",
			"controller", env.Namespace+"/"+env.Controller, "error", err)
		_ = msg.Term()
		return
	}

	// command_id = the JetStream stream sequence (a stable, unique delivery id).
	deliveryID := strconv.FormatUint(msgSequence(msg), 10)
	cmd := &mitev1.ImperativeCommand{
		CommandId: deliveryID,
		Type:      mitev1.CommandTypeReplayWebhook,
		ReplayWebhook: &mitev1.ReplayWebhookPayload{
			Path:       env.Path,
			Query:      env.Query,
			Headers:    env.Headers,
			Body:       body,
			DeliveryId: deliveryID,
		},
	}

	resultCh := waiters.register(deliveryID)
	defer waiters.unregister(deliveryID)

	if err := r.miteTransport.SendImperative(ctx, env.Namespace, env.Controller, cmd); err != nil {
		r.Logger.Warn("send replay command failed", "controller", env.Namespace+"/"+env.Controller, "error", err)
		_ = msg.NakWithDelay(replayResultTimeout)
		return
	}

	select {
	case <-ctx.Done():
		_ = msg.NakWithDelay(replayResultTimeout)
	case <-time.After(replayResultTimeout):
		// No result in time — mite may be slow or gone. Redeliver.
		_ = msg.NakWithDelay(replayResultTimeout)
	case res := <-resultCh:
		r.applyReplayResult(msg, env, res)
	}
}

// handleQueuedTenantWebhook applies the decision table to a tenant-shape
// queued webhook (design.md D13). The consumer resolves the target at
// delivery time, from the Team's current placement, so a tenant that
// moves between ingest and delivery lands on its new cell. It naks with
// the standard delay in four cases — an empty placement, a moving tenant,
// an absent or unreadable Controller, and a Controller that is not
// Connected — and terms only when the Team no longer exists. A move
// drains nothing. It re-reads the placement once more immediately before
// the send and naks when that target changed since the checks ran, so a
// move landing in that gap never dispatches to the cell the tenant left.
func (r *Reconciler) handleQueuedTenantWebhook(ctx context.Context, env webhookEnvelope, msg *nats.Msg, waiters *replayWaiters) {
	team, err := crdstore.Get[v1alpha1.Team](ctx, r.store, env.Team, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.Logger.Info("dropping tenant webhook for deleted team", "team", env.Team)
			r.publishReplayRejected(env, "team no longer exists")
			_ = msg.Term()
			return
		}
		// Transient read error: the Team owns this delivery, hold it.
		_ = msg.NakWithDelay(replayResultTimeout)
		return
	}

	placement := team.Status.Placement
	if placement == nil || placement.Cell == "" || placement.CellNamespace == "" {
		// Nak case 1: an empty placement.
		_ = msg.NakWithDelay(replayResultTimeout)
		return
	}
	if teamMoving(team) {
		// Nak case 2: the Team reports Placed=False with reason
		// TenantMoving.
		_ = msg.NakWithDelay(replayResultTimeout)
		return
	}

	cr, err := crdstore.Get[v1alpha1.Controller](ctx, r.store, placement.Cell, placement.CellNamespace)
	if err != nil {
		// Nak case 3: the named Controller is absent or unreadable. The
		// Team, not the Controller, owns this delivery — it never terms
		// here.
		_ = msg.NakWithDelay(replayResultTimeout)
		return
	}
	if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
		// Nak case 4: the named Controller is not Connected.
		_ = msg.NakWithDelay(replayResultTimeout)
		return
	}

	body, err := base64.StdEncoding.DecodeString(env.BodyB64)
	if err != nil {
		r.Logger.Warn("dropping tenant webhook with bad body encoding", "team", env.Team, "error", err)
		_ = msg.Term()
		return
	}

	// command_id = the JetStream stream sequence (a stable, unique delivery id);
	// correlation keeps using it, unchanged from the controller shape.
	commandID := strconv.FormatUint(msgSequence(msg), 10)
	cmd := &mitev1.ImperativeCommand{
		CommandId: commandID,
		Type:      mitev1.CommandTypeReplayWebhook,
		ReplayWebhook: &mitev1.ReplayWebhookPayload{
			Path:    env.Path,
			Query:   env.Query,
			Headers: env.Headers,
			Body:    body,
			// <forge>:<delivery-id>, joined from the envelope (design.md D13).
			DeliveryId: env.Forge + ":" + env.DeliveryID,
		},
	}

	// Re-read the Team immediately before the send. The nak checks above
	// ran against the placement this handler read at the top, and a move
	// can start or land in the gap between that read and this send.
	// Sending against a stale decision would dispatch to a cell the
	// tenant already left, or is leaving; nak instead so the redelivery
	// re-evaluates the full decision table against the now-current Team.
	// This checks both the placement target and the moving state,
	// because a move sets Placed=False/TenantMoving while it still holds
	// the old placement (design.md D13, D4 move order).
	current, err := crdstore.Get[v1alpha1.Team](ctx, r.store, env.Team, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.Logger.Info("dropping tenant webhook for deleted team", "team", env.Team)
			r.publishReplayRejected(env, "team no longer exists")
			_ = msg.Term()
			return
		}
		_ = msg.NakWithDelay(replayResultTimeout)
		return
	}
	currentPlacement := current.Status.Placement
	if currentPlacement == nil ||
		currentPlacement.Cell != placement.Cell ||
		currentPlacement.CellNamespace != placement.CellNamespace ||
		teamMoving(current) {
		r.Logger.Info("tenant placement changed just before send, redelivering", "team", env.Team)
		_ = msg.NakWithDelay(replayResultTimeout)
		return
	}

	resultCh := waiters.register(commandID)
	defer waiters.unregister(commandID)

	if err := r.miteTransport.SendImperative(ctx, placement.CellNamespace, placement.Cell, cmd); err != nil {
		r.Logger.Warn("send tenant replay command failed", "team", env.Team,
			"cell", placement.CellNamespace+"/"+placement.Cell, "error", err)
		_ = msg.NakWithDelay(replayResultTimeout)
		return
	}

	select {
	case <-ctx.Done():
		_ = msg.NakWithDelay(replayResultTimeout)
	case <-time.After(replayResultTimeout):
		// No result in time — mite may be slow or gone. Redeliver.
		_ = msg.NakWithDelay(replayResultTimeout)
	case res := <-resultCh:
		r.applyReplayResult(msg, env, res)
	}
}

// teamMoving reports whether team's Placed condition is currently False
// with reason TenantMoving. A tenant in that state holds its buffered
// deliveries until the move completes.
func teamMoving(team *v1alpha1.Team) bool {
	for _, c := range team.Status.Conditions {
		if c.Type == v1alpha1.TeamConditionPlaced {
			return c.Status == metav1.ConditionFalse && c.Reason == v1alpha1.TeamReasonTenantMoving
		}
	}
	return false
}

// replayAction is the JetStream disposition for a replayed webhook.
type replayAction int

const (
	ackReplay  replayAction = iota // delivered successfully; drop from the queue
	nakReplay                      // transient failure; redeliver later
	termReplay                     // permanent rejection; drop and record
)

// replayAckDecision maps an upstream HTTP status to the queue disposition
// (design D8). Status 0 means the mite reported no HTTP response at all
// (transport-level failure) and is treated as transient.
func replayAckDecision(httpStatus int) replayAction {
	switch {
	case httpStatus >= 200 && httpStatus < 300:
		return ackReplay
	case httpStatus == 429 || httpStatus >= 500 || httpStatus == 0:
		return nakReplay
	default:
		return termReplay // 4xx other than 429
	}
}

// applyReplayResult maps a mite replay result to the JetStream ack decision.
func (r *Reconciler) applyReplayResult(msg *nats.Msg, env webhookEnvelope, res *mitev1.CommandResult) {
	switch replayAckDecision(res.HttpStatus) {
	case ackReplay:
		_ = msg.Ack()
	case nakReplay:
		_ = msg.NakWithDelay(replayResultTimeout)
	case termReplay:
		r.Logger.Warn("webhook replay permanently rejected",
			"controller", env.Namespace+"/"+env.Controller, "status", res.HttpStatus, "error", res.Error)
		r.publishReplayRejected(env, fmt.Sprintf("jenkins rejected webhook (status %d)", res.HttpStatus))
		_ = msg.Term()
	}
}

// publishReplayRejected emits a webhook.replay.rejected operator activity event.
func (r *Reconciler) publishReplayRejected(env webhookEnvelope, reason string) {
	if r.activityPublisher == nil {
		return
	}
	message := reason
	if env.isTenantEnvelope() {
		// A tenant delivery names no controller or namespace to route the
		// event by, so the team name goes into the message instead.
		message = fmt.Sprintf("tenant %s: %s", env.Team, reason)
	}
	r.activityPublisher.Publish(activity.Event{
		Type:       "webhook.replay.rejected",
		Controller: env.Controller,
		Namespace:  env.Namespace,
		Message:    message,
		Timestamp:  time.Now().UTC(),
	})
}

// msgSequence extracts the JetStream stream sequence from a message; 0 if
// metadata is unavailable (correlation still works — the value only needs to be
// unique per in-flight replay, and the result is matched by this same id).
func msgSequence(msg *nats.Msg) uint64 {
	md, err := msg.Metadata()
	if err != nil || md == nil {
		return 0
	}
	return md.Sequence.Stream
}

// replayWaiters correlates mite replay results back to the goroutine that sent
// the command, keyed by command_id.
type replayWaiters struct {
	mu    sync.Mutex
	chans map[string]chan *mitev1.CommandResult
}

func (w *replayWaiters) register(id string) chan *mitev1.CommandResult {
	ch := make(chan *mitev1.CommandResult, 1)
	w.mu.Lock()
	w.chans[id] = ch
	w.mu.Unlock()
	return ch
}

func (w *replayWaiters) unregister(id string) {
	w.mu.Lock()
	delete(w.chans, id)
	w.mu.Unlock()
}

func (w *replayWaiters) deliver(res *mitev1.CommandResult) {
	w.mu.Lock()
	ch, ok := w.chans[res.CommandId]
	w.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- res:
	default:
	}
}
