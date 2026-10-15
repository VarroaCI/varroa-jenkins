package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Job;
import hudson.model.Queue;
import hudson.model.queue.CauseOfBlockage;
import hudson.model.queue.QueueTaskDispatcher;

import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.function.LongSupplier;

/**
 * Holds a queued item until its claim is granted (D1, D4).
 *
 * <p>{@code canRun} makes no network call and takes no lock of its own:
 * on the first blocking pass for an item it offers a pending claim to
 * {@link VarroaClaimQueue} and records the item's run id as started. A
 * later pass for the same run id offers nothing. When the queue refuses
 * the offer, the started set keeps no entry, so the next pass offers the
 * claim again.
 *
 * <p>An entry leaves the started set when the claim resolves — granted,
 * cancelled, given up, or an unexpected claimer error — through the
 * completion callback passed to {@link VarroaClaimQueue#offer(Queue.Item,
 * Job, Runnable)}, and in every case after one hour, through a lazy sweep
 * on each {@code canRun} pass (ruling q).
 */
@Extension
public class VarroaClaimDispatcher extends QueueTaskDispatcher {

    static final String BLOCK_MESSAGE = "awaiting Varroa claim";

    /** An entry leaves the started map after this long, even with no resolution (ruling q). */
    static final long STARTED_TTL_MILLIS = 60L * 60 * 1000;

    private final VarroaClaimQueue claimQueue;
    private final LongSupplier clock;
    private final Map<String, Long> started = new ConcurrentHashMap<>();

    public VarroaClaimDispatcher() {
        this(new VarroaClaimQueue());
    }

    /** Package-private constructor for testing: injects the claim queue. */
    VarroaClaimDispatcher(VarroaClaimQueue claimQueue) {
        this(claimQueue, System::currentTimeMillis);
    }

    /** Package-private constructor for testing: injects the claim queue and the clock. */
    VarroaClaimDispatcher(VarroaClaimQueue claimQueue, LongSupplier clock) {
        this.claimQueue = claimQueue;
        this.clock = clock;
    }

    @Override
    public CauseOfBlockage canRun(Queue.Item item) {
        return canRun(item, System.getenv(VarroaQueueDecisionHandler.GATE_ENV));
    }

    /**
     * Package-private overload that takes the gate value directly, so a
     * test can drive every path without mutating the process environment.
     */
    CauseOfBlockage canRun(Queue.Item item, String gateEnvValue) {
        if (!gateEnabled(gateEnvValue)) {
            return null;
        }
        if (!(item.getTask() instanceof Job)) {
            return null;
        }
        Job<?, ?> job = (Job<?, ?>) item.getTask();
        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);
        if (action != null && action.isGranted()) {
            return null;
        }
        if (action == null) {
            // Restored from disk, or scheduled while the gate was off:
            // stamp the same action the handler would have stamped, so
            // the claimer below never sees a missing action (ruling p).
            action = VarroaIntentAction.stampMissing(job, item.getCauses());
            item.addAction(action);
        }
        if (action.isApplyPending()) {
            // The grant waits for the job's apply gate or for its start
            // mark. A second claim now would hand the gate a second apply
            // for the same run.
            return blockedCause();
        }
        if (clock.getAsLong() < action.getClaimNotBeforeMillis()) {
            // A start mark failed. The next claim waits for its backoff
            // step, so a mark that keeps failing does not claim on every
            // queue pass.
            return blockedCause();
        }

        sweepExpiredStarted();
        String key = job.getFullName() + "#" + action.getRunId();
        if (started.putIfAbsent(key, clock.getAsLong()) == null) {
            boolean accepted = claimQueue.offer(item, job, () -> started.remove(key));
            if (!accepted) {
                started.remove(key);
            }
        }
        return blockedCause();
    }

    /** Stops the underlying {@link VarroaClaimQueue}'s executor. Called from a {@code @Terminator} (ruling r). */
    void shutdown() {
        claimQueue.shutdown();
    }

    private void sweepExpiredStarted() {
        long now = clock.getAsLong();
        started.entrySet().removeIf(e -> now - e.getValue() >= STARTED_TTL_MILLIS);
    }

    private static boolean gateEnabled(String raw) {
        return raw != null && !raw.isEmpty() && !"0".equals(raw);
    }

    /** Number of run ids the dispatcher currently tracks as started. Package-private for testing. */
    int startedCountForTesting() {
        return started.size();
    }

    /** Returns the underlying claim queue. Package-private for testing. */
    VarroaClaimQueue claimQueueForTesting() {
        return claimQueue;
    }

    private static CauseOfBlockage blockedCause() {
        return new CauseOfBlockage() {
            @Override
            public String getShortDescription() {
                return BLOCK_MESSAGE;
            }
        };
    }
}
