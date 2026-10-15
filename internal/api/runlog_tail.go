// The live tail (design D5; log-live-tail). runTailRegistry is one BFF
// process's shared leases: at most one lease and one subscription per
// run per replica, reference-counted across every local viewer, renewed
// every 30 seconds while any viewer stays connected, released on every
// disconnect (coordinator ruling (s): the start request carries the
// run's jobPath and buildNumber, which the gateway needs to populate
// TailControl).
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// runTailRenewInterval is how often an active lease is renewed.
const runTailRenewInterval = 30 * time.Second

// runTailRequestTimeout bounds one tail-control round trip to the
// gateway.
const runTailRequestTimeout = 5 * time.Second

// runTailRequest is the JSON body of a BFF request on
// bus.TailSubject(cluster, ns, cell) (coordinator ruling (s)).
type runTailRequest struct {
	RunID       string `json:"runId"`
	Action      string `json:"action"`
	JobPath     string `json:"jobPath,omitempty"`
	BuildNumber int64  `json:"buildNumber,omitempty"`
}

// runTailReply is the JSON body the gateway answers a tail-control
// request with.
type runTailReply struct {
	OK           bool   `json:"ok"`
	LeaseSeconds int    `json:"leaseSeconds,omitempty"`
	Error        string `json:"error,omitempty"`
}

// runTailEntry is one run's shared lease: the coordinates a renewal
// needs, the live subscription, the renewal loop's stop signal, and the
// viewer channels this entry fans a raw relay message out to. The
// subscription is a bus.Listener, so it subscribes again after NATS
// denies it.
type runTailEntry struct {
	cluster, ns, name string
	runID             string
	jobPath           string
	buildNumber       int64

	sub       *bus.Listener
	stopRenew chan struct{}
	viewers   map[chan []byte]struct{}
}

// runTailRegistry holds every run this replica currently tails, one
// entry per run with at least one open viewer (log-live-tail, "A viewer
// SHALL hold at most one lease per run per BFF replica").
type runTailRegistry struct {
	conn *bus.Conn

	// renewOverride replaces runTailRenewInterval when positive. Tests
	// set it to shrink the 30-second design default; production code
	// never sets it.
	renewOverride time.Duration

	mu      sync.Mutex
	entries map[string]*runTailEntry
}

// newRunTailRegistry returns a registry that sends its tail-control
// requests and live subscriptions over conn.
func newRunTailRegistry(conn *bus.Conn) *runTailRegistry {
	return &runTailRegistry{conn: conn, entries: make(map[string]*runTailEntry)}
}

// renewInterval returns renewOverride when a test set one, else the
// design default.
func (reg *runTailRegistry) renewInterval() time.Duration {
	if reg.renewOverride > 0 {
		return reg.renewOverride
	}
	return runTailRenewInterval
}

// attach joins (or starts) runID's lease and returns a channel that
// receives every raw message the gateway publishes on the run's log
// subject from this point on. The caller MUST call release exactly
// once, on every disconnect path, so the viewer count never leaks and
// strands a lease (log-live-tail, "release the count for every viewer
// whose connection ends, whatever the cause").
func (reg *runTailRegistry) attach(cluster, ns, name, runID, jobPath string, buildNumber int64) (ch <-chan []byte, release func(), err error) {
	reg.mu.Lock()
	entry, existed := reg.entries[runID]
	if !existed {
		entry = &runTailEntry{cluster: cluster, ns: ns, name: name, runID: runID, jobPath: jobPath, buildNumber: buildNumber, viewers: make(map[chan []byte]struct{})}
		reg.entries[runID] = entry
	}
	viewerCh := make(chan []byte, 64)
	entry.viewers[viewerCh] = struct{}{}
	reg.mu.Unlock()

	release = reg.detachFunc(runID, viewerCh)
	if existed {
		return viewerCh, release, nil
	}

	// A denial returns no error here: the Listener subscribes again in the
	// background. Only a local subscribe failure takes the error path.
	sub, err := reg.conn.Listen(bus.LogSubject(cluster, ns, name, runID), "", func(_ string, data []byte) {
		reg.broadcast(runID, data)
	})
	if err != nil {
		// The subscribe failed before any tail-control request was
		// sent, so there is no lease to stop — undo the registration
		// directly rather than through release(), which would send one.
		reg.mu.Lock()
		delete(entry.viewers, viewerCh)
		delete(reg.entries, runID)
		reg.mu.Unlock()
		close(viewerCh)
		return nil, func() {}, err
	}

	stopRenew := make(chan struct{})
	reg.mu.Lock()
	entry.sub = sub
	entry.stopRenew = stopRenew
	reg.mu.Unlock()

	// The start request's own failure does not tear the subscription
	// down: a chunk the gateway sends anyway still reaches this viewer,
	// and the renewal loop retries on its own schedule regardless.
	_ = reg.sendTail(entry, "start")
	go reg.renewLoop(runID, stopRenew)

	return viewerCh, release, nil
}

// broadcast fans one raw relay message out to every current viewer of
// runID, non-blocking: a slow viewer drops a message rather than
// stalling every other viewer's delivery. The send stays under reg.mu,
// the same lock detachFunc closes a viewer's channel under, so a viewer
// is never sent to after its channel closes.
func (reg *runTailRegistry) broadcast(runID string, data []byte) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	entry, ok := reg.entries[runID]
	if !ok {
		return
	}
	for ch := range entry.viewers {
		select {
		case ch <- data:
		default:
		}
	}
}

// detachFunc returns the release closure for one viewer of runID. It
// removes the viewer, and — only when it was the last one — unsubscribes,
// stops the renewal loop, and sends the stop action.
func (reg *runTailRegistry) detachFunc(runID string, viewerCh chan []byte) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			reg.mu.Lock()
			entry, ok := reg.entries[runID]
			if !ok {
				reg.mu.Unlock()
				return
			}
			if _, present := entry.viewers[viewerCh]; !present {
				reg.mu.Unlock()
				return
			}
			delete(entry.viewers, viewerCh)
			close(viewerCh)
			empty := len(entry.viewers) == 0
			if empty {
				delete(reg.entries, runID)
			}
			reg.mu.Unlock()

			if !empty {
				return
			}
			if entry.stopRenew != nil {
				close(entry.stopRenew)
			}
			// Unsubscribe waits for the Listener loop to exit, so it
			// must stay outside reg.mu.
			if entry.sub != nil {
				_ = entry.sub.Unsubscribe()
			}
			_ = reg.sendTail(entry, "stop")
		})
	}
}

// renewLoop resends the start action every runTailRenewInterval while
// runID's entry still exists, until stop fires.
func (reg *runTailRegistry) renewLoop(runID string, stop chan struct{}) {
	ticker := time.NewTicker(reg.renewInterval())
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			reg.mu.Lock()
			entry, ok := reg.entries[runID]
			reg.mu.Unlock()
			if !ok {
				return
			}
			_ = reg.sendTail(entry, "start")
		}
	}
}

// sendTail sends one tail-control request for entry's run and waits for
// the gateway's reply.
func (reg *runTailRegistry) sendTail(entry *runTailEntry, action string) error {
	body, err := json.Marshal(runTailRequest{Action: action, JobPath: entry.jobPath, BuildNumber: entry.buildNumber, RunID: entry.runID})
	if err != nil {
		return err
	}
	subject := bus.TailSubject(entry.cluster, entry.ns, entry.name)
	msg, err := reg.conn.RequestWithContext(context.Background(), subject, body, runTailRequestTimeout)
	if err != nil {
		return err
	}
	var reply runTailReply
	if err := json.Unmarshal(msg.Data, &reply); err != nil {
		return err
	}
	if !reply.OK {
		return fmt.Errorf("tail %s failed: %s", action, reply.Error)
	}
	return nil
}
