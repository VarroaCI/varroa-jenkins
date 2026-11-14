package sessions

import (
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// SubscribeKillSwitch calls refresh on each control notice. A running session
// reads the settings row before each provider call, so the stop does not need
// the notice. The notice only makes the status ticker show the change sooner.
// The returned subscription is the caller's to drain.
func SubscribeKillSwitch(conn *bus.Conn, refresh func()) (*nats.Subscription, error) {
	return conn.Subscribe(bus.AssistantControlSubject, func(*nats.Msg) {
		refresh()
	})
}
