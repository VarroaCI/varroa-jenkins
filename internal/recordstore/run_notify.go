package recordstore

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// Run notification bounds. A reader sees at most one notification per run
// per interval, and the state map holds one entry per run that changed in
// the last idle window.
const (
	runNotifyInterval = 500 * time.Millisecond
	runNotifyIdle     = 5 * time.Minute
	runNotifyJanitor  = 60 * time.Second
)

// RunNotification is the JSON payload of one live run notification. The
// BFF fan-out reads the cluster and the tenant from this body, never from
// the subject.
type RunNotification struct {
	Kind    string `json:"kind"`
	Cluster string `json:"cluster"`
	Tenant  string `json:"tenant"`
	JobPath string `json:"job_path"`
	RunID   string `json:"run_id"`
	// BuildNumber stays zero for every kind but run.started, and the
	// body then omits it.
	BuildNumber int64 `json:"build_number,omitempty"`
	// Status is empty when the write changed no run state. The reader
	// refetches the record either way, because the record is the truth.
	Status string `json:"status"`
	// Result carries the payload's result for a completed run, and the
	// TRIGGER_FAILED code for a refused trigger.
	Result string `json:"result"`
	// LockName is the qualified name of the fleet lock a fleet_locks
	// write changed, of the form <tenant>/<bare-name>. It stays empty for
	// every other kind, and the body then omits it.
	LockName string `json:"lock_name,omitempty"`
	// State is that lock's own state after the write: waiting, held,
	// releasing, or released. It stays empty for every other kind, and
	// the body then omits it.
	State string `json:"state,omitempty"`
	// Seq numbers the notifications of one publish, and At holds the
	// publish time.
	Seq uint64    `json:"seq"`
	At  time.Time `json:"at"`
}

// lockNotification builds the notification one fleet_locks write
// publishes. cluster, tenant, jobPath, lockName, and runID name the row
// the statement changed, never the caller's own request: the affected row
// can belong to another cluster, tenant, or run. state is that row's own
// state after the write. Seq and At stay zero, because Notify stamps both
// under its own lock.
func lockNotification(cluster, tenant, jobPath, lockName, runID, state string) RunNotification {
	return RunNotification{
		Kind:     "lock",
		Cluster:  cluster,
		Tenant:   tenant,
		JobPath:  jobPath,
		RunID:    runID,
		LockName: lockName,
		State:    state,
	}
}

// notificationFor builds the notification one ingest envelope publishes.
// It reads the envelope alone, so no route re-reads the row it wrote.
// ok is false for a kind that publishes nothing.
func notificationFor(env IngestEnvelope, res ApplyResult) (RunNotification, bool) {
	n := RunNotification{
		Cluster: env.Cluster,
		Tenant:  env.Tenant,
		JobPath: env.JobPath,
		RunID:   env.RunID,
	}
	switch env.Kind {
	case KindRunStarted:
		if res.SupersededStart {
			return RunNotification{}, false
		}
		n.Kind = "run"
		n.Status = "RUNNING"
		n.BuildNumber = startedBuildNumber(env)
	case KindRunCompleted:
		n.Kind = "run"
		n.Status = "COMPLETED"
		n.Result = completedResult(env)
	case KindRunStage:
		n.Kind = "stage"
	// These kinds report facts about a run without changing a state a
	// reader displays, so the notification carries the identity alone.
	case KindRunHeartbeat, KindRunLog, KindRunChanges, KindRunTests,
		KindRunResumeFailed:
		n.Kind = "run"
	case KindRunState:
		// A run-state push outcome changes no value a reader displays,
		// and a reader has nothing to refetch for it. The kind
		// publishes nothing.
		return RunNotification{}, false
	case KindRunSource:
		// A refused write changed no stored value, so it reports
		// nothing, like a superseded start.
		if res.RefusedSource {
			return RunNotification{}, false
		}
		n.Kind = "run"
	case KindRunInput:
		// A refused write changed no stored value, so it reports
		// nothing, like a refused run.source write.
		if res.RefusedInput {
			return RunNotification{}, false
		}
		n.Kind = "input"
	case KindRunCommandRefused:
		// A skipped write changed no stored value, so it reports
		// nothing. A stored notice changes the run detail, so an open
		// build page refetches it.
		if !res.CommandRefusedStored {
			return RunNotification{}, false
		}
		n.Kind = "run"
	case KindRunTriggerAccepted:
		n.Kind = "run"
		n.Status = "QUEUED"
	case KindRunTriggerFailed:
		n.Kind = "run"
		n.Status = "COMPLETED"
		n.Result = "TRIGGER_FAILED"
	default:
		// run.artifacts writes no run state, and job.schedules writes no
		// run row, so neither publishes.
		return RunNotification{}, false
	}
	return n, true
}

// startedBuildNumber reads the build number from a run.started payload. A
// payload that does not decode, or that carries no number, yields zero.
func startedBuildNumber(env IngestEnvelope) int64 {
	var p RunStartedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil || p.BuildNumber == nil {
		return 0
	}
	return *p.BuildNumber
}

// completedResult reads the result from a run.completed payload. A payload
// that does not decode, or that carries no result, yields the empty string.
func completedResult(env IngestEnvelope) string {
	var p RunCompletedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil || p.Result == nil {
		return ""
	}
	return *p.Result
}

// RunNotifier coalesces live run notifications and publishes them on the
// core-NATS subject the BFF subscribes to. It publishes at most one
// notification per run per interval, and a terminal one at once.
type RunNotifier struct {
	// send publishes one payload. It is conn.Publish in production, and
	// a test replaces it.
	send func(subject string, data []byte) error

	logger   *slog.Logger
	interval time.Duration
	idle     time.Duration
	// now reads the clock, so a test injects its own.
	now func() time.Time
	// seq numbers every published notification.
	seq atomic.Uint64
	// janitor bounds the state map when Start runs.
	janitor *time.Ticker

	// mu guards state and every entry it holds.
	mu sync.Mutex
	// state holds one entry per run, keyed by run_id.
	state map[string]*runNotifyEntry
}

// runNotifyEntry holds one run's coalescing state.
type runNotifyEntry struct {
	lastSent time.Time
	pending  *RunNotification
	timer    *time.Timer
}

// NewRunNotifier returns a notifier that publishes on conn. A nil
// connection leaves a notifier that sends nothing, which a test with no
// bus relies on.
func NewRunNotifier(conn *bus.Conn, logger *slog.Logger) *RunNotifier {
	if logger == nil {
		logger = slog.Default()
	}
	n := &RunNotifier{
		logger:   logger,
		interval: runNotifyInterval,
		idle:     runNotifyIdle,
		now:      func() time.Time { return time.Now().UTC() },
		state:    make(map[string]*runNotifyEntry),
	}
	if conn != nil {
		n.send = conn.Publish
	}
	return n
}

// Notify publishes n now or holds it for the end of its run's interval.
// Three rules decide, in this order: a terminal state publishes at once, a
// run outside its interval publishes at once, and anything else becomes
// the run's newest held state. A nil notifier publishes nothing, so a
// caller needs no guard of its own.
func (r *RunNotifier) Notify(n RunNotification) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// The clock read happens under the lock, so seq and at order
	// together.
	now := r.now()
	entry := r.state[n.RunID]

	// A terminal state never coalesces: a reader must learn that the run
	// ended, and the held state is older than this one.
	if isTerminalNotification(n) {
		if entry != nil {
			r.dropEntry(n.RunID, entry)
		}
		r.publish(n, now)
		return
	}

	// A run with no entry, or one whose last publish is a full interval
	// back, publishes at once. The state just published is newer than any
	// held state, so the held one goes.
	if entry == nil {
		r.state[n.RunID] = &runNotifyEntry{lastSent: now}
		r.publish(n, now)
		return
	}
	if !now.Before(entry.lastSent.Add(r.interval)) {
		entry.pending = nil
		if entry.timer != nil {
			entry.timer.Stop()
			entry.timer = nil
		}
		entry.lastSent = now
		r.publish(n, now)
		return
	}

	// Inside the interval: hold the newest state, and arm one timer for
	// the end of the interval when none runs.
	entry.pending = &n
	if entry.timer == nil {
		delay := entry.lastSent.Add(r.interval).Sub(now)
		entry.timer = time.AfterFunc(delay, func() { r.flush(n.RunID) })
	}
}

// isTerminalNotification reports whether a notification reports a state
// the run cannot leave.
func isTerminalNotification(n RunNotification) bool {
	return n.Status == "COMPLETED" || n.Result != ""
}

// flush publishes the notification one run holds. The run's timer calls it,
// and a test calls it directly with an injected clock.
func (r *RunNotifier) flush(runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.state[runID]
	if entry == nil {
		return
	}
	entry.timer = nil
	if entry.pending == nil {
		return
	}
	now := r.now()
	n := *entry.pending
	entry.pending = nil
	entry.lastSent = now
	r.publish(n, now)
}

// dropEntry forgets one run, and stops its timer when one runs. The caller
// holds the lock.
func (r *RunNotifier) dropEntry(runID string, entry *runNotifyEntry) {
	entry.pending = nil
	if entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}
	delete(r.state, runID)
}

// sweep deletes every entry that holds no held notification and whose last
// publish is older than the idle window. The sweep bounds the map: a run
// whose writes stay more than one interval apart arms no timer.
func (r *RunNotifier) sweep(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for runID, entry := range r.state {
		if entry.pending != nil {
			continue
		}
		if now.Sub(entry.lastSent) >= r.idle {
			delete(r.state, runID)
		}
	}
}

// Start sweeps the state map every janitor interval, and returns when ctx
// ends. The caller runs it in its own goroutine.
func (r *RunNotifier) Start(ctx context.Context) {
	ticker := time.NewTicker(runNotifyJanitor)
	defer ticker.Stop()

	r.mu.Lock()
	if r.janitor != nil {
		r.mu.Unlock()
		return
	}
	r.janitor = ticker
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.janitor == ticker {
			r.janitor = nil
		}
		r.mu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// The clock read happens under the lock, like every
			// other read of it.
			r.mu.Lock()
			now := r.now()
			r.mu.Unlock()
			r.sweep(now)
		}
	}
}

// Close stops the janitor and every timer, and publishes every held
// notification. A recorder that stops here loses no notification a run
// already earned.
func (r *RunNotifier) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.janitor != nil {
		r.janitor.Stop()
		r.janitor = nil
	}
	now := r.now()
	for runID, entry := range r.state {
		if entry.timer != nil {
			entry.timer.Stop()
			entry.timer = nil
		}
		if entry.pending != nil {
			n := *entry.pending
			entry.pending = nil
			entry.lastSent = now
			r.publish(n, now)
		}
		delete(r.state, runID)
	}
}

// publish sends one notification on its tenant's subject. The caller holds
// the lock, and a drop or a failed send logs one line and returns nothing,
// because no caller may treat a lost notification as an ingest failure.
func (r *RunNotifier) publish(n RunNotification, now time.Time) {
	if !natsSubjectToken(n.Cluster) || !natsSubjectToken(n.Tenant) {
		r.logger.Warn("dropping run notification with an unusable subject token",
			"kind", n.Kind, "cluster", n.Cluster, "tenant", n.Tenant, "runID", n.RunID)
		return
	}
	n.Seq = r.seq.Add(1)
	n.At = now
	if r.send == nil {
		return
	}
	data, err := json.Marshal(n)
	if err != nil {
		r.logger.Error("marshal run notification failed", "kind", n.Kind, "runID", n.RunID, "error", err)
		return
	}
	if err := r.send(bus.RunNotifySubject(n.Cluster, n.Tenant), data); err != nil {
		r.logger.Error("publish run notification failed", "kind", n.Kind, "runID", n.RunID, "error", err)
	}
}

// natsSubjectToken reports whether value forms one NATS subject token. An
// empty value names no token, so it is unusable. A space, a star, and a
// greater-than sign are the three characters NATS refuses inside a token.
func natsSubjectToken(value string) bool {
	return value != "" && !strings.ContainsAny(value, " *>")
}
