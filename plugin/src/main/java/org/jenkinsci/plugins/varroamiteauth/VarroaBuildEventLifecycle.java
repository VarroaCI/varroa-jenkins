package org.jenkinsci.plugins.varroamiteauth;

import hudson.ExtensionList;
import hudson.init.InitMilestone;
import hudson.init.Initializer;
import hudson.init.Terminator;

import jenkins.model.Jenkins;

import java.util.List;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Starts and stops the build-event pipeline with the Jenkins process (D11,
 * D12, ruling r).
 *
 * <p>The initializer runs only when {@code VARROA_CLAIM_GATE} is on. It
 * replays every run's spool backlog that already exists on disk, oldest
 * run first, before starting {@link BuildEventDispatcher}'s thread — the
 * same replay a Jenkins restart needs after a mite outage. The terminator
 * stops the dispatcher, the run-heartbeat timer, and the claim queue's
 * executor, whether the gate is on or not.
 */
public final class VarroaBuildEventLifecycle {

    private static final Logger LOGGER = Logger.getLogger(VarroaBuildEventLifecycle.class.getName());

    private static volatile BuildEventDispatcher dispatcher;

    private VarroaBuildEventLifecycle() {
    }

    @Initializer(after = InitMilestone.PLUGINS_STARTED)
    public static void start() {
        start(System.getenv(VarroaQueueDecisionHandler.GATE_ENV));
    }

    /**
     * Package-private overload that takes the gate value directly, so a
     * test can drive it without mutating the process environment.
     */
    static void start(String gateEnvValue) {
        if (!gateEnabled(gateEnvValue)) {
            return;
        }
        BuildEventDispatcher d = new BuildEventDispatcher();
        replaySpoolOnto(d, new RunSpool());
        d.start();
        dispatcher = d;
    }

    /** Announces every run the spool already holds, oldest first, so it posts before anything new. */
    static void replaySpoolOnto(BuildEventDispatcher d, RunSpool spool) {
        List<String> runs = spool.listRuns();
        for (String runId : runs) {
            d.noteSpooled(runId);
        }
    }

    @Terminator
    public static void stop() {
        BuildEventDispatcher d = dispatcher;
        dispatcher = null;
        if (d != null) {
            d.stop();
        }
        VarroaRunRecorder.stopHeartbeat();
        stopClaimQueue();
    }

    private static void stopClaimQueue() {
        try {
            Jenkins jenkins = Jenkins.getInstanceOrNull();
            if (jenkins == null) {
                return;
            }
            VarroaClaimDispatcher claimDispatcher = ExtensionList.lookupSingleton(VarroaClaimDispatcher.class);
            claimDispatcher.shutdown();
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaBuildEventLifecycle: no claim dispatcher extension to stop", e);
        }
    }

    /**
     * Returns the running dispatcher, or {@code null} when the gate is
     * off or it has not started. {@link VarroaRunRecorder} calls this to
     * announce a spool file it wrote on the sink-full path.
     */
    static BuildEventDispatcher current() {
        return dispatcher;
    }

    /** Clears the tracked dispatcher without stopping it. Package-private for testing. */
    static void resetForTesting() {
        dispatcher = null;
    }

    private static boolean gateEnabled(String raw) {
        return raw != null && !raw.isEmpty() && !"0".equals(raw);
    }
}
