package recordstore

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Webhook delivery outcomes, the values of the outcome label.
const (
	webhookOutcomeDelivered = "delivered"
	webhookOutcomeRetry     = "retry"
	webhookOutcomeFailed    = "failed"
	webhookOutcomeDropped   = "dropped"
)

// WebhookMetrics counts completion webhook deliveries by outcome.
type WebhookMetrics struct {
	deliveries metric.Int64Counter
}

// NewWebhookMetrics registers recorder_webhook_deliveries_total on mp.
func NewWebhookMetrics(mp metric.MeterProvider) (*WebhookMetrics, error) {
	meter := mp.Meter("varroa-recorder")
	deliveries, err := meter.Int64Counter("recorder_webhook_deliveries_total",
		metric.WithDescription("Completion webhook deliveries by outcome"))
	if err != nil {
		return nil, fmt.Errorf("register recorder_webhook_deliveries_total: %w", err)
	}
	return &WebhookMetrics{deliveries: deliveries}, nil
}

// add counts one delivery outcome. A nil receiver is a no-op.
func (m *WebhookMetrics) add(ctx context.Context, outcome string) {
	if m == nil || m.deliveries == nil {
		return
	}
	m.deliveries.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}
