package recordstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// Dispatcher tuning. The lease keeps a row out of other replicas' hands
// while one replica sends it. A crashed replica's lease ends, and the
// row is due again.
const (
	webhookTick          = 5 * time.Second
	webhookBatchSize     = 20
	webhookConcurrency   = 4
	webhookLease         = 2 * time.Minute
	webhookTimeout       = 10 * time.Second
	webhookMaxAttempts   = 6
	webhookResponseLimit = 4096
	webhookErrorLimit    = 4096
	webhookActivityType  = "tenant.webhook_failed"
)

// webhookBackoff holds the wait after each failed attempt. The sixth
// failure ends the delivery as failed, so five waits are enough.
var webhookBackoff = []time.Duration{
	10 * time.Second,
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
	30 * time.Minute,
}

// webhookBackoffAfter returns the wait after the failures-th failed
// attempt, counted from one.
func webhookBackoffAfter(failures int) time.Duration {
	i := failures - 1
	if i < 0 {
		i = 0
	}
	if i >= len(webhookBackoff) {
		i = len(webhookBackoff) - 1
	}
	return webhookBackoff[i]
}

// refuseWebhookIP reports whether the webhook dialer must not reach ip.
// The dialer refuses any address that is not global unicast.
func refuseWebhookIP(ip net.IP) bool {
	return !ip.IsGlobalUnicast()
}

// newWebhookClient returns the HTTP client that sends webhook deliveries.
// The dialer refuses the addresses refuseWebhookIP names, checked on the
// address the connection really uses, so a name that resolves to one of
// them is refused too. Redirects are not followed.
func newWebhookClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: webhookTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("webhook dial to a non-IP address refused")
			}
			if refuseWebhookIP(ip) {
				return fmt.Errorf("webhook dial to %s refused", ip)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout:       webhookTimeout,
		Transport:     &http.Transport{DialContext: dialer.DialContext},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// WebhookDispatcher sends the outbox rows of the completion webhook. Each
// round leases a batch with FOR UPDATE SKIP LOCKED, so several recorder
// replicas never send one delivery twice.
type WebhookDispatcher struct {
	pool     *pgxpool.Pool
	client   *http.Client
	activity *ActivityIngester
	metrics  *WebhookMetrics
	logger   *slog.Logger
	now      func() time.Time
	nudges   chan struct{}
}

// NewWebhookDispatcher returns a dispatcher over pool. It writes a failed
// delivery to activity and counts outcomes on metrics. A nil metrics
// value counts nothing.
func NewWebhookDispatcher(pool *pgxpool.Pool, activityIngester *ActivityIngester, metrics *WebhookMetrics, logger *slog.Logger) *WebhookDispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &WebhookDispatcher{
		pool:     pool,
		client:   newWebhookClient(),
		activity: activityIngester,
		metrics:  metrics,
		logger:   logger,
		now:      time.Now,
		nudges:   make(chan struct{}, 1),
	}
}

// Nudge asks the dispatcher to run a round now. It never blocks. A nudge
// that finds one already queued is dropped, because that queued round
// will see the same rows.
func (d *WebhookDispatcher) Nudge() {
	select {
	case d.nudges <- struct{}{}:
	default:
	}
}

// Run runs a round every few seconds, and at once after each Nudge, until
// ctx ends.
func (d *WebhookDispatcher) Run(ctx context.Context) error {
	ticker := time.NewTicker(webhookTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-d.nudges:
		}
		d.drain(ctx)
	}
}

// drain runs rounds until one leases fewer rows than a full batch.
func (d *WebhookDispatcher) drain(ctx context.Context) {
	for ctx.Err() == nil {
		if d.round(ctx) < webhookBatchSize {
			return
		}
	}
}

// webhookRow is one leased outbox row.
type webhookRow struct {
	deliveryID  uuid.UUID
	runID       uuid.UUID
	cluster     string
	tenant      string
	incarnation string
	body        []byte
	attempts    int
	key         string
}

// webhookEventFor returns the X-Varroa-Event value of a delivery key. A Team
// agent message is not a run completion, so it carries its own event type.
func webhookEventFor(key string) string {
	if strings.HasPrefix(key, teamAgentWebhookKeyPrefix) {
		return teamAgentWebhookEvent
	}
	return webhookEventRunCompleted
}

// round leases one batch, sends it, and reports how many rows it leased.
func (d *WebhookDispatcher) round(ctx context.Context) int {
	rows, err := d.lease(ctx)
	if err != nil {
		d.logger.Warn("webhook lease failed", "error", err)
		return 0
	}
	sem := make(chan struct{}, webhookConcurrency)
	var wg sync.WaitGroup
	for _, row := range rows {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			d.deliver(ctx, row)
		}()
	}
	wg.Wait()
	return len(rows)
}

// lease pushes the next attempt of up to one batch of due rows out by
// the lease length, and returns those rows.
func (d *WebhookDispatcher) lease(ctx context.Context) ([]webhookRow, error) {
	rows, err := d.pool.Query(ctx, `
		UPDATE varroa.webhook_deliveries
		SET next_attempt_at = now() + make_interval(secs => $2), updated_at = now()
		WHERE delivery_id IN (
			SELECT delivery_id FROM varroa.webhook_deliveries
			WHERE state = 'pending' AND next_attempt_at <= now()
			ORDER BY next_attempt_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING delivery_id, run_id, cluster, tenant, incarnation, body, attempts, delivery_key
	`, webhookBatchSize, webhookLease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("lease webhook deliveries: %w", err)
	}
	defer rows.Close()
	var out []webhookRow
	for rows.Next() {
		var r webhookRow
		if err := rows.Scan(&r.deliveryID, &r.runID, &r.cluster, &r.tenant, &r.incarnation, &r.body, &r.attempts, &r.key); err != nil {
			return nil, fmt.Errorf("scan leased webhook delivery: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read leased webhook deliveries: %w", err)
	}
	return out, nil
}

// webhookTarget is the receiver of a tenant as stored now.
type webhookTarget struct {
	URL string `json:"url"`
	Key string `json:"key"`
}

// currentWebhook reads the tenant's stored webhook and its current
// incarnation. It answers false when the tenant has none, which drops the
// delivery.
func (d *WebhookDispatcher) currentWebhook(ctx context.Context, cluster, tenant string) (webhookTarget, string, bool, error) {
	var (
		raw         []byte
		incarnation string
	)
	err := d.pool.QueryRow(ctx, `
		SELECT incarnation, notification_webhook FROM varroa.tenant_placements
		WHERE cluster = $1 AND tenant = $2 AND notification_webhook IS NOT NULL
	`, cluster, tenant).Scan(&incarnation, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return webhookTarget{}, "", false, nil
	}
	if err != nil {
		return webhookTarget{}, "", false, fmt.Errorf("read tenant webhook: %w", err)
	}
	var target webhookTarget
	if err := json.Unmarshal(raw, &target); err != nil {
		return webhookTarget{}, "", false, fmt.Errorf("decode tenant webhook: %w", err)
	}
	return target, incarnation, true, nil
}

// deliver sends one leased row and records the outcome.
func (d *WebhookDispatcher) deliver(ctx context.Context, row webhookRow) {
	target, incarnation, ok, err := d.currentWebhook(ctx, row.cluster, row.tenant)
	if err != nil {
		d.logger.Warn("webhook target read failed", "delivery", row.deliveryID, "error", err)
		return
	}
	// A replaced Team object has a new incarnation. Its receiver must
	// not get a delivery that the old object's run produced.
	if !ok || incarnation != row.incarnation {
		d.setState(ctx, row, "dropped", row.attempts, 0, "tenant webhook removed or the tenant was replaced")
		d.metrics.add(ctx, webhookOutcomeDropped)
		return
	}

	ts := d.now().Unix()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(row.body))
	if err != nil {
		d.fail(ctx, row, 0, "build request: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Varroa-Signature", SignWebhook([]byte(target.Key), ts, row.body))
	req.Header.Set("X-Varroa-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Varroa-Delivery", row.deliveryID.String())
	req.Header.Set("X-Varroa-Event", webhookEventFor(row.key))

	resp, err := d.client.Do(req)
	if err != nil {
		d.fail(ctx, row, 0, err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, webhookResponseLimit))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		d.succeed(ctx, row, resp.StatusCode)
		return
	}
	d.fail(ctx, row, resp.StatusCode, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, snippet))
}

// succeed marks a row delivered.
func (d *WebhookDispatcher) succeed(ctx context.Context, row webhookRow, status int) {
	if _, err := d.pool.Exec(ctx, `
		UPDATE varroa.webhook_deliveries
		SET state = 'delivered', attempts = attempts + 1, last_status = $2,
		    last_error = NULL, updated_at = now()
		WHERE delivery_id = $1
	`, row.deliveryID, status); err != nil {
		d.logger.Warn("webhook delivered mark failed", "delivery", row.deliveryID, "error", err)
	}
	d.metrics.add(ctx, webhookOutcomeDelivered)
}

// fail records one failed attempt. It schedules the next attempt, or, on
// the last allowed attempt, marks the row failed and writes an activity
// event.
func (d *WebhookDispatcher) fail(ctx context.Context, row webhookRow, status int, errText string) {
	errText = capWebhookError(errText)
	attempts := row.attempts + 1
	if attempts >= webhookMaxAttempts {
		d.setState(ctx, row, "failed", attempts, status, errText)
		d.metrics.add(ctx, webhookOutcomeFailed)
		d.recordFailure(ctx, row, attempts, errText)
		return
	}
	wait := webhookBackoffAfter(attempts)
	if _, err := d.pool.Exec(ctx, `
		UPDATE varroa.webhook_deliveries
		SET attempts = $2, last_status = $3, last_error = $4,
		    next_attempt_at = now() + make_interval(secs => $5), updated_at = now()
		WHERE delivery_id = $1
	`, row.deliveryID, attempts, nullableStatus(status), errText, wait.Seconds()); err != nil {
		d.logger.Warn("webhook retry mark failed", "delivery", row.deliveryID, "error", err)
	}
	d.metrics.add(ctx, webhookOutcomeRetry)
}

// setState moves a row to a terminal state and stores its attempt count.
func (d *WebhookDispatcher) setState(ctx context.Context, row webhookRow, state string, attempts, status int, errText string) {
	if _, err := d.pool.Exec(ctx, `
		UPDATE varroa.webhook_deliveries
		SET state = $2, attempts = $3, last_status = $4, last_error = $5,
		    updated_at = now()
		WHERE delivery_id = $1
	`, row.deliveryID, state, attempts, nullableStatus(status), errText); err != nil {
		d.logger.Warn("webhook state mark failed", "delivery", row.deliveryID, "state", state, "error", err)
	}
}

// recordFailure writes the tenant.webhook_failed activity event for a
// delivery that ran out of attempts. The event names the run's cell and
// job. A missing partition is created once, and the insert is retried.
func (d *WebhookDispatcher) recordFailure(ctx context.Context, row webhookRow, attempts int, errText string) {
	if d.activity == nil {
		return
	}
	var (
		namespace, cell, jobPath, result string
		buildNumber                      *int64
	)
	err := d.pool.QueryRow(ctx, `
		SELECT namespace, cell, job_path, COALESCE(result, ''), build_number
		FROM varroa.runs WHERE run_id = $1
		ORDER BY created_at DESC LIMIT 1
	`, row.runID).Scan(&namespace, &cell, &jobPath, &result, &buildNumber)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		d.logger.Warn("webhook failure run read failed", "delivery", row.deliveryID, "error", err)
	}

	e := activity.Event{
		Timestamp:  d.now().UTC(),
		Type:       webhookActivityType,
		Source:     "recorder",
		Severity:   "error",
		Cluster:    row.cluster,
		Namespace:  namespace,
		Controller: cell,
		ItemPath:   jobPath,
		Result:     result,
		Message: fmt.Sprintf("completion webhook for tenant %s failed after %d attempts (delivery %s): %s",
			row.tenant, attempts, row.deliveryID, errText),
	}
	if buildNumber != nil {
		e.BuildNumber = *buildNumber
	}
	e.Normalize()
	key := activity.EventKey(e)
	_, insertErr := d.activity.Insert(ctx, e, key)
	if isNoActivityPartition(insertErr) {
		if perr := ensureActivityPartitionForTime(ctx, d.pool, e.Timestamp); perr != nil {
			d.logger.Warn("create activity partition failed", "error", perr)
			return
		}
		_, insertErr = d.activity.Insert(ctx, e, key)
	}
	if insertErr != nil {
		d.logger.Warn("webhook failure activity insert failed", "delivery", row.deliveryID, "error", insertErr)
	}
}

// nullableStatus returns nil for a zero HTTP status, so the column stores
// SQL NULL when no response arrived.
func nullableStatus(status int) any {
	if status == 0 {
		return nil
	}
	return status
}

// capWebhookError trims a delivery error to the stored limit and removes
// bytes that are not valid UTF-8.
func capWebhookError(s string) string {
	if len(s) > webhookErrorLimit {
		s = s[:webhookErrorLimit]
	}
	return strings.ToValidUTF8(s, "")
}
