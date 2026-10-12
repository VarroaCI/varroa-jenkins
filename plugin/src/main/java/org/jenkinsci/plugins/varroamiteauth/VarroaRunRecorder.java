package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Action;
import hudson.model.Actionable;
import hudson.model.Cause;
import hudson.model.Job;
import hudson.model.Node;
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
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;
import java.util.logging.Level;
import java.util.logging.Logger;

import jenkins.branch.Branch;
import jenkins.model.Jenkins;
import jenkins.scm.RunWithSCM;
import jenkins.scm.api.SCMHead;
import jenkins.scm.api.SCMRevision;
import jenkins.scm.api.SCMRevisionAction;
import jenkins.scm.api.metadata.ObjectMetadataAction;
import jenkins.scm.api.mixin.ChangeRequestSCMHead;
import jenkins.scm.api.mixin.ChangeRequestSCMHead2;

import org.jenkinsci.plugins.workflow.actions.ErrorAction;
import org.jenkinsci.plugins.workflow.actions.LabelAction;
import org.jenkinsci.plugins.workflow.actions.NotExecutedNodeAction;
import org.jenkinsci.plugins.workflow.actions.ThreadNameAction;
import org.jenkinsci.plugins.workflow.actions.TimingAction;
import org.jenkinsci.plugins.workflow.actions.WarningAction;
import org.jenkinsci.plugins.workflow.actions.WorkspaceAction;
import org.jenkinsci.plugins.workflow.flow.FlowDefinition;
import org.jenkinsci.plugins.workflow.cps.nodes.StepAtomNode;
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
import org.jenkinsci.plugins.workflow.multibranch.BranchJobProperty;
import org.jenkinsci.plugins.workflow.pipelinegraphanalysis.GenericStatus;
import org.jenkinsci.plugins.workflow.pipelinegraphanalysis.StatusAndTiming;
import org.jenkinsci.plugins.workflow.steps.StepDescriptor;

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
 * 512 entries (D1). A build another process began and this one resumed
 * carries no {@code onStarted} call, so {@link Flows#onResumed} rebuilds
 * its entry from the run's own persisted action. {@link Runs#onCompleted}
 * removes the entry for every run, pipeline or freestyle, because
 * {@link Flows} never attaches for a freestyle run. Upstream ordering places {@code RunListener.onStarted}
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
    private static final int MAX_LIBRARIES = 50;
    private static final int MAX_POD_TEMPLATES = 50;
    /** The agent node names one run state remembers, so a wide fan-out holds bounded memory. */
    private static final int MAX_ALLOCATED_NODES = 1024;
    /** The number of test cases one event carries. The list is priority ordered, so the cap keeps the head. */
    static final int MAX_TEST_CASES = 500;
    private static final int MAX_STAGE_ERROR = 2048;
    private static final int LADDER_ERROR_LIMIT = 512;
    private static final int MAX_EVENT_BYTES = 512 * 1024;

    // --- The pending-input re-check (D1, D2) ---

    /** The delay of each attempt. The attempts back off, so attempt 1 is the shortest. */
    private static final long[] RECHECK_DELAYS_MILLIS = {250L, 1000L, 4000L};
    /** The attempts one arriving node buys. */
    private static final int MAX_RECHECK_RETRIES = 2;
    /**
     * Runs the deferred re-checks. The reads stay off the flow's own
     * thread, because the input action's {@code getExecutions} read is
     * synchronized and can time out.
     */
    private static final Timer INPUT_TIMER = new Timer("varroa-run-input-recheck", true);

    // --- Per-run state (D1) ---

    private static final int RUN_STATE_CAPACITY = 512;

    private static final Map<String, RunState> RUN_STATE = new LinkedHashMap<>(16, 0.75f, false) {
        @Override
        protected boolean removeEldestEntry(Map.Entry<String, RunState> eldest) {
            return size() > RUN_STATE_CAPACITY;
        }
    };

    private static final RunSpool SPOOL = new RunSpool();

    /** The live-run journal that {@link VarroaBootReconcile} reads at boot. */
    private static final RunJournal JOURNAL = new RunJournal();

    /** Identity and the shared sequence counter for one run. */
    private static final class RunState {
        final String runId;
        final String intentKey;
        final String jobPath;
        final long buildNumber;
        private final AtomicLong seq = new AtomicLong();
        /** Set once the first source probe answered a revision. */
        final AtomicBoolean sourceResolved = new AtomicBoolean();
        /**
         * The pod templates the run allocated, keyed by template name, in
         * allocation order. The graph listener adds an entry while the
         * agent is still connected, because a gone agent answers no
         * template and no image. Guarded by {@code this}.
         */
        private final Map<String, BuildPodTemplate> podTemplates = new LinkedHashMap<>();
        /** The agent node names already read into {@link #podTemplates}. Guarded by {@code this}. */
        private final Set<String> allocatedNodes = new HashSet<>();
        /**
         * Set while a {@code STARTED} event is held for this run (D8a), so
         * a {@link VarroaTemplateValuesAction} attachment or a terminal
         * state with no action can release it. {@code null} once
         * released, or when this run's {@code STARTED} event was never
         * held.
         */
        private volatile Run<?, ?> heldStartedRun;
        /**
         * The run's latest {@code varroa.summary} text. Empty until the
         * pipeline calls that step. The heartbeat timer holds no
         * reference to the run, so it reads this cache and not the run.
         */
        private volatile String summary = "";
        /**
         * The run's pending input set (D1). Empty until a read finds one.
         * The heartbeat timer holds no reference to the run, so it reads
         * this cache and not the run.
         */
        private volatile List<BuildInput> pendingInputs = List.of();
        /** Set while one re-check task is scheduled or runs. */
        final AtomicBoolean inputRecheckArmed = new AtomicBoolean();
        /** The attempts left for the current node arrival. */
        final AtomicInteger inputRecheckRetries = new AtomicInteger();

        RunState(String runId, String intentKey, String jobPath, long buildNumber) {
            this.runId = runId;
            this.intentKey = intentKey;
            this.jobPath = jobPath;
            this.buildNumber = buildNumber;
        }

        /**
         * Answers true when the agent node name needs no read: the name is
         * already read, or the template map is full.
         */
        synchronized boolean hasAllocatedNode(String nodeName) {
            return podTemplates.size() >= MAX_POD_TEMPLATES || allocatedNodes.contains(nodeName);
        }

        /**
         * Records one agent's pod template. Answers true only when the
         * template name is new. The template map holds at most
         * {@link #MAX_POD_TEMPLATES} entries, and the node name set holds
         * at most {@link #MAX_ALLOCATED_NODES} names.
         */
        synchronized boolean recordPodTemplate(String nodeName, BuildPodTemplate template) {
            if (allocatedNodes.size() < MAX_ALLOCATED_NODES) {
                allocatedNodes.add(nodeName);
            }
            if (podTemplates.containsKey(template.getName()) || podTemplates.size() >= MAX_POD_TEMPLATES) {
                return false;
            }
            podTemplates.put(template.getName(), template);
            return true;
        }

        /** Answers a copy of the recorded pod templates, in allocation order. */
        synchronized List<BuildPodTemplate> podTemplates() {
            return new ArrayList<>(podTemplates.values());
        }

        long nextSeq() {
            return seq.getAndIncrement();
        }

        /**
         * Sets the counter to {@code floor} when {@code floor} is above the
         * current value. A resumed build seeds its counter above every value
         * the previous process used, so its events order after that process's
         * events on the record store's per-run watermarks.
         */
        void seedSeq(long floor) {
            seq.accumulateAndGet(floor, Math::max);
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

    /**
     * Caches one run's summary text for its next heartbeat. The heartbeat
     * timer holds no reference to the run, so it reads this cache instead
     * of the run on every tick. A run with no live state keeps the text
     * in its own action alone: the gate is off, the run holds no claim,
     * or the run already ended.
     */
    static void recordSummary(Run<?, ?> run, String cutText) {
        RunState state = stateFor(safeJobPath(run), run.getNumber());
        if (state != null) {
            state.summary = cutText;
        }
    }

    // --- The pending-input re-check (D1, D2, D3) ---

    /**
     * The delay of the attempt the counter names. A counter of
     * {@link #MAX_RECHECK_RETRIES} answers the first, shortest delay, and
     * a counter of zero answers the last.
     */
    private static long delayFor(int retriesRemaining) {
        int index = MAX_RECHECK_RETRIES - Math.max(0, Math.min(retriesRemaining, MAX_RECHECK_RETRIES));
        return RECHECK_DELAYS_MILLIS[index];
    }

    /**
     * Schedules the run's next pending-input re-check, and resets the
     * attempt counter. Every arriving node buys a full set of attempts, so
     * a second parallel input step is never folded into a read that ran
     * before its own execution attached. The counter resets on every call,
     * so a clearing read waits the shortest delay and not the longest.
     *
     * <p>At most one task runs per run: the flag is the run's single
     * scheduled attempt.
     */
    private static void scheduleInputRecheck(Run<?, ?> run, RunState state) {
        state.inputRecheckRetries.set(MAX_RECHECK_RETRIES);
        if (!state.inputRecheckArmed.compareAndSet(false, true)) {
            return;
        }
        armNext(run, state);
    }

    /**
     * Arms one timer task. The task takes its own attempt off the counter
     * and clears the armed flag in a {@code finally} block, before it tests
     * the attempts left, so a node that arrives during the read arms the
     * next task first and this task adds no second one. The counter moves
     * before the flag clears, so a node that arrives between the two keeps
     * the full set of attempts it bought.
     */
    private static void armNext(Run<?, ?> run, RunState state) {
        INPUT_TIMER.schedule(new TimerTask() {
            @Override
            public void run() {
                int remaining;
                try {
                    recheckInputsNow(run, state);
                } finally {
                    remaining = state.inputRecheckRetries.getAndUpdate(v -> v > 0 ? v - 1 : 0);
                    state.inputRecheckArmed.set(false);
                }
                if (remaining > 0 && state.inputRecheckArmed.compareAndSet(false, true)) {
                    armNext(run, state);
                }
            }
        }, delayFor(state.inputRecheckRetries.get()));
    }

    /**
     * Re-reads the run's pending inputs, stores the result, and emits one
     * {@code run.input} event when the set changed.
     *
     * <p>A task can outlive its run. Such a task emits nothing: its
     * sequence number would sit above the completion event's, and the
     * recorder would restore a set the run already cleared. This method
     * reads nothing and throws nothing for such a run.
     */
    private static void recheckInputsNow(Run<?, ?> run, RunState state) {
        try {
            if (stateFor(state.jobPath, state.buildNumber) != state || !run.isBuilding()) {
                return;
            }
            // The timer task and a route's request thread both reach this
            // read. The read, the compare, and the write hold one lock, so
            // the last read wins and a stale read never emits a settled
            // input again with a higher sequence number.
            synchronized (state) {
                List<BuildInput> found = VarroaRunInputs.readPendingInputs(run);
                if (found.equals(state.pendingInputs)) {
                    return;
                }
                state.pendingInputs = found;
                emitRunInput(run, state, found);
            }
        } catch (RuntimeException e) {
            LOGGER.log(Level.WARNING, "VarroaRunRecorder: failed to re-check the pending inputs", e);
        }
    }

    /**
     * Re-reads the run's pending inputs for a caller outside this class,
     * in the shape {@link #recordSummary} uses. The two plugin routes call
     * it after the input step settled, so the record loses the input
     * without waiting for a node or a heartbeat. A no-op for a run with no
     * live state.
     */
    static void recheckInputs(Run<?, ?> run) {
        RunState state = stateFor(safeJobPath(run), run.getNumber());
        if (state != null) {
            recheckInputsNow(run, state);
        }
    }

    /**
     * Reports whether the arriving node is the {@code input} step's own
     * atom node. The read covers the node alone and walks no graph. The
     * plugin needs no class of the input-step plugin for this test: the
     * descriptor's function name alone names the step.
     */
    private static boolean isInputNode(FlowNode node) {
        if (!(node instanceof StepAtomNode)) {
            return false;
        }
        StepDescriptor descriptor = ((StepAtomNode) node).getDescriptor();
        return descriptor != null && "input".equals(descriptor.getFunctionName());
    }

    /**
     * Emits one {@code run.input} event carrying the run's whole pending
     * set. The event carries the run identity, the ordering fields, the
     * four template fields, and the set alone: it reads no changeset and
     * no test counter, so it calls no metadata helper.
     */
    private static void emitRunInput(Run<?, ?> run, RunState state, List<BuildInput> inputs) {
        BuildEvent.Builder b = BuildEvent.builder()
                .phase(BuildEvent.PHASE_RUN_INPUT)
                .seq(state.nextSeq())
                .emittedAtMillis(System.currentTimeMillis())
                .runId(state.runId)
                .intentKey(state.intentKey)
                .jobPath(state.jobPath)
                .buildNumber(state.buildNumber)
                .inputs(inputs);
        applyTemplateFields(b, run);
        emitCapped(b.build());
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

    static boolean gateEnabled() {
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

    /**
     * Offers the event to the sink, and spools it when the sink is full.
     * Returns false when the event reached neither the sink nor the spool.
     */
    private static boolean emit(BuildEvent event) {
        BuildEventSink sink = BuildEventSink.get();
        boolean accepted = sink.offer(event);
        if (!accepted && event.isHeartbeat()) {
            return false;
        }
        if (!accepted) {
            // The sink is full. Move every event of this same run still
            // waiting in the sink into the spool first, in the order the
            // sink held them, before spooling this one. Without this
            // step an older, unacknowledged sibling could stay stranded
            // in the sink while the backlog replay posts this (newer)
            // event first, reversing per-run order (D11).
            for (BuildEvent queued : sink.drainRun(event.getRunId())) {
                SPOOL.append(queued.getRunId(), queued);
            }
            boolean spooled = SPOOL.append(event.getRunId(), event);
            BuildEventDispatcher dispatcher = VarroaBuildEventLifecycle.current();
            if (dispatcher != null) {
                dispatcher.noteSpooled(event.getRunId());
            }
            return spooled;
        }
        return true;
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
        for (RunState state : snapshot) {
            emitHeartbeat(state);
            // The modification time records the last moment the run was
            // live here, so a boot reconcile can date the run's end.
            JOURNAL.touch(state.runId);
        }
    }

    /**
     * Emits one {@code HEARTBEAT} event for one run. The event carries the
     * run's cached pending input set on every tick, including an empty one:
     * the heartbeat is the reconciliation path for a lost {@code run.input}
     * event, and a lost event can be a clearing one. The event reads no run
     * to build that set.
     */
    private static void emitHeartbeat(RunState state) {
        BuildEvent.Builder b = BuildEvent.builder()
                .phase(BuildEvent.PHASE_HEARTBEAT)
                .seq(state.nextSeq())
                .emittedAtMillis(System.currentTimeMillis())
                .runId(state.runId)
                .intentKey(state.intentKey)
                .jobPath(state.jobPath)
                .buildNumber(state.buildNumber);
        String summary = state.summary;
        if (!summary.isEmpty()) {
            b.summary(summary);
        }
        b.inputs(state.pendingInputs);
        emitCapped(b.build());
    }

    /**
     * Rebuilds the per-run state of a build this process loaded from
     * disk, and reports whether this call rebuilt it.
     *
     * <p>A resumed build never fires {@code RunListener.onStarted}: that
     * call belongs to the process that began the build, and the pipeline
     * mesh moves a running build to another one. Without this rebuild
     * the run has no heartbeat, no stage events, and no sequence
     * counter, so the recorder sees nothing of it after the resume and
     * interrupts it at the resume deadline.
     *
     * <p>Every field comes from the run's own persisted action, which
     * travels with the build record. The run identity is therefore the
     * one the recorder dispatched, never a new one.
     *
     * <p>The first heartbeat goes out here, not one interval later. The
     * recorder clears a run's resume only on a heartbeat from the
     * instance it named, received after it asked, and it gives up six
     * minutes after that ask.
     *
     * <p>It emits no {@code STARTED} event. The run started once, in the
     * process that is gone, and a second start would rewrite the run's
     * own beginning.
     */
    static boolean attachResumedRun(Run<?, ?> run) {
        if (!gateEnabled()) {
            return false;
        }
        VarroaIntentAction action = run.getAction(VarroaIntentAction.class);
        if (action == null || action.getRunId().isEmpty()) {
            return false;
        }
        if (!run.isBuilding()) {
            // A build that ended needs no heartbeat and no listener. Its
            // completion already went out, from whichever process ran
            // it, and an entry here would heartbeat a finished run.
            return false;
        }
        String jobPath = safeJobPath(run);
        long buildNumber = run.getNumber();
        if (stateFor(jobPath, buildNumber) != null) {
            return false;
        }
        RunState state = registerRun(jobPath, buildNumber, resolveRunId(action), action.getIntentKey());
        // The wall clock holds more than every count the previous process
        // reached, and a later resume seeds it higher again, so the record
        // store accepts the events of this process.
        state.seedSeq(System.currentTimeMillis());
        // The new process holds no summary of its own. The run's own
        // action carries the text, so the first heartbeat reports it
        // before the pipeline calls varroa.summary again.
        VarroaSummaryAction summaryAction = run.getAction(VarroaSummaryAction.class);
        if (summaryAction != null) {
            state.summary = summaryAction.getSummary();
        }
        // The new process holds no pending input set of its own, so it
        // reads the run once. A build that resumes while it waits on an
        // input therefore reports that input from this process at once.
        recheckInputsNow(run, state);
        ensureHeartbeatStarted();
        emitHeartbeat(state);
        LOGGER.log(Level.INFO, () -> "VarroaRunRecorder: attached to the resumed build "
                + jobPath + "#" + buildNumber + " of run " + state.runId);
        return true;
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
                journalStart(run, action, runId, jobPath, buildNumber);

                if (VarroaTemplateDefinitions.isTemplateDefined(run.getParent())) {
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
            RunState existing = stateFor(jobPath, buildNumber);
            String pusherRunId = existing != null ? existing.runId : "";
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
                if (state != null) {
                    emitUnresolvedPodTemplates(run, state);
                }
                boolean endedByRestart = FailedProgramLoad.endedByRestart(run);
                if (endedByRestart) {
                    // The build did no work in this process. Its completion
                    // is the boot reconcile's event, from whichever caller
                    // claims it first.
                    RunJournal.Entry entry = journalEntryOf(action, runId);
                    List<String> claimIds = claimIdsOf(run, entry != null ? entry.runId : runId);
                    if (!FailedProgramLoad.claim(claimIds)) {
                        finishLogStorage(runId);
                        LOGGER.log(Level.FINE, () -> "VarroaRunRecorder.Runs: the boot reconcile already ended "
                                + jobPath + "#" + buildNumber);
                        return;
                    }
                    if (entry != null) {
                        finishLogStorage(runId);
                        BuildEvent event = bootCompletedEvent(entry, run, System.currentTimeMillis());
                        if (event == null || !emit(event)) {
                            // The size caps dropped the event, or the spool
                            // refused it, so this process holds nothing to
                            // send. The boot reconcile can still send its own.
                            FailedProgramLoad.release(claimIds);
                        }
                        return;
                    }
                }
                if (!endedByRestart && FailedProgramLoad.claimed(claimIdsOf(run, runId))) {
                    // A restart ended this build in this boot, and its
                    // completion already went out. Jenkins calls finish
                    // again when a user unpauses the refused program.
                    finishLogStorage(runId);
                    LOGGER.log(Level.FINE, () -> "VarroaRunRecorder.Runs: a restart already ended "
                            + jobPath + "#" + buildNumber);
                    return;
                }
                // A completion with no live state still orders after every
                // earlier event of the run, so it stamps the wall clock and
                // not a fresh counter.
                long seq = state != null ? state.nextSeq() : System.currentTimeMillis();

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
                // The test report archive runs before the collector, so the
                // collected list names the report with its own blob key (D2).
                List<VarroaTestReport.Case> testCases = null;
                long reportSize = -1L;
                try {
                    Object testResult = VarroaTestReport.findResult(run);
                    if (VarroaTestReport.hasSuites(testResult)) {
                        testCases = VarroaTestReport.readCases(testResult);
                        reportSize = VarroaTestReport.archiveReport(run, listener, testCases);
                    }
                } catch (Exception e) {
                    LOGGER.log(Level.WARNING, "VarroaRunRecorder.Runs: failed to archive the test report", e);
                }
                // Artifact archiving is complete at this call site (D6), so
                // the collector sees every entry the run will ever report.
                List<BuildArtifact> artifacts = ArtifactCollector.collect(run);
                b.artifacts(artifacts);
                if (testCases != null) {
                    b.testCases(VarroaTestReport.boundedCases(testCases));
                    if (reportSize >= 0) {
                        b.testReport(VarroaTestReport.reportPointer(artifacts, reportSize, testCases.size()));
                    }
                }
                LogSummary logSummary = finishLogStorage(runId);
                b.logSummary(endedByRestart ? restartLogSummary() : logSummary);
                applyTemplateFields(b, run);
                // The completion reads the run's own action and never the
                // cached value, so a run that lost its heartbeat still
                // reports its summary.
                VarroaSummaryAction summaryAction = run.getAction(VarroaSummaryAction.class);
                if (summaryAction != null && !summaryAction.getSummary().isEmpty()) {
                    b.summary(summaryAction.getSummary());
                }
                emitCapped(b.build());
            } catch (Exception e) {
                LOGGER.log(Level.WARNING, "VarroaRunRecorder.Runs: failed to emit COMPLETED", e);
            } finally {
                RunStatePusher.remove(pusherRunId);
                removeState(jobPath, buildNumber);
            }
        }

        @Override
        public void onDeleted(Run<?, ?> run) {
            try {
                journalRemove(run, "");
            } catch (Exception e) {
                LOGGER.log(Level.FINE, "VarroaRunRecorder.Runs: failed to clear the journal entry of a deleted run", e);
            }
        }
    }

    /**
     * Writes the run's journal entry. For a run that is not a pipeline it
     * also saves {@code build.xml}: Jenkins core writes that file only
     * when the build completes, and a build without it does not load
     * after a Jenkins stop. With the file, the build loads as completed
     * with the result {@code ABORTED}. A pipeline run saves the file
     * itself.
     */
    private static void journalStart(Run<?, ?> run, VarroaIntentAction action, String runId, String jobPath,
            long buildNumber) {
        boolean pipeline = run instanceof WorkflowRun;
        JOURNAL.write(new RunJournal.Entry(runId, action.getIntentKey(), jobPath, buildNumber,
                action.getDeliveryId(), run.getStartTimeInMillis(), pipeline));
        if (!pipeline) {
            try {
                run.save();
            } catch (IOException e) {
                LOGGER.log(Level.WARNING, "VarroaRunRecorder: failed to save the build record of "
                        + jobPath + "#" + buildNumber, e);
            }
        }
    }

    /**
     * Deletes the run's journal entry under every run id the run can
     * carry: the live state's id, the claim registry's id, and the
     * action's own id. The entry was written under one of them.
     *
     * <p>A completion does not call this. {@link RunSpool#append} deletes
     * the entry once the run's {@code COMPLETED} event is on disk, so a
     * stop while the event waits in the sink leaves the entry for the
     * boot reconcile.
     */
    private static void journalRemove(Run<?, ?> run, String stateRunId) {
        JOURNAL.remove(stateRunId);
        VarroaIntentAction action = run.getAction(VarroaIntentAction.class);
        if (action != null && !action.getRunId().isEmpty()) {
            JOURNAL.remove(action.getRunId());
            JOURNAL.remove(resolveRunId(action));
        }
    }

    /**
     * Builds the {@code COMPLETED} event for a run that a Jenkins stop
     * ended without a {@code RunListener.onCompleted} call. Returns
     * {@code null} when the size caps drop the event.
     *
     * <p>{@code run} is the loaded build, or {@code null} when the build
     * does not load. A loaded build reports its loaded result and its own
     * metadata. A missing build reports {@code ABORTED} and the journal's
     * identity alone. A build that recorded no duration, or that a restart
     * ended before it did work here, ends at the entry's last-seen time,
     * never before its start. The run listener also sends this event for
     * such a build, when it claims the completion first.
     *
     * <p>The log summary always reports {@code incomplete} with the reason
     * {@link LogSummary#REASON_JENKINS_RESTART}: the process that counted
     * the log chunks is gone. Without a summary the gateway would report
     * the log complete. A build that recorded a duration also gets the
     * summary, because a cap drop it saw died with the process, and a
     * short log must never read as complete.
     */
    static BuildEvent bootCompletedEvent(RunJournal.Entry entry, Run<?, ?> run, long nowMillis) {
        BuildEvent.Builder b = BuildEvent.builder()
                .phase(BuildEvent.PHASE_COMPLETED)
                .seq(nowMillis)
                .emittedAtMillis(nowMillis)
                .runId(entry.runId)
                .intentKey(entry.intentKey)
                .jobPath(entry.jobPath)
                .buildNumber(entry.buildNumber)
                .deliveryId(entry.deliveryId);
        long startedAt = entry.startedAtMillis;
        boolean recordedDuration = false;
        if (run != null) {
            applyCommonMetadata(b, run, true);
            b.stages(rebuildStages(run));
            applyTemplateFields(b, run);
            VarroaSummaryAction summaryAction = run.getAction(VarroaSummaryAction.class);
            if (summaryAction != null && !summaryAction.getSummary().isEmpty()) {
                b.summary(summaryAction.getSummary());
            }
            startedAt = run.getStartTimeInMillis();
            // finish() sets the duration of a build that a restart ended,
            // but that build did no work in this process.
            recordedDuration = run.getDuration() > 0 && !FailedProgramLoad.endedByRestart(run);
            if (run.getResult() == null) {
                b.result(Result.ABORTED.toString());
            }
        } else {
            b.result(Result.ABORTED.toString());
            b.startedAtMillis(startedAt);
        }
        if (!recordedDuration) {
            long endedAt = Math.max(startedAt, entry.lastSeenMillis);
            b.durationMillis(endedAt - startedAt);
            b.completedAtMillis(endedAt);
        }
        b.logSummary(restartLogSummary());
        return applyCaps(b.build());
    }

    private static LogSummary restartLogSummary() {
        return LogSummary.builder()
                .incomplete(true)
                .incompleteReason(LogSummary.REASON_JENKINS_RESTART)
                .build();
    }

    /**
     * Returns the journal entry of a run, read under the run ids the entry
     * can carry, or {@code null} when none is present.
     */
    private static RunJournal.Entry journalEntryOf(VarroaIntentAction action, String runId) {
        RunJournal.Entry entry = JOURNAL.read(runId);
        if (entry == null && !runId.equals(action.getRunId())) {
            entry = JOURNAL.read(action.getRunId());
        }
        return entry;
    }

    /**
     * Returns every run id a run can carry, plus {@code extraRunId}: the
     * ids under which the boot reconcile and the run listener claim the
     * completion of a build that a restart ended.
     */
    static List<String> claimIdsOf(Run<?, ?> run, String extraRunId) {
        List<String> ids = new ArrayList<>();
        ids.add(extraRunId);
        VarroaIntentAction action = run.getAction(VarroaIntentAction.class);
        if (action != null && !action.getRunId().isEmpty()) {
            ids.add(action.getRunId());
            ids.add(resolveRunId(action));
        }
        return ids;
    }

    static String resolveRunId(VarroaIntentAction action) {
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
     * Reads the source run identifier of a replayed build from {@code run}'s
     * causes: the first {@link VarroaReplayCause}'s value, or the empty
     * string when the run carries none. The actor read over the same causes
     * is independent of this read.
     */
    static String replaySourceOf(Run<?, ?> run) {
        for (Cause cause : run.getCauses()) {
            if (cause instanceof VarroaReplayCause) {
                return ((VarroaReplayCause) cause).getSourceRunId();
            }
        }
        return "";
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
                .scmRevision(truncate(action.getRevision(), MAX_FIELD_512))
                .replayOf(replaySourceOf(run))
                .grantedBuildNumber(action.getGrantedNumber());
        applyCommonMetadata(b, run, false);
        b.changeRequest(resolveChangeRequest(run));
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

        // Every SCM fact comes from a reflective read of the run's own
        // actions. A missing plugin, action, or method leaves its value
        // empty. This call site writes no scm_revision: the claim and the
        // changelog fallback keep their own writers.
        SourceFacts source = resolveBranchFacts(run);
        b.scmHead(source.scmHead);
        b.branch(source.branch);
        b.scmUrl(source.scmUrl);

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

    static boolean hasSimpleNameInHierarchy(Class<?> clazz, String simpleName) {
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

    static Object invokeNoArg(Object target, String methodName) {
        try {
            Method method = target.getClass().getMethod(methodName);
            return method.invoke(target);
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaRunRecorder: reflective call " + methodName + " failed", e);
            return null;
        }
    }

    // --- The source resolver (D2) ---

    /**
     * The source facts one run answers. Every string stays empty and both
     * lists stay empty when no read resolved them.
     */
    static final class SourceFacts {
        final String branch;
        final String scmHead;
        final String scmRevision;
        final String scmUrl;
        final String definitionPath;
        final List<BuildLibrary> libraries;
        final List<BuildPodTemplate> podTemplates;

        SourceFacts(String branch, String scmHead, String scmRevision, String scmUrl,
                String definitionPath, List<BuildLibrary> libraries, List<BuildPodTemplate> podTemplates) {
            this.branch = branch;
            this.scmHead = scmHead;
            this.scmRevision = scmRevision;
            this.scmUrl = scmUrl;
            this.definitionPath = definitionPath;
            this.libraries = libraries;
            this.podTemplates = podTemplates;
        }
    }

    /** The exact class name of the git plugin's per-run build record. */
    private static final String BUILD_DATA_CLASS_NAME = "hudson.plugins.git.util.BuildData";

    /**
     * Resolves every source fact of a run. Each field takes the first
     * non-empty answer of its own chain. The pod templates are the run
     * state's recorded set. With {@code walk} true, a flow graph walk
     * first adds every allocation the listener did not see.
     */
    static SourceFacts resolveSourceFacts(Run<?, ?> run, RunState state, boolean walk) {
        SourceFacts scmApi = resolveScmApiFacts(run);
        SourceFacts git = resolveGitFacts(run);
        String revision = firstNonEmpty(scmApi.scmRevision, git.scmRevision);
        if (revision.isEmpty()) {
            // The probe short-circuits here: a run with no revision yet
            // pays no graph walk.
            return new SourceFacts("", "", "", "", "", List.of(), List.of());
        }
        String branch = branchFrom(run, scmApi, git);
        return new SourceFacts(
                branch,
                branch,
                revision,
                git.scmUrl,
                resolveDefinitionPath(run),
                resolveLibraries(run),
                resolvePodTemplates(run, state, walk));
    }

    /**
     * Resolves the branch and the SCM head of a run: the revision
     * action's head name, else the job's own branch name, else the git
     * plugin's branch name.
     */
    private static String branchFrom(Run<?, ?> run, SourceFacts scmApi, SourceFacts git) {
        String jobBranch = truncate(IntentKey.branchNameFor(run.getParent()), MAX_FIELD_512);
        return firstNonEmpty(scmApi.scmHead, jobBranch, git.scmHead);
    }

    /**
     * Resolves the branch, the SCM head, and the SCM URL the run
     * metadata carries. The revision stays out of this shape.
     */
    private static SourceFacts resolveBranchFacts(Run<?, ?> run) {
        SourceFacts scmApi = resolveScmApiFacts(run);
        SourceFacts git = resolveGitFacts(run);
        String branch = branchFrom(run, scmApi, git);
        return new SourceFacts(branch, branch, "", git.scmUrl, "", List.of(), List.of());
    }

    /**
     * Reads the change request a multibranch run builds from the branch
     * job property. Answers null when the job is no branch job, or when
     * the branch head is no change request. Every read is guarded, so a
     * failed read leaves the value null.
     */
    static BuildEvent.ChangeRequestInfo resolveChangeRequest(Run<?, ?> run) {
        try {
            BranchJobProperty property = run.getParent().getProperty(BranchJobProperty.class);
            return property != null ? changeRequestOf(property.getBranch()) : null;
        } catch (RuntimeException | LinkageError e) {
            LOGGER.log(Level.FINE, "VarroaRunRecorder: the change request read failed", e);
            return null;
        }
    }

    /**
     * Maps the head of a branch onto a change request. Answers null for a
     * null branch or a branch whose head is no change request.
     */
    static BuildEvent.ChangeRequestInfo changeRequestOf(Branch branch) {
        if (branch == null) {
            return null;
        }
        try {
            SCMHead head = branch.getHead();
            if (!(head instanceof ChangeRequestSCMHead)) {
                return null;
            }
            ChangeRequestSCMHead request = (ChangeRequestSCMHead) head;
            String target = request.getTarget() != null ? orEmpty(request.getTarget().getName()) : "";
            String source = head instanceof ChangeRequestSCMHead2
                    ? orEmpty(((ChangeRequestSCMHead2) head).getOriginName())
                    : "";
            String title = "";
            String url = "";
            ObjectMetadataAction metadata = branch.getAction(ObjectMetadataAction.class);
            if (metadata != null) {
                title = orEmpty(metadata.getObjectDisplayName());
                url = orEmpty(metadata.getObjectUrl());
            }
            return new BuildEvent.ChangeRequestInfo(
                    truncate(orEmpty(request.getId()), MAX_FIELD_512),
                    truncate(title, MAX_FIELD_512),
                    truncate(source, MAX_FIELD_512),
                    truncate(target, MAX_FIELD_512),
                    truncate(url, MAX_FIELD_512));
        } catch (RuntimeException | LinkageError e) {
            LOGGER.log(Level.FINE, "VarroaRunRecorder: the change request read failed", e);
            return null;
        }
    }

    /**
     * Reads the SCM facts from the run's revision action. An absent
     * action leaves every value empty.
     */
    private static SourceFacts resolveScmApiFacts(Run<?, ?> run) {
        String head = "";
        String revision = "";
        SCMRevision scmRevision = revisionFromAction(run);
        if (scmRevision != null) {
            SCMHead scmHead = scmRevision.getHead();
            head = scmHead != null ? orEmpty(scmHead.getName()) : "";
            revision = stringOf(invokeNoArg(scmRevision, "getHash"));
        }
        return new SourceFacts(head, head, revision, "", "", List.of(), List.of());
    }

    /**
     * Reads the run's revision through scm-api's revision action. The
     * call goes through reflection, because scm-api restricts its static
     * entry point to its own plugin.
     */
    private static SCMRevision revisionFromAction(Run<?, ?> run) {
        try {
            Method method = SCMRevisionAction.class.getMethod("getRevision", Actionable.class);
            return (SCMRevision) method.invoke(null, run);
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaRunRecorder: the revision action read failed", e);
            return null;
        }
    }

    /**
     * Reads the git facts from the run's build record of the git plugin.
     * The lookup matches the class name exactly, because that name is the
     * plugin's identity. An absent action leaves every value empty.
     */
    private static SourceFacts resolveGitFacts(Run<?, ?> run) {
        String branch = "";
        String revision = "";
        String url = "";
        Object buildData = findActionByClassName(run, BUILD_DATA_CLASS_NAME);
        if (buildData != null) {
            Object builtRevision = invokeNoArg(buildData, "getLastBuiltRevision");
            revision = stringOf(invokeNoArg(builtRevision, "getSha1String"));
            branch = firstBranchName(invokeNoArg(builtRevision, "getBranches"));
            url = firstStringOf(invokeNoArg(buildData, "getRemoteUrls"));
        }
        return new SourceFacts(branch, branch, revision, url, "", List.of(), List.of());
    }

    /**
     * Finds the first action of a run whose class name matches exactly.
     * {@link VarroaRunInputs} reads the input step's action through this
     * method, because the plugin holds no compile dependency on that
     * plugin.
     */
    static Object findActionByClassName(Run<?, ?> run, String className) {
        for (Action action : run.getAllActions()) {
            if (action.getClass().getName().equals(className)) {
                return action;
            }
        }
        return null;
    }

    private static String firstBranchName(Object branches) {
        if (!(branches instanceof Iterable)) {
            return "";
        }
        for (Object branch : (Iterable<?>) branches) {
            String name = stringOf(invokeNoArg(branch, "getName"));
            if (!name.isEmpty()) {
                return name;
            }
        }
        return "";
    }

    private static String firstStringOf(Object values) {
        if (!(values instanceof Iterable)) {
            return "";
        }
        for (Object value : (Iterable<?>) values) {
            String text = stringOf(value);
            if (!text.isEmpty()) {
                return text;
            }
        }
        return "";
    }

    private static String stringOf(Object value) {
        return value instanceof String ? (String) value : "";
    }

    private static String firstNonEmpty(String... values) {
        for (String value : values) {
            if (value != null && !value.isEmpty()) {
                return value;
            }
        }
        return "";
    }

    /**
     * Reads the script path the job's own definition declares. An inline
     * definition holds its script, and a template definition lives in
     * another repository, so both answer no path.
     */
    private static String resolveDefinitionPath(Run<?, ?> run) {
        if (!(run.getParent() instanceof WorkflowJob)) {
            return "";
        }
        FlowDefinition definition = ((WorkflowJob) run.getParent()).getDefinition();
        if (definition == null) {
            return "";
        }
        return truncate(stringOf(invokeNoArg(definition, "getScriptPath")), MAX_FIELD_512);
    }

    /**
     * Reads the global libraries the definition loaded. An absent
     * libraries action leaves the list empty. The event caps bound this
     * list.
     */
    private static List<BuildLibrary> resolveLibraries(Run<?, ?> run) {
        // LibraryRecord declares getName() and getVersion(), and no
        // getRevision(), on the pinned pipeline-groovy-lib. The revision
        // read therefore answers empty. The proto field stays for a
        // plugin that adds the method.
        Object librariesAction = null;
        for (Action action : run.getAllActions()) {
            if (hasSimpleNameInHierarchy(action.getClass(), "LibrariesAction")) {
                librariesAction = action;
                break;
            }
        }
        if (librariesAction == null) {
            return List.of();
        }
        Object records = invokeNoArg(librariesAction, "getLibraries");
        if (!(records instanceof Iterable)) {
            return List.of();
        }
        List<BuildLibrary> libraries = new ArrayList<>();
        for (Object record : (Iterable<?>) records) {
            libraries.add(BuildLibrary.builder()
                    .name(truncate(stringOf(invokeNoArg(record, "getName")), MAX_FIELD_512))
                    .version(truncate(stringOf(invokeNoArg(record, "getVersion")), MAX_FIELD_512))
                    .revision(truncate(stringOf(invokeNoArg(record, "getRevision")), MAX_FIELD_512))
                    .build());
        }
        return libraries;
    }

    /**
     * Answers the run state's recorded pod templates, in allocation
     * order. With {@code walk} true, it first walks the flow graph and
     * records every allocation the graph listener did not see, for
     * example the allocations of a run that resumed after a restart.
     */
    private static List<BuildPodTemplate> resolvePodTemplates(Run<?, ?> run, RunState state, boolean walk) {
        if (!(run instanceof WorkflowRun)) {
            return List.of();
        }
        FlowExecution execution = ((WorkflowRun) run).getExecution();
        if (execution == null) {
            return List.of();
        }
        if (walk) {
            for (FlowNode node : new FlowGraphWalker(execution)) {
                WorkspaceAction workspace = node.getAction(WorkspaceAction.class);
                if (workspace != null) {
                    recordAllocation(state, workspace);
                }
            }
        }
        return state.podTemplates();
    }

    /**
     * Records the pod template of the agent that a workspace action
     * names. Answers true only when the run state holds no entry of that
     * template yet. The read happens while the agent is connected, so a
     * later disconnect loses no entry.
     */
    private static boolean recordAllocation(RunState state, WorkspaceAction workspace) {
        String nodeName = orEmpty(workspace.getNode());
        if (nodeName.isEmpty() || state.hasAllocatedNode(nodeName)) {
            return false;
        }
        return state.recordPodTemplate(nodeName,
                podTemplateFor(nodeName, agentLabelFrom(workspace), Jenkins.get().getNode(nodeName)));
    }

    /**
     * Builds the pod template entry of one agent. A kubernetes agent
     * answers its template's name and image. Another agent, or an agent
     * that is gone, answers its node name and no image.
     */
    static BuildPodTemplate podTemplateFor(String nodeName, String label, Node node) {
        Object template = kubernetesTemplateOf(node);
        String name = template != null ? stringOf(invokeNoArg(template, "getName")) : "";
        if (name.isEmpty()) {
            name = nodeName;
        }
        return BuildPodTemplate.builder()
                .name(truncate(name, MAX_FIELD_512))
                .label(label)
                .image(templateImage(template))
                .build();
    }

    /**
     * Reads the container image of the node that ran the build. A node
     * that is no kubernetes agent answers no image.
     */
    static String resolvePodTemplateImage(Node node) {
        return templateImage(kubernetesTemplateOf(node));
    }

    /**
     * Reads the pod template of a kubernetes agent. Any other node, and a
     * kubernetes agent whose template is gone, answers null.
     */
    private static Object kubernetesTemplateOf(Node node) {
        // KubernetesSlave declares getTemplateOrNull() and getTemplate().
        // The second throws when the template is gone, so the null-first
        // read happens first, and the throwing call is the fallback.
        if (node == null || !hasSimpleNameInHierarchy(node.getClass(), "KubernetesSlave")) {
            return null;
        }
        Object template = invokeNoArg(node, "getTemplateOrNull");
        if (template == null) {
            template = invokeNoArg(node, "getTemplate");
        }
        return template;
    }

    /** Reads the first container image a pod template declares. */
    private static String templateImage(Object template) {
        // PodTemplate declares getImage() and getContainers(), and
        // ContainerTemplate declares getImage().
        if (template == null) {
            return "";
        }
        String image = stringOf(invokeNoArg(template, "getImage"));
        if (!image.isEmpty()) {
            return image;
        }
        Object containers = invokeNoArg(template, "getContainers");
        if (!(containers instanceof Iterable)) {
            return "";
        }
        for (Object container : (Iterable<?>) containers) {
            String containerImage = stringOf(invokeNoArg(container, "getImage"));
            if (!containerImage.isEmpty()) {
                return containerImage;
            }
        }
        return "";
    }

    /**
     * Resolves the facts E1 carries: the definition path and the
     * libraries. E1 reads no SCM fact and walks no flow graph.
     */
    private static SourceFacts resolveDefinitionFacts(Run<?, ?> run) {
        return new SourceFacts("", "", "", "", resolveDefinitionPath(run), resolveLibraries(run), List.of());
    }

    /**
     * Emits the pod templates of a run that never answered a revision.
     * The graph listener records each template while its agent is
     * connected, but the revision-gated emissions never fire for such a
     * run, so this one carries the recorded set at completion. A run with
     * a revision, or with no recorded template, emits nothing here.
     */
    private static void emitUnresolvedPodTemplates(Run<?, ?> run, RunState state) {
        if (state.sourceResolved.get()) {
            return;
        }
        try {
            // The recorded set is complete when this process saw the run;
            // a state rebuilt after a restart holds none, and only then
            // does the walk recover the allocations. An agent gone by then
            // answers its node name and no image.
            List<BuildPodTemplate> templates = state.podTemplates();
            if (templates.isEmpty()) {
                templates = resolvePodTemplates(run, state, true);
            }
            if (templates.isEmpty()) {
                return;
            }
            SourceFacts facts = new SourceFacts("", "", "", "",
                    resolveDefinitionPath(run), resolveLibraries(run), templates);
            emitRunSource(run, state, facts, true, false);
        } catch (Exception e) {
            LOGGER.log(Level.WARNING,
                    "VarroaRunRecorder.Runs: failed to emit the run's pod templates", e);
        }
    }

    /**
     * Emits one {@code run.source} event for a run. The event carries
     * the run identity, the definition path, and the libraries. With
     * {@code withScm} true it also carries the branch, the SCM head, the
     * revision, the URL, the pod templates, and the changeset. The
     * builder writes a resolved value only when it is not empty.
     */
    private static void emitRunSource(Run<?, ?> run, RunState state, SourceFacts source,
            boolean withScm, boolean withChanges) {
        BuildEvent.Builder b = BuildEvent.builder()
                .phase(BuildEvent.PHASE_RUN_SOURCE)
                .seq(state.nextSeq())
                .emittedAtMillis(System.currentTimeMillis())
                .runId(state.runId)
                .intentKey(state.intentKey)
                .jobPath(state.jobPath)
                .buildNumber(state.buildNumber);
        applyCommonMetadata(b, run, false);
        applyTemplateFields(b, run);
        if (!source.definitionPath.isEmpty()) {
            b.definitionPath(source.definitionPath);
        }
        if (!source.libraries.isEmpty()) {
            b.libraries(source.libraries);
        }
        if (withScm) {
            if (!source.branch.isEmpty()) {
                b.branch(source.branch);
            }
            if (!source.scmHead.isEmpty()) {
                b.scmHead(source.scmHead);
            }
            if (!source.scmRevision.isEmpty()) {
                b.scmRevision(source.scmRevision);
            }
            if (!source.scmUrl.isEmpty()) {
                b.scmUrl(source.scmUrl);
            }
            if (!source.podTemplates.isEmpty()) {
                b.podTemplates(source.podTemplates);
            }
            if (withChanges) {
                List<BuildChange> changes = extractChanges(run);
                b.changes(changes);
                b.changeCount(changes.size());
            }
        } else {
            // The frozen table for E1 leaves the SCM facts empty. The
            // branch carries the multibranch name alone.
            b.scmHead("");
            b.scmUrl("");
            b.branch(truncate(IntentKey.branchNameFor(run.getParent()), MAX_FIELD_512));
        }
        emitCapped(b.build());
    }

    // --- Flows: attaches and detaches the graph listener (D1, D2) ---

    /** Attaches a {@link Graph} listener to a running pipeline, and detaches it at completion. */
    @Extension
    public static class Flows extends FlowExecutionListener {

        private static final Map<FlowExecution, Graph> ATTACHED =
                Collections.synchronizedMap(new WeakHashMap<>());

        @Override
        public void onRunning(FlowExecution execution) {
            attach(execution);
        }

        /**
         * A build Jenkins loads from disk resumes its execution here.
         * Jenkins calls this for a build that was running when its
         * process ended, which is every build the pipeline mesh moves.
         */
        @Override
        public void onResumed(FlowExecution execution) {
            // The mark does not depend on the gate: it only narrows which
            // builds count as ended by a restart.
            FailedProgramLoad.markResumed(runFor(execution));
            attach(execution);
        }

        /**
         * Attaches the recorder to one execution: the per-run state when
         * this process does not hold it, and the graph listener once.
         * Both entry points take this path, because only one of them
         * fires for a given build and the work is the same.
         */
        private static void attach(FlowExecution execution) {
            try {
                if (!gateEnabled()) {
                    return;
                }
                Run<?, ?> run = runFor(execution);
                if (run == null) {
                    return;
                }
                // A resumed build has no state here, because its
                // STARTED listener fired in another process.
                boolean resumed = attachResumedRun(run);
                Graph graph = new Graph(run);
                if (ATTACHED.putIfAbsent(execution, graph) == null) {
                    execution.addListener(graph);
                }
                if (resumed) {
                    scheduleResumedPush(run, execution);
                } else {
                    // E1: the flow's script compiled and the execution
                    // started. A resumed build sent its E1 from the
                    // process that loaded the flow.
                    RunState state = stateFor(safeJobPath(run), run.getNumber());
                    if (state != null) {
                        emitRunSource(run, state, resolveDefinitionFacts(run), false, false);
                    }
                }
            } catch (Exception e) {
                LOGGER.log(Level.WARNING, "VarroaRunRecorder.Flows: failed to attach the graph listener", e);
            }
        }

        /**
         * Schedules the first run-state push of a build this process
         * resumed. The graph listener arms a window for each flow node,
         * and a build that resumes inside one long step fires none, so
         * this process would replicate nothing until that step ends.
         */
        private static void scheduleResumedPush(Run<?, ?> run, FlowExecution execution) {
            RunState state = stateFor(safeJobPath(run), run.getNumber());
            if (state == null) {
                return;
            }
            RunStatePusher.scheduleForResumedRun(run, state.runId, execution);
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
                // Each allocation: record the agent's pod template while the
                // agent is still connected. The read covers the arriving node
                // and its enclosing blocks, and it walks no graph.
                boolean newTemplate = false;
                try {
                    WorkspaceAction workspace = workspaceOf(node);
                    if (workspace != null) {
                        newTemplate = recordAllocation(state, workspace);
                    }
                } catch (Exception e) {
                    LOGGER.log(Level.WARNING,
                            "VarroaRunRecorder.Graph: failed to record the run's allocation", e);
                }
                // E2: the first node at which the run answers a
                // revision. The probe never stops the listener's other
                // work: its own catch logs and falls through.
                if (!state.sourceResolved.get()) {
                    try {
                        SourceFacts source = resolveSourceFacts(run, state, true);
                        if (!source.scmRevision.isEmpty()) {
                            emitRunSource(run, state, source, true, true);
                            state.sourceResolved.set(true);
                        }
                    } catch (Exception e) {
                        LOGGER.log(Level.WARNING,
                                "VarroaRunRecorder.Graph: failed to probe the run's source", e);
                    }
                } else if (newTemplate) {
                    // E3: each allocation of a new pod template after the
                    // second emission. The event carries the whole recorded
                    // set, so the highest seq holds every template. It walks
                    // no graph, and it stops neither the stage events nor the
                    // run-state push below.
                    try {
                        emitRunSource(run, state, resolveSourceFacts(run, state, false), true, false);
                    } catch (Exception e) {
                        LOGGER.log(Level.WARNING,
                                "VarroaRunRecorder.Graph: failed to emit the allocation source event", e);
                    }
                }
                // A paused input is discovered after the node is created,
                // because the step attaches its action to the run after
                // that node exists. A node that arrives while a set is
                // cached settles or clears that set, so both cases
                // schedule the same re-check.
                if (isInputNode(node) || !state.pendingInputs.isEmpty()) {
                    try {
                        scheduleInputRecheck(run, state);
                    } catch (Exception e) {
                        LOGGER.log(Level.WARNING,
                                "VarroaRunRecorder.Graph: failed to schedule the input re-check", e);
                    }
                }
                // The push is scheduled, never performed, on this call:
                // this listener runs on the flow's own thread. A failure
                // of any kind leaves the build running and emits no
                // build event.
                RunStatePusher.scheduleFor(run, state.runId, node);
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

    /**
     * Answers the workspace action of {@code node}, or of the nearest
     * block that encloses {@code node}. The node step's start node holds
     * that action, and the body's first node is the first node whose
     * enclosing block holds it. The read covers one node plus its nesting
     * chain, and it walks no graph.
     */
    private static WorkspaceAction workspaceOf(FlowNode node) {
        if (node == null) {
            return null;
        }
        WorkspaceAction own = node.getAction(WorkspaceAction.class);
        if (own != null) {
            return own;
        }
        for (BlockStartNode enclosing : node.getEnclosingBlocks()) {
            WorkspaceAction workspace = enclosing.getAction(WorkspaceAction.class);
            if (workspace != null) {
                return workspace;
            }
        }
        return null;
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
        List<BuildLibrary> libraries = capLibraries(keepNewest(event.getLibraries(), MAX_LIBRARIES));
        if (!libraries.equals(event.getLibraries())) {
            b.libraries(libraries);
            changed = true;
        }
        List<BuildPodTemplate> podTemplates = capPodTemplates(keepNewest(event.getPodTemplates(), MAX_POD_TEMPLATES));
        if (!podTemplates.equals(event.getPodTemplates())) {
            b.podTemplates(podTemplates);
            changed = true;
        }
        String definitionPath = truncate(event.getDefinitionPath(), MAX_FIELD_512);
        if (!definitionPath.equals(event.getDefinitionPath())) {
            b.definitionPath(definitionPath);
            changed = true;
        }
        List<BuildInput> inputs = VarroaRunInputs.capInputs(event.getInputs());
        if (!inputs.equals(event.getInputs())) {
            b.inputs(inputs);
            changed = true;
        }
        if (event.getTestCases().size() > MAX_TEST_CASES) {
            b.testCases(keepFirst(event.getTestCases(), MAX_TEST_CASES));
            changed = true;
        }
        return changed ? b.build() : event;
    }

    /**
     * While the serialized event stays above 512 KiB, clears the parameter
     * lists of its {@code inputs} oldest first, then drops {@code
     * test_cases} entries from the end, then drops {@code stages}
     * entries from the oldest, then {@code changes} entries, then {@code
     * parameters} entries, then {@code pod_templates} entries, then {@code
     * libraries} entries, then truncates every remaining stage's error to
     * 512 characters (D11). Because the stage rung
     * fully drains {@code stages} before the error rung runs, the error
     * rung has no effect once the stage list is already empty. When the
     * event is still above 512 KiB, replaces it with a reduced,
     * identity-only event. When even that reduced event is above the
     * loopback's 1 MiB body limit — an identifier field the ladder
     * cannot shrink — drops it instead of returning it (ruling o).
     *
     * <p>The input rung drops no input entry. An input with no parameter
     * list holds five bounded strings, so twenty such inputs always fit
     * and the rung terminates.
     *
     * <p>The test case rung drops the last entry, and every other rung
     * drops the oldest. The case list holds the failed cases first, so the
     * last entry is the case with the lowest priority (D3). No rung drops
     * {@code test_report}.
     */
    private static BuildEvent applyOversizeLadder(BuildEvent event) {
        if (serializedSize(event) <= MAX_EVENT_BYTES) {
            return event;
        }
        BuildEvent current = event;
        while (serializedSize(current) > MAX_EVENT_BYTES && hasInputParameters(current)) {
            current = current.toBuilder().inputs(clearOldestInputParameters(current.getInputs())).build();
        }
        while (serializedSize(current) > MAX_EVENT_BYTES && !current.getTestCases().isEmpty()) {
            current = current.toBuilder().testCases(dropLast(current.getTestCases())).build();
        }
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
        while (serializedSize(current) > MAX_EVENT_BYTES && !current.getPodTemplates().isEmpty()) {
            current = current.toBuilder().podTemplates(dropOldest(current.getPodTemplates())).build();
        }
        while (serializedSize(current) > MAX_EVENT_BYTES && !current.getLibraries().isEmpty()) {
            current = current.toBuilder().libraries(dropOldest(current.getLibraries())).build();
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

    /** Reports whether any input of the event carries a parameter list. */
    private static boolean hasInputParameters(BuildEvent event) {
        for (BuildInput input : event.getInputs()) {
            if (!input.getParameters().isEmpty()) {
                return true;
            }
        }
        return false;
    }

    /**
     * Clears the parameter list of the oldest input that carries one and
     * keeps every input entry, so the ladder never drops an input and
     * always terminates. A user can still answer such an input: the
     * proceed route applies the step's own declared defaults for every
     * name a request omits.
     */
    private static List<BuildInput> clearOldestInputParameters(List<BuildInput> inputs) {
        List<BuildInput> cleared = new ArrayList<>(inputs.size());
        boolean clearedOne = false;
        for (BuildInput input : inputs) {
            if (!clearedOne && !input.getParameters().isEmpty()) {
                cleared.add(BuildInput.builder()
                        .id(input.getId())
                        .message(input.getMessage())
                        .ok(input.getOk())
                        .submitter(input.getSubmitter())
                        .submitterParameter(input.getSubmitterParameter())
                        .build());
                clearedOne = true;
            } else {
                cleared.add(input);
            }
        }
        return cleared;
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

    /** Keeps the first {@code max} entries. A priority-ordered list holds its most important entries at the head. */
    static <T> List<T> keepFirst(List<T> list, int max) {
        if (list.size() <= max) {
            return list;
        }
        return new ArrayList<>(list.subList(0, max));
    }

    /** Drops the last entry. A priority-ordered list holds its least important entry at the tail. */
    static <T> List<T> dropLast(List<T> list) {
        if (list.isEmpty()) {
            return list;
        }
        return new ArrayList<>(list.subList(0, list.size() - 1));
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

    /** Truncates every string of every library to the field cap. */
    private static List<BuildLibrary> capLibraries(List<BuildLibrary> libraries) {
        List<BuildLibrary> capped = new ArrayList<>(libraries.size());
        for (BuildLibrary library : libraries) {
            capped.add(longerThanCap(library.getName(), library.getVersion(), library.getRevision())
                    ? BuildLibrary.builder()
                            .name(truncate(library.getName(), MAX_FIELD_512))
                            .version(truncate(library.getVersion(), MAX_FIELD_512))
                            .revision(truncate(library.getRevision(), MAX_FIELD_512))
                            .build()
                    : library);
        }
        return capped;
    }

    /** Truncates every string of every pod template to the field cap. */
    private static List<BuildPodTemplate> capPodTemplates(List<BuildPodTemplate> podTemplates) {
        List<BuildPodTemplate> capped = new ArrayList<>(podTemplates.size());
        for (BuildPodTemplate template : podTemplates) {
            capped.add(longerThanCap(template.getName(), template.getLabel(), template.getImage())
                    ? BuildPodTemplate.builder()
                            .name(truncate(template.getName(), MAX_FIELD_512))
                            .label(truncate(template.getLabel(), MAX_FIELD_512))
                            .image(truncate(template.getImage(), MAX_FIELD_512))
                            .build()
                    : template);
        }
        return capped;
    }

    private static boolean longerThanCap(String... values) {
        for (String value : values) {
            if (value.length() > MAX_FIELD_512) {
                return true;
            }
        }
        return false;
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

    /**
     * Cuts {@code s} to at most {@code maxBytes} UTF-8 bytes, at the
     * largest whole character boundary at or under that limit. The
     * summary value crosses the wire as a proto3 string, and the wire
     * format requires a valid UTF-8 encoding, so a plain byte cut can
     * split a multi-byte character and make the event fail to encode.
     */
    static String truncateUtf8Bytes(String s, int maxBytes) {
        if (s == null) {
            return "";
        }
        byte[] bytes = s.getBytes(StandardCharsets.UTF_8);
        if (bytes.length <= maxBytes) {
            return s;
        }
        int end = maxBytes;
        // The byte at "end" is the first byte the cut drops. A byte whose
        // top two bits are 10 is a continuation byte, so a character
        // starts before it. The loop backs up to the nearest lead byte or
        // ASCII byte.
        while (end > 0 && (bytes[end] & 0xC0) == 0x80) {
            end--;
        }
        return new String(bytes, 0, end, StandardCharsets.UTF_8);
    }
}
