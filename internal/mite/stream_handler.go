package mite

import (
	"context"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// Sender is a function that sends an OperatorMessage to a mite over its
// gRPC CommandStream. It is safe to call from a single goroutine at a time;
// concurrent calls must be serialized externally.
type Sender func(msg *mitev1.OperatorMessage) error

// StreamHandler processes events on a mite CommandStream. BusHandler is the
// production implementation; it bridges each event to the NATS bus.
//
// The handler methods are called sequentially from the CommandStream read
// loop. OnConnect is called first and may start background goroutines; those
// goroutines should be stopped when OnDisconnect is called.
type StreamHandler interface {
	// OnConnect is called after the mite's hello has been validated and its
	// mTLS identity parsed. The send function can be used to push operator
	// messages to the mite from background goroutines. It returns a connection token that must be handed back to OnDisconnect,
	// so a late teardown from a superseded stream cannot tear down the
	// connection that replaced it.
	OnConnect(name, ns, version string, certExpiry interface{}, send Sender) int64

	// OnHeartbeat is called for each heartbeat message from the mite.
	OnHeartbeat(name, ns string, hb *mitev1.Heartbeat)

	// OnSnapshot is called for each state snapshot from the mite.
	OnSnapshot(name, ns string, snap *mitev1.StateSnapshot)

	// OnCommandResult is called when the mite reports a command result.
	OnCommandResult(name, ns string, result *mitev1.CommandResult)

	// OnTokenRefreshRequest is called when the mite requests a fresh Jenkins token.
	OnTokenRefreshRequest(name, ns string, req *mitev1.TokenRefreshRequest)

	// OnObservabilityReport is called when the mite sends an observability report.
	OnObservabilityReport(name, ns string, report *mitev1.ObservabilityReport)

	// OnContentResponse is called when the mite replies with its last-applied content.
	OnContentResponse(requestID string, resp *mitev1.ContentResponse)

	// OnJenkinsActivity is called when the mite forwards a Jenkins activity event
	// from the plugin drain endpoint. The controller/namespace come from the mTLS
	// stream identity (anti-spoof), never from the event payload.
	OnJenkinsActivity(name, ns string, evt *mitev1.JenkinsActivityEvent)

	// OnPluginInventory is called when the mite pushes a full plugin inventory.
	OnPluginInventory(name, ns string, inv *mitev1.PluginInventory)

	// IsCurrentConnection reports whether token still identifies the connection
	// the handler holds for this controller. The CommandStream read loop calls
	// it before dispatching each inbound message so a superseded stream stops
	// writing into state its replacement now owns. A zero token means the
	// handler issues no connection identity and every stream is current.
	IsCurrentConnection(name, ns string, token int64) bool

	// OnDisconnect is called when the stream ends (cleanly or with an error).
	// token is the value OnConnect returned for *this* stream; handlers must
	// ignore the call when a newer connection already owns the controller.
	OnDisconnect(name, ns string, token int64)

	// OnBuildEvent is called for each build event a mite forwards from the
	// Jenkins plugin. It returns an error only when the handler cannot
	// accept the event without delay; the CommandStream read loop then
	// ends the stream, which is the back-pressure signal contract F
	// relies on. token is the value OnConnect returned for this stream.
	OnBuildEvent(name, ns string, connToken int64, evt *mitev1.BuildEvent) error

	// OnClaimRequest is called for each claim request a mite forwards.
	// send is the same stream-bound sender OnConnect received, so the
	// handler can answer from a background goroutine with no new
	// per-connection state in server.go. token is the value OnConnect
	// returned for this stream.
	OnClaimRequest(name, ns string, connToken int64, req *mitev1.ClaimRequest, send Sender)

	// OnLogChunk is called for each console-log chunk a mite forwards from
	// the Jenkins plugin. The handler stores the chunk and acknowledges it
	// through OperatorMessage.log_ack on its own schedule, not from inside
	// this call.
	OnLogChunk(name, ns string, chunk *mitev1.LogChunk)

	// OnTailAck is called when a mite confirms or clears a live-tail lease
	// requested through OperatorMessage.tail_control.
	OnTailAck(name, ns string, ack *mitev1.TailAck)

	// OnCrossTenantRequest is called for each cross-tenant pipeline call a
	// mite forwards (design.md D3). send is the same stream-bound sender
	// OnConnect received, so the handler can answer from a background
	// goroutine with no new per-connection state in server.go. token is
	// the value OnConnect returned for this stream.
	OnCrossTenantRequest(name, ns string, connToken int64, req *mitev1.CrossTenantRequest, send Sender)

	// OnRunStateChunk is called for each run-state chunk a mite forwards
	// from the Jenkins plugin. The handler stores the chunk and answers
	// through OperatorMessage.run_state_ack.
	OnRunStateChunk(name, ns string, chunk *mitev1.RunStateChunk)

	// OnRunStateRequest is called when a mite fetches one stored state
	// file under a resume command. send is the same stream-bound sender
	// OnConnect received, so the handler answers on that connection.
	OnRunStateRequest(name, ns string, req *mitev1.RunStateRequest, send Sender)
}

// cellInstanceRecorder is an optional capability of a StreamHandler that
// tracks the pod unique identifier a mite presents at registration, for
// stamping onto that mite's later build-event envelopes. BusHandler
// implements it.
type cellInstanceRecorder interface {
	RecordCellInstance(name, ns, cellInstance string)
}

// streamBuildEventHandler is an optional capability of a StreamHandler.
// The server calls OnStreamBuildEvent in place of OnBuildEvent, with the
// cell instance of the stream's own verified certificate, so each event
// carries the instance of the stream that sent it. BusHandler implements
// it.
type streamBuildEventHandler interface {
	OnStreamBuildEvent(name, ns, cellInstance string, connToken int64, evt *mitev1.BuildEvent) error
}

// presenceKeeper is an optional capability of a StreamHandler. The server
// runs KeepPresence for each open stream on the stream context, and waits
// for it to return before it calls OnDisconnect. loop reports what the read
// loop of the stream does, and since when. BusHandler implements it.
type presenceKeeper interface {
	KeepPresence(ctx context.Context, name, ns string, token int64, loop func() (string, time.Time))
}
