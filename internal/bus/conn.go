package bus

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Config holds optional connection parameters for bus.Connect. An empty Config
// (the zero value) preserves current behavior (plaintext, no auth, default inbox).
type Config struct {
	// CAFile is a path to a PEM-encoded CA certificate file.
	// Ignored if CABytes is non-empty.
	CAFile string

	// CABytes holds raw PEM-encoded CA certificate bytes.
	// Takes precedence over CAFile.
	CABytes []byte

	// Username for NATS user/password authentication.
	// Empty means no credential is sent, which is why it is required whenever
	// PasswordFile is set: Connect rejects that combination rather than
	// sending an empty user with a real password.
	Username string

	// Password for NATS user/password authentication.
	Password string

	// PasswordFile is a path whose contents (trimmed) are the password. It is
	// re-read on every connect attempt, so a rotated credential that lands in a
	// mounted Secret is picked up without a restart. Takes precedence over
	// Password.
	PasswordFile string

	// InboxPrefix overrides the default _INBOX.> reply subject.
	// Use per-service prefixes like _INBOX_operator for ACL isolation.
	InboxPrefix string

	// StartupTimeout bounds how long Connect waits for the first successful
	// connection. Connect retries a failed initial dial or auth rather than
	// giving up, so a process that starts while the bus is down or while a
	// rotated password has not yet reached its mounted Secret waits instead of
	// exiting. Zero means DefaultStartupTimeout.
	StartupTimeout time.Duration

	// Logger receives connection lifecycle events. It is the only way to give
	// a Conn a logger: the NATS callbacks that read it run on library
	// goroutines and the credential handler fires inside nats.Connect, so a
	// logger assigned after Connect returns would be an unsynchronized write
	// to a field already being read. Nil disables that logging.
	Logger *slog.Logger
}

// IsZero returns true when no options are set (empty config).
func (c Config) IsZero() bool {
	return c.CAFile == "" && len(c.CABytes) == 0 && c.Username == "" && c.Password == "" &&
		c.PasswordFile == "" && c.InboxPrefix == ""
}

// DefaultStartupTimeout bounds Connect's initial connection wait when
// Config.StartupTimeout is unset. It is long enough to outlast a credential
// rotation reaching a mounted Secret, and short enough that a permanently
// broken configuration still surfaces as a failing process.
const DefaultStartupTimeout = 3 * time.Minute

// Conn wraps a NATS connection and its JetStream context.
type Conn struct {
	nc *nats.Conn
	js nats.JetStreamContext

	// logger is written once, by Connect, before any handler that reads it can
	// run. It is never assigned afterwards; see Config.Logger.
	logger *slog.Logger

	// replicas is the JetStream replica count applied to streams and KV
	// buckets created via this connection (EnsureKV, and the *StreamConfig
	// builders when they read Conn.Replicas()). It is sourced once from the
	// VARROA_JETSTREAM_REPLICAS env at process startup. A zero value means
	// "unset" and is treated as 1 by Replicas().
	replicas int
}

// clampReplicas clamps a JetStream replica count to a sane floor of 1.
// JetStream requires at least one replica; values < 1 (including the Conn
// zero value) are treated as 1.
func clampReplicas(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// Replicas returns the JetStream replica count used for streams and KV
// buckets created via this connection. It is always >= 1.
func (c *Conn) Replicas() int { return clampReplicas(c.replicas) }

// SetReplicas sets the JetStream replica count used by EnsureKV and by
// callers that build stream configs from Conn.Replicas(). Values < 1 are
// clamped to 1. Call this once, right after Connect, with the value read
// from VARROA_JETSTREAM_REPLICAS.
func (c *Conn) SetReplicas(n int) { c.replicas = clampReplicas(n) }

// Connect establishes a connection to the NATS server at the given URL.
// If url is empty, it connects to the default NATS URL (nats://localhost:4222).
// Optional Config values are applied at this single chokepoint.
func Connect(url string, cfg ...Config) (*Conn, error) {
	if url == "" {
		url = nats.DefaultURL
	}

	c := &Conn{}
	// Install the logger before any handler closure is built. This sits
	// outside the IsZero gate below because a Config carrying nothing but a
	// Logger is still worth honouring.
	startupTimeout := DefaultStartupTimeout
	if len(cfg) > 0 {
		c.logger = cfg[0].Logger
		if cfg[0].StartupTimeout > 0 {
			startupTimeout = cfg[0].StartupTimeout
		}
	}
	opts := []nats.Option{
		nats.Name("varroa"),
		nats.ReconnectWait(2 * time.Second),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if c.logger != nil {
				c.logger.Warn("nats disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			if c.logger != nil {
				c.logger.Info("nats reconnected")
			}
		}),
		// Retry the initial dial instead of failing the process: a pod that
		// starts while the bus is down, or inside the window before a rotated
		// Secret reaches its mount, must wait rather than crash-loop. Connect
		// blocks below until this succeeds or the startup budget runs out.
		nats.RetryOnFailedConnect(true),
		// A rotated server-side password produces repeated auth errors until
		// the mounted Secret catches up; nats.go would otherwise stop
		// reconnecting after the second one.
		nats.IgnoreAuthErrorAbort(),
		// ReconnectErrHandler only fires when the TCP dial itself fails; an
		// "Authorization Violation" during reconnect is delivered as an async
		// error, so both handlers are needed to make the outage window loud.
		nats.ReconnectErrHandler(func(_ *nats.Conn, err error) {
			if c.logger != nil {
				c.logger.Warn("nats reconnect attempt failed", "error", err)
			}
		}),
		nats.ErrorHandler(func(_ *nats.Conn, sub *nats.Subscription, err error) {
			if c.logger == nil {
				return
			}
			if sub != nil {
				c.logger.Warn("nats async error", "subject", sub.Subject, "error", err)
				return
			}
			c.logger.Warn("nats async error", "error", err)
		}),
		// Without this, nats.go fires the closed callback for a client-initiated
		// Close too, so every graceful shutdown would log the permanent-close
		// error that operators are told means an unrecoverable connection.
		nats.NoCallbacksAfterClientClose(),
		nats.ClosedHandler(func(nc *nats.Conn) {
			if c.logger != nil {
				c.logger.Error("nats connection closed permanently", "error", nc.LastError())
			}
		}),
	}

	// Apply optional Config if provided (at most one).
	if len(cfg) > 0 && !cfg[0].IsZero() {
		conf := cfg[0]

		// TLS: CA certificate.
		switch {
		case len(conf.CABytes) > 0:
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(conf.CABytes) {
				return nil, fmt.Errorf("nats connect: no CA certs parsed from CABytes")
			}
			opts = append(opts, nats.Secure(&tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			}))
		case conf.CAFile != "":
			opts = append(opts, nats.RootCAs(conf.CAFile))
		}

		// Credentials. A password file wins over a static password: the
		// handler is consulted on every connect attempt, so the process
		// follows a rotation without a restart.
		switch {
		case conf.PasswordFile != "":
			// A handler is the only way to send a rotating password, and it
			// must carry a username: without one the server receives an empty
			// user alongside a real password and rejects it as an ordinary
			// auth failure, hiding the misconfiguration.
			if conf.Username == "" {
				return nil, fmt.Errorf("nats connect: PasswordFile requires Username")
			}
			user, file := conf.Username, conf.PasswordFile
			// Fail fast at startup on an unreadable file; a later read failure
			// during reconnect only logs, so a transient kubelet resync gap
			// does not turn into a fatal error.
			if _, err := readPasswordFile(file); err != nil {
				return nil, fmt.Errorf("nats connect: %w", err)
			}
			opts = append(opts, nats.UserInfoHandler(func() (string, string) {
				pass, err := readPasswordFile(file)
				if err != nil && c.logger != nil {
					c.logger.Warn("nats password file unreadable; retrying with empty password", "path", file, "error", err)
				}
				return user, pass
			}))
		case conf.Username != "":
			opts = append(opts, nats.UserInfo(conf.Username, conf.Password))
		}

		// Custom inbox prefix.
		if conf.InboxPrefix != "" {
			opts = append(opts, nats.CustomInboxPrefix(conf.InboxPrefix))
		}
	}

	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	c.nc = nc

	// RetryOnFailedConnect makes nats.Connect hand back a connection that is
	// still only RECONNECTING, so callers would otherwise get a Conn whose
	// JetStream and KV calls fail. Wait for a real connection here; the
	// registered handlers log each failed attempt while this blocks.
	if err := c.waitConnected(startupTimeout); err != nil {
		nc.Close()
		return nil, err
	}

	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("nats jetstream: %w", err)
	}
	c.js = js

	return c, nil
}

// readPasswordFile reads and trims the credential stored at path.
func readPasswordFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read bus password file %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// waitConnected blocks until the connection reaches CONNECTED, the server
// closes it for good, or the budget expires.
func (c *Conn) waitConnected(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		switch c.nc.Status() {
		case nats.CONNECTED:
			return nil
		case nats.CLOSED:
			return fmt.Errorf("nats connect: connection closed before it was established: %w", c.nc.LastError())
		}
		if time.Now().After(deadline) {
			if lastErr := c.nc.LastError(); lastErr != nil {
				return fmt.Errorf("nats connect: not connected after %s: %w", timeout, lastErr)
			}
			return fmt.Errorf("nats connect: not connected after %s", timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Connected reports whether the underlying NATS connection is currently
// established. It is the single source for readiness checks and the
// connected gauge.
func (c *Conn) Connected() bool {
	return c.nc != nil && c.nc.IsConnected()
}

// RegisterMetrics exports varroa.bus.connected (1 while connected, 0
// otherwise) on the given meter, tagged with the calling component.
func (c *Conn) RegisterMetrics(meter metric.Meter, component string) error {
	_, err := meter.Int64ObservableGauge(
		"varroa.bus.connected",
		metric.WithDescription("1 while the NATS bus connection is established, 0 otherwise"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			v := int64(0)
			if c.Connected() {
				v = 1
			}
			obs.Observe(v, metric.WithAttributes(attribute.String("component", component)))
			return nil
		}),
	)
	return err
}

// NATSConn returns the underlying *nats.Conn for advanced use.
func (c *Conn) NATSConn() *nats.Conn { return c.nc }

// JetStream returns the JetStream context.
func (c *Conn) JetStream() nats.JetStreamContext { return c.js }

// Publish publishes data to the given subject on core NATS (lossy, at-most-once).
func (c *Conn) Publish(subject string, data []byte) error {
	return c.nc.Publish(subject, data)
}

// Subscribe subscribes to the given subject on core NATS. The callback is
// invoked on incoming messages. It is safe to call from multiple goroutines
// across different subjects.
func (c *Conn) Subscribe(subject string, cb nats.MsgHandler) (*nats.Subscription, error) {
	return c.nc.Subscribe(subject, cb)
}

// QueueSubscribe subscribes with a queue group for load-balanced delivery.
func (c *Conn) QueueSubscribe(subject, queue string, cb nats.MsgHandler) (*nats.Subscription, error) {
	return c.nc.QueueSubscribe(subject, queue, cb)
}

// Request sends a request and waits for a single response (request-reply).
func (c *Conn) Request(subject string, data []byte, timeout time.Duration) (*nats.Msg, error) {
	return c.nc.Request(subject, data, timeout)
}

// RequestWithContext sends a request and waits for a response, respecting
// context cancellation. If the context is cancelled before a response arrives,
// RequestWithContext returns the context error.
func (c *Conn) RequestWithContext(ctx context.Context, subject string, data []byte, timeout time.Duration) (*nats.Msg, error) {
	type result struct {
		msg *nats.Msg
		err error
	}
	ch := make(chan result, 1)
	go func() {
		msg, err := c.nc.Request(subject, data, timeout)
		ch <- result{msg, err}
	}()
	select {
	case r := <-ch:
		return r.msg, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// DataHandler is a simplified subscription callback that receives only
// the message data (not the full nats.Msg). This lets callers use bus
// subscriptions without importing the nats package.
type DataHandler func(data []byte)

// Sub wraps a NATS subscription so callers can unsubscribe without
// importing the nats package.
type Sub struct {
	natsSub *nats.Subscription
}

// Unsubscribe removes the subscription.
func (s *Sub) Unsubscribe() error {
	if s.natsSub == nil {
		return nil
	}
	return s.natsSub.Unsubscribe()
}

// SubscribeData subscribes to the given subject with a simplified callback
// that receives only the message payload.
func (c *Conn) SubscribeData(subject string, cb DataHandler) (*Sub, error) {
	natsSub, err := c.nc.Subscribe(subject, func(msg *nats.Msg) {
		cb(msg.Data)
	})
	if err != nil {
		return nil, err
	}
	return &Sub{natsSub: natsSub}, nil
}

// RequestHandler is a callback for NATS request-reply subscriptions.
// It receives the request payload and returns the response payload. If the
// returned slice is nil, the request is not responded to (the caller times out).
type RequestHandler func(requestData []byte) (responseData []byte)

// SubscribeRequest subscribes to the given subject with a request-reply handler.
// The handler receives request data and must return response data. If queue is
// non-empty, queue group delivery is used for load-balanced processing.
//
// This wraps nc.QueueSubscribe/nc.Subscribe + msg.Respond() so callers do not
// need to import the nats package directly.
func (c *Conn) SubscribeRequest(subject, queue string, handler RequestHandler) (*Sub, error) {
	cb := func(msg *nats.Msg) {
		resp := handler(msg.Data)
		if resp != nil {
			_ = msg.Respond(resp)
		}
	}
	var (
		natsSub *nats.Subscription
		err     error
	)
	if queue != "" {
		natsSub, err = c.nc.QueueSubscribe(subject, queue, cb)
	} else {
		natsSub, err = c.nc.Subscribe(subject, cb)
	}
	if err != nil {
		return nil, err
	}
	return &Sub{natsSub: natsSub}, nil
}

// Close drains and closes the NATS connection.
func (c *Conn) Close() {
	if c.logger != nil {
		c.logger.Info("connection closed")
	}
	c.nc.Close()
}
