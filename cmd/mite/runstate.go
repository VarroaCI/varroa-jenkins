// runstate.go holds the mite's half of the pipeline mesh: the loopback
// route that forwards one state chunk out of the cell, and the resume
// command that materializes a lost run's state, adopts its agent nodes,
// and reloads its job.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

const (
	// The X-Varroa-* headers POST /v1/run-state carries. X-Varroa-Job-Path
	// and X-Varroa-Relpath are percent-encoded; every other header is a
	// plain decimal.
	hdrStateRunID       = "X-Varroa-Run-Id"
	hdrStateJobPath     = "X-Varroa-Job-Path"
	hdrStateBuildNumber = "X-Varroa-Build-Number"
	hdrStateRelpath     = "X-Varroa-Relpath"
	hdrStateGen         = "X-Varroa-State-Gen"
	hdrStateSeq         = "X-Varroa-Seq"
	hdrStateTotal       = "X-Varroa-Total"
	hdrStateRawSize     = "X-Varroa-Raw-Size"
	hdrStateObjectSize  = "X-Varroa-Object-Size"
	hdrStatePusherEpoch = "X-Varroa-Pusher-Epoch"

	// hdrStateReason names the gateway's refusal on a 503, and
	// hdrStatePauseMs names the wait a busy answer asks for.
	hdrStateReason  = "X-Varroa-State-Reason"
	hdrStatePauseMs = "X-Varroa-Pause-Ms"

	// runStateChunkMaxBytes is the mite's own cap on one POST
	// /v1/run-state body, matching every other loopback route.
	runStateChunkMaxBytes = 1 << 20 // 1 MiB

	// runStateFetchTimeout bounds the wait for one file's responses. The
	// resume command's own deadline bounds the whole sequence.
	runStateFetchTimeout = 30 * time.Second

	// runStateFetchAttempts bounds the retry of one file. A transient
	// read that is not retried here costs the run this command's whole
	// remaining deadline, and one takeover round on top of it: the
	// recorder answers a failed fetch by arming the run again, and the
	// next round needs a target instance that is new.
	runStateFetchAttempts = 3

	// runStateFetchBackoff is the first wait between two fetch
	// attempts. Each further wait doubles it.
	runStateFetchBackoff = 500 * time.Millisecond

	// maxRunStateWaiters bounds the in-flight fetch set, reusing the
	// claim route's own 64-entry cap.
	maxRunStateWaiters = maxClaimWaiters

	// runStateResponseQueue bounds one waiter's buffered responses. A
	// 64 MiB file arrives as at most 128 chunks, so this queue never
	// fills for a file inside the per-run cap.
	runStateResponseQueue = 512

	// varroaAdoptNodePath is the plugin root action the resume calls once
	// for each named agent node.
	varroaAdoptNodePath = "/varroa-run/adopt-node"

	// varroaLoadBuildPath is the plugin root action the resume calls
	// once, after the reload, to load the restored build.
	varroaLoadBuildPath = "/varroa-run/load-build"

	// resumeInstanceMismatch names the failure a mite reports when the
	// command fences it out.
	resumeInstanceMismatch = "INSTANCE_MISMATCH"

	// resumeExpired names the failure a mite reports for a command that
	// reached it after the recorder gave up on the run.
	resumeExpired = "RESUME_EXPIRED"

	// resumeLoadFailed names the failure a mite reports when the
	// restored build does not load.
	resumeLoadFailed = "RESUME_LOAD_FAILED"

	// resumeNotBuilding names the failure a mite reports when the
	// restored build loads but does not run.
	resumeNotBuilding = "RESUME_NOT_BUILDING"

	// resumeProbeFailed names the failure a mite reports when it cannot
	// learn whether the build already runs. The command fails closed on
	// that answer, because a resume that writes over a running build
	// costs more than a resume that does not happen.
	resumeProbeFailed = "RESUME_PROBE_FAILED"

	// resumeProbeAttempts bounds the probe's own retry. The wait between
	// two attempts starts at runStateFetchBackoff and doubles, and the
	// command's deadline bounds the loop.
	resumeProbeAttempts = 3

	// adoptNodeTimeout bounds one adopt call.
	adoptNodeTimeout = 15 * time.Second

	// loadBuildTimeout bounds the load call. The load deserializes the
	// build's flow program and starts its execution, so it needs more
	// room than an adopt call.
	loadBuildTimeout = 60 * time.Second
)

// runStateRefusal is what the gateway last answered for one run. A
// permanent refusal answers 503 for every later post of that run; a
// transient one answers 503 once and clears; a busy answer names a
// pause window.
type runStateRefusal struct {
	reason    string
	permanent bool
	pauseMS   int
	until     time.Time
}

// runStateAckStore holds the gateway's last answer per run, so the
// loopback route can name a refusal reason on the run's next post. The
// log route's own pause window has the same shape.
type runStateAckStore struct {
	mu       sync.Mutex
	refusals map[string]runStateRefusal
}

func newRunStateAckStore() *runStateAckStore {
	return &runStateAckStore{refusals: make(map[string]runStateRefusal)}
}

// record stores one gateway answer. A stored answer clears the run's
// entry.
func (s *runStateAckStore) record(ack *mitev1.RunStateAck) {
	if ack == nil || ack.RunId == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ack.Status {
	case ackStatusStored:
		delete(s.refusals, ack.RunId)
	case ackStatusBusy:
		pause := ack.PauseMs
		if pause <= 0 {
			pause = defaultLogBusyPauseMS
		}
		s.refusals[ack.RunId] = runStateRefusal{
			reason:  ack.Reason,
			pauseMS: pause,
			until:   time.Now().Add(time.Duration(pause) * time.Millisecond),
		}
	case ackStatusRejected:
		permanent := ack.Reason == stateReasonStateCap || ack.Reason == stateReasonForeignRun
		s.refusals[ack.RunId] = runStateRefusal{reason: ack.Reason, permanent: permanent}
	}
}

// take answers the refusal that applies to runID's next post, and
// removes a transient one so it applies once. A permanent refusal stays
// and a busy window stays until it passes.
func (s *runStateAckStore) take(runID string) (runStateRefusal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.refusals[runID]
	if !ok {
		return runStateRefusal{}, false
	}
	switch {
	case r.permanent:
		return r, true
	case !r.until.IsZero():
		if time.Now().Before(r.until) {
			return r, true
		}
		delete(s.refusals, runID)
		return runStateRefusal{}, false
	default:
		delete(s.refusals, runID)
		return r, true
	}
}

// Gateway refusal reasons the mite forwards verbatim. The set is the
// whole set the loopback route may name.
const (
	stateReasonStateCap   = "state-cap"
	stateReasonForeignRun = "foreign-run"
	stateReasonBudgetFull = "gateway-budget-full"
)

// handleRunState serves POST /v1/run-state. It answers 202 after a
// successful forward, 400 for malformed metadata, 401 for a bad bearer,
// 413 for a body over the chunk cap, and 503 when no stream is open or
// when the gateway refused this run's last post.
func (a *Agent) handleRunState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.authenticated(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	chunk, ok := parseRunStateHeaders(r.Header)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// The mite stamps its own pod identity, so a cell cannot claim
	// another one. The plugin sends no instance header.
	chunk.CellInstance = a.cfg.CellInstance

	body, err := io.ReadAll(io.LimitReader(r.Body, runStateChunkMaxBytes+1))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(body) > runStateChunkMaxBytes {
		w.Header().Set(hdrLogMaxChunkBytes, strconv.Itoa(runStateChunkMaxBytes))
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	chunk.GzipContent = body

	if refusal, refused := a.runStateAcks.take(chunk.RunId); refused {
		w.Header().Set(hdrStateReason, refusal.reason)
		if refusal.reason == stateReasonBudgetFull && refusal.pauseMS > 0 {
			w.Header().Set(hdrStatePauseMs, strconv.Itoa(refusal.pauseMS))
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	if err := a.sendRunStateChunkLocked(chunk); err != nil {
		a.closeOnSendError(err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// parseRunStateHeaders reads the nine X-Varroa-* headers the route
// carries and builds a RunStateChunk. Every header is required, so a
// missing or unparsable one reports ok=false.
func parseRunStateHeaders(h http.Header) (*mitev1.RunStateChunk, bool) {
	runID := h.Get(hdrStateRunID)
	if runID == "" {
		return nil, false
	}
	jobPath, err := url.QueryUnescape(h.Get(hdrStateJobPath))
	if err != nil || jobPath == "" {
		return nil, false
	}
	relpath, err := url.QueryUnescape(h.Get(hdrStateRelpath))
	if err != nil || relpath == "" {
		return nil, false
	}
	buildNumber, err := strconv.ParseInt(h.Get(hdrStateBuildNumber), 10, 64)
	if err != nil {
		return nil, false
	}
	stateGen, err := strconv.ParseInt(h.Get(hdrStateGen), 10, 64)
	if err != nil || stateGen < 1 {
		return nil, false
	}
	seq, err := strconv.ParseInt(h.Get(hdrStateSeq), 10, 64)
	if err != nil || seq < 0 {
		return nil, false
	}
	total, err := strconv.ParseInt(h.Get(hdrStateTotal), 10, 64)
	if err != nil || total < 1 {
		return nil, false
	}
	rawSize, err := strconv.ParseInt(h.Get(hdrStateRawSize), 10, 64)
	if err != nil || rawSize < 0 {
		return nil, false
	}
	objectSize, err := strconv.ParseInt(h.Get(hdrStateObjectSize), 10, 64)
	if err != nil || objectSize <= 0 {
		return nil, false
	}
	// The push generation counts inside one Jenkins process. The epoch
	// names that process, so the gateway can tell a retry of a push from
	// the first push of a restarted one, which counts from 1 again.
	pusherEpoch, err := strconv.ParseInt(h.Get(hdrStatePusherEpoch), 10, 64)
	if err != nil || pusherEpoch <= 0 {
		return nil, false
	}
	return &mitev1.RunStateChunk{
		RunId: runID, JobPath: jobPath, BuildNumber: buildNumber, Relpath: relpath,
		StateGen: stateGen, Seq: seq, Total: total, RawSize: rawSize, ObjectSize: objectSize,
		PusherEpoch: pusherEpoch,
	}, true
}

// sendRunStateChunkLocked sends one chunk on the command stream under
// the shared send lock.
func (a *Agent) sendRunStateChunkLocked(chunk *mitev1.RunStateChunk) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	if a.stream == nil {
		return errStreamNotConnected
	}
	return a.stream.Send(&mitev1.MiteMessage{Message: &mitev1.MiteMessage_RunStateChunk{RunStateChunk: chunk}})
}

// --- runStateWaiterMap ---

// runStateWaiterMap correlates an in-flight fetch with the
// run_state_response messages that arrive on the command stream, by
// request id. One fetch receives several responses, so each waiter
// holds a buffered queue.
type runStateWaiterMap struct {
	mu      sync.Mutex
	waiters map[uint64]chan *mitev1.RunStateResponse
}

func newRunStateWaiterMap() *runStateWaiterMap {
	return &runStateWaiterMap{waiters: make(map[uint64]chan *mitev1.RunStateResponse)}
}

// register adds a waiter for requestID. It returns ok=false with no
// waiter added when the map is already at its cap.
func (m *runStateWaiterMap) register(requestID uint64) (chan *mitev1.RunStateResponse, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.waiters) >= maxRunStateWaiters {
		return nil, false
	}
	ch := make(chan *mitev1.RunStateResponse, runStateResponseQueue)
	m.waiters[requestID] = ch
	return ch, true
}

// resolve delivers resp to its waiter without blocking the stream read
// loop. It is a no-op for an unknown id and for a full queue, both of
// which leave the fetch to time out.
func (m *runStateWaiterMap) resolve(resp *mitev1.RunStateResponse) {
	if resp == nil || resp.RequestId == nil {
		return
	}
	m.mu.Lock()
	ch, ok := m.waiters[*resp.RequestId]
	m.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- resp:
	default:
	}
}

// cancel removes a waiter, so a response that arrives after a timeout
// finds no entry.
func (m *runStateWaiterMap) cancel(requestID uint64) {
	m.mu.Lock()
	delete(m.waiters, requestID)
	m.mu.Unlock()
}

// --- the resume command ---

// adoptNodeRequest is the body POST /varroa-run/adopt-node takes.
type adoptNodeRequest struct {
	Name string `json:"name"`
}

// loadBuildRequest is the body POST /varroa-run/load-build takes. A
// request with Probe set loads nothing and reports whether the build
// already runs.
type loadBuildRequest struct {
	JobPath     string `json:"jobPath"`
	BuildNumber int64  `json:"buildNumber"`
	Probe       bool   `json:"probe,omitempty"`
}

// loadBuildAnswer is what POST /varroa-run/load-build returns: whether
// the loaded build runs, whether it holds a place on the flow execution
// list, and, for a probe, whether it already ended here.
type loadBuildAnswer struct {
	Building   bool `json:"building"`
	Registered bool `json:"registered"`
	Finished   bool `json:"finished"`
}

// applyResumeBuild applies one RESUME_BUILD command. It fetches each
// named file, writes it under the Jenkins home, adopts every named node
// the disk holds, reloads the job, and reports the outcome.
//
// It fails the whole command at the first failure, so a half-written
// build is never reloaded.
func (a *Agent) applyResumeBuild(ctx context.Context, payload *mitev1.ResumeBuildPayload) (err error) {
	if payload == nil {
		return errors.New("resume_build payload is nil")
	}
	if payload.RunId == "" {
		return errors.New("resume_build payload names no run")
	}
	if len(payload.StateFiles) == 0 {
		return errors.New("resume_build payload lists no file")
	}

	// The recorder assigned this run to one pod. A pod that rolls
	// before the command arrives holds the same controller name and a
	// different instance, so it must refuse the command instead of
	// restoring a run the recorder gave to another pod. The check runs
	// before any disk work.
	//
	// An absent instance fails the same way. The field is required, and
	// a command that names no instance fences nothing: it would restore
	// files on whichever pod happens to own the controller.
	if payload.Instance == "" {
		return fmt.Errorf("%s: the command names no instance", resumeInstanceMismatch)
	}
	if payload.Instance != a.cfg.CellInstance {
		a.Logger.Error("resume refused",
			"run_id", payload.RunId, "reason", resumeInstanceMismatch,
			"command_instance", payload.Instance, "own_instance", a.cfg.CellInstance)
		return fmt.Errorf("%s: the command names instance %s and this mite is %s",
			resumeInstanceMismatch, payload.Instance, a.cfg.CellInstance)
	}

	// A command can wait in the queue. Past the deadline the recorder
	// has already marked the run, so restoring its files would resurrect
	// a build that ends the moment it starts. The command's own
	// execution bound cannot catch this: that window opens when the
	// worker picks the command up, not when the recorder sent it.
	if payload.DeadlineUnix > 0 && time.Now().Unix() >= payload.DeadlineUnix {
		a.Logger.Error("resume refused",
			"run_id", payload.RunId, "reason", resumeExpired,
			"deadline_unix", payload.DeadlineUnix)
		return fmt.Errorf("%s: the resume deadline passed at %d", resumeExpired, payload.DeadlineUnix)
	}

	// Every step runs inside the absolute deadline, not only the first
	// test. A command picked up just before that moment would otherwise
	// fetch, reload, and load past the recorder's own mark, and the
	// cancel the mark publishes would queue behind it. WithDeadline
	// keeps the earlier of the two, so the command's execution bound
	// still applies.
	if payload.DeadlineUnix > 0 {
		deadline := time.Unix(payload.DeadlineUnix, 0)
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
		// This runs before the cancel above, so the outcome is read
		// while the context still holds its reason. A failure past the
		// deadline is named as an expiry, whatever step it stopped at.
		defer func() {
			if err != nil && time.Now().After(deadline) {
				a.Logger.Error("resume refused",
					"run_id", payload.RunId, "reason", resumeExpired,
					"deadline_unix", payload.DeadlineUnix, "error", err)
				err = fmt.Errorf("%s: the resume deadline passed at %d: %w",
					resumeExpired, payload.DeadlineUnix, err)
			}
		}()
	}

	home := a.jenkinsHomeDir
	if home == "" {
		return errors.New("resume_build has no Jenkins home")
	}

	// The transport redelivers a command whose result was lost, so the
	// same resume can arrive twice. The second delivery must touch
	// nothing: rewriting the files of a build that already runs would
	// corrupt its flow program, and reloading its job under a live
	// execution is worse. The probe loads nothing, so it cannot resume
	// a build before this command adopted its nodes.
	//
	// The command fails closed on a probe that cannot answer. An
	// unanswered probe leaves the mite unable to tell a rescue from a
	// repeat, and a write over a running build costs more than a resume
	// that does not happen. The arm window bounds that loss, as it
	// bounds a resume that fails for any other reason.
	answer, probeErr := a.probeResumedBuild(ctx, payload.JobPath, payload.BuildNumber)
	if probeErr != nil {
		a.Logger.Error("resume refused",
			"run_id", payload.RunId, "reason", resumeProbeFailed,
			"job_path", payload.JobPath, "error", probeErr)
		return fmt.Errorf("%s: %w", resumeProbeFailed, probeErr)
	}
	if answer.Building {
		a.Logger.Info("resume already running",
			"run_id", payload.RunId, "job_path", payload.JobPath,
			"build_number", payload.BuildNumber, "instance", payload.Instance)
		return nil
	}
	// A build that ended between the first delivery and this one is
	// finished work. Restoring the files of the run it was would start
	// the same build again under the same number.
	if reason := resumeAlreadyFinished(home, payload, answer); reason != "" {
		a.Logger.Info("resume already finished",
			"run_id", payload.RunId, "job_path", payload.JobPath,
			"build_number", payload.BuildNumber, "evidence", reason)
		return nil
	}

	// The resume path speaks at INFO from here on. An operator reading
	// one cluster's logs needs to place the resume in time: when it
	// started, how long each step took, and which step failed.
	started := time.Now()
	a.Logger.Info("resume started",
		"run_id", payload.RunId, "job_path", payload.JobPath,
		"build_number", payload.BuildNumber, "files", len(payload.StateFiles),
		"instance", payload.Instance)

	var nodeNames []string
	var slowestPath string
	var slowestFetch time.Duration
	var fetchedBytes int
	for _, file := range payload.StateFiles {
		relPath := file.Relpath
		parsed, absPath, err := checkRehydrationPath(home, relPath)
		if err != nil {
			return fmt.Errorf("resume_build: %w", err)
		}
		if err := checkResumePathBinding(parsed, payload); err != nil {
			return fmt.Errorf("resume_build: %w", err)
		}
		if parsed.kind == rehydrationPathNodeConfig {
			nodeNames = append(nodeNames, parsed.nodeName)
		}

		fetchStart := time.Now()
		data, err := a.fetchRunStateFileWithRetry(ctx, payload.RunId, relPath, file.PusherEpoch, file.StateGen)
		if err != nil {
			a.Logger.Error("resume fetch failed",
				"run_id", payload.RunId, "relpath", relPath, "error", err)
			return fmt.Errorf("resume_build: %w", err)
		}
		if took := time.Since(fetchStart); took > slowestFetch {
			slowestFetch = took
			slowestPath = relPath
		}
		fetchedBytes += len(data)
		// A node's configuration belongs to the running JVM, so an
		// existing one is never overwritten. Every build file of the
		// command's own build may be written over.
		allowOverwrite := parsed.kind != rehydrationPathNodeConfig
		if err := writeRehydrationFileAt(absPath, data, allowOverwrite); err != nil {
			if parsed.kind == rehydrationPathNodeConfig && errors.Is(err, errRehydrationFileExists) {
				// The running JVM already holds this node's config. The
				// mite skips the write and still adopts the node below.
				continue
			}
			return fmt.Errorf("resume_build: %w", err)
		}
	}

	// Every named node the disk holds is adopted, not only one this
	// attempt wrote. An attempt that failed after writing a node file
	// would otherwise never adopt it, and the build would wait for a
	// node that never appears.
	a.Logger.Info("resume files written",
		"run_id", payload.RunId, "files", len(payload.StateFiles), "bytes", fetchedBytes,
		"slowest_file", slowestPath, "slowest_fetch", slowestFetch.String(),
		"elapsed", time.Since(started).String())

	for _, name := range nodeNames {
		if _, err := os.Stat(filepath.Join(home, "nodes", name, "config.xml")); err != nil {
			continue
		}
		if err := a.adoptNode(ctx, name); err != nil {
			a.Logger.Error("resume adopt-node failed",
				"run_id", payload.RunId, "node", name, "error", err)
			return fmt.Errorf("resume_build: %w", err)
		}
		a.Logger.Info("resume adopted a node", "run_id", payload.RunId, "node", name)
	}

	reloadStart := time.Now()
	if err := a.getJenkinsClient().ReloadItem(ctx, payload.JobPath); err != nil {
		a.Logger.Error("resume reload failed",
			"run_id", payload.RunId, "job_path", payload.JobPath, "error", err)
		return fmt.Errorf("resume_build: reload %s: %w", payload.JobPath, err)
	}
	reload := time.Since(reloadStart)

	// The reload loads the job, never its builds. Jenkins loads a build
	// on demand, so a build directory written after the job was loaded
	// stays invisible, and the resume machinery that runs inside that
	// load never runs. The load is forced here, inside the command's own
	// deadline, so a resume that reports success has a running build
	// behind it.
	loadStart := time.Now()
	loaded, err := a.loadResumedBuild(ctx, payload.JobPath, payload.BuildNumber)
	if err != nil {
		a.Logger.Error("resume load-build failed",
			"run_id", payload.RunId, "reason", resumeLoadFailed,
			"job_path", payload.JobPath, "build_number", payload.BuildNumber, "error", err)
		return fmt.Errorf("%s: %w", resumeLoadFailed, err)
	}
	a.Logger.Info("resume build loaded",
		"run_id", payload.RunId, "job_path", payload.JobPath,
		"build_number", payload.BuildNumber, "building", loaded.Building,
		"registered", loaded.Registered, "load", time.Since(loadStart).String())
	if !loaded.Building {
		a.Logger.Error("resume refused",
			"run_id", payload.RunId, "reason", resumeNotBuilding,
			"job_path", payload.JobPath, "build_number", payload.BuildNumber)
		return fmt.Errorf("%s: build %d of %s does not run after the reload",
			resumeNotBuilding, payload.BuildNumber, payload.JobPath)
	}

	a.Logger.Info("resume finished",
		"run_id", payload.RunId, "job_path", payload.JobPath,
		"reload", reload.String(), "elapsed", time.Since(started).String())
	return nil
}

// probeResumedBuild reports what the cell's Jenkins process already
// holds for the named build. The probe loads nothing.
//
// It retries, because the command fails closed on a probe that cannot
// answer and a single refused connection would then cost the run its
// rescue. The command's deadline bounds the loop.
func (a *Agent) probeResumedBuild(ctx context.Context, jobPath string, buildNumber int64) (loadBuildAnswer, error) {
	var lastErr error
	wait := runStateFetchBackoff
	for attempt := 0; attempt < resumeProbeAttempts; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return loadBuildAnswer{}, fmt.Errorf("probe %s: %w", jobPath, ctx.Err())
			}
			timer.Stop()
			wait *= 2
		}
		answer, err := a.callLoadBuild(ctx, loadBuildRequest{
			JobPath: jobPath, BuildNumber: buildNumber, Probe: true,
		})
		if err == nil {
			return answer, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return loadBuildAnswer{}, err
		}
	}
	return loadBuildAnswer{}, lastErr
}

// resumeAlreadyFinished reports the evidence that this target already
// ran the command's build to its end, or an empty string when it holds
// none.
//
// Jenkins is asked first, and the disk answers for a build this process
// has not loaded. Both readings are local: the recorder cannot answer
// this, because a resume moves a run's owner without a claim behind it.
func resumeAlreadyFinished(home string, payload *mitev1.ResumeBuildPayload, answer loadBuildAnswer) string {
	if answer.Finished {
		return "jenkins"
	}
	dir := resumeBuildDir(home, payload)
	if dir == "" {
		return ""
	}
	// CpsFlowExecution renames the flow's own directory when the flow
	// ends, so this name exists for a completed pipeline alone.
	if _, err := os.Stat(filepath.Join(dir, "workflow-completed")); err == nil {
		return "workflow-completed"
	}
	record, err := os.ReadFile(filepath.Join(dir, "build.xml"))
	if err == nil && bytes.Contains(record, []byte("<result>")) {
		return "build-record"
	}
	return ""
}

// resumeBuildDir answers the directory of the command's own build under
// home, or an empty string when the command names no build record.
func resumeBuildDir(home string, payload *mitev1.ResumeBuildPayload) string {
	for _, file := range payload.StateFiles {
		parsed, absPath, err := checkRehydrationPath(home, file.Relpath)
		if err != nil || parsed.kind != rehydrationPathBuildXML {
			continue
		}
		if checkResumePathBinding(parsed, payload) != nil {
			continue
		}
		return filepath.Dir(absPath)
	}
	return ""
}

// loadResumedBuild asks the plugin to load one numbered build and
// reports what the plugin answers. A non-2xx answer is an error, so the
// caller fails the command rather than report a resume that restored
// files and started nothing.
func (a *Agent) loadResumedBuild(ctx context.Context, jobPath string, buildNumber int64) (loadBuildAnswer, error) {
	return a.callLoadBuild(ctx, loadBuildRequest{JobPath: jobPath, BuildNumber: buildNumber})
}

// callLoadBuild posts one load-build request and decodes the answer.
func (a *Agent) callLoadBuild(ctx context.Context, request loadBuildRequest) (loadBuildAnswer, error) {
	var answer loadBuildAnswer
	jobPath := request.JobPath
	buildNumber := request.BuildNumber
	body, err := json.Marshal(request)
	if err != nil {
		return answer, fmt.Errorf("encode load-build body: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, loadBuildTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost,
		"http://localhost:8080"+varroaLoadBuildPath, bytes.NewReader(body))
	if err != nil {
		return answer, fmt.Errorf("build load-build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.currentJenkinsToken())

	httpClient := &http.Client{Timeout: loadBuildTimeout}
	resp, err := httpClient.Do(req)
	if err != nil {
		return answer, fmt.Errorf("load build %d of %s: %w", buildNumber, jobPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return answer, fmt.Errorf("load build %d of %s: Jenkins answered %d",
			buildNumber, jobPath, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return answer, fmt.Errorf("decode load-build answer: %w", err)
	}
	return answer, nil
}

// checkResumePathBinding binds one shape-valid path to the command's own
// job and build. A shape-valid path can otherwise name another job or
// another build, and the mite is the last hop before the filesystem.
func checkResumePathBinding(parsed parsedRehydrationPath, payload *mitev1.ResumeBuildPayload) error {
	if parsed.kind == rehydrationPathNodeConfig {
		return nil
	}
	if parsed.jobPath != payload.JobPath {
		return fmt.Errorf("path names job %q, not %q", parsed.jobPath, payload.JobPath)
	}
	if int64(parsed.buildNumber) != payload.BuildNumber {
		return fmt.Errorf("path names build %d, not %d", parsed.buildNumber, payload.BuildNumber)
	}
	return nil
}

// fetchRunStateFileWithRetry fetches one file, retrying a failed read
// with a doubling wait. The command's own deadline bounds the loop: the
// context ends the wait, so a retry never outlives the command.
//
// The retry is the mite's own repair, and it is the cheapest one. A
// failed resume answers one CommandResult, and the recorder arms the
// run again rather than sending this same command back. The run then
// waits for a target instance that is new, so a transient read that is
// not retried here costs this deadline and at least one more round.
//
// A push the object key cannot name fails at once. The gateway refuses
// such a request every time, so a retry can only spend the command's
// deadline on an answer that never changes.
func (a *Agent) fetchRunStateFileWithRetry(ctx context.Context, runID, relPath string, pusherEpoch, stateGen int64) ([]byte, error) {
	if pusherEpoch <= 0 || stateGen <= 0 {
		return nil, fmt.Errorf("fetch %s: the file names no pusher epoch or no push generation", relPath)
	}
	var lastErr error
	wait := runStateFetchBackoff
	for attempt := 0; attempt < runStateFetchAttempts; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, fmt.Errorf("fetch %s: %w", relPath, ctx.Err())
			}
			timer.Stop()
			wait *= 2
		}
		data, err := a.fetchRunStateFile(ctx, runID, relPath, pusherEpoch, stateGen)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// fetchRunStateFile asks the gateway for one stored file and returns its
// inflated bytes. A response carrying an error fails the fetch, so the
// caller writes no partial file.
func (a *Agent) fetchRunStateFile(ctx context.Context, runID, relPath string, pusherEpoch, stateGen int64) ([]byte, error) {
	requestID := a.runStateSeq.Add(1)
	ch, ok := a.runStateWaiters.register(requestID)
	if !ok {
		return nil, fmt.Errorf("fetch %s: the mite holds too many open fetches", relPath)
	}
	defer a.runStateWaiters.cancel(requestID)

	req := &mitev1.RunStateRequest{
		RequestId: &requestID, RunId: runID, Relpath: relPath,
		PusherEpoch: pusherEpoch, StateGen: stateGen,
	}
	if err := a.sendRunStateRequestLocked(req); err != nil {
		a.closeOnSendError(err)
		return nil, fmt.Errorf("fetch %s: %w", relPath, err)
	}

	timer := time.NewTimer(runStateFetchTimeout)
	defer timer.Stop()

	parts := make(map[int64][]byte)
	var total int64
	for {
		select {
		case resp := <-ch:
			if resp.Error != "" {
				return nil, fmt.Errorf("fetch %s: %s", relPath, resp.Error)
			}
			data, err := inflateRehydrationContent(resp.GzipContent, resp.RawSize)
			if err != nil {
				return nil, fmt.Errorf("fetch %s: %w", relPath, err)
			}
			parts[resp.Seq] = data
			total = resp.Total
			if total > 0 && int64(len(parts)) == total {
				return joinRunStateParts(parts, total)
			}
		case <-timer.C:
			return nil, fmt.Errorf("fetch %s: no answer inside the fetch budget", relPath)
		case <-ctx.Done():
			return nil, fmt.Errorf("fetch %s: %w", relPath, ctx.Err())
		}
	}
}

// joinRunStateParts concatenates the parts in ascending sequence order.
// A missing sequence fails the fetch.
func joinRunStateParts(parts map[int64][]byte, total int64) ([]byte, error) {
	var out bytes.Buffer
	for seq := int64(0); seq < total; seq++ {
		p, ok := parts[seq]
		if !ok {
			return nil, fmt.Errorf("the answer is missing chunk %d of %d", seq, total)
		}
		out.Write(p)
	}
	return out.Bytes(), nil
}

// sendRunStateRequestLocked sends one fetch on the command stream under
// the shared send lock.
func (a *Agent) sendRunStateRequestLocked(req *mitev1.RunStateRequest) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	if a.stream == nil {
		return errStreamNotConnected
	}
	return a.stream.Send(&mitev1.MiteMessage{Message: &mitev1.MiteMessage_RunStateRequest{RunStateRequest: req}})
}

// adoptNode posts one node name to the plugin's adopt route, the same
// way the tail control posts to its own root action. A 409 means the
// running JVM already holds that node, which is a success for this
// file.
func (a *Agent) adoptNode(ctx context.Context, name string) error {
	body, err := json.Marshal(adoptNodeRequest{Name: name})
	if err != nil {
		return fmt.Errorf("encode adopt-node body: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, adoptNodeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost,
		"http://localhost:8080"+varroaAdoptNodePath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build adopt-node request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.currentJenkinsToken())

	httpClient := &http.Client{Timeout: adoptNodeTimeout}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("adopt node %s: %w", name, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusConflict || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return nil
	}
	return fmt.Errorf("adopt node %s: Jenkins answered %d", name, resp.StatusCode)
}
