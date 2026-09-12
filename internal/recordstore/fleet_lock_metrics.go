package recordstore

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/jackc/pgx/v5/pgxpool"
)

// FleetLockMetrics holds the fleet lock's four instruments on the
// "varroa-recorder" meter, design.md D15: three gauges reading the
// stored row counts by state, and one counter for a row a sweep or an
// acquire deletes for expiry.
type FleetLockMetrics struct {
	held      metric.Int64ObservableGauge
	waiting   metric.Int64ObservableGauge
	releasing metric.Int64ObservableGauge
	expired   metric.Int64Counter
}

// NewFleetLockMetrics registers recorder_fleet_lock_held,
// recorder_fleet_lock_waiting, recorder_fleet_lock_releasing, and
// recorder_fleet_lock_expired_total. The three gauges read pool directly
// on every collection; a lookup failure skips that one collection
// instead of erroring the whole callback, matching the ingest backlog
// gauges' own convention.
func NewFleetLockMetrics(pool *pgxpool.Pool) (*FleetLockMetrics, error) {
	meter := otel.Meter("varroa-recorder")
	m := &FleetLockMetrics{}

	var err error
	m.held, err = meter.Int64ObservableGauge("recorder_fleet_lock_held",
		metric.WithDescription("fleet_locks rows in the held state"),
		metric.WithInt64Callback(fleetLockStateGaugeCallback(pool, FleetLockStateHeld)))
	if err != nil {
		return nil, fmt.Errorf("register recorder_fleet_lock_held: %w", err)
	}

	m.waiting, err = meter.Int64ObservableGauge("recorder_fleet_lock_waiting",
		metric.WithDescription("fleet_locks rows in the waiting state"),
		metric.WithInt64Callback(fleetLockStateGaugeCallback(pool, FleetLockStateWaiting)))
	if err != nil {
		return nil, fmt.Errorf("register recorder_fleet_lock_waiting: %w", err)
	}

	m.releasing, err = meter.Int64ObservableGauge("recorder_fleet_lock_releasing",
		metric.WithDescription("fleet_locks rows in the releasing state, the cancellation backlog"),
		metric.WithInt64Callback(fleetLockStateGaugeCallback(pool, FleetLockStateReleasing)))
	if err != nil {
		return nil, fmt.Errorf("register recorder_fleet_lock_releasing: %w", err)
	}

	m.expired, err = meter.Int64Counter("recorder_fleet_lock_expired_total",
		metric.WithDescription("fleet_locks rows a sweep or an acquire deleted for expiry"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_fleet_lock_expired_total: %w", err)
	}

	return m, nil
}

// fleetLockStateGaugeCallback answers a callback counting fleet_locks
// rows whose state equals state.
func fleetLockStateGaugeCallback(pool *pgxpool.Pool, state string) func(ctx context.Context, obs metric.Int64Observer) error {
	return func(ctx context.Context, obs metric.Int64Observer) error {
		var count int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM varroa.fleet_locks WHERE state = $1`, state).Scan(&count); err == nil {
			obs.Observe(count)
		}
		return nil
	}
}

// addExpired counts n rows a sweep or an acquire deleted for expiry. A
// nil receiver, or a non-positive n, is a no-op.
func (m *FleetLockMetrics) addExpired(ctx context.Context, n int) {
	if m == nil || m.expired == nil || n <= 0 {
		return
	}
	m.expired.Add(ctx, int64(n))
}
