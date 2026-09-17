package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// Loopback listener (D7/D8). The co-located Jenkins plugin talks to the mite
// over 127.0.0.1 to post build events and to ask for a run claim. This is
// the mite's only loopback listener.
const (
	defaultLoopbackPort       = 9095
	loopbackTokenDirDefault   = "/var/run/varroa"
	loopbackTokenFileName     = "loopback-token"
	loopbackMaxBodyBytes      = 1 << 20 // 1 MiB
	loopbackReadHeaderTimeout = 5 * time.Second
	loopbackWriteTimeout      = 10 * time.Second
	loopbackShutdownTimeout   = 5 * time.Second

	maxClaimWaiters       = 64
	defaultClaimTimeoutMS = 4500

	defaultEventRetainBytes = 16 * 1024 * 1024 // 16 MiB

	// unavailableCode is the one failure shape every /v1/claim error path
	// returns: an empty decision with this code. The plugin reads one shape
	// on every path, whether the mite answered or the connection failed.
	unavailableCode = "UNAVAILABLE"

	// heartbeatPhase is the only BuildEvent phase the mite may drop instead
	// of retaining.
	heartbeatPhase = "HEARTBEAT"

	ackStatusStored   = "STORED"
	ackStatusBusy     = "BUSY"
	ackStatusRejected = "REJECTED"

	// logChunkMaxBytes is the mite's own cap on one POST /v1/log body,
	// matching loopbackMaxBodyBytes (the cap on every other loopback
	// route). A body over this cap answers 413 with X-Varroa-Max-Chunk-Bytes.
	logChunkMaxBytes = 1 << 20 // 1 MiB

	// The X-Varroa-* headers POST /v1/log carries. X-Varroa-Job-Path is
	// percent-encoded; every other header is a plain decimal or boolean.
	hdrLogRunID            = "X-Varroa-Run-Id"
	hdrLogJobPath          = "X-Varroa-Job-Path"
	hdrLogBuildNumber      = "X-Varroa-Build-Number"
	hdrLogSeq              = "X-Varroa-Seq"
	hdrLogNodeID           = "X-Varroa-Node-Id"
	hdrLogStageID          = "X-Varroa-Stage-Id"
	hdrLogFirstLineUnixMs  = "X-Varroa-First-Line-Unix-Ms"
	hdrLogDroppedBytes     = "X-Varroa-Dropped-Bytes"
	hdrLogFinal            = "X-Varroa-Final"
	hdrLogMaxChunkBytes    = "X-Varroa-Max-Chunk-Bytes"
	defaultLogRetainBytes  = 64 * 1024 * 1024 // 64 MiB
	defaultLogBusyPauseMS  = 1000
	varroaLogTailPath      = "/varroa-log/tail" // Jenkins root action the plugin registers
	tailControlActionStart = "start"
	tailHTTPRequestTimeout = 15 * time.Second

	// Cross-tenant loopback routes (design.md D2). maxCrossTenantWaiters
	// reuses maxClaimWaiters directly, because the two routes share one
	// 64-entry cap by definition, not by coincidence.
	maxCrossTenantWaiters = maxClaimWaiters

	defaultCrossTenantMiteTimeoutMS = 9500
	// crossTenantMiteTimeoutCapMS is the highest value
	// VARROA_CROSSTENANT_MITE_TIMEOUT_MS may resolve to. The listener's
	// write timeout is 10 seconds, so a longer wait would lose the
	// response on the socket.
	crossTenantMiteTimeoutCapMS = 10000
	// crossTenantPluginReadTimeoutDefaultMS is
	// VARROA_CROSSTENANT_READ_TIMEOUT_MS's own default. The mite cannot
	// read the plugin's environment, so the nesting check compares
	// against this default rather than the plugin's actual value.
	crossTenantPluginReadTimeoutDefaultMS = 10000
)

// crossTenantRouteKinds names the kinds each cross-tenant route accepts
// (design.md D2). Together the four sets are the whole closed set of eight
// kinds. A body whose kind is not in its own route's set is refused with
// 400, whether that kind belongs to one of the other three routes or to
// none of them.
var crossTenantRouteKinds = map[string]map[string]bool{
	"build": {
		"build": true,
	},
	"status": {
		"status":          true,
		"last_successful": true,
	},
	"artifact": {
		"artifact_list": true,
		"artifact_url":  true,
	},
	"lock": {
		"lock_acquire": true,
		"lock_renew":   true,
		"lock_release": true,
	},
}

var (
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
	// buildEventsRetainedBytes above.
	logChunksRetainedBytes metric.Int64UpDownCounter
)

func init() {
	meter := otel.Meter("varroa-mite")
	claimsGranted, _ = meter.Int64Counter("varroa.mite.claims.granted",
		metric.WithDescription("Run claims the recorder granted"),
	)
	claimsDuplicate, _ = meter.Int64Counter("varroa.mite.claims.duplicate",
		metric.WithDescription("Run claims the recorder marked duplicate"),
	)
	claimsVetoed, _ = meter.Int64Counter("varroa.mite.claims.vetoed",
		metric.WithDescription("Run claims the recorder vetoed"),
	)
	claimsFailed, _ = meter.Int64Counter("varroa.mite.claims.failed",
		metric.WithDescription("Run claims that ended with no decision (timeout, cap, disconnect)"),
	)
	buildEventsForwarded, _ = meter.Int64Counter("varroa.mite.build_events.forwarded",
		metric.WithDescription("Build events sent on the command stream"),
	)
	buildEventsAcked, _ = meter.Int64Counter("varroa.mite.build_events.acked",
		metric.WithDescription("Build events the gateway acknowledged as stored"),
	)
	buildEventsRejected, _ = meter.Int64Counter("varroa.mite.build_events.rejected",
		metric.WithDescription("Build events the gateway acknowledged as rejected"),
	)
	buildEventsRetainedBytes, _ = meter.Int64UpDownCounter("varroa.mite.build_events.retained_bytes",
		metric.WithDescription("Bytes of build events retained pending acknowledgement"),
	)
	logChunksForwarded, _ = meter.Int64Counter("varroa.mite.log_chunks.forwarded",
		metric.WithDescription("Console-log chunks sent on the command stream"),
	)
	logChunksAcked, _ = meter.Int64Counter("varroa.mite.log_chunks.acked",
		metric.WithDescription("Console-log chunks the gateway acknowledged as stored"),
	)
	logChunksRejected, _ = meter.Int64Counter("varroa.mite.log_chunks.rejected",
		metric.WithDescription("Console-log chunks the gateway acknowledged as rejected"),
	)
	logChunksRetainedBytes, _ = meter.Int64UpDownCounter("varroa.mite.log_chunks.retained_bytes",
		metric.WithDescription("Bytes of console-log chunks retained pending acknowledgement"),
	)
}

// logRetainBytesFromEnv resolves VARROA_MITE_LOG_RETAIN_BYTES, defaulting
// to 64 MiB.
func logRetainBytesFromEnv() int64 {
	raw := os.Getenv("VARROA_MITE_LOG_RETAIN_BYTES")
	if raw == "" {
		return defaultLogRetainBytes
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return defaultLogRetainBytes
	}
	return n
}

// errStreamNotConnected is returned by the locked send helpers when the
// command stream has never connected or is between reconnects.
var errStreamNotConnected = errors.New("mite: command stream not connected")

// envOrInt reads an integer env var, falling back to def on a missing or
// unparsable value.
func envOrInt(key string, def int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// loopbackPortFromEnv resolves MITE_LOOPBACK_PORT, defaulting to 9095.
func loopbackPortFromEnv() int {
	return envOrInt("MITE_LOOPBACK_PORT", defaultLoopbackPort)
}

// claimTimeoutFromEnv resolves MITE_CLAIM_TIMEOUT_MS, defaulting to 4500ms.
func claimTimeoutFromEnv() time.Duration {
	return time.Duration(envOrInt("MITE_CLAIM_TIMEOUT_MS", defaultClaimTimeoutMS)) * time.Millisecond
}

// crossTenantMiteTimeoutFromEnv resolves VARROA_CROSSTENANT_MITE_TIMEOUT_MS
// (design.md D14, mite-loopback-api spec). An absent variable keeps the
// 9500ms default. A value that is not a whole number is refused: the
// default stays in force and logger logs one WARNING. A resolved value
// above 10 seconds is clamped to 10 seconds, because the listener's write
// timeout is 10 seconds, and logger logs one WARNING for the clamp. The
// mite cannot read the plugin's own VARROA_CROSSTENANT_READ_TIMEOUT_MS, so
// the nesting check compares the resolved value against that variable's
// 10-second default: a value at or above it logs one further WARNING and
// still serves, because the mite cannot prove which side is misconfigured.
func crossTenantMiteTimeoutFromEnv(logger *slog.Logger) time.Duration {
	ms := defaultCrossTenantMiteTimeoutMS
	if raw := os.Getenv("VARROA_CROSSTENANT_MITE_TIMEOUT_MS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			logger.Warn("VARROA_CROSSTENANT_MITE_TIMEOUT_MS is not a whole number of milliseconds, keeping the default",
				"value", raw, "default_ms", defaultCrossTenantMiteTimeoutMS)
		} else {
			ms = n
		}
	}
	if ms > crossTenantMiteTimeoutCapMS {
		logger.Warn("VARROA_CROSSTENANT_MITE_TIMEOUT_MS exceeds the loopback listener's write timeout, clamping",
			"value_ms", ms, "clamped_ms", crossTenantMiteTimeoutCapMS)
		ms = crossTenantMiteTimeoutCapMS
	}
	if ms >= crossTenantPluginReadTimeoutDefaultMS {
		logger.Warn("the mite cross-tenant wait is at least as long as the plugin read budget's default; each budget must stay shorter than the budget outside it",
			"mite_timeout_ms", ms, "plugin_read_timeout_default_ms", crossTenantPluginReadTimeoutDefaultMS)
	}
	return time.Duration(ms) * time.Millisecond
}

// eventRetainBytesFromEnv resolves VARROA_EVENT_RETAIN_BYTES, defaulting to
// 16 MiB.
func eventRetainBytesFromEnv() int64 {
	raw := os.Getenv("VARROA_EVENT_RETAIN_BYTES")
	if raw == "" {
		return defaultEventRetainBytes
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return defaultEventRetainBytes
	}
	return n
}

// generateLoopbackToken makes a 32-byte random bearer token, encoded
// base64url without padding (base64.RawURLEncoding). This encoding is a
// contract: the mite writes the file and the Jenkins plugin's Java client
// reads it, so both sides must agree on the same codec.
func generateLoopbackToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate loopback token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// effectiveLoopbackPort resolves the port to bind: an explicit non-negative
// field (test override, 0 meaning "ask the OS for a free port") wins over
// the MITE_LOOPBACK_PORT env var and the 9095 default.
func (a *Agent) effectiveLoopbackPort() int {
	if a.loopbackPort >= 0 {
		return a.loopbackPort
	}
	return loopbackPortFromEnv()
}

// startLoopback generates the bearer token, writes it before the listener
// opens, and starts the HTTP listener on 127.0.0.1. It stops the listener
// when ctx is cancelled, mirroring the activity poller's start/stop wiring.
func (a *Agent) startLoopback(ctx context.Context) error {
	token, err := generateLoopbackToken()
	if err != nil {
		return err
	}
	a.loopbackToken = token

	dir := a.loopbackTokenDir
	if dir == "" {
		dir = loopbackTokenDirDefault
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create loopback token dir: %w", err)
	}
	// The file must exist before the listener opens, so no request can
	// arrive before the token does.
	if err := os.WriteFile(filepath.Join(dir, loopbackTokenFileName), []byte(token), 0644); err != nil {
		return fmt.Errorf("write loopback token: %w", err)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", a.effectiveLoopbackPort())
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen loopback %s: %w", addr, err)
	}

	// Resolved here, rather than at NewAgent time, so the nesting and clamp
	// warnings above reach the real logger: main.go assigns a.Logger after
	// NewAgent returns, and Run calls startLoopback after that assignment.
	a.crossTenantTimeout = crossTenantMiteTimeoutFromEnv(a.Logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/events", a.handleEvents)
	mux.HandleFunc("/v1/claim", a.handleClaim)
	mux.HandleFunc("/v1/log", a.handleLog)
	mux.HandleFunc("/v1/run-state", a.handleRunState)
	mux.HandleFunc("/v1/cross-tenant/build", a.crossTenantHandler("build"))
	mux.HandleFunc("/v1/cross-tenant/status", a.crossTenantHandler("status"))
	mux.HandleFunc("/v1/cross-tenant/artifact", a.crossTenantHandler("artifact"))
	mux.HandleFunc("/v1/cross-tenant/lock", a.crossTenantHandler("lock"))

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: loopbackReadHeaderTimeout,
		WriteTimeout:      loopbackWriteTimeout,
	}
	a.loopbackServer = srv
	a.loopbackListener = ln

	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			a.Logger.Error("loopback listener error", "error", serveErr)
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), loopbackShutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	return nil
}

// authenticated reports whether the request carries the loopback bearer
// token, compared with crypto/subtle.ConstantTimeCompare.
func (a *Agent) authenticated(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	presented := strings.TrimPrefix(h, prefix)
	return subtle.ConstantTimeCompare([]byte(presented), []byte(a.loopbackToken)) == 1
}

// sendBuildEventLocked forwards evt under sendMu, so it never races the
// heartbeat goroutine or another forward.
func (a *Agent) sendBuildEventLocked(evt *mitev1.BuildEvent) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	if a.stream == nil {
		return errStreamNotConnected
	}
	return a.stream.Send(&mitev1.MiteMessage{Message: &mitev1.MiteMessage_BuildEvent{BuildEvent: evt}})
}

// sendClaimRequestLocked sends req under sendMu.
func (a *Agent) sendClaimRequestLocked(req *mitev1.ClaimRequest) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	if a.stream == nil {
		return errStreamNotConnected
	}
	return a.stream.Send(&mitev1.MiteMessage{Message: &mitev1.MiteMessage_ClaimRequest{ClaimRequest: req}})
}

// sendCrossTenantRequestLocked sends req under sendMu.
func (a *Agent) sendCrossTenantRequestLocked(req *mitev1.CrossTenantRequest) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	if a.stream == nil {
		return errStreamNotConnected
	}
	return a.stream.Send(&mitev1.MiteMessage{Message: &mitev1.MiteMessage_CrossTenantRequest{CrossTenantRequest: req}})
}

// replayRetainedEvents resends every retained build event, ordered per run
// by sequence, after a (re)connect. It holds sendMu for the whole pass, so a
// concurrent /v1/events forward waits behind it — the replay always lands
// before any new forward on the new stream.
func (a *Agent) replayRetainedEvents() {
	events := a.eventRetention.snapshotOrdered()
	if len(events) == 0 {
		return
	}
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	if a.stream == nil {
		return
	}
	for _, evt := range events {
		msg := &mitev1.MiteMessage{Message: &mitev1.MiteMessage_BuildEvent{BuildEvent: evt}}
		if err := a.stream.Send(msg); err != nil {
			a.closeOnSendError(err)
			return
		}
	}
}

// sendLogChunkLocked forwards chunk under sendMu, so it never races the
// heartbeat goroutine or another forward.
func (a *Agent) sendLogChunkLocked(chunk *mitev1.LogChunk) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	if a.stream == nil {
		return errStreamNotConnected
	}
	return a.stream.Send(&mitev1.MiteMessage{Message: &mitev1.MiteMessage_LogChunk{LogChunk: chunk}})
}

// replayRetainedLogChunks resends every retained log chunk, ordered per
// run by sequence, after a (re)connect. It holds sendMu for the whole
// pass, so a concurrent /v1/log forward waits behind it — the replay
// always lands before any new forward on the new stream.
func (a *Agent) replayRetainedLogChunks() {
	chunks := a.logRetention.snapshotOrdered()
	if len(chunks) == 0 {
		return
	}
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	if a.stream == nil {
		return
	}
	for _, chunk := range chunks {
		msg := &mitev1.MiteMessage{Message: &mitev1.MiteMessage_LogChunk{LogChunk: chunk}}
		if err := a.stream.Send(msg); err != nil {
			a.closeOnSendError(err)
			return
		}
	}
}

// closeOnSendError closes the connection to unblock the receive loop, as
// the root AGENTS.md's send-error pattern requires — but only for a genuine
// send failure on a live stream. errStreamNotConnected means the stream, and
// therefore a.conn, may never have been set; closing a nil *grpc.ClientConn
// panics, so that case is a no-op here.
func (a *Agent) closeOnSendError(err error) {
	if errors.Is(err, errStreamNotConnected) {
		return
	}
	a.Logger.Warn("loopback forward failed, closing connection", "error", err)
	_ = a.conn.Close()
}

// handleEventAck applies one OperatorMessage.event_ack: it releases the
// retained event, or records the ack against an in-flight reservation when
// the event has not committed yet, counts it, and buffers the ack for the
// next /v1/events response so the plugin can release its spool.
func (a *Agent) handleEventAck(ctx context.Context, ack *mitev1.EventAck) {
	if ack == nil {
		return
	}
	if freed, ok := a.eventRetention.release(ack.RunId, ack.EventSeq, ack.Status); ok {
		buildEventsRetainedBytes.Add(ctx, -freed)
	}
	switch ack.Status {
	case ackStatusStored:
		buildEventsAcked.Add(ctx, 1)
	case ackStatusRejected:
		buildEventsRejected.Add(ctx, 1)
	}
	a.eventAcks.push(ack)
}

// handleLogAck applies one OperatorMessage.log_ack. ack_seq is
// status-dependent (design.md D3): STORED and REJECTED both name a
// release point, cumulative for STORED and per-chunk for REJECTED. BUSY
// opens a pause window on POST /v1/log for this run instead of releasing
// anything.
func (a *Agent) handleLogAck(ctx context.Context, ack *mitev1.LogAck) {
	if ack == nil {
		return
	}
	switch ack.Status {
	case ackStatusStored:
		freed := a.logRetention.release(ack.RunId, ack.AckSeq)
		logChunksRetainedBytes.Add(ctx, -freed)
		logChunksAcked.Add(ctx, 1)
	case ackStatusBusy:
		pauseMS := ack.PauseMs
		if pauseMS <= 0 {
			pauseMS = defaultLogBusyPauseMS
		}
		a.logRetention.pause(ack.RunId, time.Duration(pauseMS)*time.Millisecond)
	case ackStatusRejected:
		freed := a.logRetention.release(ack.RunId, ack.AckSeq)
		logChunksRetainedBytes.Add(ctx, -freed)
		logChunksRejected.Add(ctx, 1)
	}
}

// eventSize is the byte size the retention bound charges against an event:
// the same JSON encoding the mite sends on the wire.
func eventSize(evt *mitev1.BuildEvent) int64 {
	data, err := json.Marshal(evt)
	if err != nil {
		return 0
	}
	return int64(len(data))
}

// handleEvents serves POST /v1/events.
func (a *Agent) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.authenticated(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, loopbackMaxBodyBytes)
	var events []*mitev1.BuildEvent
	if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
		writeDecodeStatus(w, err)
		return
	}

	ctx := r.Context()
	for _, evt := range events {
		if evt == nil {
			continue
		}
		if evt.Phase == heartbeatPhase {
			// Best-effort: dropped, not retained, when the stream is down.
			if err := a.sendBuildEventLocked(evt); err == nil {
				buildEventsForwarded.Add(ctx, 1)
			}
			continue
		}

		size := eventSize(evt)
		key := retentionKey{runID: evt.RunId, seq: evt.Seq}
		id, _, ok := a.eventRetention.reserve(key, size)
		if !ok {
			// At the cap: forward nothing further, drop nothing retained.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if err := a.sendBuildEventLocked(evt); err != nil {
			a.eventRetention.rollback(id)
			a.closeOnSendError(err)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		// commit alone reports the retained-bytes delta: a retried delivery
		// can supersede this reservation before this commit call runs, and
		// only commit knows whether it still owns the charge it must
		// report.
		buildEventsRetainedBytes.Add(ctx, a.eventRetention.commit(id, evt, size))
		buildEventsForwarded.Add(ctx, 1)
	}

	acks := a.eventAcks.drain()
	if acks == nil {
		acks = []*mitev1.EventAck{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(acks)
}

// writeDecodeStatus answers 413 for an oversized body and 400 for any other
// decode failure.
func writeDecodeStatus(w http.ResponseWriter, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	w.WriteHeader(http.StatusBadRequest)
}

// handleClaim serves POST /v1/claim.
func (a *Agent) handleClaim(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.authenticated(r) {
		a.writeClaimFailure(ctx, w, http.StatusUnauthorized, "")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, loopbackMaxBodyBytes)
	var req mitev1.ClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			a.writeClaimFailure(ctx, w, http.StatusRequestEntityTooLarge, "")
			return
		}
		a.writeClaimFailure(ctx, w, http.StatusBadRequest, "")
		return
	}

	// The mite allocates the request id and the cell instance, overwriting
	// whatever the posted body carried.
	req.RequestId = uuid.NewString()
	req.CellInstance = a.cfg.CellInstance

	ch, ok := a.claimWaiters.register(req.RequestId)
	if !ok {
		a.writeClaimFailure(ctx, w, http.StatusTooManyRequests, req.RequestId)
		return
	}

	if err := a.sendClaimRequestLocked(&req); err != nil {
		a.claimWaiters.cancel(req.RequestId)
		a.closeOnSendError(err)
		a.writeClaimFailure(ctx, w, http.StatusServiceUnavailable, req.RequestId)
		return
	}

	timer := time.NewTimer(a.claimTimeout)
	defer timer.Stop()

	select {
	case resp := <-ch:
		countClaimDecision(ctx, resp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	case <-timer.C:
		a.claimWaiters.cancel(req.RequestId)
		a.writeClaimFailure(ctx, w, http.StatusGatewayTimeout, req.RequestId)
	}
}

// crossTenantKindField is the one field the mite reads from a cross-tenant
// loopback body: enough to route and validate the kind. Every other field
// in the body is opaque to the mite; it forwards the body verbatim as
// CrossTenantRequest.payload_json.
type crossTenantKindField struct {
	Kind string `json:"kind"`
}

// crossTenantHandler builds the handler for one of the four
// POST /v1/cross-tenant/* routes (design.md D2). route names the
// allowed-kind set in crossTenantRouteKinds.
func (a *Agent) crossTenantHandler(route string) http.HandlerFunc {
	allowed := crossTenantRouteKinds[route]
	return func(w http.ResponseWriter, r *http.Request) {
		a.handleCrossTenant(w, r, allowed)
	}
}

// handleCrossTenant serves one cross-tenant loopback route. It reuses the
// loopback listener's token check and body cap, and the claim route's
// correlation pattern: allocate a request id, register a waiter, send the
// request, and block for the matching reply on crossTenantWaiters, the
// waiter map this capability adds for the cross-tenant kinds (design.md
// D2, mite-loopback-api spec). It answers 400 for a body that does not
// decode or whose kind is outside allowedKinds, 401 for a wrong bearer,
// 413 for a body over the shared 1 MiB cap, 429 above the 64-waiter cap,
// 503 when the command stream is down, and 504 after the mite's wait
// budget elapses with no reply.
func (a *Agent) handleCrossTenant(w http.ResponseWriter, r *http.Request, allowedKinds map[string]bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.authenticated(r) {
		writeCrossTenantFailure(w, http.StatusUnauthorized, nil, "")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, loopbackMaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeCrossTenantFailure(w, http.StatusRequestEntityTooLarge, nil, "")
			return
		}
		writeCrossTenantFailure(w, http.StatusBadRequest, nil, "")
		return
	}

	var kf crossTenantKindField
	if err := json.Unmarshal(body, &kf); err != nil || !allowedKinds[kf.Kind] {
		writeCrossTenantFailure(w, http.StatusBadRequest, nil, kf.Kind)
		return
	}

	requestID := a.crossTenantSeq.Add(1)
	ch, ok := a.crossTenantWaiters.register(requestID)
	if !ok {
		writeCrossTenantFailure(w, http.StatusTooManyRequests, &requestID, kf.Kind)
		return
	}

	req := &mitev1.CrossTenantRequest{RequestId: &requestID, Kind: kf.Kind, PayloadJson: string(body)}
	if err := a.sendCrossTenantRequestLocked(req); err != nil {
		a.crossTenantWaiters.cancel(requestID)
		a.closeOnSendError(err)
		writeCrossTenantFailure(w, http.StatusServiceUnavailable, &requestID, kf.Kind)
		return
	}

	timer := time.NewTimer(a.crossTenantTimeout)
	defer timer.Stop()

	select {
	case resp := <-ch:
		writeCrossTenantPayload(w, http.StatusOK, resp)
	case <-timer.C:
		a.crossTenantWaiters.cancel(requestID)
		writeCrossTenantFailure(w, http.StatusGatewayTimeout, &requestID, kf.Kind)
	}
}

// writeCrossTenantPayload answers with the response's payload_json as the
// body, not the transport envelope: the plugin reads one flat shape
// (run_id, state, reason_code, or code) on every cross-tenant route, and
// the request_id and kind of the envelope are the mite's own bookkeeping.
// An empty payload answers an empty JSON object.
func writeCrossTenantPayload(w http.ResponseWriter, status int, resp *mitev1.CrossTenantResponse) {
	body := "{}"
	if resp != nil && resp.PayloadJson != "" {
		body = resp.PayloadJson
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// unavailableCrossTenantResponse is the one failure shape every
// cross-tenant route's error path returns: an empty reply carrying the
// UNAVAILABLE code in its payload, so the plugin reads one shape on every
// path whether the mite answered or the connection failed.
func unavailableCrossTenantResponse(requestID *uint64, kind string) *mitev1.CrossTenantResponse {
	return &mitev1.CrossTenantResponse{
		RequestId:   requestID,
		Kind:        kind,
		PayloadJson: `{"code":"` + unavailableCode + `"}`,
	}
}

// writeCrossTenantFailure answers status with the unavailable payload.
func writeCrossTenantFailure(w http.ResponseWriter, status int, requestID *uint64, kind string) {
	writeCrossTenantPayload(w, status, unavailableCrossTenantResponse(requestID, kind))
}

// handleLog serves POST /v1/log. It answers 202 after a successful send,
// 400 for malformed metadata, 401 for a bad bearer, 413 for a body over
// the mite's own chunk cap, and 503 when no stream is open, when the
// retained buffer is at its bound, or while a BUSY pause window is open
// for this run.
func (a *Agent) handleLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.authenticated(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	chunk, ok := parseLogChunkHeaders(r.Header)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// The mite is cluster-unaware, but it is cell-aware: it stamps its own
	// pod identity so the gateway can compare it with the ledger's owner.
	// The plugin sends no instance header.
	chunk.CellInstance = a.cfg.CellInstance

	body, err := io.ReadAll(io.LimitReader(r.Body, logChunkMaxBytes+1))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(body) > logChunkMaxBytes {
		w.Header().Set(hdrLogMaxChunkBytes, strconv.Itoa(logChunkMaxBytes))
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	chunk.Data = body

	if a.logRetention.paused(chunk.RunId) {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	size := int64(len(body))
	if !a.logRetention.reserve(size) {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if err := a.sendLogChunkLocked(chunk); err != nil {
		a.logRetention.rollback(size)
		a.closeOnSendError(err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	a.logRetention.commit(chunk, size)
	logChunksRetainedBytes.Add(r.Context(), size)
	logChunksForwarded.Add(r.Context(), 1)
	w.WriteHeader(http.StatusAccepted)
}

// parseLogChunkHeaders reads the nine X-Varroa-* headers proposal.md
// names and builds a LogChunk. It reports ok=false for a missing
// required field or a value that fails to parse: run_id, job_path,
// build_number, seq, and final are required; node_id, stage_id,
// first_line_unix_ms, and dropped_bytes are optional.
func parseLogChunkHeaders(h http.Header) (*mitev1.LogChunk, bool) {
	runID := h.Get(hdrLogRunID)
	if runID == "" {
		return nil, false
	}
	jobPath, err := url.QueryUnescape(h.Get(hdrLogJobPath))
	if err != nil || jobPath == "" {
		return nil, false
	}
	buildNumber, err := strconv.ParseInt(h.Get(hdrLogBuildNumber), 10, 64)
	if err != nil {
		return nil, false
	}
	seq, err := strconv.ParseInt(h.Get(hdrLogSeq), 10, 64)
	if err != nil || seq < 0 {
		return nil, false
	}
	final, err := strconv.ParseBool(h.Get(hdrLogFinal))
	if err != nil {
		return nil, false
	}

	chunk := &mitev1.LogChunk{
		RunId:       runID,
		JobPath:     jobPath,
		BuildNumber: buildNumber,
		Seq:         seq,
		NodeId:      h.Get(hdrLogNodeID),
		StageId:     h.Get(hdrLogStageID),
		Final:       final,
	}
	if v := h.Get(hdrLogFirstLineUnixMs); v != "" {
		ms, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, false
		}
		chunk.FirstLineUnixMs = ms
	}
	if v := h.Get(hdrLogDroppedBytes); v != "" {
		db, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, false
		}
		chunk.DroppedBytes = db
	}
	return chunk, true
}

// tailControlRequest is the JSON body the mite posts to the Jenkins
// plugin's tail root action. Field names mirror TailControl.
type tailControlRequest struct {
	RunID        string `json:"runId"`
	JobPath      string `json:"jobPath"`
	BuildNumber  int64  `json:"buildNumber"`
	Action       string `json:"action"`
	LeaseSeconds int    `json:"leaseSeconds"`
}

// handleTailControl applies one OperatorMessage.tail_control: it calls
// the Jenkins plugin's tail endpoint with the cached Jenkins bearer JWT,
// then answers MiteMessage.tail_ack with the same control_id. It runs in
// its own goroutine so a slow local Jenkins call never blocks the
// CommandStream read loop from dispatching the next message.
func (a *Agent) handleTailControl(ctx context.Context, tc *mitev1.TailControl) {
	if tc == nil {
		return
	}
	active := false
	errStr := ""

	body, marshalErr := json.Marshal(tailControlRequest{
		RunID:        tc.RunId,
		JobPath:      tc.JobPath,
		BuildNumber:  tc.BuildNumber,
		Action:       tc.Action,
		LeaseSeconds: tc.LeaseSeconds,
	})
	if marshalErr != nil {
		errStr = marshalErr.Error()
	} else {
		reqCtx, cancel := context.WithTimeout(ctx, tailHTTPRequestTimeout)
		defer cancel()
		req, reqErr := http.NewRequestWithContext(reqCtx, http.MethodPost, "http://localhost:8080"+varroaLogTailPath, bytes.NewReader(body))
		if reqErr != nil {
			errStr = reqErr.Error()
		} else {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+a.currentJenkinsToken())
			httpClient := &http.Client{Timeout: tailHTTPRequestTimeout}
			resp, doErr := httpClient.Do(req)
			if doErr != nil {
				errStr = doErr.Error()
			} else {
				_ = resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					active = tc.Action == tailControlActionStart
				} else {
					errStr = fmt.Sprintf("upstream status: %d", resp.StatusCode)
				}
			}
		}
	}

	ack := &mitev1.TailAck{RunId: tc.RunId, ControlId: tc.ControlId, Active: active, Error: errStr}
	a.sendMu.Lock()
	sendErr := a.stream.Send(&mitev1.MiteMessage{Message: &mitev1.MiteMessage_TailAck{TailAck: ack}})
	a.sendMu.Unlock()
	if sendErr != nil {
		a.Logger.Warn("send tail ack failed", "error", sendErr)
	}
}

// unavailableClaimResponse is the one failure shape every /v1/claim error
// path returns.
func unavailableClaimResponse(requestID string) *mitev1.ClaimResponse {
	return &mitev1.ClaimResponse{RequestId: requestID, Code: unavailableCode}
}

// writeClaimFailure answers status with an unavailable ClaimResponse body
// and counts it.
func (a *Agent) writeClaimFailure(ctx context.Context, w http.ResponseWriter, status int, requestID string) {
	resp := unavailableClaimResponse(requestID)
	countClaimDecision(ctx, resp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// countClaimDecision counts a ClaimResponse under the counter its decision
// names. An empty decision — every mite-generated failure and a gateway
// failure alike — counts as failed.
func countClaimDecision(ctx context.Context, resp *mitev1.ClaimResponse) {
	switch resp.Decision {
	case "granted":
		claimsGranted.Add(ctx, 1)
	case "duplicate":
		claimsDuplicate.Add(ctx, 1)
	case "vetoed":
		claimsVetoed.Add(ctx, 1)
	default:
		claimsFailed.Add(ctx, 1)
	}
}

// --- claimWaiterMap ---

// claimWaiterMap correlates an in-flight /v1/claim call with the
// ClaimResponse that arrives on the command stream, by request id. It holds
// at most maxClaimWaiters entries.
type claimWaiterMap struct {
	mu      sync.Mutex
	waiters map[string]chan *mitev1.ClaimResponse
}

func newClaimWaiterMap() *claimWaiterMap {
	return &claimWaiterMap{waiters: make(map[string]chan *mitev1.ClaimResponse)}
}

// register adds a waiter for requestID. It returns ok=false with no waiter
// added when the map is already at its cap.
func (m *claimWaiterMap) register(requestID string) (chan *mitev1.ClaimResponse, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.waiters) >= maxClaimWaiters {
		return nil, false
	}
	ch := make(chan *mitev1.ClaimResponse, 1)
	m.waiters[requestID] = ch
	return ch, true
}

// resolve delivers resp to the waiter for its request id and removes the
// entry. It is a no-op for an unknown id, such as a response for a request
// that already timed out.
func (m *claimWaiterMap) resolve(resp *mitev1.ClaimResponse) {
	if resp == nil {
		return
	}
	m.mu.Lock()
	ch, ok := m.waiters[resp.RequestId]
	if ok {
		delete(m.waiters, resp.RequestId)
	}
	m.mu.Unlock()
	if ok {
		ch <- resp
	}
}

// cancel removes a waiter without delivering a response, so a response that
// arrives after a timeout finds no entry.
func (m *claimWaiterMap) cancel(requestID string) {
	m.mu.Lock()
	delete(m.waiters, requestID)
	m.mu.Unlock()
}

// releaseAll delivers an unavailable response to every open waiter and
// empties the map. Called on stream teardown, so no waiting HTTP handler
// goroutine blocks past the end of the session.
func (m *claimWaiterMap) releaseAll() {
	m.mu.Lock()
	waiters := m.waiters
	m.waiters = make(map[string]chan *mitev1.ClaimResponse)
	m.mu.Unlock()
	for id, ch := range waiters {
		ch <- unavailableClaimResponse(id)
	}
}

// --- crossTenantWaiterMap ---

// crossTenantWaiterMap correlates an in-flight cross-tenant loopback call
// with the CrossTenantResponse that arrives on the command stream, by
// request id. It mirrors claimWaiterMap's shape and its 429/504/503 and
// release-on-teardown rules (design.md D2), applied to the numeric request
// id and response type the cross-tenant routes carry.
type crossTenantWaiterMap struct {
	mu      sync.Mutex
	waiters map[uint64]chan *mitev1.CrossTenantResponse
}

func newCrossTenantWaiterMap() *crossTenantWaiterMap {
	return &crossTenantWaiterMap{waiters: make(map[uint64]chan *mitev1.CrossTenantResponse)}
}

// register adds a waiter for requestID. It returns ok=false with no waiter
// added when the map is already at maxCrossTenantWaiters.
func (m *crossTenantWaiterMap) register(requestID uint64) (chan *mitev1.CrossTenantResponse, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.waiters) >= maxCrossTenantWaiters {
		return nil, false
	}
	ch := make(chan *mitev1.CrossTenantResponse, 1)
	m.waiters[requestID] = ch
	return ch, true
}

// resolve delivers resp to the waiter for its request id and removes the
// entry. It is a no-op for an unknown id, such as a response for a request
// that already timed out, and for a response carrying no request id.
func (m *crossTenantWaiterMap) resolve(resp *mitev1.CrossTenantResponse) {
	if resp == nil || resp.RequestId == nil {
		return
	}
	m.mu.Lock()
	ch, ok := m.waiters[*resp.RequestId]
	if ok {
		delete(m.waiters, *resp.RequestId)
	}
	m.mu.Unlock()
	if ok {
		ch <- resp
	}
}

// cancel removes a waiter without delivering a response, so a response that
// arrives after a timeout finds no entry.
func (m *crossTenantWaiterMap) cancel(requestID uint64) {
	m.mu.Lock()
	delete(m.waiters, requestID)
	m.mu.Unlock()
}

// releaseAll delivers an unavailable response to every open waiter and
// empties the map. Called on stream teardown, so no waiting HTTP handler
// goroutine blocks past the end of the session.
func (m *crossTenantWaiterMap) releaseAll() {
	m.mu.Lock()
	waiters := m.waiters
	m.waiters = make(map[uint64]chan *mitev1.CrossTenantResponse)
	m.mu.Unlock()
	for id, ch := range waiters {
		reqID := id
		ch <- unavailableCrossTenantResponse(&reqID, "")
	}
}

// --- eventRetentionStore ---

type retentionKey struct {
	runID string
	seq   int64
}

type retainedEvent struct {
	evt   *mitev1.BuildEvent
	bytes int64
}

// reservationID names one reserve call. commit and rollback act only on the
// reservation their id names, never on whatever reservation currently owns
// the same key. A second reserve call for a key supersedes the first: the
// first id then owns nothing, and a commit or rollback that later arrives
// for it is a safe no-op instead of acting on the second reservation by
// mistake.
type reservationID uint64

// pendingEventReservation tracks one open reserve call, from the call until
// its matching commit or rollback. key is the entry it reserves for.
// charged is the amount this reservation has added to totalBytes.
//
// acked and ackedStatus record an event_ack that release read for key
// while this reservation was still open and no committed entry existed
// yet. commit checks acked to decide whether to retain the event at all.
// A reservation that supersedes this one starts a fresh
// pendingEventReservation and inherits neither field.
type pendingEventReservation struct {
	key         retentionKey
	charged     int64
	acked       bool
	ackedStatus string
}

// eventRetentionStore holds every forwarded, unacknowledged build event,
// ordered per run by sequence, bounded by a total byte cap.
//
// Accounting invariant, true after every method call returns: totalBytes
// equals the sum of bytes over byKey plus the sum of charged over pending.
// checkInvariant asserts this. Once every reservation reserve opened has
// been resolved by commit or rollback (pending is empty), the invariant
// reduces to totalBytes == sum of bytes over byKey.
//
// A second reserve call for a key that already has an open reservation
// supersedes it: it refunds the open reservation's charge and opens a new
// one under a new id. commit and rollback act only on the id passed to
// them, so a call that names a superseded id finds no reservation, applies
// no charge, and reports a zero delta on the retained-bytes metric — the
// metric never moves at reserve time, only at commit or release, so a
// superseded reservation has nothing there to correct. This is what stops
// two overlapping handlers for the same key from ever both adding a charge
// to the metric for the same bytes.
//
// On a first delivery, an event_ack for key can arrive after reserve opens
// a reservation and before that reservation's commit runs: no committed
// entry exists yet for release to find. release then records the ack on
// the reservation currently open for key instead of discarding it. commit
// checks that flag: an acked reservation retains nothing and refunds its
// own charge, so the event never enters byKey and no later ack for the
// same key is needed to release it.
type eventRetentionStore struct {
	mu         sync.Mutex
	byKey      map[retentionKey]*retainedEvent
	pending    map[reservationID]*pendingEventReservation
	openByKey  map[retentionKey]reservationID // the reservation currently open for key, if any
	order      map[string][]int64             // runID -> retained sequences
	totalBytes int64
	maxBytes   int64
	nextID     reservationID
}

func newEventRetentionStore(maxBytes int64) *eventRetentionStore {
	return &eventRetentionStore{
		byKey:     make(map[retentionKey]*retainedEvent),
		pending:   make(map[reservationID]*pendingEventReservation),
		openByKey: make(map[retentionKey]reservationID),
		order:     make(map[string][]int64),
		maxBytes:  maxBytes,
	}
}

// reserve opens a reservation for a post of size for key and charges the
// byte cap for it. When key already holds a retained entry, it charges only
// the delta between size and the existing entry's bytes, so a retry of the
// same (run_id, event_seq) at the cap costs nothing extra and a same-size
// retry always succeeds. A reserve call for a key with an already-open
// reservation refunds that reservation's charge first and closes it, so two
// reserve calls for the same key without an intervening commit or rollback
// never double charge the cap — the newer call supersedes the older one and
// owns key from this point on. It returns the new reservation's id, the
// charged delta, and false with no charge applied when the delta would push
// the total over the cap.
//
// A same-size-or-smaller replacement charges nothing here and defers the
// whole adjustment to commit, even though the final size is already known:
// only a strictly larger replacement can breach the cap, so only that case
// needs the eager charge admission control exists for. Charging a shrink
// here would double-subtract when an event_ack releases the still-larger
// committed entry before this reservation resolves — release always removes
// the full committed size, and an eager partial charge for the same bytes
// has nowhere left to reconcile against once that entry is gone.
//
// Call commit with the returned id after a successful send, or rollback
// with it after a failed one. Neither call touches the retained-bytes
// metric through reserve's own charge: that charge is internal
// admission-control bookkeeping, never reported to a caller, so a
// supersession needs no metric correction later.
func (s *eventRetentionStore) reserve(key retentionKey, size int64) (reservationID, int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := int64(0)
	if old, exists := s.byKey[key]; exists {
		base = old.bytes
	}
	if oldID, exists := s.openByKey[key]; exists {
		if p, ok := s.pending[oldID]; ok {
			s.totalBytes -= p.charged
			delete(s.pending, oldID)
		}
		delete(s.openByKey, key)
	}
	delta := size - base
	charged := int64(0)
	if delta > 0 {
		if s.totalBytes+delta > s.maxBytes {
			return 0, delta, false
		}
		charged = delta
		s.totalBytes += charged
	}
	s.nextID++
	id := s.nextID
	s.pending[id] = &pendingEventReservation{key: key, charged: charged}
	s.openByKey[key] = id
	return id, charged, true
}

// rollback closes the reservation id names, because the send failed, and
// refunds exactly what it charged. It returns the metric delta the caller
// must apply, which is always zero: reserve never moves the retained-bytes
// metric, so rollback never has anything there to undo. It is a no-op when
// id owns no open reservation — a later reserve for the same key already
// superseded it, or id was never issued.
func (s *eventRetentionStore) rollback(id reservationID) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, exists := s.pending[id]
	if !exists {
		return 0
	}
	s.totalBytes -= p.charged
	delete(s.pending, id)
	delete(s.openByKey, p.key)
	return 0
}

// commit retains evt after a successful send and closes the reservation id
// names, if id still owns one. It returns the metric delta the caller must
// apply to the retained-bytes gauge.
//
// When id still owns its reservation, that delta is the full change to
// retained bytes for this key: the difference between size and whatever
// byKey already held for it (zero for a new key). reserve's own charge
// never touched the metric, so commit reports the whole change here rather
// than only the top-up over that internal charge — this holds even when an
// event_ack released the prior committed entry for this key while the send
// was in flight, because byKey is read fresh under the lock rather than
// trusted from reserve time. A duplicate delivery of the same (run_id,
// event_seq) that still owns its reservation replaces the existing entry in
// place: it keeps the entry's ordering position, so a retry never doubles
// the ordering slice.
//
// When id owns no reservation — a later reserve for the same key already
// superseded it — commit returns zero and leaves byKey and totalBytes
// untouched. The reservation that superseded it is the one whose own commit
// or rollback reconciles this key's accounting; touching byKey or
// totalBytes here as well would either double the retained-bytes delta or
// desync totalBytes from the sum the invariant expects.
//
// When id's reservation carries acked (release recorded an event_ack for
// this key while the reservation was still open), commit retains nothing:
// the event is already acknowledged, and the gateway sends no second ack
// for the same (run_id, event_seq) to release it later. It refunds the
// reservation's own charge and returns zero, the same net effect on
// totalBytes and the retained-bytes metric that a release arriving after
// this commit would have left.
func (s *eventRetentionStore) commit(id reservationID, evt *mitev1.BuildEvent, size int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, exists := s.pending[id]
	if !exists {
		return 0
	}
	delete(s.pending, id)
	delete(s.openByKey, p.key)

	if p.acked {
		s.totalBytes -= p.charged
		return 0
	}

	key := p.key
	base := int64(0)
	if old, exists := s.byKey[key]; exists {
		base = old.bytes
	} else {
		s.order[evt.RunId] = append(s.order[evt.RunId], evt.Seq)
	}
	s.totalBytes += size - base - p.charged
	s.byKey[key] = &retainedEvent{evt: evt, bytes: size}
	return size - base
}

// release drops the retained event for (runID, seq), if present, and
// returns the bytes it freed. It never touches a reservation reserve may
// still hold open for the same key: an in-flight resend must still land in
// byKey with its own byte charge once its commit or rollback resolves, not
// be silently absorbed by a release that fires while it is in flight.
//
// When no committed entry exists for key, release checks for a
// reservation currently open on key instead of stopping there. A first
// delivery's event_ack can arrive in that window, between its reserve and
// its commit. release records the ack on the open reservation (status
// remembered) so commit can retain nothing once it runs, rather than
// committing an event that is already acknowledged and will never be
// acknowledged again. A key with no committed entry and no open
// reservation has nothing for release to act on; it returns false, as
// before.
func (s *eventRetentionStore) release(runID string, seq int64, status string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := retentionKey{runID: runID, seq: seq}
	entry, ok := s.byKey[key]
	if !ok {
		if id, exists := s.openByKey[key]; exists {
			if p, exists := s.pending[id]; exists {
				p.acked = true
				p.ackedStatus = status
			}
		}
		return 0, false
	}
	delete(s.byKey, key)
	s.totalBytes -= entry.bytes
	seqs := s.order[runID]
	for i, sq := range seqs {
		if sq == seq {
			s.order[runID] = append(seqs[:i], seqs[i+1:]...)
			break
		}
	}
	if len(s.order[runID]) == 0 {
		delete(s.order, runID)
	}
	return entry.bytes, true
}

// checkInvariant reports whether totalBytes equals the sum of bytes over
// byKey plus the sum of charged over pending, and describes the mismatch
// when it does not. This holds at every point in the store's lifecycle; a
// caller that has resolved every open reservation may additionally check
// pendingCount() == 0, which reduces the invariant to totalBytes == sum of
// bytes over byKey.
func (s *eventRetentionStore) checkInvariant() (ok bool, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sumKey, sumPending int64
	for _, e := range s.byKey {
		sumKey += e.bytes
	}
	for _, p := range s.pending {
		sumPending += p.charged
	}
	want := sumKey + sumPending
	if s.totalBytes != want {
		return false, fmt.Sprintf("totalBytes=%d, sum(byKey)=%d, sum(pending charged)=%d, want=%d",
			s.totalBytes, sumKey, sumPending, want)
	}
	return true, ""
}

// pendingCount returns the number of currently open reservations, so a test
// can assert none outlived its commit or rollback.
func (s *eventRetentionStore) pendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// snapshotOrdered returns every retained event, ordered by run and then by
// ascending sequence, for a post-reconnect replay.
func (s *eventRetentionStore) snapshotOrdered() []*mitev1.BuildEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*mitev1.BuildEvent
	for runID, seqs := range s.order {
		sorted := append([]int64(nil), seqs...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		for _, seq := range sorted {
			if entry, ok := s.byKey[retentionKey{runID: runID, seq: seq}]; ok {
				out = append(out, entry.evt)
			}
		}
	}
	return out
}

// --- logRetentionStore ---

type logChunkKey struct {
	runID string
	seq   int64
}

type retainedLogChunk struct {
	chunk *mitev1.LogChunk
	bytes int64
}

// logRetentionStore holds every forwarded, unacknowledged log chunk,
// ordered per run by ascending sequence, bounded by a total byte cap
// (VARROA_MITE_LOG_RETAIN_BYTES). It also tracks a per-run BUSY pause
// window: POST /v1/log answers 503 for a run while its window is open.
type logRetentionStore struct {
	mu          sync.Mutex
	byKey       map[logChunkKey]*retainedLogChunk
	order       map[string][]int64 // runID -> retained sequences
	totalBytes  int64
	maxBytes    int64
	pausedUntil map[string]time.Time
}

func newLogRetentionStore(maxBytes int64) *logRetentionStore {
	return &logRetentionStore{
		byKey:       make(map[logChunkKey]*retainedLogChunk),
		order:       make(map[string][]int64),
		maxBytes:    maxBytes,
		pausedUntil: make(map[string]time.Time),
	}
}

// paused reports whether runID is inside an open BUSY pause window.
func (s *logRetentionStore) paused(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.pausedUntil[runID]
	return ok && time.Now().Before(until)
}

// pause opens a BUSY pause window for runID lasting d.
func (s *logRetentionStore) pause(runID string, d time.Duration) {
	s.mu.Lock()
	s.pausedUntil[runID] = time.Now().Add(d)
	s.mu.Unlock()
}

// reserve charges size against the byte cap. It returns false and charges
// nothing when the cap would be reached or exceeded. Call commit after a
// successful send, or rollback after a failed one.
func (s *logRetentionStore) reserve(size int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.totalBytes+size > s.maxBytes {
		return false
	}
	s.totalBytes += size
	return true
}

// rollback releases a reservation that was never committed, because the
// send failed.
func (s *logRetentionStore) rollback(size int64) {
	s.mu.Lock()
	s.totalBytes -= size
	s.mu.Unlock()
}

// commit retains chunk after a successful send. A duplicate delivery of
// the same (run_id, seq) replaces the existing entry in place: it keeps
// the entry's ordering position and releases the previously reserved
// bytes, so a retry never doubles the ordering slice or leaks the old
// reservation.
func (s *logRetentionStore) commit(chunk *mitev1.LogChunk, size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := logChunkKey{runID: chunk.RunId, seq: chunk.Seq}
	if old, exists := s.byKey[key]; exists {
		s.totalBytes -= old.bytes
	} else {
		s.order[chunk.RunId] = append(s.order[chunk.RunId], chunk.Seq)
	}
	s.byKey[key] = &retainedLogChunk{chunk: chunk, bytes: size}
}

// release drops every retained chunk of runID at or below seq — the
// cumulative release rule STORED and REJECTED share (design.md D3) — and
// returns the bytes it freed.
func (s *logRetentionStore) release(runID string, seq int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var freed int64
	seqs := s.order[runID]
	kept := seqs[:0]
	for _, sq := range seqs {
		key := logChunkKey{runID: runID, seq: sq}
		if sq <= seq {
			if entry, ok := s.byKey[key]; ok {
				freed += entry.bytes
				s.totalBytes -= entry.bytes
				delete(s.byKey, key)
			}
			continue
		}
		kept = append(kept, sq)
	}
	if len(kept) == 0 {
		delete(s.order, runID)
	} else {
		s.order[runID] = kept
	}
	return freed
}

// snapshotOrdered returns every retained log chunk, ordered by run and
// then by ascending sequence, for a post-reconnect replay.
func (s *logRetentionStore) snapshotOrdered() []*mitev1.LogChunk {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*mitev1.LogChunk
	for runID, seqs := range s.order {
		sorted := append([]int64(nil), seqs...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		for _, seq := range sorted {
			if entry, ok := s.byKey[logChunkKey{runID: runID, seq: seq}]; ok {
				out = append(out, entry.chunk)
			}
		}
	}
	return out
}

// --- ackBuffer ---

// ackBuffer holds EventAck values read from the stream since the last
// /v1/events response drained them.
type ackBuffer struct {
	mu    sync.Mutex
	items []*mitev1.EventAck
}

func newAckBuffer() *ackBuffer {
	return &ackBuffer{}
}

func (b *ackBuffer) push(ack *mitev1.EventAck) {
	b.mu.Lock()
	b.items = append(b.items, ack)
	b.mu.Unlock()
}

// drain returns every buffered ack and empties the buffer.
func (b *ackBuffer) drain() []*mitev1.EventAck {
	b.mu.Lock()
	defer b.mu.Unlock()
	items := b.items
	b.items = nil
	return items
}
