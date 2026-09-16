package recordstore

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// InterruptedMetrics holds the interrupted-run marking's instruments on
// the "varroa-recorder" meter.
type InterruptedMetrics struct {
	marked         metric.Int64Counter
	skipped        metric.Int64Counter
	cancel         metric.Int64Counter
	sweepSkipped   metric.Int64Counter
	retryPublished metric.Int64Counter
	cellLoss       metric.Int64Counter
	intentRefired  metric.Int64Counter
}

// NewInterruptedMetrics registers the interrupted-run marking's
// counters: recorder_interrupted_marked_total (labelled "path"),
// recorder_interrupted_skipped_total, recorder_interrupted_cancel_total,
// recorder_sweep_skipped_total (labelled "reason"),
// recorder_retry_published_total, recorder_cell_loss_confirmed_total
// (labelled "path"), and recorder_intent_refired_total (labelled
// "outcome" and "reason").
func NewInterruptedMetrics() (*InterruptedMetrics, error) {
	meter := otel.Meter("varroa-recorder")

	marked, err := meter.Int64Counter("recorder_interrupted_marked_total",
		metric.WithDescription("Runs marked interrupted, labelled by path (announce or sweep) and status (queued or running)"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_interrupted_marked_total: %w", err)
	}
	skipped, err := meter.Int64Counter("recorder_interrupted_skipped_total",
		metric.WithDescription("Runs the conditional interrupted-marking update did not change"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_interrupted_skipped_total: %w", err)
	}
	cancel, err := meter.Int64Counter("recorder_interrupted_cancel_total",
		metric.WithDescription("cancel_run actions published for a marked run on a live cell instance"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_interrupted_cancel_total: %w", err)
	}
	sweepSkipped, err := meter.Int64Counter("recorder_sweep_skipped_total",
		metric.WithDescription("Interrupted-sweep rounds skipped, labelled by reason"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_sweep_skipped_total: %w", err)
	}
	retryPublished, err := meter.Int64Counter("recorder_retry_published_total",
		metric.WithDescription("Automatic retry_run actions published"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_retry_published_total: %w", err)
	}

	cellLoss, err := meter.Int64Counter("recorder_cell_loss_confirmed_total",
		metric.WithDescription("Confirmed cell losses the recorder acted on, labelled by path (announce or sweep)"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_cell_loss_confirmed_total: %w", err)
	}
	intentRefired, err := meter.Int64Counter("recorder_intent_refired_total",
		metric.WithDescription("Granted intents of a confirmed-gone cell, labelled by outcome and reason"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_intent_refired_total: %w", err)
	}

	return &InterruptedMetrics{
		marked:         marked,
		skipped:        skipped,
		cancel:         cancel,
		sweepSkipped:   sweepSkipped,
		retryPublished: retryPublished,
		cellLoss:       cellLoss,
		intentRefired:  intentRefired,
	}, nil
}

// addMarked counts one mark, labelled by path ("announce" or "sweep")
// and by status ("queued" or "running"). A nil receiver is a no-op.
func (m *InterruptedMetrics) addMarked(ctx context.Context, path, status string) {
	if m == nil || m.marked == nil {
		return
	}
	m.marked.Add(ctx, 1, metric.WithAttributes(attribute.String("path", path), attribute.String("status", status)))
}

// addSkipped counts one row the conditional marking update did not
// change. A nil receiver is a no-op.
func (m *InterruptedMetrics) addSkipped(ctx context.Context) {
	if m == nil || m.skipped == nil {
		return
	}
	m.skipped.Add(ctx, 1)
}

// addCancel counts one published cancel_run action. A nil receiver is a
// no-op.
func (m *InterruptedMetrics) addCancel(ctx context.Context) {
	if m == nil || m.cancel == nil {
		return
	}
	m.cancel.Add(ctx, 1)
}

// addSweepSkipped counts one skipped sweep round, labelled by reason. A
// nil receiver is a no-op.
func (m *InterruptedMetrics) addSweepSkipped(ctx context.Context, reason string) {
	if m == nil || m.sweepSkipped == nil {
		return
	}
	m.sweepSkipped.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// addRetryPublished counts one published automatic retry_run action. A
// nil receiver is a no-op.
func (m *InterruptedMetrics) addRetryPublished(ctx context.Context) {
	if m == nil || m.retryPublished == nil {
		return
	}
	m.retryPublished.Add(ctx, 1)
}

// addCellLoss counts one run marked from a confirmed cell loss,
// labelled by path ("announce" or "sweep"). A nil receiver is a no-op.
func (m *InterruptedMetrics) addCellLoss(ctx context.Context, path string) {
	if m == nil || m.cellLoss == nil {
		return
	}
	m.cellLoss.Add(ctx, 1, metric.WithAttributes(attribute.String("path", path)))
}

// addRefired counts one re-fire outcome, labelled by outcome
// ("published" or "skipped") and by reason ("parameterized" or
// "no-placement", empty for a published outcome). A nil receiver is a
// no-op.
func (m *InterruptedMetrics) addRefired(ctx context.Context, outcome, reason string) {
	if m == nil || m.intentRefired == nil {
		return
	}
	m.intentRefired.Add(ctx, 1,
		metric.WithAttributes(attribute.String("outcome", outcome), attribute.String("reason", reason)))
}
