package main

import (
	"fmt"

	"go.opentelemetry.io/otel/metric"
)

// miteMeterName is the scope of every mite instrument.
const miteMeterName = "varroa-mite"

// miteMetrics holds the instruments of one Agent. NewAgent creates them
// from the provider that main passes. A test that builds an Agent by hand
// and reaches a counted path sets this field too, because a nil instrument
// panics.
type miteMetrics struct {
	activityEventsDropped   metric.Int64Counter
	activityEventsForwarded metric.Int64Counter

	claimsGranted   metric.Int64Counter
	claimsDuplicate metric.Int64Counter
	claimsVetoed    metric.Int64Counter
	claimsFailed    metric.Int64Counter

	buildEventsForwarded metric.Int64Counter
	buildEventsAcked     metric.Int64Counter
	buildEventsRejected  metric.Int64Counter
	// buildEventsRetainedBytes is an UpDownCounter, not a Gauge: it moves up
	// on every retained event and down on every release or rollback.
	buildEventsRetainedBytes metric.Int64UpDownCounter

	logChunksForwarded metric.Int64Counter
	logChunksAcked     metric.Int64Counter
	logChunksRejected  metric.Int64Counter
	// logChunksRetainedBytes is an UpDownCounter for the same reason as
	// buildEventsRetainedBytes.
	logChunksRetainedBytes metric.Int64UpDownCounter
}

// newMiteMetrics creates the mite instruments on the "varroa-mite" meter
// of mp.
func newMiteMetrics(mp metric.MeterProvider) (miteMetrics, error) {
	meter := mp.Meter(miteMeterName)
	var m miteMetrics
	counters := []struct {
		dst  *metric.Int64Counter
		name string
		desc string
	}{
		{&m.activityEventsDropped, "varroa.mite.activity.events.dropped",
			"Number of events the plugin reported as dropped"},
		{&m.activityEventsForwarded, "varroa.mite.activity.events.forwarded",
			"Number of Jenkins activity events forwarded to the gateway"},
		{&m.claimsGranted, "varroa.mite.claims.granted",
			"Run claims the recorder granted"},
		{&m.claimsDuplicate, "varroa.mite.claims.duplicate",
			"Run claims the recorder marked duplicate"},
		{&m.claimsVetoed, "varroa.mite.claims.vetoed",
			"Run claims the recorder vetoed"},
		{&m.claimsFailed, "varroa.mite.claims.failed",
			"Run claims that ended with no decision (timeout, cap, disconnect)"},
		{&m.buildEventsForwarded, "varroa.mite.build_events.forwarded",
			"Build events sent on the command stream"},
		{&m.buildEventsAcked, "varroa.mite.build_events.acked",
			"Build events the gateway acknowledged as stored"},
		{&m.buildEventsRejected, "varroa.mite.build_events.rejected",
			"Build events the gateway acknowledged as rejected"},
		{&m.logChunksForwarded, "varroa.mite.log_chunks.forwarded",
			"Console-log chunks sent on the command stream"},
		{&m.logChunksAcked, "varroa.mite.log_chunks.acked",
			"Console-log chunks the gateway acknowledged as stored"},
		{&m.logChunksRejected, "varroa.mite.log_chunks.rejected",
			"Console-log chunks the gateway acknowledged as rejected"},
	}
	for _, c := range counters {
		counter, err := meter.Int64Counter(c.name, metric.WithDescription(c.desc))
		if err != nil {
			return miteMetrics{}, fmt.Errorf("create %s counter: %w", c.name, err)
		}
		*c.dst = counter
	}
	upDown := []struct {
		dst  *metric.Int64UpDownCounter
		name string
		desc string
	}{
		{&m.buildEventsRetainedBytes, "varroa.mite.build_events.retained_bytes",
			"Bytes of build events retained pending acknowledgement"},
		{&m.logChunksRetainedBytes, "varroa.mite.log_chunks.retained_bytes",
			"Bytes of console-log chunks retained pending acknowledgement"},
	}
	for _, c := range upDown {
		counter, err := meter.Int64UpDownCounter(c.name, metric.WithDescription(c.desc))
		if err != nil {
			return miteMetrics{}, fmt.Errorf("create %s counter: %w", c.name, err)
		}
		*c.dst = counter
	}
	return m, nil
}
