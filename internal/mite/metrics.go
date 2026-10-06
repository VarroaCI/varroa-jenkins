package mite

import (
	"fmt"

	"go.opentelemetry.io/otel/metric"
)

// gatewayMeterName is the scope of every gateway instrument.
const gatewayMeterName = "varroa-gateway"

// gatewayMetrics holds the instruments of one BusHandler. NewBusHandler
// creates them from the provider that main passes. A test that builds a
// BusHandler by hand sets this field too, because a nil instrument panics.
type gatewayMetrics struct {
	// claimsRejected counts a claim that the gateway refuses before the
	// claim reaches the record store: a cell claim whose job path names no
	// tenant that the cell hosts.
	claimsRejected metric.Int64Counter

	logChunksRejected metric.Int64Counter
	logChunksBusy     metric.Int64Counter
	logAppendRetries  metric.Int64Counter
	runStateBytes     metric.Int64Counter
	runStateRejected  metric.Int64Counter

	// miteStreamConnections is an UpDownCounter, not a Gauge: the handler
	// adds +1 and -1 deltas. A synchronous gauge keeps the last value, so it
	// reports 1 or -1, not the number of active connections.
	miteStreamConnections metric.Int64UpDownCounter
	miteStreamDisconnects metric.Int64Counter
	miteWatchFailures     metric.Int64Counter

	buildEventsDropped   metric.Int64Counter
	triggerFailedTotal   metric.Int64Counter
	triggerAcceptedTotal metric.Int64Counter
	resumeFailedTotal    metric.Int64Counter
	commandRefusedTotal  metric.Int64Counter
}

// newGatewayMetrics creates the gateway instruments on the "varroa-gateway"
// meter of mp.
func newGatewayMetrics(mp metric.MeterProvider) (gatewayMetrics, error) {
	meter := mp.Meter(gatewayMeterName)
	var m gatewayMetrics
	counters := []struct {
		dst  *metric.Int64Counter
		name string
		desc string
	}{
		{&m.claimsRejected, "varroa.gateway.claims.rejected",
			"Claims the gateway refused before making a ledger request"},
		{&m.logChunksRejected, "varroa.gateway.log.chunk.rejected",
			"Console-log chunks the gateway rejected (foreign run, empty run id, or the finalize deadline)"},
		{&m.logChunksBusy, "varroa.gateway.log.chunk.busy",
			"Console-log chunks the gateway answered BUSY, by reason"},
		{&m.logAppendRetries, "varroa.gateway.log.append.retries",
			"Blob store append retries for console-log chunks and index entries"},
		{&m.runStateBytes, "varroa.gateway.run_state.bytes",
			"Inflated bytes of run-state objects the gateway stored"},
		{&m.runStateRejected, "varroa.gateway.run_state.rejected",
			"Run-state chunks the gateway rejected, by reason"},
		{&m.miteStreamDisconnects, "varroa.mite.stream.disconnects",
			"Mite stream disconnect count"},
		{&m.miteWatchFailures, "varroa.mite.watch.failures",
			"Bus watch setup failures per mite connection (bridge is degraded until it succeeds)"},
		{&m.buildEventsDropped, "varroa.gateway.build_events.dropped",
			"Build events and build event publishes the gateway dropped or rejected"},
		{&m.triggerFailedTotal, "varroa.gateway.trigger_failed",
			"Failed build triggers the gateway published as a record"},
		{&m.triggerAcceptedTotal, "varroa.gateway.trigger_accepted",
			"Accepted build triggers the gateway published as a record"},
		{&m.resumeFailedTotal, "varroa.gateway.resume_failed",
			"Failed build resumes the gateway published as a record"},
		{&m.commandRefusedTotal, "varroa.gateway.command_refused",
			"Refused cancels and input answers the gateway published as a record"},
	}
	for _, c := range counters {
		counter, err := meter.Int64Counter(c.name, metric.WithDescription(c.desc))
		if err != nil {
			return gatewayMetrics{}, fmt.Errorf("create %s counter: %w", c.name, err)
		}
		*c.dst = counter
	}
	connections, err := meter.Int64UpDownCounter("varroa.mite.stream.connections",
		metric.WithDescription("Active mite stream connections"),
	)
	if err != nil {
		return gatewayMetrics{}, fmt.Errorf("create varroa.mite.stream.connections counter: %w", err)
	}
	m.miteStreamConnections = connections
	return m, nil
}
