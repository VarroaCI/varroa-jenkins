package recordstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestPublishAfterCommit_ReingestWritesNoSecondRow writes one event through
// the outbox helper, then re-ingests the same event as a consumer would. The
// second write must find the row and write nothing.
func TestPublishAfterCommit_ReingestWritesNoSecondRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	e := activity.Event{
		Cluster: "core", Timestamp: time.Now().UTC(), Source: "recorder",
		Type: "tenant.placement_moved", Namespace: "jenkins", Controller: "cell-x",
		Message: "publish-after-commit re-ingest",
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var out activityOutbox
	if err := insertActivityForPublish(ctx, tx, e, &out); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(out.events) != 1 {
		t.Fatalf("outbox holds %d events, want 1", len(out.events))
	}
	published := out.events[0]

	inserted, err := NewActivityIngester(pool).Insert(ctx, published, activity.EventKey(published))
	if err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	if inserted {
		t.Fatal("the re-ingested copy reported a new row")
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.activity WHERE message = $1`, e.Message).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("activity rows = %d, want 1", n)
	}
}

// TestPublishAfterCommit_RollbackPublishesNothing rolls the transaction back
// and checks that no row lands and the outbox stays empty of nothing written.
func TestPublishAfterCommit_RollbackPublishesNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	e := activity.Event{
		Cluster: "core", Timestamp: time.Now().UTC(), Source: "recorder",
		Type: "tenant.placement_moved", Namespace: "jenkins", Controller: "cell-y",
		Message: "rolled back publish",
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var out activityOutbox
	if err := insertActivityForPublish(ctx, tx, e, &out); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	publishActivityAfterCommit(nil, &out)
	if len(out.events) != 0 {
		t.Fatalf("outbox holds %d events after the publish step, want 0", len(out.events))
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.activity WHERE message = $1`, e.Message).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("activity rows after rollback = %d, want 0", n)
	}
}

// activityLogBuffer is a log sink that a publisher goroutine and the test can
// share.
type activityLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *activityLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *activityLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestPublishAfterCommit_StoresTheEventOnceTheStreamAnswers publishes one
// event before the activity stream exists. The ack-mode publisher must keep
// retrying until the stream answers, then store the event once.
func TestPublishAfterCommit_StoresTheEventOnceTheStreamAnswers(t *testing.T) {
	t.Parallel()

	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	e := activity.Event{
		Cluster: "core", Namespace: "jenkins", Controller: "cell-x", Source: "recorder",
		Type: "build.failed", Severity: "error", Message: "publish before the stream",
		Timestamp: time.Now().UTC().Truncate(time.Microsecond),
	}
	out := activityOutbox{events: []activity.Event{e}}

	buf := &activityLogBuffer{}
	pub := activity.NewAckedPublisher("core", conn, activity.AckedOptions{
		Logger: slog.New(slog.NewTextHandler(buf, nil)),
	})
	publishActivityAfterCommit(pub, &out)
	if out.events != nil {
		t.Fatal("publishActivityAfterCommit did not empty the outbox")
	}

	waitForRow(t, func() bool {
		return strings.Contains(buf.String(), "did not acknowledge an event; retrying")
	})

	if err := EnsureActivityStream(conn, 1); err != nil {
		t.Fatalf("ensure activity stream: %v", err)
	}
	pub.Close(15 * time.Second)

	info, err := conn.StreamInfo(bus.ActivityStreamName)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.State.Msgs != 1 {
		t.Fatalf("stream holds %d messages, want 1", info.State.Msgs)
	}

	if err := conn.EnsureConsumer(bus.ActivityStreamName, ActivityRecorderConsumerName, activityRecorderConsumerConfig()); err != nil {
		t.Fatalf("ensure consumer: %v", err)
	}
	subject := bus.ActivitySubject("core", "jenkins", "cell-x")
	sub, err := conn.PullSubscribe(subject, bus.ActivityStreamName, ActivityRecorderConsumerName)
	if err != nil {
		t.Fatalf("pull subscribe: %v", err)
	}
	msgs, err := conn.DrainConsumer(sub, 1)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("drained %d messages, want 1", len(msgs))
	}
	if msgs[0].Subject != subject {
		t.Fatalf("subject = %q, want %q", msgs[0].Subject, subject)
	}
	want, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal(msgs[0].Data, want) {
		t.Fatalf("stored bytes differ from the row bytes:\n got %s\nwant %s", msgs[0].Data, want)
	}
}

// TestPublishAfterCommit_AckModeCopyWritesNoSecondRow writes one row, publishes
// its event through an ack-mode publisher, and runs the activity consumer. The
// consumer must find the row already present and write no second row.
func TestPublishAfterCommit_AckModeCopyWritesNoSecondRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	mp, reader := newTestMeterProvider(t)
	ctx := context.Background()
	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	if err := EnsureActivityStream(conn, 1); err != nil {
		t.Fatalf("ensure activity stream: %v", err)
	}

	e := activity.Event{
		Cluster: "core", Namespace: "jenkins", Controller: "cell-z", Source: "recorder",
		Type: "build.failed", Severity: "error", Message: "ack-mode copy writes no second row",
		Timestamp: time.Now().UTC(),
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var out activityOutbox
	if err := insertActivityForPublish(ctx, tx, e, &out); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	pub := activity.NewAckedPublisher("core", conn, activity.AckedOptions{})
	publishActivityAfterCommit(pub, &out)
	pub.Close(15 * time.Second)

	metrics, err := NewActivityMetrics(mp, conn, bus.ActivityStreamName, ActivityRecorderConsumerName)
	if err != nil {
		t.Fatalf("new activity metrics: %v", err)
	}
	consumerCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = RunActivityIngestConsumer(consumerCtx, conn, NewActivityIngester(pool), metrics, 30*24*time.Hour, nil)
	}()

	waitForRow(t, func() bool {
		return metricValue(t, reader, "recorder_activity_duplicate_total") >= 1
	})
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.activity WHERE message = $1`, e.Message).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("activity rows = %d, want 1", n)
	}
}

// newActivityStreamPublisher starts a bus, creates the activity stream, and
// returns an ack-mode publisher over it, with the connection it uses.
func newActivityStreamPublisher(t *testing.T) (*activity.Publisher, *bus.Conn) {
	t.Helper()
	s := startIngestTestServer(t)
	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	if err := EnsureActivityStream(conn, 1); err != nil {
		t.Fatalf("ensure activity stream: %v", err)
	}
	return activity.NewAckedPublisher("core", conn, activity.AckedOptions{}), conn
}

// storedActivityEvents closes pub, then reads every event the activity
// stream holds. It returns nil when the stream is empty.
func storedActivityEvents(t *testing.T, pub *activity.Publisher, conn *bus.Conn) []activity.Event {
	t.Helper()
	pub.Close(15 * time.Second)
	info, err := conn.StreamInfo(bus.ActivityStreamName)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.State.Msgs == 0 {
		return nil
	}
	if err := conn.EnsureConsumer(bus.ActivityStreamName, ActivityRecorderConsumerName, activityRecorderConsumerConfig()); err != nil {
		t.Fatalf("ensure consumer: %v", err)
	}
	sub, err := conn.PullSubscribe("activity.>", bus.ActivityStreamName, ActivityRecorderConsumerName)
	if err != nil {
		t.Fatalf("pull subscribe: %v", err)
	}
	msgs, err := conn.DrainConsumer(sub, int(info.State.Msgs))
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	var out []activity.Event
	for _, m := range msgs {
		var e activity.Event
		if err := json.Unmarshal(m.Data, &e); err != nil {
			t.Fatalf("decode stored event: %v", err)
		}
		out = append(out, e)
	}
	return out
}

// assertStoredRowEvent checks that events holds one event of typ, and that
// its row is already in the table: a re-ingest of that event writes nothing.
func assertStoredRowEvent(t *testing.T, pool *pgxpool.Pool, db *sql.DB, events []activity.Event, typ string) {
	t.Helper()
	if len(events) != 1 || events[0].Type != typ {
		t.Fatalf("stored events = %+v, want one %s", events, typ)
	}
	e := events[0]
	e.Normalize()
	key := activity.EventKey(e)
	var rowKey string
	if err := db.QueryRow(`SELECT event_key FROM varroa.activity WHERE type = $1`, typ).Scan(&rowKey); err != nil {
		t.Fatalf("read %s row: %v", typ, err)
	}
	if rowKey != key {
		t.Fatalf("row key = %q, published event key = %q", rowKey, key)
	}
	inserted, err := NewActivityIngester(pool).Insert(context.Background(), e, key)
	if err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	if inserted {
		t.Fatal("the published event has no stored row")
	}
}

// TestRecorderActivityWritersPublish scans the recorder source. Only the
// shared helper and the activity-recorder consumer may write an activity
// row, so every other writer must publish after its commit.
func TestRecorderActivityWritersPublish(t *testing.T) {
	t.Parallel()

	insertRe := regexp.MustCompile(`(?i)insert\s+into\s+varroa\.activity\b`)
	ingesterRe := regexp.MustCompile(`ActivityIngester`)
	// The recorder command may wire the ingester into its consumer, so it is
	// checked for a direct row write only.
	for _, root := range []string{".", "../../cmd/recorder"} {
		files, err := filepath.Glob(filepath.Join(root, "*.go"))
		if err != nil {
			t.Fatalf("glob: %v", err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			body, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			if insertRe.Match(body) && filepath.Base(f) != "activity_publish.go" && filepath.Base(f) != "activity_ingest.go" {
				t.Errorf("%s writes an activity row outside insertActivityForPublish", f)
			}
			if root == "." && ingesterRe.Match(body) && filepath.Base(f) != "activity_ingest.go" && filepath.Base(f) != "activity_consumer.go" {
				t.Errorf("%s uses ActivityIngester, which publishes nothing", f)
			}
		}
	}
}
