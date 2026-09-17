package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Action;
import hudson.model.Cause;
import hudson.model.CauseAction;
import hudson.model.Job;
import hudson.model.Queue;
import hudson.model.Run;
import hudson.security.ACL;

import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.ArrayBlockingQueue;
import java.util.concurrent.ThreadFactory;
import java.util.concurrent.ThreadPoolExecutor;
import java.util.concurrent.TimeUnit;
import java.util.logging.Level;
import java.util.logging.Logger;

import jenkins.model.Jenkins;

import org.jenkinsci.plugins.workflow.flow.FlowExecutionOwner;

/**
 * Admits every queued item and stamps a fresh {@link VarroaIntentAction}
 * on it (D4 step order). Runs inside the Jenkins queue lock and performs
 * no network call and no other I/O; it returns at once on every path.
 *
 * <p>The claim itself starts later, in {@link VarroaClaimDispatcher}: at
 * this point {@code Queue.schedule2} has not yet created the {@link
 * Queue.Item}, so no claimer could act on it.
 *
 * <p>This handler also derives the claim's {@code actor} (D5): the
 * action's own actor when {@link VarroaRunTriggerAction} already stamped
 * one, otherwise the scheduling authentication's name, with {@code
 * anonymous} and {@link ACL#SYSTEM2} normalized to the empty string.
 *
 * <p>It refuses one shape: a waiting {@code build} step for a job this
 * cell does not home. It reads {@link VarroaHomeCache} from memory for
 * that test and triggers no refresh. The claimer, not this handler, owns
 * the forward of a step that does not wait.
 */
@Extension
public class VarroaQueueDecisionHandler extends Queue.QueueDecisionHandler {

    private static final Logger LOGGER = Logger.getLogger(VarroaQueueDecisionHandler.class.getName());

    static final String GATE_ENV = "VARROA_CLAIM_GATE";

    /**
     * The action a waiting {@code build} step adds. The step adds it when
     * it waits for the downstream build to start or to finish, and only
     * then, so one test covers both wait modes.
     *
     * <p>The test matches the simple class name. The plugin that owns the
     * step is no compile dependency here, and the name survived that
     * plugin's own package move.
     */
    static final String BUILD_TRIGGER_ACTION = "BuildTriggerAction";

    /** Notice tasks held while the single notice thread writes. */
    private static final int NOTICE_QUEUE_CAPACITY = 32;

    /**
     * Writes a refusal notice off the queue thread. It is not the claim
     * pool: that pool's 256 places are reserved for pending claims, and a
     * notice there would consume one.
     *
     * <p>The rejection policy is abort, never discard. The handler reads
     * the rejection and logs the same line, so a refusal always leaves a
     * record.
     */
    static final ThreadPoolExecutor NOTICE_EXECUTOR = new ThreadPoolExecutor(
            1, 1, 0L, TimeUnit.MILLISECONDS, new ArrayBlockingQueue<>(NOTICE_QUEUE_CAPACITY),
            noticeThreadFactory(), new ThreadPoolExecutor.AbortPolicy());

    private final VarroaHomeCache homeCache;

    public VarroaQueueDecisionHandler() {
        this(VarroaHomeCache.get());
    }

    /** Package-private constructor for testing: injects the home cache. */
    VarroaQueueDecisionHandler(VarroaHomeCache homeCache) {
        this.homeCache = homeCache;
    }

    @Override
    public boolean shouldSchedule(Queue.Task task, List<Action> actions) {
        return shouldSchedule(task, actions, System.getenv(GATE_ENV));
    }

    /**
     * Package-private overload that takes the gate value directly, so a
     * test can drive every path without mutating the process environment.
     */
    boolean shouldSchedule(Queue.Task task, List<Action> actions, String gateEnvValue) {
        try {
            return decide(task, actions, gateEnvValue);
        } catch (Exception e) {
            // A defect here must never block scheduling: the claim gate
            // is a veto mechanism, not a source of new build failures.
            LOGGER.log(Level.WARNING, "VarroaQueueDecisionHandler: admitting after an unexpected error", e);
            return true;
        }
    }

    private boolean decide(Queue.Task task, List<Action> actions, String gateEnvValue) {
        // Step 1: the gate is off.
        if (!gateEnabled(gateEnvValue)) {
            return true;
        }

        // Step 2: no job, no build record.
        if (!(task instanceof Job)) {
            return true;
        }
        Job<?, ?> job = (Job<?, ?>) task;

        VarroaIntentAction existing = findAction(actions);

        // Step 3: an already-granted intent always admits.
        if (existing != null && existing.isGranted()) {
            return true;
        }

        // Step 4: the first rejection the handler makes.
        if (existing != null && DuplicateRegistry.get().contains(existing.getRunId())) {
            return false;
        }

        // Step 4b: the second rejection. A waiting build step cannot wait
        // on a run this JVM does not host, and a queue refusal fails the
        // step in every wait mode, so only a waiting step is refused.
        if (isWaitingBuildStep(actions) && homeCache.forwardSlotFor(job.getFullName()).isPresent()) {
            refuseWaitingStep(job, actions);
            return false;
        }

        // Step 5 and step 6: read the clock once, mint the run id, read
        // the delivery id and the admission revision, and stamp a fresh
        // action only when the list holds none.
        if (existing == null) {
            List<Cause> causes = causesFromActions(actions);
            VarroaIntentAction stamped = VarroaIntentAction.stampMissing(job, causes);
            stamped.setActor(currentAuthenticationActor());
            actions.add(stamped);
        } else if (existing.getActor().isEmpty()) {
            existing.setActor(currentAuthenticationActor());
        }

        // Step 7.
        return true;
    }

    private static boolean gateEnabled(String raw) {
        return raw != null && !raw.isEmpty() && !"0".equals(raw);
    }

    /**
     * Reads {@code Jenkins.getAuthentication2().getName()} once, and
     * normalizes {@code anonymous} and {@link ACL#SYSTEM2}'s name to the
     * empty string.
     */
    private static String currentAuthenticationActor() {
        String name = Jenkins.getAuthentication2().getName();
        if (name == null || "anonymous".equals(name) || ACL.SYSTEM2.getName().equals(name)) {
            return "";
        }
        return name;
    }

    private static VarroaIntentAction findAction(List<Action> actions) {
        for (Action action : actions) {
            if (action instanceof VarroaIntentAction) {
                return (VarroaIntentAction) action;
            }
        }
        return null;
    }

    private static List<Cause> causesFromActions(List<Action> actions) {
        List<Cause> causes = new ArrayList<>();
        for (Action action : actions) {
            if (action instanceof CauseAction) {
                causes.addAll(((CauseAction) action).getCauses());
            }
        }
        return causes;
    }

    // --- The waiting-step refusal ---

    /** True when the action list holds a waiting {@code build} step's action. */
    static boolean isWaitingBuildStep(List<Action> actions) {
        for (Action action : actions) {
            if (action != null && BUILD_TRIGGER_ACTION.equals(action.getClass().getSimpleName())) {
                return true;
            }
        }
        return false;
    }

    /**
     * Counts the refusal and hands the notice to the notice executor.
     *
     * <p>Every throwable of the hand-off is caught here, so none reaches
     * the outer catch, which admits the item. A hand-off that does not run
     * logs the same line at {@code WARNING}, in one call, because the
     * queue lock is held.
     */
    private void refuseWaitingStep(Job<?, ?> job, List<Action> actions) {
        PluginCounters.increment(PluginCounters.FORWARD_REFUSED);
        String notice = noticeFor(job.getFullName());
        try {
            Run<?, ?> caller = callerRun(actions);
            NOTICE_EXECUTOR.execute(() -> writeNotice(caller, notice));
        } catch (Throwable t) {
            LOGGER.log(Level.WARNING, notice);
        }
    }

    /**
     * The two-line notice, naming the job and the supported call.
     *
     * <p>The two calls take two path forms. {@code build job:} takes the
     * job's own path, so the first line names it in full. {@code
     * varroa.build} takes a path below the target tenant's folder, and the
     * responder qualifies it with that tenant, so the second line names
     * the path below the tenant. A full path there would build
     * {@code <tenant>/<tenant>/<job>}, which exists on no cell.
     */
    static String noticeFor(String jobPath) {
        return "job " + jobPath + " runs on another slot of this tenant. `build job: '" + jobPath
                + "', wait: true` cannot wait on it.\n"
                + "Use varroa.build(job: '" + VarroaHomeCache.pathBelowTenant(jobPath)
                + "', wait: true) instead.";
    }

    /** The run that scheduled this item, from its upstream cause, or null. */
    private static Run<?, ?> callerRun(List<Action> actions) {
        for (Cause cause : causesFromActions(actions)) {
            if (cause instanceof Cause.UpstreamCause) {
                Run<?, ?> run = ((Cause.UpstreamCause) cause).getUpstreamRun();
                if (run != null) {
                    return run;
                }
            }
        }
        return null;
    }

    /**
     * Writes the notice where the caller reads it: the caller run's own
     * console. A run that resolves no listener takes the Jenkins system
     * log instead, so the refusal always leaves a record.
     */
    private static void writeNotice(Run<?, ?> caller, String notice) {
        try {
            if (caller instanceof FlowExecutionOwner.Executable) {
                FlowExecutionOwner owner = ((FlowExecutionOwner.Executable) caller).asFlowExecutionOwner();
                if (owner != null) {
                    owner.getListener().getLogger().println(notice);
                    return;
                }
            }
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaQueueDecisionHandler: the caller console was not reachable", e);
        }
        LOGGER.log(Level.WARNING, notice);
    }

    private static ThreadFactory noticeThreadFactory() {
        return runnable -> {
            Thread thread = new Thread(runnable, "varroa-forward-notice");
            thread.setDaemon(true);
            return thread;
        };
    }
}
