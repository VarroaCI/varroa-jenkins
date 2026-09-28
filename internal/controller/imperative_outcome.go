package controller

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// Imperative command outcome labels. A deferred result is a postponed
// SAFE_RESTART drain, not a failure, so it takes its own label.
const (
	imperativeOutcomeSuccess  = "success"
	imperativeOutcomeFailure  = "failure"
	imperativeOutcomeDeferred = "deferred"
)

// imperativeFailedEventType is the activity type of one failed imperative
// command result.
const imperativeFailedEventType = "command.failed"

// imperativeResultsTotal counts every imperative command result the
// operator drains, labelled by kind and outcome.
var imperativeResultsTotal metric.Int64Counter

func init() { bindImperativeOutcomeMetrics() }

// bindImperativeOutcomeMetrics (re)binds the counter against the current
// global MeterProvider. A test re-invokes it after it swaps in a test
// MeterProvider.
func bindImperativeOutcomeMetrics() {
	m := otel.Meter("varroa-operator")
	imperativeResultsTotal, _ = m.Int64Counter("varroa.imperative_command.results",
		metric.WithDescription("Imperative command results from the mite, labelled by kind and outcome"),
	)
}

// imperativeOutcome answers the outcome label of one result.
func imperativeOutcome(res *mitev1.CommandResult) string {
	switch {
	case res.Success:
		return imperativeOutcomeSuccess
	case res.Deferred:
		return imperativeOutcomeDeferred
	default:
		return imperativeOutcomeFailure
	}
}

// imperativeFailureEvent builds the activity row of a failed imperative
// result. It answers false for a success and for a deferral: the activity
// stream holds audit-worthy events only.
func imperativeFailureEvent(cr *v1alpha1.Controller, res *mitev1.CommandResult, now time.Time) (activity.Event, bool) {
	if imperativeOutcome(res) != imperativeOutcomeFailure {
		return activity.Event{}, false
	}
	kind := res.CommandType.Label()
	subject := kind
	if res.JobPath != "" {
		subject = fmt.Sprintf("%s of %s", kind, res.JobPath)
	}
	if res.RunId != "" {
		subject = fmt.Sprintf("%s (run %s)", subject, res.RunId)
	}
	return activity.Event{
		Timestamp:  now,
		Type:       imperativeFailedEventType,
		Source:     "operator",
		Severity:   "warning",
		Controller: cr.Name,
		Namespace:  cr.Namespace,
		Reason:     kind,
		ItemPath:   res.JobPath,
		Message:    fmt.Sprintf("%s failed: %s", subject, res.Error),
	}, true
}

// recordImperativeOutcome counts one imperative result and publishes one
// activity row when the command failed. The mite and the gateway keep no
// other operator-visible record of a refused command.
func (r *Reconciler) recordImperativeOutcome(ctx context.Context, cr *v1alpha1.Controller, res *mitev1.CommandResult) {
	imperativeResultsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("kind", res.CommandType.Label()),
		attribute.String("outcome", imperativeOutcome(res)),
	))
	if r.activityPublisher == nil {
		return
	}
	if event, ok := imperativeFailureEvent(cr, res, time.Now().UTC()); ok {
		r.activityPublisher.Publish(event)
	}
}
