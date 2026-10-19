package sessions

import (
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// SubscribeKillSwitch refreshes the settings cache each time the global
// kill switch changes. Every running session reads the switch before each
// provider call, so a refresh is enough to stop the next call. The returned
// subscription is the caller's to drain.
func SubscribeKillSwitch(conn *bus.Conn, refresh func()) (*nats.Subscription, error) {
	return conn.Subscribe(bus.AssistantControlSubject, func(*nats.Msg) {
		refresh()
	})
}
