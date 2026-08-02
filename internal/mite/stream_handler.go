package mite

import (
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// Sender is a function that sends an OperatorMessage to a mite over its
// gRPC CommandStream. It is safe to call from a single goroutine at a time;
// concurrent calls must be serialized externally.
type Sender func(msg *mitev1.OperatorMessage) error

// StreamHandler processes events on a mite CommandStream. Implementations
// replace the in-memory Registry with alternative backends (e.g., a NATS bus).
//
// The handler methods are called sequentially from the CommandStream read
// loop. OnConnect is called first and may start background goroutines; those
// goroutines should be stopped when OnDisconnect is called.
type StreamHandler interface {
	// OnConnect is called after the mite's hello has been validated and its
	// mTLS identity parsed. The send function can be used to push operator
	// messages to the mite from background goroutines. The stream parameter
	// is the raw gRPC CommandStream server stream; handlers that need direct
	// access (e.g. RegistryHandler for Registry.Register) may type-assert it.
	OnConnect(name, ns, version string, certExpiry interface{}, send Sender, stream interface{})

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
	// Only active in the gateway process (BusHandler); other implementations stub it out.
	OnContentResponse(requestID string, resp *mitev1.ContentResponse)

	// OnJenkinsActivity is called when the mite forwards a Jenkins activity event
	// from the plugin drain endpoint. The controller/namespace come from the mTLS
	// stream identity (anti-spoof), never from the event payload.
	OnJenkinsActivity(name, ns string, evt *mitev1.JenkinsActivityEvent)

	// OnPluginInventory is called when the mite pushes a full plugin inventory.
	OnPluginInventory(name, ns string, inv *mitev1.PluginInventory)

	// OnDisconnect is called when the stream ends (cleanly or with an error).
	OnDisconnect(name, ns string)
}

// noopHandler is used when no StreamHandler is set (should not happen in
// practice since Server defaults to RegistryHandler).
type noopHandler struct{}

func (noopHandler) OnConnect(_, _, _ string, _ interface{}, _ Sender, _ interface{}) {}
func (noopHandler) OnHeartbeat(_, _ string, _ *mitev1.Heartbeat)                     {}
func (noopHandler) OnSnapshot(_, _ string, _ *mitev1.StateSnapshot)                  {}
func (noopHandler) OnCommandResult(_, _ string, _ *mitev1.CommandResult)             {}
func (noopHandler) OnTokenRefreshRequest(_, _ string, _ *mitev1.TokenRefreshRequest) {}
func (noopHandler) OnObservabilityReport(_, _ string, _ *mitev1.ObservabilityReport) {}
func (noopHandler) OnContentResponse(_ string, _ *mitev1.ContentResponse)            {}
func (noopHandler) OnJenkinsActivity(_, _ string, _ *mitev1.JenkinsActivityEvent)    {}
func (noopHandler) OnPluginInventory(_, _ string, _ *mitev1.PluginInventory)         {}
func (noopHandler) OnDisconnect(_, _ string)                                         {}

// Ensure noopHandler implements StreamHandler.
var _ StreamHandler = noopHandler{}
