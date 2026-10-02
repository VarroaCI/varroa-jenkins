package main

import (
	"encoding/json"
	"log/slog"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// broodEvent is the four-field payload the gateway publishes on
// events.brood.<cluster>. It names a Controller, never a pod and never
// an instance identifier.
type broodEvent struct {
	Event     string `json:"event"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Cluster   string `json:"cluster"`
}

// broodEventDisconnected and broodEventConnected are the two event values
// this subscriber acts on. The gateway publishes disconnected within about
// 40 seconds of a mite stream ending, and connected when a stream opens.
const (
	broodEventDisconnected = "disconnected"
	broodEventConnected    = "connected"
)

// broodReconcileTrigger is the subset of the Controller reconciler the
// brood subscriber needs. TriggerReconcile routes a wake to the owning
// shard. TriggerOwnedReconcile enqueues only on the owning replica.
type broodReconcileTrigger interface {
	TriggerReconcile(cluster, name, namespace string)
	TriggerOwnedReconcile(cluster, name, namespace string)
}

// startBroodWatch subscribes to the operator's own brood subject. On a
// disconnected event it enqueues a reconcile for the named Controller,
// which performs the pod read, and wakes the tenant ticker, so the
// placement announcement carries the new state without waiting for the
// interval. On a connected event the owning replica reconciles at once.
// That reconcile reads the new epoch and pushes the desired state, which
// NATS does not keep for a reconnecting mite.
//
// The Listener subscribes again after NATS denies the subscription, at
// start or on an authorization reload.
//
// The event decides nothing. It only removes latency: every rule that
// acts on a lost cell rests on the operator's own pod read, so a missed
// event costs one tick and no more.
func startBroodWatch(conn *bus.Conn, cluster string, trigger broodReconcileTrigger,
	wake chan<- struct{}, logger *slog.Logger) (*bus.Listener, error) {
	if logger == nil {
		logger = slog.Default()
	}
	return conn.Listen(bus.BroodSubject(cluster), "", func(_ string, data []byte) {
		handleBroodEvent(data, cluster, trigger, wake, logger)
	})
}

// handleBroodEvent applies the subscriber's rules to one message. It
// ignores a malformed payload, a message naming a foreign cluster, and
// every event but disconnected and connected.
func handleBroodEvent(data []byte, cluster string, trigger broodReconcileTrigger,
	wake chan<- struct{}, logger *slog.Logger) {
	var ev broodEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		logger.Warn("brood event decode failed", "error", err)
		return
	}
	if ev.Cluster != cluster {
		return
	}
	if ev.Name == "" || ev.Namespace == "" {
		return
	}
	switch ev.Event {
	case broodEventDisconnected:
		if trigger != nil {
			trigger.TriggerReconcile(cluster, ev.Name, ev.Namespace)
		}
		notifyWake(wake)
	case broodEventConnected:
		// Every replica receives this broadcast, and so does the owner.
		// A routed wake from each other replica only adds patches.
		if trigger != nil {
			trigger.TriggerOwnedReconcile(cluster, ev.Name, ev.Namespace)
		}
	}
}
