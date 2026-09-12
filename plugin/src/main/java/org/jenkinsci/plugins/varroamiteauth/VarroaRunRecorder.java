package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Action;
import hudson.model.Cause;
import hudson.model.Job;
import hudson.model.ParameterValue;
import hudson.model.ParametersAction;
import hudson.model.Result;
import hudson.model.Run;
import hudson.model.TaskListener;
import hudson.model.User;
import hudson.model.labels.LabelAtom;
import hudson.model.listeners.RunListener;
import hudson.scm.ChangeLogSet;

import java.io.IOException;
import java.lang.reflect.Method;
import java.nio.charset.StandardCharsets;
import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.Collections;
import java.util.Deque;
import java.util.HashMap;
import java.util.HashSet;
import java.util.Iterator;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.Timer;
import java.util.TimerTask;
import java.util.WeakHashMap;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicLong;
import java.util.logging.Level;
import java.util.logging.Logger;

import jenkins.scm.RunWithSCM;

import org.jenkinsci.plugins.workflow.actions.ErrorAction;
import org.jenkinsci.plugins.workflow.actions.LabelAction;
import org.jenkinsci.plugins.workflow.actions.NotExecutedNodeAction;
import org.jenkinsci.plugins.workflow.actions.ThreadNameAction;
import org.jenkinsci.plugins.workflow.actions.TimingAction;
import org.jenkinsci.plugins.workflow.actions.WarningAction;
import org.jenkinsci.plugins.workflow.actions.WorkspaceAction;
import org.jenkinsci.plugins.workflow.flow.FlowDefinition;
import org.jenkinsci.plugins.workflow.flow.FlowExecution;
import org.jenkinsci.plugins.workflow.flow.FlowExecutionListener;
import org.jenkinsci.plugins.workflow.flow.FlowExecutionOwner;
import org.jenkinsci.plugins.workflow.flow.GraphListener;
import org.jenkinsci.plugins.workflow.graph.BlockEndNode;
import org.jenkinsci.plugins.workflow.graph.BlockStartNode;
import org.jenkinsci.plugins.workflow.graph.FlowGraphWalker;
import org.jenkinsci.plugins.workflow.graph.FlowNode;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.jenkinsci.plugins.workflow.pipelinegraphanalysis.GenericStatus;
import org.jenkinsci.plugins.workflow.pipelinegraphanalysis.StatusAndTiming;

import org.jenkinsci.plugins.varroamiteauth.template.VarroaTemplateValuesAction;

/**
 * Emits a {@link BuildEvent} for every run when the claim gate is on (D1,
 * D12). {@code RunListener} and {@code FlowExecutionListener} are abstract
 * classes in Jenkins core, so this outer class holds one static nested
 * {@code @Extension} class per extension point:
 *
 * <ul>
 *   <li>{@link Runs} ({@code RunListener}) emits {@code STARTED} and
 *       {@code COMPLETED}.
 *   <li>{@link Flows} ({@code FlowExecutionListener}) attaches and detaches
 *       the graph listener for a pipeline run.
 *   <li>{@link Graph} ({@code GraphListener}) emits {@code STAGE_STARTED}
 *       and {@code STAGE}.
 * </ul>
 *
 * <p>A per-run state map, keyed by {@code <job full name>#<build number>},
 * holds the run identity and a shared sequence counter. It is bounded at
 * 512 entries (D1). {@link Runs#onCompleted} removes the entry for every
 * run, pipeline or freestyle, because {@link Flows} never attaches for a
 * freestyle run. Upstream ordering places {@code RunListener.onStarted}
 * before {@code FlowExecutionListener.onRunning}, and {@code
 * RunListener.onCompleted} before {@code FlowExecutionListener.onCompleted}
 * (workflow-job-plugin, {@code WorkflowRun.java}, {@code run()} and
 * {@code finish()}), so the state entry a pipeline's {@link Graph}
 * consults is always present by the time it needs it, and {@link
 * Runs#onCompleted} never races {@link Flows#onCompleted} for the same
 * entry.
 *
 * <p>An event that offers to {@link BuildEventSink} and is refused (the
 * sink is full) is spooled by this class on the calling thread, because
 * the dispatcher only spools an event the sink accepted (D11).
 */
public final class VarroaRunRecorder {

    private static final Logger LOGGER = Logger.getLogger(VarroaRunRecorder.class.getName());

    private VarroaRunRecorder() {
    }

    // --- D11 size caps ---

    private static final int MAX_STAGES = 1000;
    private static final int MAX_CHANGES = 200;
    private static final int MAX_PARAMETERS = 200;
    private static final int MAX_PARAM_NAME = 256;
    private static final int MAX_PARAM_VALUE = 512;
    private static final int MAX_FIELD_512 = 512;
    private static final int MAX_STAGE_ERROR = 2048;
    private static final int LADDER_ERROR_LIMIT = 512;
    private static final int MAX_EVENT_BYTES = 512 * 1024;

    // --- Per-run state (D1) ---

    private static final int RUN_STATE_CAPACITY = 512;

    private static final Map<String, RunState> RUN_STATE = new LinkedHashMap<>(16, 0.75f, false) {
        @Override
        protected boolean removeEldestEntry(Map.Entry<String, RunState> eldest) {
            return size() > RUN_STATE_CAPACITY;
        }
    };

    private static final RunSpool SPOOL = new RunSpool();

    /** Identity and the shared sequence counter for one run. */
    private static final class RunState {
        final String runId;
        final String intentKey;
        final String jobPath;
        final long buildNumber;
        private final AtomicLong seq = new AtomicLong();
        /**
         * Set while a {@code STARTED} event is held for this run (D8a), so
         * a {@link VarroaTemplateValuesAction} attachment or a terminal
         * state with no action can release it. {@code null} once
         * released, or when this run's {@code STARTED} event was never
         * held.
         */
        private volatile Run<?, ?> heldStartedRun;

        RunState(String runId, String intentKey, String jobPath, long buildNumber) {
            this.runId = runId;
            this.intentKey = intentKey;
            this.jobPath = jobPath;
            this.buildNumber = buildNumber;
        }

        long nextSeq() {
            return seq.getAndIncrement();
        }
    }

    private static String stateKey(String jobPath, long buildNumber) {
        return jobPath + "#" + buildNumber;
    }

    private static RunState registerRun(String jobPath, long buildNumber, String runId, String intentKey) {
        RunState state = new RunState(runId, intentKey, jobPath, buildNumber);
        synchronized (RUN_STATE) {
            RUN_STATE.put(stateKey(jobPath, buildNumber), state);
        }
        return state;
    }

    private static RunState stateFor(String jobPath, long buildNumber) {
        synchronized (RUN_STATE) {
            return RUN_STATE.get(stateKey(jobPath, buildNumber));
        }
    }

    private static void removeState(String jobPath, long buildNumber) {
        synchronized (RUN_STATE) {
            RUN_STATE.remove(stateKey(jobPath, buildNumber));
        }
    }

    /** Number of live per-run state entries. Package-private for testing. */
    static int stateSizeForTesting() {
        synchronized (RUN_STATE) {
            return RUN_STATE.size();
        }
    }

    /** Registers a run's state directly, bypassing a real build. Package-private for testing. */
    static void registerRunForTesting(String jobPath, long buildNumber, String runId, String intentKey) {
        registerRun(jobPath, buildNumber, runId, intentKey);
    }

    /** Removes a run's state directly. Package-private for testing. */
    static void removeRunForTesting(String jobPath, long buildNumber) {
        removeState(jobPath, buildNumber);
    }

    /** Clears every per-run state entry. Package-private for testing. */
    static void resetStateForTesting() {
        synchronized (RUN_STATE) {
            RUN_STATE.clear();
        }
    }

    // --- Gate (D12) ---

    private static volatile String gateOverrideForTesting;

    private static boolean gateEnabled() {
        String override = gateOverrideForTesting;
        String raw = override != null ? override : System.getenv(VarroaQueueDecisionHandler.GATE_ENV);
        return raw != null && !raw.isEmpty() && !"0".equals(raw);
    }

    /**
     * Forces the gate value for a test. {@code RunListener}, {@code
     * FlowExecutionListener}, and {@code GraphListener} extension points
     * are invoked directly by Jenkins during a real build, so a test
     * cannot pass the gate value through a call argument the way {@link
     * VarroaQueueDecisionHandler} does. Pass {@code null} to fall back to
     * the process environment. Package-private for testing.
     */
    static void setGateOverrideForTesting(String value) {
        gateOverrideForTesting = value;
    }

    // --- Sink and spool (D11) ---

    private static void emit(BuildEvent event) {
        BuildEventSink sink = BuildEventSink.get();
        boolean accepted = sink.offer(event);
        if (!accepted && !event.isHeartbeat()) {
            // The sink is full. Move every event of this same run still
            // waiting in the sink into the spool first, in the order the
            // sink held them, before spooling this one. Without this
            // step an older, unacknowledged sibling could stay stranded
            // in the sink while the backlog replay posts this (newer)
            // event first, reversing per-run order (D11).
            for (BuildEvent queued : sink.drainRun(event.getRunId())) {
                SPOOL.append(queued.getRunId(), queued);
            }
            SPOOL.append(event.getRunId(), event);
            BuildEventDispatcher dispatcher = VarroaBuildEventLifecycle.current();
            if (dispatcher != null) {
                dispatcher.noteSpooled(event.getRunId());
            }
        }
    }

    /** Applies the D11 caps and the oversize ladder, then emits the result, unless it was dropped. */
    private static void emitCapped(BuildEvent event) {
        BuildEvent capped = applyCaps(event);
        if (capped != null) {
            emit(capped);
        }
    }

    // --- Heartbeat (D13) ---

    private static final long DEFAULT_HEARTBEAT_INTERVAL_SECONDS = 60L;
    private static final AtomicBoolean HEARTBEAT_STARTED = new AtomicBoolean(false);
    private static volatile Timer heartbeatTimer;

    private static long heartbeatIntervalMillis() {
        long seconds = DEFAULT_HEARTBEAT_INTERVAL_SECONDS;
        String raw = System.getenv("VARROA_RUN_HEARTBEAT_SECONDS");
        if (raw != null && !raw.isEmpty()) {
            try {
                long parsed = Long.parseLong(raw);
                if (parsed > 0) {
                    seconds = parsed;
                }
            } catch (NumberFormatException e) {
                LOGGER.log(Level.FINE, "VarroaRunRecorder: invalid VARROA_RUN_HEARTBEAT_SECONDS", e);
            }
        }
        return seconds * 1000L;
    }

    private static void ensureHeartbeatStarted() {
        if (HEARTBEAT_STARTED.compareAndSet(false, true)) {
            long intervalMillis = heartbeatIntervalMillis();
            Timer timer = new Timer("varroa-run-heartbeat", true);
            timer.scheduleAtFixedRate(new TimerTask() {
                @Override
                public void run() {
                    heartbeatTick();
                }
            }, intervalMillis, intervalMillis);
            heartbeatTimer = timer;
        }
    }

    /**
     * Cancels the heartbeat timer and resets the started flag, so a later
     * run can start it again. {@link VarroaBuildEventLifecycle} calls
     * this from its {@code @Terminator} (ruling r).
     */
    static void stopHeartbeat() {
        Timer timer = heartbeatTimer;
        heartbeatTimer = null;
        if (timer != null) {
            timer.cancel();
        }
        HEARTBEAT_STARTED.set(false);
    }

    /** Starts the heartbeat timer directly, bypassing a real build. Package-private for testing. */
    static void ensureHeartbeatStartedForTesting() {
        ensureHeartbeatStarted();
    }

    /** {@code true} once the heartbeat timer has started. Package-private for testing. */
    static boolean heartbeatStartedForTesting() {
        return HEARTBEAT_STARTED.get();
    }

    /**
     * Emits one {@code HEARTBEAT} event for every live run entry.
     * Package-private so a test can step one tick without waiting for the
     * real timer interval.
     */
    static void heartbeatTick() {
        List<RunState> snapshot;
        synchronized (RUN_STATE) {
            snapshot = new ArrayList<>(RUN_STATE.values());
        }
        long now = System.currentTimeMillis();
        for (RunState state : snapshot) {
            BuildEvent event = BuildEvent.builder()
                    .phase(BuildEvent.PHASE_HEARTBEAT)
                    .seq(state.nextSeq())
                    .emittedAtMillis(now)
                    .runId(state.runId)
                    .intentKey(state.intentKey)
                    .jobPath(state.jobPath)
                    .buildNumber(state.buildNumber)
                    .build();
            emit(event);
        }
    }

    // --- Runs: STARTED and COMPLETED (D1, D3) ---

    /** Emits {@code STARTED} and {@code COMPLETED} for every run. */
    @Extension
    public static class Runs extends RunListener<Run<?, ?>> {

        @Override
        public void onStarted(Run<?, ?> run, TaskListener listener) {
            try {
                if (!gateEnabled()) {
                    return;
                }
                VarroaIntentAction action = run.getAction(VarroaIntentAction.class);
                if (action == null || action.getRunId().isEmpty()) {
                    return;
                }
                String jobPath = safeJobPath(run);
                long buildNumber = run.getNumber();
                String runId = resolveRunId(action);
                RunState state = registerRun(jobPath, buildNumber, runId, action.getIntentKey());
                ensureHeartbeatStarted();

                if (isVarroaTemplateDefinition(run)) {
                    // D8a: FlowDefinition.create() can attach a
                    // VarroaTemplateValuesAction after this listener
                    // fires. Hold the STARTED event until the action
                    // attaches, or until the run completes with none.
                    state.heldStartedRun = run;
                    return;
                }

                emitCapped(startedEventBuilder(run, state, action).build());
            } catch (Exception e) {
                LOGGER.log(Level.WARNING, "VarroaRunRecorder.Runs: failed to emit STARTED", e);
            }
        }

        @Override
        public void onCompleted(Run<?, ?> run, TaskListener listener) {
            String jobPath = safeJobPath(run);
            long buildNumber = run.getNumber();
            try {
                if (!gateEnabled()) {
                    return;
                }
                VarroaIntentAction action = run.getAction(VarroaIntentAction.class);
                if (action == null || action.getRunId().isEmpty()) {
                    return;
                }
                RunState state = stateFor(jobPath, buildNumber);
                if (state != null) {
                    // A run whose definition never attached a
                    // VarroaTemplateValuesAction still owes its held
                    // STARTED event, with empty template fields (D8a).
                    releaseHeld(run, state, action, null);
                }
                String runId = resolveRunId(action);
                long seq = state != null ? state.nextSeq() : 0L;

                BuildEvent.Builder b = BuildEvent.builder()
                        .phase(BuildEvent.PHASE_COMPLETED)
                        .seq(seq)
                        .emittedAtMillis(System.currentTimeMillis())
                        .runId(runId)
                        .intentKey(action.getIntentKey())
                        .jobPath(jobPath)
                        .buildNumber(buildNumber)
                        .deliveryId(action.getDeliveryId());
                applyCommonMetadata(b, run, true);
                b.stages(rebuildStages(run));
                // Artifact archiving is complete at this call site (D6), so
                // the collector sees every entry the run will ever report.
                b.artifacts(ArtifactCollector.collect(run));
                b.logSummary(finishLogStorage(runId));
                applyTemplateFields(b, run);
                emitCapped(b.build());
            } catch (Exception e) {
                LOGGER.log(Level.WARNING, "VarroaRunRecorder.Runs: failed to emit COMPLETED", e);
            } finally {
                removeState(jobPath, buildNumber);
            }
        }
    }

    private static String resolveRunId(VarroaIntentAction action) {
        String fromRegistry = ClaimRegistry.get().runIdFor(action.getIntentKey());
        return fromRegistry != null && !fromRegistry.isEmpty() ? fromRegistry : action.getRunId();
    }

    /**
     * Runs {@link VarroaLogStorage#finish()} for {@code runId} (D2, D6) and
     * returns its {@link LogSummary}. Returns an empty, non-incomplete
     * summary when no {@link VarroaLogStorageFactory} ever captured this
     * run — {@code VARROA_MITE_LOG_URL} was unset, or the build carried no
     * run id, so the build used Jenkins' own log storage.
     */
    private static LogSummary finishLogStorage(String runId) {
        VarroaLogStorageFactory factory = VarroaLogStorageFactory.get();
        LogSummary summary = factory != null ? factory.finish(runId) : null;
        return summary != null ? summary : LogSummary.builder().build();
    }

    private static String safeJobPath(Run<?, ?> run) {
        try {
            return run.getParent().getFullName();
        } catch (Exception e) {
            return "";
        }
    }

    // --- D8a: the held STARTED event for a marker- or item-declared template run ---

    /**
     * The two {@code FlowDefinition} classes that attach a {@link
     * VarroaTemplateValuesAction} (D8). Named by class rather than by
     * type: this class cannot hold a compile dependency on the template
     * package's flow definitions. Keep this set in sync with every
     * FlowDefinition that attaches the action.
     */
    private static final Set<String> DEFAULT_TEMPLATE_DEFINITION_CLASSES = Set.of(
            "org.jenkinsci.plugins.varroamiteauth.template.VarroaMarkerFlowDefinition",
            "org.jenkinsci.plugins.varroamiteauth.template.VarroaTemplateFlowDefinition");

    private static volatile Set<String> templateDefinitionClassNamesOverrideForTesting;

    /**
     * Reports whether {@code run}'s job is defined by a Varroa template
     * flow definition (D8a). {@code RunListener.onStarted} can fire
     * before such a definition's {@code create} attaches the run action,
     * so this test decides whether {@link Runs#onStarted} holds the
     * {@code STARTED} event rather than emitting it at once.
     */
    private static boolean isVarroaTemplateDefinition(Run<?, ?> run) {
        if (!(run.getParent() instanceof WorkflowJob)) {
            return false;
        }
        FlowDefinition definition = ((WorkflowJob) run.getParent()).getDefinition();
        if (definition == null) {
            return false;
        }
        Set<String> names = templateDefinitionClassNamesOverrideForTesting;
        if (names == null) {
            names = DEFAULT_TEMPLATE_DEFINITION_CLASSES;
        }
        return names.contains(definition.getClass().getName());
    }

    /**
     * Registers the {@code FlowDefinition} class names {@link
     * #isVarroaTemplateDefinition} treats as holding the {@code STARTED}
     * event, replacing {@link #DEFAULT_TEMPLATE_DEFINITION_CLASSES}. Pass
     * {@code null} to restore the default set. Package-private for
     * testing: a test double stands in for the real flow definitions,
     * which this package does not depend on.
     */
    static void setTemplateDefinitionClassNamesForTesting(Set<String> names) {
        templateDefinitionClassNamesOverrideForTesting = names;
    }

    /** Builds a {@code STARTED} event from {@code run}'s current state. Does not emit it. */
    private static BuildEvent.Builder startedEventBuilder(Run<?, ?> run, RunState state, VarroaIntentAction action) {
        BuildEvent.Builder b = BuildEvent.builder()
                .phase(BuildEvent.PHASE_STARTED)
                .seq(state.nextSeq())
                .emittedAtMillis(System.currentTimeMillis())
                .runId(state.runId)
                .intentKey(state.intentKey)
                .jobPath(state.jobPath)
                .buildNumber(state.buildNumber)
                .deliveryId(action.getDeliveryId())
                .scmRevision(truncate(action.getRevision(), MAX_FIELD_512));
        applyCommonMetadata(b, run, false);
        return b;
    }

    /**
     * Fills the four template fields (38 to 41) from {@code run}'s {@link
     * VarroaTemplateValuesAction}, whichever state it carries. A no-op —
     * every field stays the empty string — for a run with no such action,
     * which is the default definition path (D8a, {@code
     * build-event-capture}). Every emitted event applies this except
     * {@code HEARTBEAT}, which never calls it.
     */
    private static void applyTemplateFields(BuildEvent.Builder b, Run<?, ?> run) {
        VarroaTemplateValuesAction action = run.getAction(VarroaTemplateValuesAction.class);
        if (action == null) {
            return;
        }
        b.templateName(action.getTemplateName())
                .templateVersion(action.getTemplateVersion())
                .markerPath(action.getMarkerPath())
                .markerRevision(action.getMarkerRevision());
    }

    /**
     * Releases {@code state}'s held {@code STARTED} event, if one is
     * held, filling the four template fields from {@code templateAction}
     * when it is not {@code null}. A no-op when no event is held — either
     * this run's STARTED event was never held, or a previous call already
     * released it.
     */
    private static void releaseHeld(Run<?, ?> run, RunState state, VarroaIntentAction intentAction,
            VarroaTemplateValuesAction templateAction) {
        if (state.heldStartedRun == null) {
            return;
        }
        state.heldStartedRun = null;
        if (intentAction == null || intentAction.getRunId().isEmpty()) {
            return;
        }
        BuildEvent.Builder b = startedEventBuilder(run, state, intentAction);
        if (templateAction != null) {
            b.templateName(templateAction.getTemplateName())
                    .templateVersion(templateAction.getTemplateVersion())
                    .markerPath(templateAction.getMarkerPath())
                    .markerRevision(templateAction.getMarkerRevision());
        }
        emitCapped(b.build());
    }

    /**
     * Releases the held {@code STARTED} event for {@code run}, filling
     * the four template fields from {@code templateAction} (D8a,
     * {@code build-event-capture}). {@link VarroaTemplateValuesAction#onAttached}
     * calls this the moment a Varroa flow definition attaches the action,
     * whichever state it carries. A no-op when this run's STARTED event
     * was not held — the gate was off, the run carries no claim, or the
     * event already released.
     */
    public static void releaseHeldStartedEvent(Run<?, ?> run, VarroaTemplateValuesAction templateAction) {
        try {
            String jobPath = safeJobPath(run);
            long buildNumber = run.getNumber();
            RunState state = stateFor(jobPath, buildNumber);
            if (state == null) {
                return;
            }
            VarroaIntentAction intentAction = run.getAction(VarroaIntentAction.class);
            releaseHeld(run, state, intentAction, templateAction);
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "VarroaRunRecorder: failed to release the held STARTED event", e);
        }
    }

    // --- D3: run metadata ---

    private static void applyCommonMetadata(BuildEvent.Builder b, Run<?, ?> run, boolean completed) {
        Job<?, ?> job = run.getParent();
        b.jobDisplayName(truncate(job.getDisplayName(), MAX_FIELD_512));
        b.buildDisplayName(truncate(run.getDisplayName(), MAX_FIELD_512));
        b.url(truncate(run.getUrl(), MAX_FIELD_512));
        b.result(run.getResult() != null ? run.getResult().toString() : "");

        List<Cause> causes = run.getCauses();
        b.actor(ActivityRunListener.resolveActorFromCauses(causes));
        List<String> causeTypes = new ArrayList<>();
        for (Cause c : causes) {
            causeTypes.add(c.getClass().getSimpleName());
        }
        b.causeTypes(causeTypes);
        b.causeDescription(IntentKey.causeDescription(causes));

        String upstreamJobPath = "";
        long upstreamBuildNumber = 0;
        for (Cause c : causes) {
            if (c instanceof Cause.UpstreamCause) {
                Cause.UpstreamCause upstream = (Cause.UpstreamCause) c;
                upstreamJobPath = upstream.getUpstreamProject();
                upstreamBuildNumber = upstream.getUpstreamBuild();
                break;
            }
        }
        // upstream_job_path is an identifier: the caps table never truncates it.
        b.upstreamJobPath(upstreamJobPath != null ? upstreamJobPath : "");
        b.upstreamBuildNumber(upstreamBuildNumber);

        long queuedAt = run.getTimeInMillis();
        long startedAt = run.getStartTimeInMillis();
        b.queuedAtMillis(queuedAt);
        b.startedAtMillis(startedAt);
        b.queueWaitMillis(Math.max(0, startedAt - queuedAt));
        b.estimatedDurationMillis(run.getEstimatedDuration());

        if (completed) {
            long duration = run.getDuration();
            if (duration == 0) {
                duration = Math.max(0, System.currentTimeMillis() - startedAt);
            }
            b.durationMillis(duration);
            b.completedAtMillis(startedAt + duration);

            List<BuildChange> changes = extractChanges(run);
            b.changes(changes);
            b.changeCount(changes.size());
            String newestRevision = "";
            for (BuildChange c : changes) {
                if (!c.getCommitId().isEmpty()) {
                    newestRevision = c.getCommitId();
                }
            }
            if (!newestRevision.isEmpty()) {
                b.scmRevision(truncate(newestRevision, MAX_FIELD_512));
            }

            int[] counts = readTestCounters(run.getAllActions());
            b.testTotal(counts[0]);
            b.testPassed(counts[1]);
            b.testFailed(counts[2]);
            b.testSkipped(counts[3]);
        }

        // No scm-api or branch-api compile dependency (D3): best-effort branch
        // name from the job's own short name under a multi-branch parent.
        String branchName = truncate(IntentKey.branchNameFor(job), MAX_FIELD_512);
        b.scmHead(branchName);
        b.branch(branchName);
        b.scmUrl("");

        ParametersAction params = run.getAction(ParametersAction.class);
        Map<String, String> values = new LinkedHashMap<>();
        List<String> masked = new ArrayList<>();
        if (params != null) {
            for (ParameterValue pv : params.getParameters()) {
                String name = truncate(pv.getName(), MAX_PARAM_NAME);
                if (pv.isSensitive()) {
                    masked.add(pv.getName());
                    values.put(name, "");
                } else {
                    values.put(name, truncate(stringValue(pv), MAX_PARAM_VALUE));
                }
            }
        }
        b.parameters(values);
        b.maskedParameterNames(masked);
    }

    private static String stringValue(ParameterValue pv) {
        try {
            Method m = pv.getClass().getMethod("getValue");
            Object v = m.invoke(pv);
            return v != null ? String.valueOf(v) : "";
        } catch (Exception e) {
            return String.valueOf(pv);
        }
    }

    @SuppressWarnings({"rawtypes", "unchecked"})
    private static List<BuildChange> extractChanges(Run<?, ?> run) {
        if (!(run instanceof RunWithSCM)) {
            return List.of();
        }
        RunWithSCM scmRun = (RunWithSCM) run;
        List<ChangeLogSet> sets;
        try {
            sets = scmRun.getChangeSets();
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaRunRecorder: failed to read change sets", e);
            return List.of();
        }
        if (sets == null) {
            return List.of();
        }
        List<BuildChange> changes = new ArrayList<>();
        for (Object rawSet : sets) {
            ChangeLogSet set = (ChangeLogSet) rawSet;
            for (Object rawEntry : set) {
                changes.add(entryToChange((ChangeLogSet.Entry) rawEntry));
            }
        }
        return changes;
    }

    private static BuildChange entryToChange(ChangeLogSet.Entry entry) {
        String commitId = "";
        try {
            String v = entry.getCommitId();
            commitId = v != null ? v : "";
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaRunRecorder: commit id unavailable", e);
        }
        String authorId = "";
        String authorDisplayName = "";
        try {
            User author = entry.getAuthor();
            if (author != null) {
                authorId = author.getId();
                authorDisplayName = author.getDisplayName();
            }
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaRunRecorder: change author unavailable", e);
        }
        String message = "";
        try {
            String m = entry.getMsg();
            message = m != null ? m : "";
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaRunRecorder: change message unavailable", e);
        }
        long timestamp = 0;
        try {
            timestamp = entry.getTimestamp();
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaRunRecorder: change timestamp unavailable", e);
        }
        int affected = 0;
        try {
            affected = entry.getAffectedPaths().size();
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaRunRecorder: affected path count unavailable", e);
        }
        return BuildChange.builder()
                .commitId(truncate(commitId, MAX_FIELD_512))
                .authorId(authorId)
                .authorDisplayName(authorDisplayName)
                .message(truncate(message, MAX_FIELD_512))
                .timestampMillis(timestamp)
                .affectedPathCount(affected)
                .build();
    }

    /**
     * Reads the test counters by reflection (D3): {@code
     * hudson.tasks.test.AbstractTestResultAction} ships in the {@code
     * junit} plugin, which the coordinator does not allocate to C2. Every
     * counter is {@code 0} when no such action is present.
     */
    static int[] readTestCounters(List<? extends Action> actions) {
        for (Action action : actions) {
            if (hasSimpleNameInHierarchy(action.getClass(), "AbstractTestResultAction")) {
                int total = intOf(invokeNoArg(action, "getTotalCount"));
                int failed = intOf(invokeNoArg(action, "getFailCount"));
                int skipped = intOf(invokeNoArg(action, "getSkipCount"));
                int passed = Math.max(0, total - failed - skipped);
                return new int[] {total, passed, failed, skipped};
            }
        }
        return new int[] {0, 0, 0, 0};
    }

    private static int intOf(Object v) {
        return v instanceof Number ? ((Number) v).intValue() : 0;
    }

    private static boolean hasSimpleNameInHierarchy(Class<?> clazz, String simpleName) {
        Class<?> current = clazz;
        while (current != null && current != Object.class) {
            if (current.getSimpleName().equals(simpleName)) {
                return true;
            }
            for (Class<?> iface : current.getInterfaces()) {
                if (hasSimpleNameInHierarchy(iface, simpleName)) {
                    return true;
                }
            }
            current = current.getSuperclass();
        }
        return false;
    }

    private static Object invokeNoArg(Object target, String methodName) {
        try {
            Method method = target.getClass().getMethod(methodName);
            return method.invoke(target);
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaRunRecorder: reflective call " + methodName + " failed", e);
            return null;
        }
    }

    // --- Flows: attaches and detaches the graph listener (D1, D2) ---

    /** Attaches a {@link Graph} listener to a running pipeline, and detaches it at completion. */
    @Extension
    public static class Flows extends FlowExecutionListener {

        private static final Map<FlowExecution, Graph> ATTACHED =
                Collections.synchronizedMap(new WeakHashMap<>());

        @Override
        public void onRunning(FlowExecution execution) {
            try {
                if (!gateEnabled()) {
                    return;
                }
                Run<?, ?> run = runFor(execution);
                if (run == null) {
                    return;
                }
                Graph graph = new Graph(run);
                ATTACHED.put(execution, graph);
                execution.addListener(graph);
            } catch (Exception e) {
                LOGGER.log(Level.WARNING, "VarroaRunRecorder.Flows: failed to attach the graph listener", e);
            }
        }

        @Override
        public void onCompleted(FlowExecution execution) {
            Graph graph = ATTACHED.remove(execution);
            if (graph != null) {
                execution.removeListener(graph);
            }
        }

        private static Run<?, ?> runFor(FlowExecution execution) {
            try {
                FlowExecutionOwner owner = execution.getOwner();
                if (owner == null) {
                    return null;
                }
                Object executable = owner.getExecutable();
                return executable instanceof Run ? (Run<?, ?>) executable : null;
            } catch (IOException e) {
                LOGGER.log(Level.FINE, "VarroaRunRecorder.Flows: could not resolve the owning run", e);
                return null;
            }
        }
    }

    // --- Graph: STAGE_STARTED and STAGE (D2) ---

    /**
     * Emits {@code STAGE_STARTED} when a labelled block opens, and {@code
     * STAGE} when it ends. Scoped to one {@code FlowExecution}; {@link
     * Flows} creates and attaches one instance per running pipeline.
     */
    @SuppressWarnings({"rawtypes", "unchecked"})
    public static class Graph implements GraphListener {

        private final Run<?, ?> run;

        Graph(Run<?, ?> run) {
            this.run = run;
        }

        @Override
        public void onNewHead(FlowNode node) {
            try {
                if (!gateEnabled()) {
                    return;
                }
                String jobPath = safeJobPath(run);
                RunState state = stateFor(jobPath, run.getNumber());
                if (state == null) {
                    return;
                }
                if (node instanceof BlockStartNode && hasLabel(node)) {
                    emitStageStarted(run, state, (BlockStartNode) node);
                } else if (node instanceof BlockEndNode) {
                    BlockStartNode start = ((BlockEndNode) node).getStartNode();
                    if (hasLabel(start)) {
                        emitStageEnded(run, state, start, (BlockEndNode) node);
                    }
                }
            } catch (Exception e) {
                LOGGER.log(Level.WARNING, "VarroaRunRecorder.Graph: failed to process a flow node", e);
            }
        }
    }

    static boolean hasLabel(FlowNode node) {
        return node != null && node.getAction(LabelAction.class) != null;
    }

    static boolean isParallel(FlowNode node) {
        return node.getAction(ThreadNameAction.class) != null;
    }

    private static void emitStageStarted(Run<?, ?> run, RunState state, BlockStartNode start) {
        BuildStage stage = baseStageBuilder(start)
                .status(BuildStage.STATUS_IN_PROGRESS)
                .startedAtMillis(TimingAction.getStartTime(start))
                .endedAtMillis(0)
                .durationMillis(0)
                .pauseDurationMillis(0)
                .error("")
                .build();
        emitStageEvent(run, state, BuildEvent.PHASE_STAGE_STARTED, stage);
    }

    private static void emitStageEnded(Run<?, ?> run, RunState state, BlockStartNode start, BlockEndNode end) {
        long startedAt = TimingAction.getStartTime(start);
        long endedAt = TimingAction.getStartTime(end);
        List<FlowNode> blockNodes = nodesInBlock(start, end);
        String status = deriveActionStatus(start, end, blockNodes);
        String error = errorMessage(start, end);
        BuildStage stage = baseStageBuilder(start)
                .status(status)
                .startedAtMillis(startedAt)
                .endedAtMillis(endedAt)
                .durationMillis(Math.max(0, endedAt - startedAt))
                .pauseDurationMillis(0)
                .error(truncate(error, MAX_STAGE_ERROR))
                .build();
        emitStageEvent(run, state, BuildEvent.PHASE_STAGE, stage);
    }

    private static void emitStageEvent(Run<?, ?> run, RunState state, String phase, BuildStage stage) {
        BuildEvent.Builder eb = BuildEvent.builder()
                .phase(phase)
                .seq(state.nextSeq())
                .emittedAtMillis(System.currentTimeMillis())
                .runId(state.runId)
                .intentKey(state.intentKey)
                .jobPath(state.jobPath)
                .buildNumber(state.buildNumber)
                .stages(List.of(stage));
        applyTemplateFields(eb, run);
        BuildEvent event = eb.build();
        emitCapped(event);
    }

    static BuildStage.Builder baseStageBuilder(BlockStartNode start) {
        boolean parallel = isParallel(start);
        LabelAction label = (LabelAction) start.getAction(LabelAction.class);
        String name = label != null ? truncate(label.getDisplayName(), MAX_FIELD_512) : "";
        String parallelBranchName = "";
        if (parallel) {
            ThreadNameAction thread = (ThreadNameAction) start.getAction(ThreadNameAction.class);
            parallelBranchName = truncate(thread.getThreadName(), MAX_FIELD_512);
        }
        WorkspaceAction workspace = (WorkspaceAction) start.getAction(WorkspaceAction.class);
        String nodeName = workspace != null ? truncate(workspace.getNode(), MAX_FIELD_512) : "";
        String agentLabel = workspace != null ? agentLabelFrom(workspace) : "";
        return BuildStage.builder()
                .stageId(start.getId())
                .parentIds(idsOf(start.getParents()))
                .enclosingId(orEmpty(start.getEnclosingId()))
                .parentId(walkToRecordedParent(start))
                .name(name)
                .nodeType(parallel ? "parallel" : "stage")
                .parallelBranchName(parallelBranchName)
                .nodeName(nodeName)
                .agentLabel(agentLabel);
    }

    private static String agentLabelFrom(WorkspaceAction workspace) {
        Set<LabelAtom> labels = workspace.getLabels();
        if (labels == null || labels.isEmpty()) {
            return "";
        }
        List<String> names = new ArrayList<>();
        for (LabelAtom l : labels) {
            names.add(l.getName());
        }
        Collections.sort(names);
        return truncate(String.join(",", names), MAX_FIELD_512);
    }

    /**
     * Walks {@code node.getEnclosingBlocks()} outward and returns the id
     * of the first block that carries a {@link LabelAction} — the nearest
     * enclosing block the plugin also emits. Returns {@code ""} when the
     * walk finds none.
     */
    static String walkToRecordedParent(FlowNode node) {
        for (BlockStartNode enclosing : node.getEnclosingBlocks()) {
            if (hasLabel(enclosing)) {
                return enclosing.getId();
            }
        }
        return "";
    }

    private static List<String> idsOf(List<FlowNode> nodes) {
        List<String> ids = new ArrayList<>();
        for (FlowNode n : nodes) {
            ids.add(n.getId());
        }
        return ids;
    }

    private static String orEmpty(String s) {
        return s != null ? s : "";
    }

    /**
     * Collects every node from {@code end} back to {@code start}
     * inclusive, following every parent edge. Bounds the walk to the
     * block: it does not follow a parent edge past {@code start}.
     */
    static List<FlowNode> nodesInBlock(FlowNode start, FlowNode end) {
        List<FlowNode> nodes = new ArrayList<>();
        Set<String> visited = new HashSet<>();
        Deque<FlowNode> stack = new ArrayDeque<>();
        stack.push(end);
        while (!stack.isEmpty()) {
            FlowNode node = stack.pop();
            if (!visited.add(node.getId())) {
                continue;
            }
            nodes.add(node);
            if (node.getId().equals(start.getId())) {
                continue;
            }
            for (FlowNode parent : node.getParents()) {
                stack.push(parent);
            }
        }
        return nodes;
    }

    /**
     * Derives a stage status from node actions (D2), checked in order: an
     * interrupted-typed error, any other error, a warning-typed
     * unstable result anywhere in the block, a not-executed start node,
     * else success. This runs at a block end, when the run may still be
     * in progress and a chunk-status call could report an in-progress
     * value for a stage that already ended.
     */
    static String deriveActionStatus(FlowNode start, FlowNode end, List<FlowNode> blockNodes) {
        if (isInterrupted(start) || isInterrupted(end)) {
            return "ABORTED";
        }
        if (hasError(start) || hasError(end)) {
            return "FAILURE";
        }
        for (FlowNode n : blockNodes) {
            WarningAction warning = (WarningAction) n.getAction(WarningAction.class);
            if (warning != null && Result.UNSTABLE.equals(warning.getResult())) {
                return "UNSTABLE";
            }
        }
        if (start.getAction(NotExecutedNodeAction.class) != null) {
            return "NOT_BUILT";
        }
        return "SUCCESS";
    }

    private static boolean hasError(FlowNode node) {
        return node.getAction(ErrorAction.class) != null;
    }

    private static boolean isInterrupted(FlowNode node) {
        ErrorAction error = (ErrorAction) node.getAction(ErrorAction.class);
        if (error == null || error.getError() == null) {
            return false;
        }
        return "FlowInterruptedException".equals(error.getError().getClass().getSimpleName());
    }

    private static String errorMessage(FlowNode start, FlowNode end) {
        ErrorAction error = (ErrorAction) end.getAction(ErrorAction.class);
        if (error == null) {
            error = (ErrorAction) start.getAction(ErrorAction.class);
        }
        return error != null ? error.getDisplayName() : "";
    }

    // --- D2: completion rebuild ---

    /**
     * Rebuilds the whole stage list from the finished {@code
     * FlowExecution}, so a run that lost a live {@code STAGE} event still
     * produces a complete DAG. Refines each stage's status with {@code
     * computeChunkStatus2} using the real surrounding nodes, keeping the
     * action-derived status when the refined value is still in progress.
     * A run that is not a {@code WorkflowRun}, or whose execution is
     * unavailable, carries an empty {@code stages} list.
     */
    @SuppressWarnings({"rawtypes", "unchecked"})
    private static List<BuildStage> rebuildStages(Run<?, ?> run) {
        if (!(run instanceof WorkflowRun)) {
            return List.of();
        }
        WorkflowRun wfRun = (WorkflowRun) run;
        FlowExecution execution = wfRun.getExecution();
        if (execution == null) {
            return List.of();
        }

        Map<String, FlowNode> firstChildOf = new HashMap<>();
        List<FlowNode> all = new ArrayList<>();
        for (FlowNode node : new FlowGraphWalker(execution)) {
            all.add(node);
            for (FlowNode parent : node.getParents()) {
                firstChildOf.putIfAbsent(parent.getId(), node);
            }
        }

        List<BuildStage> stages = new ArrayList<>();
        for (FlowNode node : all) {
            if (!(node instanceof BlockStartNode) || !hasLabel(node)) {
                continue;
            }
            BlockStartNode start = (BlockStartNode) node;
            BlockEndNode end = start.getEndNode();
            if (end == null) {
                // The block never closed; the live STAGE_STARTED event
                // already carried it, and a terminal record is not owed.
                continue;
            }
            List<FlowNode> blockNodes = nodesInBlock(start, end);
            String actionStatus = deriveActionStatus(start, end, blockNodes);
            FlowNode before = start.getParents().isEmpty() ? null : start.getParents().get(0);
            FlowNode after = firstChildOf.get(end.getId());
            String status = refineStatus(wfRun, before, start, end, after, actionStatus);
            String error = errorMessage(start, end);
            long startedAt = TimingAction.getStartTime(start);
            long endedAt = TimingAction.getStartTime(end);
            BuildStage stage = baseStageBuilder(start)
                    .status(status)
                    .startedAtMillis(startedAt)
                    .endedAtMillis(endedAt)
                    .durationMillis(Math.max(0, endedAt - startedAt))
                    .pauseDurationMillis(0)
                    .error(truncate(error, MAX_STAGE_ERROR))
                    .build();
            stages.add(stage);
        }
        return stages;
    }

    private static String refineStatus(WorkflowRun run, FlowNode before, FlowNode first, FlowNode last,
            FlowNode after, String actionStatus) {
        try {
            GenericStatus generic = StatusAndTiming.computeChunkStatus2(run, before, first, last, after);
            return mapGenericStatus(generic, actionStatus);
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaRunRecorder: computeChunkStatus2 failed; keeping the action-derived status", e);
            return actionStatus;
        }
    }

    static String mapGenericStatus(GenericStatus generic, String fallback) {
        if (generic == null) {
            return fallback;
        }
        switch (generic) {
            case SUCCESS:
                return "SUCCESS";
            case UNSTABLE:
                return "UNSTABLE";
            case FAILURE:
                return "FAILURE";
            case ABORTED:
                return "ABORTED";
            case NOT_EXECUTED:
                return "NOT_BUILT";
            default:
                // IN_PROGRESS, PAUSED_PENDING_INPUT, QUEUED: keep the
                // action-derived status (D2) — a stage that ended during
                // a running build never reports NOT_BUILT this way.
                return fallback;
        }
    }

    // --- D11: size caps and the oversize ladder ---

    /**
     * Mirrors the mite loopback's hard body limit (D6, D11, ruling o): 1
     * MiB. Package-private so {@link BuildEventDispatcher} can size its
     * own post cap with headroom under this value.
     */
    static final int LOOPBACK_BODY_LIMIT_BYTES = 1024 * 1024;

    /**
     * Bytes the loopback's {@code /v1/events} body adds around a solo
     * posted event: the JSON array brackets {@code [} and {@code ]}. The
     * oversize-ladder check below must compare against the framed size,
     * not the bare event size, or an event that fits the raw limit can
     * still draw a {@code 413} once posted inside its array (ruling o).
     */
    private static final int JSON_ARRAY_FRAMING_BYTES = 2;

    private static final java.util.concurrent.atomic.AtomicLong OVERSIZED_DROPPED =
            new java.util.concurrent.atomic.AtomicLong();

    /**
     * Applies the D11 hard caps, then the oversize ladder. Returns {@code
     * null} when even the reduced, identity-only event still exceeds the
     * loopback's body limit — an identifier field too large for the
     * ladder to shrink. The caller must not emit a {@code null} result:
     * this method has already logged the drop and counted it (ruling o).
     * Package-private for testing.
     */
    static BuildEvent applyCaps(BuildEvent event) {
        return applyOversizeLadder(applyHardCaps(event));
    }

    private static BuildEvent applyHardCaps(BuildEvent event) {
        BuildEvent.Builder b = event.toBuilder();
        boolean changed = false;
        if (event.getStages().size() > MAX_STAGES) {
            b.stages(keepNewest(event.getStages(), MAX_STAGES));
            changed = true;
        }
        if (event.getChanges().size() > MAX_CHANGES) {
            b.changes(keepNewest(event.getChanges(), MAX_CHANGES));
            changed = true;
        }
        if (event.getParameters().size() > MAX_PARAMETERS) {
            b.parameters(keepNewestParams(event.getParameters(), MAX_PARAMETERS));
            changed = true;
        }
        return changed ? b.build() : event;
    }

    /**
     * While the serialized event stays above 512 KiB, drops {@code
     * stages} entries from the oldest, then {@code changes} entries from
     * the oldest, then {@code parameters} entries, then truncates every
     * remaining stage's error to 512 characters (D11). Because the stage
     * rung fully drains {@code stages} before the error rung runs, the
     * error rung has no effect once the stage list is already empty.
     * When the event is still above 512 KiB, replaces it with a reduced,
     * identity-only event. When even that reduced event is above the
     * loopback's 1 MiB body limit — an identifier field the ladder
     * cannot shrink — drops it instead of returning it (ruling o).
     */
    private static BuildEvent applyOversizeLadder(BuildEvent event) {
        if (serializedSize(event) <= MAX_EVENT_BYTES) {
            return event;
        }
        BuildEvent current = event;
        while (serializedSize(current) > MAX_EVENT_BYTES && !current.getStages().isEmpty()) {
            current = current.toBuilder().stages(dropOldest(current.getStages())).build();
        }
        while (serializedSize(current) > MAX_EVENT_BYTES && !current.getChanges().isEmpty()) {
            current = current.toBuilder().changes(dropOldest(current.getChanges())).build();
        }
        while (serializedSize(current) > MAX_EVENT_BYTES && !current.getParameters().isEmpty()) {
            current = current.toBuilder().parameters(dropOldestParam(current.getParameters())).build();
        }
        if (serializedSize(current) > MAX_EVENT_BYTES && !current.getStages().isEmpty()) {
            current = current.toBuilder().stages(truncateStageErrors(current.getStages())).build();
        }
        if (serializedSize(current) > MAX_EVENT_BYTES) {
            current = reducedEvent(current);
        }
        int reducedSize = serializedSize(current);
        // Compares the framed body a solo post actually sends
        // ("[" + json + "]") against the loopback's limit, not the bare
        // event size: a body that fits the raw limit but not the framed
        // one still draws a 413 that retries forever (ruling o).
        if (reducedSize + JSON_ARRAY_FRAMING_BYTES > LOOPBACK_BODY_LIMIT_BYTES) {
            LOGGER.log(Level.WARNING,
                    "VarroaRunRecorder: dropping an event above the loopback body limit, run={0} size={1}",
                    new Object[] {event.getRunId(), reducedSize});
            OVERSIZED_DROPPED.incrementAndGet();
            return null;
        }
        return current;
    }

    /**
     * Number of events dropped for exceeding the loopback body limit,
     * matching the package-private accessor shape {@link BuildEventSink}
     * and {@link ActivitySink} use for their own dropped counters.
     */
    static long oversizedDroppedCount() {
        return OVERSIZED_DROPPED.get();
    }

    /** Resets the oversized-drop counter. Package-private for testing. */
    static void resetOversizedDroppedForTesting() {
        OVERSIZED_DROPPED.set(0);
    }

    private static <T> List<T> keepNewest(List<T> list, int max) {
        if (list.size() <= max) {
            return list;
        }
        return new ArrayList<>(list.subList(list.size() - max, list.size()));
    }

    private static Map<String, String> keepNewestParams(Map<String, String> params, int max) {
        if (params.size() <= max) {
            return params;
        }
        List<Map.Entry<String, String>> entries = new ArrayList<>(params.entrySet());
        List<Map.Entry<String, String>> kept = entries.subList(entries.size() - max, entries.size());
        Map<String, String> result = new LinkedHashMap<>();
        for (Map.Entry<String, String> e : kept) {
            result.put(e.getKey(), e.getValue());
        }
        return result;
    }

    private static <T> List<T> dropOldest(List<T> list) {
        if (list.isEmpty()) {
            return list;
        }
        return new ArrayList<>(list.subList(1, list.size()));
    }

    private static Map<String, String> dropOldestParam(Map<String, String> params) {
        if (params.isEmpty()) {
            return params;
        }
        Map<String, String> result = new LinkedHashMap<>(params);
        Iterator<String> it = result.keySet().iterator();
        it.next();
        it.remove();
        return result;
    }

    private static List<BuildStage> truncateStageErrors(List<BuildStage> stages) {
        List<BuildStage> result = new ArrayList<>(stages.size());
        for (BuildStage s : stages) {
            if (s.getError().length() > LADDER_ERROR_LIMIT) {
                result.add(s.toBuilder().error(truncate(s.getError(), LADDER_ERROR_LIMIT)).build());
            } else {
                result.add(s);
            }
        }
        return result;
    }

    /**
     * Replaces an event that is still above 512 KiB with the identity
     * fields, the phase, the sequence, the result, and the timings only
     * (D11). This event is always small, so the plugin never drops an
     * event and never posts a body the loopback refuses.
     */
    private static BuildEvent reducedEvent(BuildEvent event) {
        return BuildEvent.builder()
                .phase(event.getPhase())
                .seq(event.getSeq())
                .emittedAtMillis(event.getEmittedAtMillis())
                .runId(event.getRunId())
                .intentKey(event.getIntentKey())
                .jobPath(event.getJobPath())
                .buildNumber(event.getBuildNumber())
                .result(event.getResult())
                .queuedAtMillis(event.getQueuedAtMillis())
                .startedAtMillis(event.getStartedAtMillis())
                .completedAtMillis(event.getCompletedAtMillis())
                .durationMillis(event.getDurationMillis())
                .queueWaitMillis(event.getQueueWaitMillis())
                .estimatedDurationMillis(event.getEstimatedDurationMillis())
                .build();
    }

    private static int serializedSize(BuildEvent event) {
        return event.toJson().toString().getBytes(StandardCharsets.UTF_8).length;
    }

    private static String truncate(String s, int max) {
        if (s == null) {
            return "";
        }
        return s.length() > max ? s.substring(0, max) : s;
    }
}
