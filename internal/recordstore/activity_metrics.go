package recordstore

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// ActivityMetrics holds the recorder's activity-ingest instruments on
// the "varroa-recorder" meter: three counters D3's disposition table
// drives, and one observable gauge reporting the activity-recorder
// consumer's backlog.
type ActivityMetrics struct {
	duplicate metric.Int64Counter
	expired   metric.Int64Counter
	unknown   metric.Int64Counter
	backlog   metric.Int64ObservableGauge
}

// NewActivityMetrics registers the activity-ingest instruments. conn
// reads the activity-recorder consumer's pending count for the backlog
// gauge; a lookup failure during one collection skips that collection
// rather than erroring the whole callback.
func NewActivityMetrics(conn *bus.Conn, streamName, consumerName string) (*ActivityMetrics, error) {
	meter := otel.Meter("varroa-recorder")
	m := &ActivityMetrics{}

	var err error
	m.duplicate, err = meter.Int64Counter("recorder_activity_duplicate_total",
		metric.WithDescription("Activity events whose insert matched an existing row"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_activity_duplicate_total: %w", err)
	}

	m.expired, err = meter.Int64Counter("recorder_activity_expired_total",
		metric.WithDescription("Activity events acknowledged because they predate the configured retention"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_activity_expired_total: %w", err)
	}

	m.unknown, err = meter.Int64Counter("recorder_activity_unknown_total",
		metric.WithDescription("Activity messages acknowledged because their payload does not decode"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_activity_unknown_total: %w", err)
	}

	m.backlog, err = meter.Int64ObservableGauge("recorder_activity_backlog_msgs",
		metric.WithDescription("Pending message count on the activity-recorder consumer"),
		metric.WithInt64Callback(func(_ context.Context, obs metric.Int64Observer) error {
			// A lookup failure skips this collection instead of erroring
			// it, so one slow response never breaks metrics scraping.
			if info, infoErr := conn.ConsumerInfo(streamName, consumerName); infoErr == nil {
				obs.Observe(int64(info.NumPending))
			}
			return nil
		}))
	if err != nil {
		return nil, fmt.Errorf("register recorder_activity_backlog_msgs: %w", err)
	}

	return m, nil
}

// addDuplicate counts one recorder_activity_duplicate_total event.
func (m *ActivityMetrics) addDuplicate(ctx context.Context) {
	if m == nil || m.duplicate == nil {
		return
	}
	m.duplicate.Add(ctx, 1)
}

// addExpired counts one recorder_activity_expired_total event.
func (m *ActivityMetrics) addExpired(ctx context.Context) {
	if m == nil || m.expired == nil {
		return
	}
	m.expired.Add(ctx, 1)
}

// addUnknown counts one recorder_activity_unknown_total event.
func (m *ActivityMetrics) addUnknown(ctx context.Context) {
	if m == nil || m.unknown == nil {
		return
	}
	m.unknown.Add(ctx, 1)
}
