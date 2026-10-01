package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// startPermissionTestServer starts an embedded server with one user that
// may publish only on "allowed.>".
func startPermissionTestServer(t *testing.T) *server.Server {
	t.Helper()
	s, err := server.NewServer(&server.Options{
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		Users: []*server.User{{
			Username: "limited",
			Password: "limited_pass",
			Permissions: &server.Permissions{
				Publish:   &server.SubjectPermission{Allow: []string{"allowed.>"}},
				Subscribe: &server.SubjectPermission{Allow: []string{"_INBOX.>"}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(s.Shutdown)
	return s
}

// permissionViolations reads the varroa.bus.permission_violations sum for
// component from reader.
func permissionViolations(t *testing.T, reader *sdkmetric.ManualReader, component string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "varroa.bus.permission_violations" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("permission violations is %T, want an int64 sum", m.Data)
			}
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value("component"); ok && v.AsString() == component {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// TestPermissionViolationCountedAndLogged pins that a denied publish logs
// at Error and counts once. A core publish returns no error, so this is
// the only signal an operator gets.
func TestPermissionViolationCountedAndLogged(t *testing.T) {
	s := startPermissionTestServer(t)
	var out syncBuffer
	logger := slog.New(slog.NewTextHandler(&out, nil))

	conn, err := Connect(s.ClientURL(), Config{Username: "limited", Password: "limited_pass", Logger: logger})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	if err := conn.RegisterMetrics(provider.Meter("test"), "recorder"); err != nil {
		t.Fatalf("RegisterMetrics: %v", err)
	}

	if err := conn.Publish("allowed.one", []byte("x")); err != nil {
		t.Fatalf("allowed publish: %v", err)
	}
	if err := conn.NATSConn().Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := permissionViolations(t, reader, "recorder"); got != 0 {
		t.Fatalf("violations after an allowed publish = %d, want 0", got)
	}

	if err := conn.Publish("denied.one", []byte("x")); err != nil {
		t.Fatalf("a denied core publish must not return an error: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return permissionViolations(t, reader, "recorder") == 1
	})
	// A second collection must not count the same violation again.
	if got := permissionViolations(t, reader, "recorder"); got != 1 {
		t.Fatalf("violations = %d, want 1", got)
	}

	logged := out.String()
	if !strings.Contains(logged, "level=ERROR") || !strings.Contains(logged, "nats permission violation") {
		t.Fatalf("want an Error-level permission violation log, got %q", logged)
	}
	if !strings.Contains(logged, "denied.one") {
		t.Fatalf("want the denied subject in the log, got %q", logged)
	}
}

// TestConfirmSubscription_ReportsDenial pins that ConfirmSubscription turns
// the server's asynchronous rejection of a subscription into an error.
// nats.go returns no error from Subscribe for that case.
func TestConfirmSubscription_ReportsDenial(t *testing.T) {
	s := startPermissionTestServer(t)
	conn, err := Connect(s.ClientURL(), Config{Username: "limited", Password: "limited_pass"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	denied, release := conn.WatchSubscribeDenied("denied.sub")
	defer release()
	sub, err := conn.SubscribeData("denied.sub", func([]byte) {})
	if err != nil {
		t.Fatalf("a denied subscribe must not return an error at once: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := conn.ConfirmSubscription(denied); !errors.Is(err, ErrSubscribeDenied) {
		t.Fatalf("ConfirmSubscription = %v, want ErrSubscribeDenied", err)
	}

	allowed, releaseAllowed := conn.WatchSubscribeDenied("_INBOX.ok")
	defer releaseAllowed()
	okSub, err := conn.SubscribeData("_INBOX.ok", func([]byte) {})
	if err != nil {
		t.Fatalf("allowed subscribe: %v", err)
	}
	defer func() { _ = okSub.Unsubscribe() }()
	if err := conn.ConfirmSubscription(allowed); err != nil {
		t.Fatalf("ConfirmSubscription for an allowed subject = %v, want nil", err)
	}
}

// TestWatchSubscribeDenied_SignalsOnlyTheDeniedSubject pins that a denial
// reaches the watchers of its own subject and no other watcher, and that a
// released watcher gets nothing.
func TestWatchSubscribeDenied_SignalsOnlyTheDeniedSubject(t *testing.T) {
	var w deniedWatchers
	a, releaseA := w.add("mite.core.ns.a.desired")
	b, releaseB := w.add("mite.core.ns.b.desired")
	defer releaseB()
	gone, releaseGone := w.add("mite.core.ns.a.desired")
	releaseGone()

	w.notify(errors.New(`nats: permissions violation: Permissions Violation for Subscription to "mite.core.ns.a.desired"`))
	w.notify(errors.New(`nats: permissions violation: Permissions Violation for Subscription to "mite.core.ns.a.desired"`))

	select {
	case <-a:
	default:
		t.Fatal("the watcher of the denied subject got no signal")
	}
	select {
	case <-a:
		t.Fatal("a second denial queued a second signal; one pending signal is enough")
	default:
	}
	select {
	case <-b:
		t.Fatal("the watcher of another subject got a signal")
	default:
	}
	select {
	case <-gone:
		t.Fatal("a released watcher got a signal")
	default:
	}
	releaseA()

	if isSubscribeDenial(fmt.Errorf("%w: Permissions Violation for Publish to \"x\"", nats.ErrPermissionViolation)) {
		t.Fatal("a publish violation counted as a subscription denial")
	}
	if !isSubscribeDenial(fmt.Errorf("%w: Permissions Violation for Subscription to \"x\"", nats.ErrPermissionViolation)) {
		t.Fatal("a subscription violation did not count as a subscription denial")
	}
}
