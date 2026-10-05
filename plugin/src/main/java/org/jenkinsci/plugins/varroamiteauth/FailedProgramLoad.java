package org.jenkinsci.plugins.varroamiteauth;

import hudson.AbortException;
import hudson.init.InitMilestone;
import hudson.init.Initializer;
import hudson.model.Run;

import java.util.Collection;
import java.util.HashSet;
import java.util.List;
import java.util.Set;
import java.util.logging.Level;
import java.util.logging.Logger;

import org.jenkinsci.plugins.workflow.actions.ErrorAction;
import org.jenkinsci.plugins.workflow.flow.FlowExecution;
import org.jenkinsci.plugins.workflow.graph.FlowEndNode;
import org.jenkinsci.plugins.workflow.graph.FlowNode;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;

/**
 * Finds a pipeline build that a Jenkins restart ended before it did work
 * in this process, and keeps one completion for it in this process.
 *
 * <p>Two cases match. In the first, the program does not load: the
 * execution ends with a {@code FlowEndNode} at once, and Jenkins calls
 * {@code WorkflowRun.finish} about one second later. In the second,
 * workflow-job refuses to resume a build older than its maximum
 * resumption age: the load marks the build completed with no result, and
 * a timer thread calls {@code finish} later. In both cases {@code finish}
 * fires {@code RunListener.onCompleted}, so the boot reconcile and the
 * run listener can both end the same build, in either order. Each one
 * calls {@link #claim} first, and only the caller that gets the claim
 * sends the {@code COMPLETED} event.
 *
 * <p>The claim set holds only such builds, so it stays small. The set of
 * resumed builds holds one entry for each build this process resumed. A
 * new Jenkins boot clears both sets.
 */
public final class FailedProgramLoad {

    private static final Logger LOGGER = Logger.getLogger(FailedProgramLoad.class.getName());

    /**
     * The message of the error that workflow-cps records on the end node
     * of a program that does not load. The restart test pins it against
     * the workflow-cps version this plugin builds with.
     */
    static final String LOAD_FAILURE_MESSAGE = "Failed to load program";

    /** The time this Jenkins boot started its plugins. Zero before the first boot. */
    private static volatile long bootMillis;

    /** Guarded by the class lock. */
    private static final Set<String> CLAIMED = new HashSet<>();

    /**
     * The external ids of the builds whose execution this process resumed.
     * Guarded by the class lock.
     */
    private static final Set<String> RESUMED = new HashSet<>();

    private FailedProgramLoad() {
    }

    /**
     * Records the boot time before any job loads, and clears the claims
     * of an earlier boot in the same JVM.
     */
    @Initializer(after = InitMilestone.PLUGINS_STARTED)
    public static void markBoot() {
        markBoot(System.currentTimeMillis());
    }

    static synchronized void markBoot(long nowMillis) {
        bootMillis = nowMillis;
        CLAIMED.clear();
        RESUMED.clear();
    }

    /**
     * Records that this process resumed the execution of {@code run}.
     * Jenkins calls {@code FlowExecutionListener.onResumed} inside the
     * build load, so the mark is present before any caller reads it.
     */
    static synchronized void markResumed(Run<?, ?> run) {
        if (run != null) {
            RESUMED.add(run.getExternalizableId());
        }
    }

    private static synchronized boolean wasResumed(Run<?, ?> run) {
        return RESUMED.contains(run.getExternalizableId());
    }

    /**
     * True when a Jenkins restart ended {@code run} before it did work in
     * this process: its program did not load, or Jenkins refused to resume
     * it.
     */
    static boolean endedByRestart(Run<?, ?> run) {
        return matches(run) || refusedResume(run);
    }

    /**
     * True when {@code run} is a pipeline build that started before this
     * boot, that this process did not resume, and whose execution is not
     * complete. workflow-job leaves a build in this state when it refuses
     * to resume it: it skips {@code onResumed}, and it calls {@code finish}
     * without an end node. A build that resumed and is then killed does
     * not match, because the resume marks it first.
     */
    static boolean refusedResume(Run<?, ?> run) {
        if (!(run instanceof WorkflowRun)) {
            return false;
        }
        long boot = bootMillis;
        if (boot == 0L || run.getStartTimeInMillis() >= boot || wasResumed(run)) {
            return false;
        }
        try {
            FlowExecution execution = ((WorkflowRun) run).getExecution();
            return execution != null && !execution.isComplete();
        } catch (RuntimeException e) {
            LOGGER.log(Level.FINE, "FailedProgramLoad: could not read the execution of " + run, e);
            return false;
        }
    }

    /**
     * True when {@code run} is a pipeline build that started before this
     * boot and whose execution ended because its program did not load.
     * A build that started in this boot never matches. A build that
     * started before this boot, resumed, and then failed with the same
     * message does match: the two facts are a signal, not a proof.
     */
    static boolean matches(Run<?, ?> run) {
        if (!(run instanceof WorkflowRun)) {
            return false;
        }
        long boot = bootMillis;
        if (boot == 0L || run.getStartTimeInMillis() >= boot) {
            return false;
        }
        try {
            FlowExecution execution = ((WorkflowRun) run).getExecution();
            if (execution == null || !execution.isComplete()) {
                return false;
            }
            List<FlowNode> heads = execution.getCurrentHeads();
            if (heads.size() != 1 || !(heads.get(0) instanceof FlowEndNode)) {
                return false;
            }
            ErrorAction error = heads.get(0).getError();
            Throwable cause = error != null ? error.getError() : null;
            return cause instanceof AbortException && LOAD_FAILURE_MESSAGE.equals(cause.getMessage());
        } catch (RuntimeException e) {
            LOGGER.log(Level.FINE, "FailedProgramLoad: could not read the end node of " + run, e);
            return false;
        }
    }

    /**
     * Claims the completion of one build under every run id it can carry.
     * Returns false when an earlier call already claimed any of the ids.
     * Empty and null ids are ignored.
     */
    static synchronized boolean claim(Collection<String> runIds) {
        for (String id : runIds) {
            if (id != null && !id.isEmpty() && CLAIMED.contains(id)) {
                return false;
            }
        }
        for (String id : runIds) {
            if (id != null && !id.isEmpty()) {
                CLAIMED.add(id);
            }
        }
        return true;
    }

    /** Releases a claim, so that the other caller can send the event. */
    static synchronized void release(Collection<String> runIds) {
        for (String id : runIds) {
            if (id != null) {
                CLAIMED.remove(id);
            }
        }
    }

    /** Package-private for testing. */
    static synchronized int resumedCountForTesting() {
        return RESUMED.size();
    }

    /** Package-private for testing. */
    static synchronized int claimedCountForTesting() {
        return CLAIMED.size();
    }
}
