package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// replayResultTimeout bounds the wait for the mite's result. The message then
// gets a nak for later redelivery. It stays below replayAckWait.
const replayResultTimeout = 30 * time.Second

// replayAckWait is the AckWait of the replay consumer.
const replayAckWait = 60 * time.Second

// replayHoldRefresh is the interval of the progress acks for held messages. It
// stays well below replayAckWait, so the server never delivers a held message
// again.
const replayHoldRefresh = replayAckWait / 3

// replayFetchBatch is the most messages that one fetch returns.
const replayFetchBatch = 16

// replayLaneReadTimeout bounds the Tenant read that picks the lane of a tenant
// delivery.
const replayLaneReadTimeout = 5 * time.Second

// replayGiveUpAge is the oldest delivery the consumer still replays. It stays ten
// minutes below the stream MaxAge, so the consumer sees the delivery once more and
// records the drop. The stream deletes an older message and records nothing.
const replayGiveUpAge = bus.WebhookStreamMaxAge - 10*time.Minute

const (
	replayRebuildMinBackoff     = time.Second
	replayRebuildMaxBackoff     = 30 * time.Second
	replayRebuildResetAfter     = time.Minute      // a pass this long restarts the backoff
	replayConsumerCheckInterval = 30 * time.Second // a pass confirms its durable exists
)

// webhookEnvelope is the queued webhook shape the BFF writes to the
// varroa_webhooks stream (see internal/api/handlers_hibernation.go). It
// carries either the controller shape (Namespace and Controller set) or
// the tenant shape (Tenant set, Namespace and Controller both empty) —
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

	// Tenant, Forge, and DeliveryID are set only on a tenant envelope. Tenant
	// is the Tenant name; Forge and DeliveryID are the door's own forge
	// identification and derived delivery id, joined into
	// ReplayWebhookPayload.delivery_id as "<forge>:<delivery-id>".
	Tenant     string `json:"tenant,omitempty"`
	Forge      string `json:"forge,omitempty"`
	DeliveryID string `json:"deliveryId,omitempty"`
}

// isTenantEnvelope reports whether env is the tenant shape. The decision
// reads only the envelope's own fields, never the subject, so a
// Kubernetes namespace literally named "tenant" is never mistaken for the
// tenant shape (design.md D13).
func (env webhookEnvelope) isTenantEnvelope() bool {
	return env.Controller == "" && env.Tenant != ""
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
//   - delivery older than replayGiveUpAge         → Term + rejected activity event
//
// Each target cell has its own lane, so a replay that waits for its result holds
// only its own cell.
//
// A failed consumer pass rebuilds the consumer after a backoff. This function
// returns only when ctx ends.
func (r *Reconciler) RunWebhookReplay(ctx context.Context, conn *bus.Conn, cluster string, replicas int) error {
	superviseReplay(ctx, r.Logger, cluster, func(ctx context.Context) error {
		return r.runWebhookReplayPass(ctx, conn, cluster, replicas, replayConsumerCheckInterval)
	}, waitOrDone, time.Now)
	return nil
}

// superviseReplay runs pass until ctx ends. When a pass returns, it logs the
// error and waits before it runs the pass again. The backoff doubles from
// replayRebuildMinBackoff up to replayRebuildMaxBackoff. A pass that ran for at
// least replayRebuildResetAfter resets the backoff.
func superviseReplay(ctx context.Context, logger *slog.Logger, cluster string,
	pass func(context.Context) error, wait func(context.Context, time.Duration) bool, now func() time.Time) {
	backoff := replayRebuildMinBackoff
	for ctx.Err() == nil {
		start := now()
		err := pass(ctx)
		if ctx.Err() != nil {
			return
		}
		if now().Sub(start) >= replayRebuildResetAfter {
			backoff = replayRebuildMinBackoff
		}
		logger.Error("webhook replay consumer stopped; rebuilding",
			"cluster", cluster, "error", err, "retryIn", backoff)
		if !wait(ctx, backoff) {
			return
		}
		backoff = min(2*backoff, replayRebuildMaxBackoff)
	}
}

// waitOrDone waits for d or until ctx ends. It reports true when the wait ran out.
func waitOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// runWebhookReplayPass runs one consumer session. It returns nil when ctx ends.
// It returns an error when the consumer cannot be set up, a fetch fails, or the
// durable consumer check fails. Every checkEvery, it confirms the durable still
// exists, so a consumer deleted from outside is rebuilt.
func (r *Reconciler) runWebhookReplayPass(ctx context.Context, conn *bus.Conn, cluster string, replicas int, checkEvery time.Duration) error {
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
		AckWait:       replayAckWait,
		MaxDeliver:    -1, // replayGiveUpAge is the real bound; the stream MaxAge is the backstop
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
	// The Listener subscribes again after NATS denies the subscription.
	resultSub, err := conn.Listen(bus.WebhookResultWildcard(cluster), "", func(_ string, data []byte) {
		var res mitev1.CommandResult
		if err := json.Unmarshal(data, &res); err != nil {
			r.Logger.Warn("unmarshal replay result failed", "error", err)
			return
		}
		waiters.deliver(&res)
	})
	if err != nil {
		return fmt.Errorf("subscribe webhook results: %w", err)
	}
	defer func() { _ = resultSub.Unsubscribe() }()

	// The lanes stop before the result subscription ends, so a handler in
	// flight still receives its result or the end of laneCtx.
	laneCtx, cancelLanes := context.WithCancel(ctx)
	lanes := newReplayLanes(laneCtx, func(hctx context.Context, msg *nats.Msg) {
		r.handleQueuedWebhook(hctx, cluster, msg, waiters)
	})
	lanes.keepAlive()
	defer func() {
		cancelLanes()
		lanes.wait()
	}()

	r.Logger.Info("webhook replay consumer started", "cluster", cluster)
	lastCheck := time.Now()
	for {
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(lastCheck) >= checkEvery {
			lastCheck = time.Now()
			checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := conn.ConsumerInfoContext(checkCtx, bus.WebhookStreamName, durable)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("check webhook replay consumer: %w", err)
			}
		}
		msgs, err := conn.DrainConsumer(sub, replayFetchBatch)
		if err != nil {
			// A fetch timeout with no messages is normal.
			if errors.Is(err, nats.ErrTimeout) {
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("fetch queued webhooks: %w", err)
		}
		// Resolve the lane keys at once. A read per message in turn could
		// hold a late message past AckWait before any progress ack covers it.
		keys := make([]string, len(msgs))
		var keyWG sync.WaitGroup
		for i, msg := range msgs {
			keyWG.Add(1)
			go func(i int, msg *nats.Msg) {
				defer keyWG.Done()
				keys[i] = r.replayLaneKey(laneCtx, msg)
			}(i, msg)
		}
		keyWG.Wait()
		for i, msg := range msgs {
			seq := msgSequence(msg)
			if !lanes.add(keys[i], seq, msg) {
				r.Logger.Debug("skipping a redelivery of a held webhook", "cluster", cluster, "sequence", seq)
			}
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
		r.publishReplayRejected(webhookEnvelope{}, "unparseable queued webhook; dropped")
		_ = msg.Term()
		return
	}

	// Defense-in-depth: the consumer's FilterSubject already scopes to this
	// cluster, but never replay a webhook whose envelope names another cluster.
	if env.Cluster != cluster {
		r.Logger.Warn("dropping cross-cluster webhook", "envelopeCluster", env.Cluster, "cluster", cluster)
		r.publishReplayRejected(env, fmt.Sprintf("queued webhook names cluster %s, not %s; dropped", env.Cluster, cluster))
		_ = msg.Term()
		return
	}

	if age, ok := deliveryAge(msg, env, time.Now()); ok && age >= replayGiveUpAge {
		r.Logger.Warn("dropping queued webhook past the replay bound", "cluster", cluster,
			"delivery", deliveryLabel(env), "age", age)
		r.publishReplayRejected(env, fmt.Sprintf("delivery %s was not replayed within %s; dropped",
			deliveryLabel(env), replayGiveUpAge))
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
		r.publishReplayRejected(env, "webhook body is not valid base64; dropped")
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
// delivery time, from the Tenant's current placement, so a tenant that
// moves between ingest and delivery lands on its new cell. It naks with
// the standard delay in four cases — an empty placement, a moving tenant,
// an absent or unreadable Controller, and a Controller that is not
// Connected — and terms only when the Tenant no longer exists. A move
// drains nothing. It re-reads the placement once more immediately before
// the send and naks when that target changed since the checks ran, so a
// move landing in that gap never dispatches to the cell the tenant left.
func (r *Reconciler) handleQueuedTenantWebhook(ctx context.Context, env webhookEnvelope, msg *nats.Msg, waiters *replayWaiters) {
	tenant, err := crdstore.Get[v1alpha1.Tenant](ctx, r.store, env.Tenant, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.Logger.Info("dropping tenant webhook for deleted tenant", "tenant", env.Tenant)
			r.publishReplayRejected(env, "tenant no longer exists")
			_ = msg.Term()
			return
		}
		// Transient read error: the Tenant owns this delivery, hold it.
		_ = msg.NakWithDelay(replayResultTimeout)
		return
	}

	placement := tenant.Status.Placement
	if placement == nil || placement.Cell == "" || placement.CellNamespace == "" {
		// Nak case 1: an empty placement.
		_ = msg.NakWithDelay(replayResultTimeout)
		return
	}
	if tenantMoving(tenant) {
		// Nak case 2: the Tenant reports Placed=False with reason
		// TenantMoving.
		_ = msg.NakWithDelay(replayResultTimeout)
		return
	}

	cr, err := crdstore.Get[v1alpha1.Controller](ctx, r.store, placement.Cell, placement.CellNamespace)
	if err != nil {
		// Nak case 3: the named Controller is absent or unreadable. The
		// Tenant, not the Controller, owns this delivery — it never terms
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
		r.Logger.Warn("dropping tenant webhook with bad body encoding", "tenant", env.Tenant, "error", err)
		r.publishReplayRejected(env, "webhook body is not valid base64; dropped")
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

	// Re-read the Tenant immediately before the send. The nak checks above
	// ran against the placement this handler read at the top, and a move
	// can start or land in the gap between that read and this send.
	// Sending against a stale decision would dispatch to a cell the
	// tenant already left, or is leaving; nak instead so the redelivery
	// re-evaluates the full decision table against the now-current Tenant.
	// This checks both the placement target and the moving state,
	// because a move sets Placed=False/TenantMoving while it still holds
	// the old placement (design.md D13, D4 move order).
	current, err := crdstore.Get[v1alpha1.Tenant](ctx, r.store, env.Tenant, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.Logger.Info("dropping tenant webhook for deleted tenant", "tenant", env.Tenant)
			r.publishReplayRejected(env, "tenant no longer exists")
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
		tenantMoving(current) {
		r.Logger.Info("tenant placement changed just before send, redelivering", "tenant", env.Tenant)
		_ = msg.NakWithDelay(replayResultTimeout)
		return
	}

	resultCh := waiters.register(commandID)
	defer waiters.unregister(commandID)

	if err := r.miteTransport.SendImperative(ctx, placement.CellNamespace, placement.Cell, cmd); err != nil {
		r.Logger.Warn("send tenant replay command failed", "tenant", env.Tenant,
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

// tenantMoving reports whether tenant's Placed condition is currently False
// with reason TenantMoving. A tenant in that state holds its buffered
// deliveries until the move completes.
func tenantMoving(tenant *v1alpha1.Tenant) bool {
	for _, c := range tenant.Status.Conditions {
		if c.Type == v1alpha1.TenantConditionPlaced {
			return c.Status == metav1.ConditionFalse && c.Reason == v1alpha1.TenantReasonTenantMoving
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
		// event by, so the tenant name goes into the message instead.
		message = fmt.Sprintf("tenant %s: %s", env.Tenant, reason)
	}
	r.activityPublisher.Publish(activity.Event{
		Type:       "webhook.replay.rejected",
		Source:     "operator",
		Severity:   "warning",
		Controller: env.Controller,
		Namespace:  env.Namespace,
		Message:    message,
		Timestamp:  time.Now().UTC(),
	})
}

// deliveryAge returns how long the delivery has waited in the queue. It reads the
// stream timestamp when present, then the envelope's receivedAt. It reports false
// when neither is known, so a message without a timestamp is never expired.
func deliveryAge(msg *nats.Msg, env webhookEnvelope, now time.Time) (time.Duration, bool) {
	if md, err := msg.Metadata(); err == nil && md != nil && !md.Timestamp.IsZero() {
		return now.Sub(md.Timestamp), true
	}
	if t, err := time.Parse(time.RFC3339, env.ReceivedAt); err == nil {
		return now.Sub(t), true
	}
	return 0, false
}

// deliveryLabel names a queued delivery in logs and events.
func deliveryLabel(env webhookEnvelope) string {
	if env.isTenantEnvelope() {
		return env.Forge + ":" + env.DeliveryID
	}
	return env.Path
}

// replayLaneKey names the lane of a queued webhook. A lane sends one replay at
// a time, because the mite of a controller runs one command at a time. A
// controller envelope uses "<namespace>/<controller>". A tenant envelope uses
// the placement cell of the tenant in the same form, so the tenants of one cell
// share a lane. A tenant with no readable placement uses "tenant:<name>". An
// envelope that does not decode uses the empty key.
func (r *Reconciler) replayLaneKey(ctx context.Context, msg *nats.Msg) string {
	var env webhookEnvelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		return ""
	}
	if !env.isTenantEnvelope() {
		return env.Namespace + "/" + env.Controller
	}
	readCtx, cancel := context.WithTimeout(ctx, replayLaneReadTimeout)
	defer cancel()
	tenant, err := crdstore.Get[v1alpha1.Tenant](readCtx, r.store, env.Tenant, "")
	if err != nil || tenant.Status.Placement == nil ||
		tenant.Status.Placement.Cell == "" || tenant.Status.Placement.CellNamespace == "" {
		return "tenant:" + env.Tenant
	}
	return tenant.Status.Placement.CellNamespace + "/" + tenant.Status.Placement.Cell
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
