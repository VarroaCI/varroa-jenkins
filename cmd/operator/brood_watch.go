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

// broodEventDisconnected is the one event value this subscriber acts
// on. The gateway publishes it within about 40 seconds of a mite stream
// ending.
const broodEventDisconnected = "disconnected"

// broodReconcileTrigger is the subset of the Controller reconciler the
// brood subscriber needs.
type broodReconcileTrigger interface {
	TriggerReconcile(cluster, name, namespace string)
}

// startBroodWatch subscribes to the operator's own brood subject. On a
// disconnected event it enqueues a reconcile for the named Controller,
// which performs the pod read, and wakes the tenant ticker, so the
// placement announcement carries the new state without waiting for the
// interval.
//
// The event decides nothing. It only removes latency: every rule that
// acts on a lost cell rests on the operator's own pod read, so a missed
// event costs one tick and no more.
func startBroodWatch(conn *bus.Conn, cluster string, trigger broodReconcileTrigger,
	wake chan<- struct{}, logger *slog.Logger) (*bus.Sub, error) {
	if logger == nil {
		logger = slog.Default()
	}
	return conn.SubscribeData(bus.BroodSubject(cluster), func(data []byte) {
		handleBroodEvent(data, cluster, trigger, wake, logger)
	})
}

// handleBroodEvent applies the subscriber's rules to one message. It
// ignores a malformed payload, a message naming a foreign cluster, and
// every event but disconnected.
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
	if ev.Event != broodEventDisconnected {
		return
	}
	if ev.Name == "" || ev.Namespace == "" {
		return
	}
	if trigger != nil {
		trigger.TriggerReconcile(cluster, ev.Name, ev.Namespace)
	}
	notifyWake(wake)
}
