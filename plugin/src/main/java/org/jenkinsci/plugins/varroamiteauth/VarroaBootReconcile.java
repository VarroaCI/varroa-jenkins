package org.jenkinsci.plugins.varroamiteauth;

import hudson.init.InitMilestone;
import hudson.init.Initializer;
import hudson.model.Job;
import hudson.model.Run;
import hudson.security.ACL;
import hudson.security.ACLContext;

import jenkins.model.Jenkins;

import java.util.concurrent.TimeUnit;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Ends, at Jenkins boot, each build that a Jenkins stop ended without a
 * {@code RunListener.onCompleted} call.
 *
 * <p>A container that stops inside a live pod (an OOM kill, a liveness
 * kill, {@code kill -9}) restarts with the same pod, so the record store
 * still names this pod the run's owner. Jenkins loads such a freestyle
 * build as completed, and fires no listener, so no {@code COMPLETED}
 * event leaves the plugin. This class reads {@link RunJournal} and sends
 * that event for each entry, under the run's own identity.
 *
 * <p>It runs after the jobs load, and after {@link
 * VarroaBuildEventLifecycle} starts the dispatcher. It writes each event
 * to the run's spool before it deletes the entry, so a stop between the
 * two steps loses no event. A second boot then finds the spooled event
 * and sends no second one.
 */
public final class VarroaBootReconcile {

    private static final Logger LOGGER = Logger.getLogger(VarroaBootReconcile.class.getName());

    /**
     * The age at which an entry expires. An entry older than this, by its
     * last-seen time, is deleted with a warning and sends no event.
     */
    static final long ENTRY_MAX_AGE_MILLIS = TimeUnit.DAYS.toMillis(7);

    private VarroaBootReconcile() {
    }

    @Initializer(after = InitMilestone.JOB_CONFIG_ADAPTED)
    public static void reconcileAtBoot() {
        if (!VarroaRunRecorder.gateEnabled()) {
            return;
        }
        try (ACLContext ignored = ACL.as2(ACL.SYSTEM2)) {
            reconcile(new RunJournal(), new RunSpool(), System.currentTimeMillis());
        } catch (RuntimeException e) {
            LOGGER.log(Level.WARNING, "VarroaBootReconcile: the boot reconcile failed", e);
        }
    }

    /**
     * Applies the reconcile rules to every journal entry, and returns the
     * number of {@code COMPLETED} events it spooled. Package-private for
     * testing.
     */
    static int reconcile(RunJournal journal, RunSpool spool, long nowMillis) {
        int sent = 0;
        for (RunJournal.Entry entry : journal.readAll()) {
            try {
                if (reconcileOne(journal, spool, entry, nowMillis)) {
                    sent++;
                }
            } catch (RuntimeException e) {
                LOGGER.log(Level.WARNING, "VarroaBootReconcile: failed to reconcile run " + entry.runId, e);
            }
        }
        return sent;
    }

    private static boolean reconcileOne(RunJournal journal, RunSpool spool, RunJournal.Entry entry, long nowMillis) {
        if (spoolHoldsCompleted(spool, entry.runId)) {
            journal.remove(entry.runId);
            return false;
        }
        long lastSeen = Math.max(entry.startedAtMillis, entry.lastSeenMillis);
        if (nowMillis - lastSeen > ENTRY_MAX_AGE_MILLIS) {
            LOGGER.log(Level.WARNING, () -> "VarroaBootReconcile: deleting the expired entry of run " + entry.runId
                    + " (" + entry.jobPath + "#" + entry.buildNumber + "), last seen at " + lastSeen);
            journal.remove(entry.runId);
            return false;
        }
        Run<?, ?> run = loadRun(entry);
        if (run != null && run.isBuilding()) {
            // A pipeline resumes on its own. Its own completion deletes
            // the entry.
            return false;
        }
        if (run == null && entry.pipeline) {
            // A pipeline saves its build record when it starts, so a
            // missing one moved to another cell or was deleted.
            LOGGER.log(Level.INFO, () -> "VarroaBootReconcile: the pipeline build " + entry.jobPath + "#"
                    + entry.buildNumber + " of run " + entry.runId + " does not load; deleting its entry");
            journal.remove(entry.runId);
            return false;
        }
        BuildEvent event = VarroaRunRecorder.bootCompletedEvent(entry, run, nowMillis);
        if (event != null) {
            if (!spool.append(entry.runId, event)) {
                // The entry is the only record of the lost run. It stays,
                // so the next boot tries again.
                LOGGER.log(Level.WARNING, () -> "VarroaBootReconcile: failed to spool the completion of run "
                        + entry.runId + "; keeping its entry");
                return false;
            }
            BuildEventDispatcher dispatcher = VarroaBuildEventLifecycle.current();
            if (dispatcher != null) {
                dispatcher.noteSpooled(entry.runId);
            }
            PluginCounters.increment(PluginCounters.RUNS_RECONCILED_AT_BOOT);
            LOGGER.log(Level.INFO, () -> "VarroaBootReconcile: ended the lost build " + entry.jobPath + "#"
                    + entry.buildNumber + " of run " + entry.runId + " with result " + event.getResult());
        }
        journal.remove(entry.runId);
        return event != null;
    }

    private static boolean spoolHoldsCompleted(RunSpool spool, String runId) {
        for (RunSpool.Entry e : spool.readOrdered(runId)) {
            if (!e.runState && BuildEvent.PHASE_COMPLETED.equals(e.phase)) {
                return true;
            }
        }
        return false;
    }

    /**
     * Loads the entry's build. Returns {@code null} when the job or the
     * build does not load, or when the loaded build carries another run
     * id: the number then names a later build, and the entry's own build
     * is gone.
     */
    private static Run<?, ?> loadRun(RunJournal.Entry entry) {
        Jenkins jenkins = Jenkins.getInstanceOrNull();
        if (jenkins == null || entry.jobPath.isEmpty() || entry.buildNumber <= 0
                || entry.buildNumber > Integer.MAX_VALUE) {
            return null;
        }
        Job<?, ?> job = jenkins.getItemByFullName(entry.jobPath, Job.class);
        if (job == null) {
            return null;
        }
        Run<?, ?> run = job.getBuildByNumber((int) entry.buildNumber);
        if (run == null) {
            return null;
        }
        VarroaIntentAction action = run.getAction(VarroaIntentAction.class);
        if (action != null && !action.getRunId().isEmpty() && !entry.runId.equals(action.getRunId())
                && !entry.runId.equals(VarroaRunRecorder.resolveRunId(action))) {
            return null;
        }
        return run;
    }
}
