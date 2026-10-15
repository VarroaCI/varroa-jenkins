package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// grantHoldAge is release test (b) of the grant hold: a grant at least
// this old releases the hold. A live instance that started the build sends
// a heartbeat every pluginHeartbeatInterval, and a heartbeat records the
// build. The value is the interrupted-run spec's moved window, so the
// recorder holds one such window and not two.
const grantHoldAge = movedHeartbeatWindow

// errConsumerReadNeeded tells Claim that a release test released a held
// claim and the decision now needs the ingest consumer's state. Claim
// rolls the transaction back, reads the consumer with no lock held, and
// runs the claim again with that reading.
var errConsumerReadNeeded = errors.New("the grant hold needs a reading of the ingest consumer")

// consumerReading is one reading of the recorder's own varroa_builds
// consumer. behind is true when the consumer can still hold a start the
// stored owner sent, and message then names the reason.
type consumerReading struct {
	behind  bool
	message string
}

// grantHold holds a repeat claim for an unstarted run from an instance
// other than the stored owner. The stored owner holds a grant, and its
// build can have started before its run.started reached the recorder.
// The hold answers NOT_READY, which the plugin retries, until one of two
// release tests holds and the ingest consumer is fresh:
//
//   - (a) tenant_placements.dead_instances confirms the stored owner gone,
//     and the confirmation is older than grace.
//   - (b) the grant is at least grantHoldAge old.
//
// The consumer test makes sure the stored owner's own events reached the
// runs table before the ledger trusts their absence.
type grantHold struct {
	grace      time.Duration
	maxPending uint64
	consumer   func(ctx context.Context) (*nats.ConsumerInfo, error)
	// now is the recorder clock. Release test (a) and the consumer test
	// compare against it, as the re-fire and the interrupted sweep do.
	now func() time.Time
}

// SetGrantHold wires the grant hold. conn reads the recorder's own
// varroa_builds consumer, grace is the confirmed-gone grace, and
// maxPending is the backlog at which the consumer counts as behind. A
// zero grace applies DefaultConfirmedGoneGrace, and a zero maxPending
// applies DefaultSweepMaxPending. cmd/recorder calls this once, after
// NewLedger. A Ledger this method never touches holds no claim.
func (l *Ledger) SetGrantHold(conn *bus.Conn, grace time.Duration, maxPending uint64) {
	l.hold = newGrantHold(func(ctx context.Context) (*nats.ConsumerInfo, error) {
		return conn.ConsumerInfoContext(ctx, bus.BuildsStreamName, RecorderConsumerName)
	}, grace, maxPending)
}

// GrantHoldWired reports whether SetGrantHold wired the grant hold.
func (l *Ledger) GrantHoldWired() bool {
	return l.hold != nil
}

// newGrantHold returns a grantHold over consumer, with the defaults
// SetGrantHold applies. The grace is never shorter than the ingest
// AckWait: readConsumer counts an unacknowledged start as behind only
// once the stream redelivers it.
func newGrantHold(consumer func(ctx context.Context) (*nats.ConsumerInfo, error), grace time.Duration,
	maxPending uint64) *grantHold {
	if grace <= 0 {
		grace = DefaultConfirmedGoneGrace
	}
	if grace < ackWait {
		grace = ackWait
	}
	if maxPending == 0 {
		maxPending = DefaultSweepMaxPending
	}
	return &grantHold{grace: grace, maxPending: maxPending, consumer: consumer, now: time.Now}
}

// holds reports whether the repeat claim req for the unstarted run row
// must wait, and the message the NOT_READY answer carries. A nil hold
// holds nothing. Only a GRANTED intent with a named owner can be held, and
// only for a request from another instance: a same-instance repeat moves
// no owner, and a row with no instance names no owner to wait for. A
// request that names no instance is never the owner, because the owner
// always sent its own pod UID, so the hold covers it too.
//
// reading is the consumer state Claim read outside the transaction. A
// nil reading for a released claim answers errConsumerReadNeeded, so the
// network read never runs while the claim holds a row lock.
func (h *grantHold) holds(ctx context.Context, tx pgx.Tx, req ClaimRequest, row storedIntent,
	reading *consumerReading) (bool, string, error) {
	if h == nil || row.state != stateGranted {
		return false, "", nil
	}
	owner := ""
	if row.cellInstance != nil {
		owner = *row.cellInstance
	}
	if owner == "" || owner == req.CellInstance {
		return false, "", nil
	}

	// Test (b) reads two values of the database clock, so it compares
	// them with each other and never with the recorder clock.
	released := row.grantedAt == nil || row.dbNow.Sub(*row.grantedAt) >= grantHoldAge
	if !released {
		dead, err := readDeadInstancesTx(ctx, tx, req.Cluster, req.Tenant)
		if err != nil {
			return false, "", err
		}
		released = confirmedGoneInstance(dead, owner, h.now(), h.grace)
	}
	if !released {
		return true, fmt.Sprintf("instance %s holds the grant of this run and can still be building it", owner), nil
	}
	if reading == nil {
		return false, "", errConsumerReadNeeded
	}
	if reading.behind {
		return true, reading.message + ", so a start from instance " + owner + " can still be in transit", nil
	}
	return false, "", nil
}

// readConsumer reads the recorder's own varroa_builds consumer, bounded by
// ctx. A consumer state the read cannot return counts as behind, so the
// hold fails closed and the plugin retries.
//
// The consumer is behind when the interrupted sweep's backlog gate skips,
// or when it holds a redelivered message. newGrantHold keeps the grace no
// shorter than the ingest AckWait, so a start the stored owner sent and
// the recorder has not acknowledged by release (a) is a redelivery.
func (h *grantHold) readConsumer(ctx context.Context) consumerReading {
	info, readErr := h.consumer(ctx)
	if readErr != nil {
		return consumerReading{behind: true, message: "the ingest consumer state is not readable: " + readErr.Error()}
	}
	if reason, skip := sweepConsumerSkip(info, h.maxPending, h.now()); skip {
		return consumerReading{behind: true, message: "the ingest consumer is behind (" + reason + ")"}
	}
	if info.NumRedelivered > 0 {
		return consumerReading{behind: true, message: "the ingest consumer holds a redelivered message"}
	}
	return consumerReading{}
}

// readDeadInstancesTx reads the dead_instances list of cluster/tenant's
// placement row inside tx. A missing row answers an empty list. A TxFence
// already locked the row, so the read then sees the state the fence saw.
func readDeadInstancesTx(ctx context.Context, tx pgx.Tx, cluster, tenant string) ([]TenantSyncDeadInstance, error) {
	var raw json.RawMessage
	err := tx.QueryRow(ctx, `
		SELECT dead_instances FROM varroa.tenant_placements WHERE cluster = $1 AND tenant = $2
	`, cluster, tenant).Scan(&raw)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read dead instances: %w", err)
	}
	var dead []TenantSyncDeadInstance
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &dead); err != nil {
			return nil, fmt.Errorf("decode dead instances: %w", err)
		}
	}
	return dead, nil
}
