package org.jenkinsci.plugins.varroamiteauth;

import groovy.json.JsonSlurper;
import hudson.model.Job;
import hudson.model.Queue;
import net.sf.json.JSONObject;

import java.io.IOException;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.OptionalInt;
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
 *
 * <p>A claimer thread also owns the intra-tenant forward. It resolves the
 * job's home slot from {@link VarroaHomeCache} before it posts the claim,
 * and forwards a job this cell does not home. A {@code NOT_HOME} veto
 * names the home slot, and the claimer forwards to that slot first. The
 * forward is a network call, so it cannot run in the queue decision
 * handler or in the dispatcher: both hold the Jenkins queue lock.
 *
 * <p>A forward keeps the item's run id. The recorder triggers the home
 * with that run id, so the run the API answered is the run that builds.
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
    private static final String REASON_NOT_HOME = "NOT_HOME";

    /**
     * The sequence a forward sends. The preserved intent key carries the
     * submission's identity, so the sequence is constant.
     */
    private static final int FORWARD_SEQ = 1;

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
    private final VarroaHomeCache homeCache;
    private final ThreadPoolExecutor executor;
    private final Set<String> pendingKeys = ConcurrentHashMap.newKeySet();

    public VarroaClaimQueue() {
        this(new MiteLoopbackClient(), System::currentTimeMillis, Thread::sleep, VarroaHomeCache.get());
    }

    /** Package-private constructor for testing: injects every collaborator. */
    VarroaClaimQueue(MiteLoopbackClient client, Clock clock, Sleeper sleeper) {
        this(client, clock, sleeper, VarroaHomeCache.get());
    }

    /** Package-private constructor for testing: injects the home cache as well. */
    VarroaClaimQueue(MiteLoopbackClient client, Clock clock, Sleeper sleeper, VarroaHomeCache homeCache) {
        this.client = client;
        this.clock = clock;
        this.sleeper = sleeper;
        this.homeCache = homeCache;
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
        LastAnswer last = new LastAnswer();
        while (true) {
            attempt++;
            IntentKey.Result derived = IntentKey.derive(item, job, action);
            action.setOrigin(derived.origin);
            action.setIntentKey(derived.intentKey);
            action.setRevision(derived.revision);

            // A refused forward proves the cached map disagrees with the
            // recorder. The next attempt claims, so the fence names the home.
            OptionalInt home = last.forwardRefused ? OptionalInt.empty()
                    : homeCache.forwardSlotFor(job.getFullName());
            last.forwardRefused = false;
            if (home.isPresent()) {
                if (forwardOnce(item, job, action, derived, home.getAsInt(), last)) {
                    return;
                }
            } else if (claimOnce(item, job, action, derived, attempt, last)) {
                return;
            }

            if (clock.nowMillis() >= deadline) {
                LOGGER.log(Level.WARNING,
                        "VarroaClaimQueue: giving up on job {0} after one hour; home slot {1}, run {2}; last answer {3}",
                        new Object[] {job.getFullName(), homeSlotLabel(job), action.getRunId(), last.text});
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

    /**
     * Posts one claim and applies the decision table. Answers true when
     * the submission is resolved, and false when the caller must retry.
     *
     * <p>A {@code NOT_HOME} veto proves the home cache was stale. It
     * refreshes the cache and forwards at once, and it cancels no item:
     * the one-hour budget bounds a repeat. It forwards to the slot the
     * veto names, else to the refreshed cache's slot. With neither, the
     * caller retries.
     */
    private boolean claimOnce(Queue.Item item, Job<?, ?> job, VarroaIntentAction action,
            IntentKey.Result derived, int attempt, LastAnswer last) {
        JSONObject body = buildRequestBody(item, job, action, derived, attempt);
        MiteLoopbackClient.Response resp = client.post(CLAIM_PATH, body.toString());
        Decision decision = parseDecision(resp);
        if (decision.kind != Kind.GRANTED) {
            last.text = decision.describe();
        }

        switch (decision.kind) {
            case GRANTED:
                if (!applyBuildNumber(job, decision.buildNumber)) {
                    cancel(item);
                    return true;
                }
                ClaimRegistry.get().record(derived.intentKey, action.getRunId());
                action.setGranted(true);
                return true;
            case DUPLICATE:
                DuplicateRegistry.get().record(action.getRunId());
                // A duplicate is an expected answer: a redelivered command
                // or a lost race. The item is cancelled quietly.
                LOGGER.log(Level.INFO, "VarroaClaimQueue: duplicate claim for run {0}; cancelled the queue item",
                        action.getRunId());
                cancel(item);
                return true;
            case VETOED_NOT_HOME:
                homeCache.refresh();
                OptionalInt target = decision.homeSlot.isPresent() ? decision.homeSlot
                        : homeCache.forwardSlotFor(job.getFullName());
                return target.isPresent() && forwardOnce(item, job, action, derived, target.getAsInt(), last);
            case VETOED_NOT_READY:
            case RETRY:
                return false;
            case VETOED_OTHER:
            case UNKNOWN:
            default:
                LOGGER.log(Level.WARNING, "VarroaClaimQueue: claim refused for job {0}, run {1}: {2}",
                        new Object[] {job.getFullName(), action.getRunId(), decision.describe()});
                cancel(item);
                return true;
        }
    }

    // --- The intra-tenant forward ---

    /**
     * Forwards one submission to the holder of {@code homeSlot} through
     * the cross-tenant build route, and cancels the local queue item on
     * success. Answers false when the forward failed, so the caller
     * retries it on the claim's own ladder.
     *
     * <p>The caller tenant and the target tenant are the same tenant, so
     * the grant allows the call with no export. The forward carries the
     * derived intent key unchanged, so two members that fire one cron in
     * one minute reach one ledger row. It carries the item's run id as
     * {@code caller_run_id}, and the recorder triggers the home with it.
     *
     * <p>A reply with a reason code, such as {@code NOT_READY} while the
     * home slot holds no cell, keeps the item. The next attempt then
     * claims instead of forwarding.
     */
    private boolean forwardOnce(Queue.Item item, Job<?, ?> job, VarroaIntentAction action,
            IntentKey.Result derived, int homeSlot, LastAnswer last) {
        String jobPath = job.getFullName();
        String tenant = VarroaHomeCache.tenantOf(jobPath);
        MiteLoopbackClient.Response resp = client.crossTenantBuild(jobPath, action.getRunId(),
                FORWARD_SEQ, tenant, VarroaHomeCache.pathBelowTenant(jobPath),
                parametersOf(item), derived.intentKey);
        VarroaCrossTenantReply reply = VarroaCrossTenantReply.parse(resp);
        if (reply.isFailure() || !reply.reasonCode().isEmpty()) {
            last.text = reply.isFailure() ? reply.describe() : reply.describeVeto();
            last.forwardRefused = !reply.isFailure();
            LOGGER.log(Level.FINE,
                    "VarroaClaimQueue: the forward of job {0} to slot {1} failed; retrying",
                    new Object[] {jobPath, homeSlot});
            return false;
        }
        PluginCounters.increment(PluginCounters.forwardedCounterFor(derived.intentKey));
        LOGGER.log(Level.INFO, "VarroaClaimQueue: forwarded job {0} to home slot {1}",
                new Object[] {jobPath, homeSlot});
        cancel(item);
        return true;
    }

    /**
     * The item's parameters, name to value. A forwarded build must keep
     * them, because the home schedules the job from this list alone.
     */
    private static Map<String, Object> parametersOf(Queue.Item item) {
        Map<String, String> values = new LinkedHashMap<>();
        IntentKey.collectParameters(item, values, new HashSet<>());
        return new LinkedHashMap<>(values);
    }

    /** The job's home slot as a log value, or {@code none} with no cached opinion. */
    private String homeSlotLabel(Job<?, ?> job) {
        OptionalInt slot = homeCache.homeSlotOf(job.getFullName());
        return slot.isPresent() ? Integer.toString(slot.getAsInt()) : "none";
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
        GRANTED, DUPLICATE, VETOED_NOT_READY, VETOED_NOT_HOME, VETOED_OTHER, RETRY, UNKNOWN
    }

    /**
     * The newest answer that did not grant a claim, as the give-up line
     * shows it. One claim runs on one thread, so the field needs no lock.
     */
    private static final class LastAnswer {
        String text = "none";
        /** True when the newest forward got a reason code back. */
        boolean forwardRefused;
    }

    private static final class Decision {
        final Kind kind;
        final long buildNumber;
        /** The failure code of a reply with no decision, or empty. */
        final String code;
        final String reasonCode;
        final String message;
        final String error;
        /** The home slot a {@code NOT_HOME} veto names, or empty. */
        final OptionalInt homeSlot;

        Decision(Kind kind, long buildNumber) {
            this(kind, buildNumber, "", "", "", "", OptionalInt.empty());
        }

        Decision(Kind kind, long buildNumber, String code, String reasonCode, String message, String error) {
            this(kind, buildNumber, code, reasonCode, message, error, OptionalInt.empty());
        }

        Decision(Kind kind, long buildNumber, String code, String reasonCode, String message, String error,
                OptionalInt homeSlot) {
            this.kind = kind;
            this.buildNumber = buildNumber;
            this.code = code;
            this.reasonCode = reasonCode;
            this.message = message;
            this.error = error;
            this.homeSlot = homeSlot;
        }

        /** Builds a decision of {@code kind} that carries the reply's own reason and text fields. */
        static Decision of(Kind kind, long buildNumber, Map<String, Object> body) {
            return new Decision(kind, buildNumber, stringOf(body.get("code")), stringOf(body.get("reason_code")),
                    stringOf(body.get("message")), stringOf(body.get("error")), homeSlotOf(body.get("home_slot")));
        }

        /** Reads {@code home_slot}: a non-negative number, else empty. */
        private static OptionalInt homeSlotOf(Object v) {
            if (!(v instanceof Number)) {
                return OptionalInt.empty();
            }
            int slot = ((Number) v).intValue();
            return slot >= 0 ? OptionalInt.of(slot) : OptionalInt.empty();
        }

        /**
         * The answer as {@code <reason>: <text>}. The reason is the reason
         * code, else the failure code, else {@code UNKNOWN}. The text is the
         * message, else the error. An empty text leaves the reason alone.
         */
        String describe() {
            String reason = !reasonCode.isEmpty() ? reasonCode : !code.isEmpty() ? code : "UNKNOWN";
            String text = !message.isEmpty() ? message : error;
            return text == null || text.isEmpty() ? reason : reason + ": " + text;
        }

        private static String stringOf(Object v) {
            return v == null ? "" : String.valueOf(v);
        }
    }

    @SuppressWarnings("unchecked")
    private Decision parseDecision(MiteLoopbackClient.Response resp) {
        if (resp.isTransportFailure()) {
            return new Decision(Kind.RETRY, 0, VarroaCrossTenantReply.CODE_UNAVAILABLE, "", "", resp.error);
        }
        Map<String, Object> body;
        try {
            Object parsed = new JsonSlurper().parseText(resp.body);
            if (!(parsed instanceof Map)) {
                return new Decision(Kind.UNKNOWN, 0, "", "", "", "the reply is not a JSON object");
            }
            body = (Map<String, Object>) parsed;
        } catch (Exception e) {
            return new Decision(Kind.UNKNOWN, 0, "", "", "", "the reply did not decode");
        }

        // The mite encodes an empty decision with omitempty, so a failure
        // body holds no decision key. A missing key and an empty value
        // must reach the same failure-code test.
        Object rawDecision = body.get("decision");
        String decision = rawDecision == null ? "" : String.valueOf(rawDecision);
        switch (decision) {
            case DECISION_GRANTED:
                return new Decision(Kind.GRANTED, toLong(body.get("build_number")));
            case DECISION_DUPLICATE:
                return new Decision(Kind.DUPLICATE, 0);
            case DECISION_VETOED:
                String reason = String.valueOf(body.get("reason_code"));
                if (REASON_NOT_READY.equals(reason)) {
                    return Decision.of(Kind.VETOED_NOT_READY, 0, body);
                }
                if (REASON_NOT_HOME.equals(reason)) {
                    return Decision.of(Kind.VETOED_NOT_HOME, 0, body);
                }
                return Decision.of(Kind.VETOED_OTHER, 0, body);
            case "":
                Object code = body.get("code");
                if (code != null && !String.valueOf(code).isEmpty()) {
                    return Decision.of(Kind.RETRY, 0, body);
                }
                return Decision.of(Kind.UNKNOWN, 0, body);
            default:
                return new Decision(Kind.UNKNOWN, 0, "", "", "", "unknown decision " + decision);
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
