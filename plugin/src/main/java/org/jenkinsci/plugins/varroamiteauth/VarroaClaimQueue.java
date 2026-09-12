package org.jenkinsci.plugins.varroamiteauth;

import groovy.json.JsonSlurper;
import hudson.model.Job;
import hudson.model.Queue;
import net.sf.json.JSONObject;

import java.io.IOException;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.ArrayBlockingQueue;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.RejectedExecutionException;
import java.util.concurrent.ThreadFactory;
import java.util.concurrent.ThreadPoolExecutor;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Bounded work queue and four daemon threads that claim a queued item
 * through the mite loopback and unblock {@link VarroaClaimDispatcher}
 * (D1, D4).
 *
 * <p>A claimer thread derives the origin and the intent key (D5), posts a
 * {@code ClaimRequest}, and applies the decision table. A grant applies
 * the build number first (D4's five steps, contract G3), then sets the
 * action's granted flag, which releases the dispatcher.
 */
public class VarroaClaimQueue {

    private static final Logger LOGGER = Logger.getLogger(VarroaClaimQueue.class.getName());

    static final String CLAIM_PATH = "/v1/claim";

    /** Bounded work queue capacity. */
    static final int CAPACITY = 256;

    /** Daemon claimer threads. */
    static final int THREADS = 4;

    /** Retry backoff ladder; the last value repeats forever. */
    static final long[] BACKOFF_MILLIS = {1000L, 2000L, 5000L, 10_000L, 30_000L, 60_000L};

    /** A pending claim gives up after one hour. */
    static final long LIFETIME_MILLIS = 60L * 60 * 1000;

    private static final String DECISION_GRANTED = "granted";
    private static final String DECISION_DUPLICATE = "duplicate";
    private static final String DECISION_VETOED = "vetoed";
    private static final String REASON_NOT_READY = "NOT_READY";

    /** Seam for a test to skip real wall-clock waits. */
    interface Sleeper {
        void sleep(long millis) throws InterruptedException;
    }

    /** Millisecond clock seam so a test can pin the retry timing. */
    interface Clock {
        long nowMillis();
    }

    private final MiteLoopbackClient client;
    private final Clock clock;
    private final Sleeper sleeper;
    private final ThreadPoolExecutor executor;
    private final Set<String> pendingKeys = ConcurrentHashMap.newKeySet();

    public VarroaClaimQueue() {
        this(new MiteLoopbackClient(), System::currentTimeMillis, Thread::sleep);
    }

    /** Package-private constructor for testing: injects every collaborator. */
    VarroaClaimQueue(MiteLoopbackClient client, Clock clock, Sleeper sleeper) {
        this.client = client;
        this.clock = clock;
        this.sleeper = sleeper;
        this.executor = new ThreadPoolExecutor(THREADS, THREADS, 0L, TimeUnit.MILLISECONDS,
                new ArrayBlockingQueue<>(CAPACITY), daemonThreadFactory(),
                new ThreadPoolExecutor.AbortPolicy());
    }

    /**
     * Offers a pending claim for {@code item}. Returns {@code true} when
     * the queue accepted the offer or a claim for the same job and run id
     * is already pending; {@code false} when the bounded queue is full.
     */
    public boolean offer(Queue.Item item, Job<?, ?> job) {
        return offer(item, job, null);
    }

    /**
     * Offers a pending claim for {@code item}, as {@link #offer(Queue.Item, Job)},
     * and runs {@code onResolved} once the claim resolves: granted,
     * cancelled, given up after one hour, or after an unexpected claimer
     * error (ruling q). {@code onResolved} never runs when the queue
     * refuses the offer, because no claim started. The caller may pass
     * {@code null} for no callback.
     */
    public boolean offer(Queue.Item item, Job<?, ?> job, Runnable onResolved) {
        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);
        String runId = action != null ? action.getRunId() : "";
        String key = job.getFullName() + "#" + runId;
        if (!pendingKeys.add(key)) {
            return true;
        }
        try {
            executor.execute(() -> {
                try {
                    runClaim(item, job, action);
                } catch (Exception e) {
                    LOGGER.log(Level.WARNING, "VarroaClaimQueue: claimer thread failed", e);
                    cancel(item);
                } finally {
                    pendingKeys.remove(key);
                    if (onResolved != null) {
                        onResolved.run();
                    }
                }
            });
            return true;
        } catch (RejectedExecutionException e) {
            pendingKeys.remove(key);
            return false;
        }
    }

    /**
     * Stops the four claimer threads. {@link VarroaBuildEventLifecycle}
     * calls this from its {@code @Terminator} (ruling r). Idempotent.
     */
    public void shutdown() {
        executor.shutdownNow();
    }

    /** {@code true} once {@link #shutdown()} has run. Package-private for testing. */
    boolean isShutdownForTesting() {
        return executor.isShutdown();
    }

    /** {@code true} when a claim for {@code key} is still pending. Package-private for testing. */
    boolean isPendingForTesting(String key) {
        return pendingKeys.contains(key);
    }

    private void runClaim(Queue.Item item, Job<?, ?> job, VarroaIntentAction action) {
        if (action == null) {
            // The dispatcher stamps an action on every item before it
            // offers a claim (ruling p), so this never runs in
            // production. It leaves the item blocked rather than
            // cancelling it for a condition the claimer did not cause.
            LOGGER.log(Level.WARNING, "VarroaClaimQueue: item carries no VarroaIntentAction; leaving it blocked");
            return;
        }
        long deadline = clock.nowMillis() + LIFETIME_MILLIS;
        int attempt = 0;
        while (true) {
            attempt++;
            IntentKey.Result derived = IntentKey.derive(item, job, action);
            action.setOrigin(derived.origin);
            action.setIntentKey(derived.intentKey);
            action.setRevision(derived.revision);

            JSONObject body = buildRequestBody(item, job, action, derived, attempt);
            MiteLoopbackClient.Response resp = client.post(CLAIM_PATH, body.toString());
            Decision decision = parseDecision(resp);

            switch (decision.kind) {
                case GRANTED:
                    if (!applyBuildNumber(job, decision.buildNumber)) {
                        cancel(item);
                        return;
                    }
                    ClaimRegistry.get().record(derived.intentKey, action.getRunId());
                    action.setGranted(true);
                    return;
                case DUPLICATE:
                    DuplicateRegistry.get().record(action.getRunId());
                    LOGGER.log(Level.WARNING, "VarroaClaimQueue: duplicate claim for run {0}", action.getRunId());
                    cancel(item);
                    return;
                case VETOED_NOT_READY:
                case RETRY:
                    break;
                case VETOED_OTHER:
                case UNKNOWN:
                default:
                    LOGGER.log(Level.WARNING, "VarroaClaimQueue: claim refused for run {0}", action.getRunId());
                    cancel(item);
                    return;
            }

            if (clock.nowMillis() >= deadline) {
                LOGGER.log(Level.WARNING,
                        "VarroaClaimQueue: giving up on pending claim for run {0} after one hour",
                        action.getRunId());
                cancel(item);
                return;
            }
            try {
                sleeper.sleep(backoffFor(attempt));
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                cancel(item);
                return;
            }
        }
    }

    // --- Build number apply (contract G3) ---

    /**
     * Applies the granted build number under the job's monitor (D4's five
     * steps). Never called from {@code canRun}: it takes the job monitor
     * and writes a file.
     */
    boolean applyBuildNumber(Job<?, ?> job, long grantedNumber) {
        synchronized (job) {
            int before = job.getNextBuildNumber();
            if (before > grantedNumber) {
                LOGGER.log(Level.WARNING,
                        "VarroaClaimQueue: NUMBER_CONFLICT, job {0} next build number {1} already exceeds the granted {2}",
                        new Object[] {job.getFullName(), before, grantedNumber});
                return false;
            }
            try {
                job.updateNextBuildNumber((int) grantedNumber);
            } catch (IOException e) {
                LOGGER.log(Level.WARNING, "VarroaClaimQueue: failed to persist the granted build number", e);
                return false;
            }
            int after = job.getNextBuildNumber();
            if (after != grantedNumber) {
                LOGGER.log(Level.WARNING,
                        "VarroaClaimQueue: NUMBER_CONFLICT, updateNextBuildNumber was a no-op for job {0}: stayed {1}, wanted {2}",
                        new Object[] {job.getFullName(), after, grantedNumber});
                return false;
            }
            return true;
        }
    }

    // --- Request body ---

    private JSONObject buildRequestBody(Queue.Item item, Job<?, ?> job, VarroaIntentAction action,
            IntentKey.Result derived, int attempt) {
        JSONObject body = new JSONObject();
        body.put("origin", derived.origin);
        body.put("intent_key", derived.intentKey);
        body.put("run_id", action.getRunId());
        body.put("job_path", job.getFullName());
        body.put("job_display_name", job.getDisplayName());
        body.put("branch", IntentKey.branchNameFor(job));
        body.put("actor", action.getActor());
        body.put("cause", IntentKey.causeDescription(item.getCauses()));
        body.put("params_digest", IntentKey.paramsDigest(item));
        body.put("attempt", attempt);
        body.put("requested_at_millis", clock.nowMillis());
        body.put("jenkins_next_build_number", job.getNextBuildNumber());
        return body;
    }

    // --- Decision parsing ---

    private enum Kind {
        GRANTED, DUPLICATE, VETOED_NOT_READY, VETOED_OTHER, RETRY, UNKNOWN
    }

    private static final class Decision {
        final Kind kind;
        final long buildNumber;

        Decision(Kind kind, long buildNumber) {
            this.kind = kind;
            this.buildNumber = buildNumber;
        }
    }

    @SuppressWarnings("unchecked")
    private Decision parseDecision(MiteLoopbackClient.Response resp) {
        if (resp.isTransportFailure()) {
            return new Decision(Kind.RETRY, 0);
        }
        Map<String, Object> body;
        try {
            Object parsed = new JsonSlurper().parseText(resp.body);
            if (!(parsed instanceof Map)) {
                return new Decision(Kind.UNKNOWN, 0);
            }
            body = (Map<String, Object>) parsed;
        } catch (Exception e) {
            return new Decision(Kind.UNKNOWN, 0);
        }

        Object rawDecision = body.get("decision");
        if (rawDecision == null) {
            return new Decision(Kind.UNKNOWN, 0);
        }
        String decision = String.valueOf(rawDecision);
        switch (decision) {
            case DECISION_GRANTED:
                return new Decision(Kind.GRANTED, toLong(body.get("build_number")));
            case DECISION_DUPLICATE:
                return new Decision(Kind.DUPLICATE, 0);
            case DECISION_VETOED:
                String reason = String.valueOf(body.get("reason_code"));
                return REASON_NOT_READY.equals(reason)
                        ? new Decision(Kind.VETOED_NOT_READY, 0)
                        : new Decision(Kind.VETOED_OTHER, 0);
            case "":
                Object code = body.get("code");
                if (code != null && !String.valueOf(code).isEmpty()) {
                    return new Decision(Kind.RETRY, 0);
                }
                return new Decision(Kind.UNKNOWN, 0);
            default:
                return new Decision(Kind.UNKNOWN, 0);
        }
    }

    private static long toLong(Object v) {
        if (v instanceof Number) {
            return ((Number) v).longValue();
        }
        try {
            return Long.parseLong(String.valueOf(v));
        } catch (NumberFormatException e) {
            return 0;
        }
    }

    // --- Cancel ---

    private void cancel(Queue.Item item) {
        try {
            // The queue replaces an item's instance as it moves from waiting
            // to blocked to buildable, and Queue.cancel(Item) removes only the
            // list its instance belongs to. Resolve the live instance by id,
            // or the cancel is a silent no-op and the item claims forever.
            Queue queue = Queue.getInstance();
            Queue.Item live = queue.getItem(item.getId());
            queue.cancel(live != null ? live : item);
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaClaimQueue: failed to cancel item", e);
        }
    }

    static long backoffFor(int attempt) {
        int index = Math.min(attempt - 1, BACKOFF_MILLIS.length - 1);
        return BACKOFF_MILLIS[Math.max(0, index)];
    }

    private static ThreadFactory daemonThreadFactory() {
        AtomicInteger counter = new AtomicInteger();
        return runnable -> {
            Thread thread = new Thread(runnable, "varroa-claim-queue-" + counter.incrementAndGet());
            thread.setDaemon(true);
            return thread;
        };
    }
}
