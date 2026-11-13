package bus

import (
	"context"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// JetStream health bounds of every bus connection. A NATS server cut off
// from its cluster peers still answers pings, so the ping bounds do not
// see it. Every JSProbeInterval, the Conn asks for the JetStream account
// status ($JS.API.INFO). Only a server in contact with the JetStream meta
// leader answers it. At each JSProbeFailures-th failed probe in a row, the
// Conn opens a second connection to the bus URL and probes there. It
// reconnects only when that connection reaches another server and the
// probe passes there. A JetStream outage of the whole cluster thus causes
// no reconnect. A cut-off meta leader keeps its role for up to
// metaStepdownBound. JSMoveBound is the longest time from the loss of the
// peers to the first reconnect when another server answers.
const (
	JSProbeInterval   = 10 * time.Second
	JSProbeTimeout    = 3 * time.Second
	JSProbeFailures   = 2
	metaStepdownBound = 20 * time.Second
	JSMoveBound       = metaStepdownBound + JSProbeFailures*JSProbeInterval + 3*JSProbeTimeout
)

// jsEnv is what the JetStream health loop reads and does. connJSEnv is the
// production form. Tests use a fake.
type jsEnv interface {
	connected() bool
	server() (id, name string)
	probe() error
	probeOther() (id, name string, err error)
	forceReconnect() error
}

type connJSEnv struct {
	ctx context.Context
	c   *Conn
}

func (e connJSEnv) connected() bool { return e.c.nc.IsConnected() }

func (e connJSEnv) server() (string, string) {
	return e.c.nc.ConnectedServerId(), e.c.nc.ConnectedServerName()
}

func (e connJSEnv) probe() error { return probeJetStream(e.ctx, e.c.js) }

// probeOther opens a second connection with the identity of the Conn and
// probes JetStream there. A Service or a server list can send it to
// another server. A failed dial gives an empty id.
func (e connJSEnv) probeOther() (string, string, error) {
	// nats.Connect takes no context, so the dial runs aside. A cancelled loop
	// returns at once. A dial that finishes later closes its own connection.
	type dialed struct {
		nc  *nats.Conn
		err error
	}
	ch := make(chan dialed, 1)
	go func() {
		nc, err := e.c.dialProbe()
		ch <- dialed{nc, err}
	}()
	var d dialed
	select {
	case d = <-ch:
	case <-e.ctx.Done():
		go func() {
			if late := <-ch; late.nc != nil {
				late.nc.Close()
			}
		}()
		return "", "", e.ctx.Err()
	}
	if d.err != nil {
		return "", "", d.err
	}
	nc := d.nc
	defer nc.Close()
	id, name := nc.ConnectedServerId(), nc.ConnectedServerName()
	js, err := nc.JetStream()
	if err != nil {
		return id, name, err
	}
	return id, name, probeJetStream(e.ctx, js)
}

func (e connJSEnv) forceReconnect() error { return e.c.nc.ForceReconnect() }

func probeJetStream(ctx context.Context, js nats.JetStreamContext) error {
	ctx, cancel := context.WithTimeout(ctx, JSProbeTimeout)
	defer cancel()
	_, err := js.AccountInfo(nats.Context(ctx))
	return err
}

// jsMonitor counts failed probes and decides when to move. Only the health
// loop goroutine uses it. failures must be at least 1.
type jsMonitor struct {
	logger   *slog.Logger
	failures int
	fails    int
	held     bool
}

func (m *jsMonitor) log(level slog.Level, msg string, args ...any) {
	if m.logger != nil {
		m.logger.Log(context.Background(), level, msg, args...)
	}
}

// step runs one health check. It returns true when it started a reconnect.
func (m *jsMonitor) step(env jsEnv) bool {
	// nats.go owns a connection that is down.
	if !env.connected() {
		return false
	}
	err := env.probe()
	if err == nil {
		if m.fails >= m.failures {
			_, name := env.server()
			m.log(slog.LevelInfo, "nats server answers JetStream again", "server", name)
		}
		m.fails, m.held = 0, false
		return false
	}
	m.fails++
	// Check another server only at each failures-th failure in a row.
	if m.fails%m.failures != 0 {
		return false
	}
	id, name := env.server()
	otherID, otherName, otherErr := env.probeOther()
	if otherID == id {
		m.log(slog.LevelDebug, "the probe connection reached the same NATS server", "server", name)
		return false
	}
	if otherErr != nil {
		level := slog.LevelDebug
		if !m.held {
			level = slog.LevelWarn
		}
		m.held = true
		m.log(level, "no other NATS server answers JetStream; keeping the connection",
			"server", name, "error", err, "probeError", otherErr)
		return false
	}
	m.log(slog.LevelWarn, "nats server does not answer JetStream; reconnecting to another server",
		"server", name, "error", err, "answeringServer", otherName)
	if rerr := env.forceReconnect(); rerr != nil {
		m.log(slog.LevelWarn, "nats reconnect failed", "error", rerr)
		return false
	}
	// One more failure checks again, so a reconnect that reaches the same
	// server is retried on the next tick.
	m.fails, m.held = m.failures-1, false
	return true
}

// startJSHealth starts the JetStream health loop of c.
func (c *Conn) startJSHealth(interval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	c.jsCancel = cancel
	c.jsDone = make(chan struct{})
	go c.runJSHealth(ctx, interval, connJSEnv{ctx: ctx, c: c})
}

func (c *Conn) runJSHealth(ctx context.Context, interval time.Duration, env jsEnv) {
	defer close(c.jsDone)
	m := &jsMonitor{logger: c.logger, failures: JSProbeFailures}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.step(env)
		}
	}
}

// stopJSHealth stops the loop and waits for it. A probe connection that is
// still dialing can delay the return by up to JSProbeTimeout.
func (c *Conn) stopJSHealth() {
	if c.jsCancel == nil {
		return
	}
	c.jsCancel()
	<-c.jsDone
}
