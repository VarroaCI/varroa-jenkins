package bus

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
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
	// Empty means no credential is sent.
	Username string

	// Password for NATS user/password authentication.
	Password string

	// InboxPrefix overrides the default _INBOX.> reply subject.
	// Use per-service prefixes like _INBOX_operator for ACL isolation.
	InboxPrefix string
}

// IsZero returns true when no options are set (empty config).
func (c Config) IsZero() bool {
	return c.CAFile == "" && len(c.CABytes) == 0 && c.Username == "" && c.Password == "" && c.InboxPrefix == ""
}

// Conn wraps a NATS connection and its JetStream context.
type Conn struct {
	nc     *nats.Conn
	js     nats.JetStreamContext
	Logger *slog.Logger

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
	opts := []nats.Option{
		nats.Name("varroa"),
		nats.ReconnectWait(2 * time.Second),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if c.Logger != nil {
				c.Logger.Warn("nats disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			if c.Logger != nil {
				c.Logger.Info("nats reconnected")
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

		// Credentials.
		if conf.Username != "" {
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

	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("nats jetstream: %w", err)
	}
	c.js = js

	return c, nil
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
	if c.Logger != nil {
		c.Logger.Info("connection closed")
	}
	c.nc.Close()
}
