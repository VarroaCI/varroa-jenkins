package bus

import (
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestConnectSetsLivenessOptions proves every Connect form applies the ping
// and write bounds. The nats.go defaults would keep a silent dead connection
// for minutes.
func TestConnectSetsLivenessOptions(t *testing.T) {
	s := startTestServer(t)
	forms := map[string][]Config{
		"no config":    nil,
		"empty config": {Config{}},
		"inbox prefix": {Config{InboxPrefix: "_INBOX_test"}},
	}
	for name, cfg := range forms {
		t.Run(name, func(t *testing.T) {
			conn, err := Connect(s.ClientURL(), cfg...)
			if err != nil {
				t.Fatalf("Connect: %v", err)
			}
			defer conn.Close()
			opts := conn.nc.Opts
			if opts.PingInterval != PingInterval {
				t.Errorf("PingInterval = %s, want %s", opts.PingInterval, PingInterval)
			}
			if opts.MaxPingsOut != MaxPingsOut {
				t.Errorf("MaxPingsOut = %d, want %d", opts.MaxPingsOut, MaxPingsOut)
			}
			if opts.FlusherTimeout != WriteTimeout {
				t.Errorf("FlusherTimeout = %s, want %s", opts.FlusherTimeout, WriteTimeout)
			}
		})
	}
}

// partitionProxy forwards TCP between a client and a NATS server. cutAll
// makes every existing connection silent: bytes are read and dropped in both
// directions, so neither side sees an error. This models a partition that
// drops packets without closing the socket.
type partitionProxy struct {
	ln     net.Listener
	target string

	mu    sync.Mutex
	pairs []*proxyPair
}

type proxyPair struct {
	client, server net.Conn
	cut            atomic.Bool
}

func startPartitionProxy(t *testing.T, target string) *partitionProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &partitionProxy{ln: ln, target: target}
	go p.serve()
	t.Cleanup(p.close)
	return p
}

func (p *partitionProxy) serve() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		pair := &proxyPair{client: client, server: server}
		p.mu.Lock()
		p.pairs = append(p.pairs, pair)
		p.mu.Unlock()
		go pipe(pair, server, client)
		go pipe(pair, client, server)
	}
}

// pipe copies src to dst. While the pair is cut, it reads and drops the bytes.
func pipe(pair *proxyPair, src, dst net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 && !pair.cut.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				err = werr
			}
		}
		if err != nil {
			_ = pair.client.Close()
			_ = pair.server.Close()
			return
		}
	}
}

// cutAll silences every connection accepted so far.
func (p *partitionProxy) cutAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pair := range p.pairs {
		pair.cut.Store(true)
	}
}

func (p *partitionProxy) close() {
	_ = p.ln.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pair := range p.pairs {
		_ = pair.client.Close()
		_ = pair.server.Close()
	}
}

func (p *partitionProxy) URL() string {
	return "nats://" + p.ln.Addr().String()
}

// TestConnectReconnectsAfterSilentPartition proves a Conn notices a silent
// dead path through missed pings and reconnects on its own.
func TestConnectReconnectsAfterSilentPartition(t *testing.T) {
	s := startTestServer(t)
	p := startPartitionProxy(t, strings.TrimPrefix(s.ClientURL(), "nats://"))

	var out syncBuffer
	conn, err := Connect(p.URL(), Config{
		Logger:         slog.New(slog.NewTextHandler(&out, nil)),
		pingInterval:   500 * time.Millisecond,
		StartupTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	// Control check: a live path answers the pings, so no stale connection.
	time.Sleep(2 * time.Second)
	if strings.Contains(out.String(), "stale connection") {
		t.Fatalf("stale connection on a live path:\n%s", out.String())
	}

	p.cutAll()
	waitFor(t, 10*time.Second, func() bool {
		return strings.Contains(out.String(), "stale connection")
	})
	waitFor(t, 10*time.Second, func() bool {
		return strings.Contains(out.String(), "nats reconnected")
	})
	if !conn.Connected() {
		t.Fatal("expected Connected() after reconnect")
	}
	if err := conn.NATSConn().FlushTimeout(2 * time.Second); err != nil {
		t.Fatalf("flush after reconnect: %v", err)
	}
}
