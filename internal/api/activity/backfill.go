package activity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

const (
	// defaultLimit is the default number of events returned by Recent.
	defaultLimit = 200
	// defaultMaxFetch is the hard bound on how many messages the JetStream
	// backfill will fetch per request, preventing unbounded reads.
	defaultMaxFetch = 500
)

// Backfill is the interface for reading recent activity events. The handler
// uses this to remain mode-agnostic (JetStream vs in-memory ring).
type Backfill interface {
	// Recent returns up to limit most-recent events, newest-first.
	// scope == "" means all controllers (global feed); "ns/name" scopes to a
	// local-cluster controller; "cluster/ns/name" scopes cross-cluster.
	Recent(ctx context.Context, scope string, limit int) ([]Event, error)
}

// Querier provides filtered, paginated activity history and retention metadata.
type Querier interface {
	Query(ctx context.Context, query Query) (Page, error)
	Retention() (string, int)
}

// Query describes filters and pagination for activity history.
type Query struct {
	Cursor                                                        string
	Limit                                                         int
	Start, End                                                    *time.Time
	Cluster, Controller, Namespace, Source, Severity, Actor, Type string
	Authorize                                                     func(Event) bool
}

// Page is one page of activity history results.
type Page struct {
	Items         []Event `json:"items"`
	NextCursor    string  `json:"nextCursor,omitempty"`
	HasMore       bool    `json:"hasMore"`
	RetentionMode string  `json:"retentionMode"`
	RetentionDays int     `json:"retentionDays,omitempty"`
}

var (
	// ErrInvalidCursor indicates a malformed cursor or one for different filters.
	ErrInvalidCursor = errors.New("invalid activity cursor")
	// ErrCursorExpired indicates a cursor outside the retained activity window.
	ErrCursorExpired = errors.New("activity cursor expired")
)

type cursor struct {
	Offset    int       `json:"offset"`
	Direction string    `json:"direction"`
	Filters   string    `json:"filters"`
	Timestamp time.Time `json:"timestamp"`
}

func filterKey(q Query) string {
	start, end := "", ""
	if q.Start != nil {
		start = q.Start.UTC().Format(time.RFC3339Nano)
	}
	if q.End != nil {
		end = q.End.UTC().Format(time.RFC3339Nano)
	}
	return strings.Join([]string{start, end, q.Cluster, q.Controller, q.Namespace, q.Source, q.Severity, q.Actor, q.Type}, "\x00")
}

func encodeCursor(c cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}
func decodeCursor(raw string, q Query) (cursor, error) {
	var c cursor
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(b, &c) != nil || c.Offset < 0 || c.Filters != filterKey(q) {
		return c, ErrInvalidCursor
	}
	return c, nil
}

// Matches reports whether an event satisfies an activity query's filters.
func Matches(e Event, q Query) bool {
	e.Normalize()
	if q.Start != nil && e.Timestamp.Before(*q.Start) {
		return false
	}
	if q.End != nil && !e.Timestamp.Before(*q.End) {
		return false
	}
	return (q.Cluster == "" || e.Cluster == q.Cluster) && (q.Controller == "" || e.Controller == q.Controller || e.Namespace+"/"+e.Controller == q.Controller) &&
		(q.Namespace == "" || e.Namespace == q.Namespace) && (q.Source == "" || e.Source == q.Source) && (q.Severity == "" || e.Severity == q.Severity) &&
		(q.Actor == "" || e.Actor == q.Actor) && (q.Type == "" || e.Type == q.Type)
}

// jetstreamBackfill reads recent activity events from the varroa_activity
// JetStream stream via an ephemeral consumer.
type jetstreamBackfill struct {
	cluster       string
	conn          *bus.Conn
	stream        string
	maxFetch      int
	retentionDays int
}

// NewJetStreamBackfill creates a JetStream-backed Backfill with the given
// maxFetch bound (recommended: defaultMaxFetch from this package).
func NewJetStreamBackfill(cluster string, conn *bus.Conn, stream string, maxFetch int, configuredDays ...int) Backfill {
	if maxFetch <= 0 {
		maxFetch = defaultMaxFetch
	}
	days := 7
	if len(configuredDays) > 0 && (configuredDays[0] == 7 || configuredDays[0] == 30 || configuredDays[0] == 90) {
		days = configuredDays[0]
	}
	return &jetstreamBackfill{
		cluster:       cluster,
		conn:          conn,
		stream:        stream,
		maxFetch:      maxFetch,
		retentionDays: days,
	}
}

// Recent reads the most-recent events from the stream using an end-anchored
// ephemeral consumer. It fetches up to maxFetch messages forward from
// OptStartSeq = max(1, lastSeq - maxFetch), then returns the last limit
// events in newest-first order.
func (jb *jetstreamBackfill) Recent(ctx context.Context, scope string, limit int) ([]Event, error) {
	filterSubject := bus.ActivityWildcard
	if scope != "" {
		parts := strings.SplitN(scope, "/", 3)
		switch len(parts) {
		case 3: // "cluster/ns/name" — cross-cluster scope from the handler
			filterSubject = bus.ActivitySubject(parts[0], parts[1], parts[2])
		case 2: // "ns/name" — local-cluster default
			filterSubject = bus.ActivitySubject(jb.cluster, parts[0], parts[1])
		}
	}

	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > jb.maxFetch {
		limit = jb.maxFetch
	}

	// Get stream info to compute the end-anchored start sequence.
	si, err := jb.conn.JetStream().StreamInfo(jb.stream)
	if err != nil {
		return nil, fmt.Errorf("stream info %s: %w", jb.stream, err)
	}

	lastSeq := int64(si.State.LastSeq)
	startSeq := lastSeq - int64(jb.maxFetch)
	if startSeq < 1 {
		startSeq = 1
	}

	// Create an ephemeral consumer (no Durable name — auto-deletes on inactivity).
	consumerName := fmt.Sprintf("backfill-%d", time.Now().UnixNano())
	_, err = jb.conn.JetStream().AddConsumer(jb.stream, &nats.ConsumerConfig{
		Name:              consumerName,
		FilterSubject:     filterSubject,
		DeliverPolicy:     nats.DeliverByStartSequencePolicy,
		OptStartSeq:       uint64(startSeq),
		AckPolicy:         nats.AckNonePolicy, // no ack needed for backfill
		InactiveThreshold: 5 * time.Second,    // auto-cleanup
	})
	if err != nil {
		return nil, fmt.Errorf("create ephemeral consumer %s: %w", consumerName, err)
	}
	defer func() {
		_ = jb.conn.JetStream().DeleteConsumer(jb.stream, consumerName)
	}()

	// Bind to the pre-created ephemeral consumer. The durable arg MUST be ""
	// when using nats.Bind — passing the consumer name there makes nats.go
	// demand a durable of that name, which mismatches the ephemeral consumer's
	// empty Durable field ("configuration requests durable to be ..., but
	// consumer's value is ''").
	sub, err := jb.conn.JetStream().PullSubscribe(filterSubject, "", nats.Bind(jb.stream, consumerName))
	if err != nil {
		return nil, fmt.Errorf("pull subscribe %s/%s: %w", jb.stream, consumerName, err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	// Fetch in batches up to maxFetch.
	var all []*nats.Msg
	fetchLimit := jb.maxFetch
	for fetchLimit > 0 {
		batch := fetchLimit
		if batch > 100 {
			batch = 100
		}
		msgs, err := sub.Fetch(batch, nats.MaxWait(2*time.Second))
		if err != nil {
			// Timeout or no more messages — done fetching.
			break
		}
		all = append(all, msgs...)
		fetchLimit -= len(msgs)
		if len(msgs) < batch {
			// Fewer than requested means we've exhausted the available messages.
			break
		}
	}

	// Unmarshal events, newest-first.
	events := make([]Event, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		var e Event
		if err := json.Unmarshal(all[i].Data, &e); err != nil {
			continue
		}
		events = append(events, e)
	}

	// Cap to limit.
	if len(events) > limit {
		events = events[:limit]
	}

	return events, nil
}

func (jb *jetstreamBackfill) Retention() (string, int) { return "on", jb.retentionDays }

func (jb *jetstreamBackfill) Query(ctx context.Context, q Query) (Page, error) {
	// Recent is end-anchored and bounded by maxFetch. Cursor offsets bind paging to filters.
	all, err := jb.Recent(ctx, "", jb.maxFetch)
	if err != nil {
		return Page{}, err
	}
	return querySlice(all, q, "on", jb.retentionDays)
}

// ringBackfill reads recent activity events from the in-memory ring buffer
// (off-mode fallback).
type ringBackfill struct {
	store *Store
}

// NewRingBackfill creates an in-memory ring-backed Backfill.
func NewRingBackfill(store *Store) Backfill {
	return &ringBackfill{store: store}
}

// Recent reads events from the local ring buffer. For per-controller requests,
// scope is "ns/name" — we split on "/" and pass the name part (controller token)
// to store.List, which filters on e.Controller.
func (rb *ringBackfill) Recent(_ context.Context, scope string, limit int) ([]Event, error) {
	if limit <= 0 || limit > defaultLimit {
		limit = defaultLimit
	}
	controller := ""
	if scope != "" {
		// "ns/name" or "cluster/ns/name" — the controller token is last.
		parts := strings.Split(scope, "/")
		if len(parts) >= 2 {
			controller = parts[len(parts)-1]
		}
	}
	all := rb.store.List(controller)
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (rb *ringBackfill) Retention() (string, int) { return "off", 0 }
func (rb *ringBackfill) Query(_ context.Context, q Query) (Page, error) {
	return querySlice(rb.store.List(""), q, "off", 0)
}

func querySlice(events []Event, q Query, mode string, days int) (Page, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 250 {
		limit = 250
	}
	offset := 0
	direction := "backward"
	if q.Start != nil {
		direction = "forward"
	}
	if q.Cursor != "" {
		c, err := decodeCursor(q.Cursor, q)
		if err != nil {
			return Page{}, err
		}
		if c.Direction != direction {
			return Page{}, ErrInvalidCursor
		}
		if c.Offset > len(events) {
			return Page{}, ErrCursorExpired
		}
		offset = c.Offset
	}
	filtered := make([]Event, 0, limit+1)
	scanned := 0
	for i := offset; i < len(events) && scanned < 5000 && len(filtered) <= limit; i++ {
		scanned++
		e := events[i]
		e.Normalize()
		if q.Authorize != nil && !q.Authorize(e) {
			continue
		}
		if Matches(e, q) {
			filtered = append(filtered, e)
		}
	}
	hasMore := offset+scanned < len(events) || len(filtered) > limit
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	p := Page{Items: filtered, HasMore: hasMore, RetentionMode: mode, RetentionDays: days}
	if hasMore {
		p.NextCursor = encodeCursor(cursor{Offset: offset + scanned, Direction: direction, Filters: filterKey(q), Timestamp: time.Now().UTC()})
	}
	return p, nil
}
