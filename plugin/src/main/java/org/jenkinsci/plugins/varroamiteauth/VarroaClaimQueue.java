package org.jenkinsci.plugins.varroamiteauth;

import groovy.json.JsonSlurper;
import hudson.model.Cause;
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
import java.util.concurrent.ScheduledThreadPoolExecutor;
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
 * {@code ClaimRequest}, and applies the decision table. A grant goes to
 * {@link BuildNumberGate}, which applies one grant of a job at a time:
 * Jenkins numbers a dispatched item from the job's counter, so a second
 * applied grant would give the first item the second number. The apply
 * sets the build number (D4's five steps, contract G3). A grant below the
 * job's counter keeps the item and claims again with {@code
 * number_conflict}.
 *
 * <p>After the number applies, a claimer thread posts the start mark: the
 * same claim route, with {@code start_mark} set. Only a {@code granted}
 * answer sets the action's granted flag, which releases the dispatcher.
 * The ledger therefore holds a mark for every build that can start, and
 * an intent with no mark proves that its build did not start. The gate
 * thread never posts the mark, because one slow round trip there would
 * hold the grants of every job. A {@code duplicate} answer cancels the
 * item. Any other answer opens the gate, leaves the item blocked, and
 * defers the next claim by the backoff step of its failure count.
 *
 * <p>A claimer thread runs one attempt of a claim and then releases the
 * thread. An answer that retries parks the claim on the
 * {@code varroa-claim-retry} scheduler thread for the next ladder step.
 * When the step ends, the scheduler hands the claim back to the claimer
 * pool. A waiting claim thus holds no claimer thread, and a refused claim
 * does not delay the claims of other jobs. A hand-back that the full work
 * queue refuses waits one more second, bounded by the claim's deadline.
 *
 * <p>A branch-caused item can wait up to 30 s for its accepted revision.
 * Each attempt reads the branch project factory once. While the revision
 * is not accepted, the attempt sends no request and parks the claim on
 * the same scheduler for two seconds. The revision wait thus holds no
 * claimer thread either. A revision poll is not a claim post, so it does
 * not advance the ladder or the request's {@code attempt} field.
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
 * <p>A {@code CAPPED} veto means a tenant or cell concurrency cap holds
 * the run. The claim waits at a fixed interval and has no lifetime limit:
 * each capped answer restarts the one-hour budget. It never cancels the item.
 *
 * <p>A {@code NOT_READY} veto waits at most {@link #NOT_READY_RETRY_CAP_MILLIS}
 * before the next claim. Transport failures keep the full
 * {@link #BACKOFF_MILLIS} ladder. Both kinds keep the one-hour lifetime.
 */
public class VarroaClaimQueue {

    private static final Logger LOGGER = Logger.getLogger(VarroaClaimQueue.class.getName());

    static final String CLAIM_PATH = "/v1/claim";

    /** The cap on pending claims, parked and queued together. */
    static final int CAPACITY = 256;

    /** Daemon claimer threads. */
    static final int THREADS = 4;

    /** Retry backoff ladder; the last value repeats forever. */
    static final long[] BACKOFF_MILLIS = {1000L, 2000L, 5000L, 10_000L, 30_000L, 60_000L};

    /** A pending claim gives up after one hour. */
    static final long LIFETIME_MILLIS = 60L * 60 * 1000;

    /**
     * The fixed wait between claims of a run that a concurrency cap holds.
     * A capped claim keeps no lifetime limit, so the interval never backs off.
     */
    static final long CAPPED_RETRY_MILLIS = 10_000L;

    /**
     * The longest wait after a {@code NOT_READY} veto. A NOT_READY hold can
     * end at any time, so the claimer asks again at most every 10 seconds.
     */
    static final long NOT_READY_RETRY_CAP_MILLIS = 10_000L;

    /** The wait before a hand-back that the full work queue refused tries again. */
    static final long HANDBACK_RETRY_MILLIS = 1000L;

    private static final String DECISION_GRANTED = "granted";
    private static final String DECISION_DUPLICATE = "duplicate";
    private static final String DECISION_VETOED = "vetoed";
    private static final String REASON_NOT_READY = "NOT_READY";
    private static final String REASON_NOT_HOME = "NOT_HOME";
    private static final String REASON_CAPPED = "CAPPED";

    /**
     * The sequence a forward sends. The preserved intent key carries the
     * submission's identity, so the sequence is constant.
     */
    private static final int FORWARD_SEQ = 1;

    /**
     * Runs a task after a delay. A claim waits here between two attempts,
     * so the wait holds no claimer thread. A test runs the task at once.
     */
    interface Scheduler {
        /**
         * Runs {@code task} after {@code delayMillis}. Throws
         * {@link RejectedExecutionException} after {@link #shutdownNow()}.
         */
        void schedule(Runnable task, long delayMillis);

        /** Drops every waiting task. */
        default void shutdownNow() {
        }
    }

    /** Millisecond clock seam so a test can pin the retry timing. */
    interface Clock {
        long nowMillis();
    }

    /**
     * Derives the origin and the intent key of a claim. Answers
     * {@code null} while a branch-caused item waits for its revision.
     * The production deriver is {@link IntentKey#derive(Queue.Item, Job,
     * VarroaIntentAction, long, long)}.
     */
    interface Deriver {
        IntentKey.Result derive(Queue.Item item, Job<?, ?> job, VarroaIntentAction action, long nowMillis,
                long revisionDeadlineMillis);
    }

    private final MiteLoopbackClient client;
    private final Clock clock;
    private final Scheduler scheduler;
    private final VarroaHomeCache homeCache;
    private final ThreadPoolExecutor executor;
    private final int capacity;
    private final Set<String> pendingKeys = ConcurrentHashMap.newKeySet();
    private final BuildNumberGate gate;
    private final Deriver deriver;

    public VarroaClaimQueue() {
        this(new MiteLoopbackClient(), System::currentTimeMillis, retryScheduler(), VarroaHomeCache.get());
    }

    /** Package-private constructor for testing: injects every collaborator. */
    VarroaClaimQueue(MiteLoopbackClient client, Clock clock, Scheduler scheduler) {
        this(client, clock, scheduler, VarroaHomeCache.get());
    }

    /** Package-private constructor for testing: injects the home cache as well. */
    VarroaClaimQueue(MiteLoopbackClient client, Clock clock, Scheduler scheduler, VarroaHomeCache homeCache) {
        this(client, clock, scheduler, homeCache, BuildNumberGate.get());
    }

    /** Package-private constructor for testing: injects the apply gate as well. */
    VarroaClaimQueue(MiteLoopbackClient client, Clock clock, Scheduler scheduler, VarroaHomeCache homeCache,
            BuildNumberGate gate) {
        this(client, clock, scheduler, homeCache, gate, CAPACITY);
    }

    /** Package-private constructor for testing: sets the pending cap as well. */
    VarroaClaimQueue(MiteLoopbackClient client, Clock clock, Scheduler scheduler, VarroaHomeCache homeCache,
            BuildNumberGate gate, int capacity) {
        this(client, clock, scheduler, homeCache, gate, capacity, IntentKey::derive);
    }

    /** Package-private constructor for testing: injects the deriver as well. */
    VarroaClaimQueue(MiteLoopbackClient client, Clock clock, Scheduler scheduler, VarroaHomeCache homeCache,
            BuildNumberGate gate, int capacity, Deriver deriver) {
        this.gate = gate;
        this.deriver = deriver;
        this.client = client;
        this.clock = clock;
        this.scheduler = scheduler;
        this.homeCache = homeCache;
        this.capacity = capacity;
        this.executor = new ThreadPoolExecutor(THREADS, THREADS, 0L, TimeUnit.MILLISECONDS,
                new ArrayBlockingQueue<>(capacity), daemonThreadFactory(),
                new ThreadPoolExecutor.AbortPolicy());
    }

    /**
     * Offers a pending claim for {@code item}. Returns {@code true} when
     * the queue accepted the offer or a claim for the same job and run id
     * is already pending; {@code false} when the pending cap is reached.
     */
    public boolean offer(Queue.Item item, Job<?, ?> job) {
        return offer(item, job, null);
    }

    /**
     * Offers a pending claim for {@code item}, as {@link #offer(Queue.Item, Job)},
     * and runs {@code onResolved} once the claim resolves: granted,
     * cancelled, given up after one hour, left the queue, or after an
     * unexpected claimer error (ruling q). {@code onResolved} never runs
     * when the queue refuses the offer, because no claim started. The
     * caller may pass {@code null} for no callback.
     *
     * <p>The cap counts every pending claim, parked on the scheduler or
     * queued for a claimer thread.
     */
    public boolean offer(Queue.Item item, Job<?, ?> job, Runnable onResolved) {
        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);
        String runId = action != null ? action.getRunId() : "";
        String key = job.getFullName() + "#" + runId;
        synchronized (pendingKeys) {
            if (pendingKeys.contains(key)) {
                return true;
            }
            if (pendingKeys.size() >= capacity) {
                return false;
            }
            pendingKeys.add(key);
        }
        PendingClaim pc = new PendingClaim(item, job, action, key, onResolved);
        try {
            executor.execute(() -> runAttempt(pc));
            return true;
        } catch (RejectedExecutionException e) {
            pendingKeys.remove(key);
            return false;
        }
    }

    /**
     * Stops the four claimer threads and the retry scheduler, and drops
     * every pending claim. {@link VarroaBuildEventLifecycle} calls this
     * from its {@code @Terminator} (ruling r). Idempotent.
     *
     * <p>A dropped claim cancels no queue item. Jenkins saves the blocked
     * item with its {@link VarroaIntentAction}, and after a restart the
     * dispatcher offers the claim again with the same run id.
     */
    public void shutdown() {
        executor.shutdownNow();
        scheduler.shutdownNow();
        pendingKeys.clear();
    }

    /** {@code true} once {@link #shutdown()} has run. Package-private for testing. */
    boolean isShutdownForTesting() {
        return executor.isShutdown();
    }

    /** {@code true} when a claim for {@code key} is still pending. Package-private for testing. */
    boolean isPendingForTesting(String key) {
        return pendingKeys.contains(key);
    }

    /** The number of claimer threads that run an attempt now. Package-private for testing. */
    int activeClaimersForTesting() {
        return executor.getActiveCount();
    }

    /**
     * The state of one claim between two attempts. One attempt runs at a
     * time, and each hand-back goes through an executor, which orders the
     * writes of one attempt before the reads of the next. The fields thus
     * need no lock.
     */
    private static final class PendingClaim {
        final Queue.Item item;
        final Job<?, ?> job;
        final VarroaIntentAction action;
        final String key;
        final Runnable onResolved;
        final LastAnswer last = new LastAnswer();
        boolean started;
        long deadline;
        long revisionDeadline;
        int attempt;

        PendingClaim(Queue.Item item, Job<?, ?> job, VarroaIntentAction action, String key, Runnable onResolved) {
            this.item = item;
            this.job = job;
            this.action = action;
            this.key = key;
            this.onResolved = onResolved;
        }
    }

    /** Runs one attempt on a claimer thread, and resolves the claim unless the attempt parked it. */
    private void runAttempt(PendingClaim pc) {
        boolean resolved = true;
        try {
            resolved = attemptOnce(pc);
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "VarroaClaimQueue: claimer thread failed", e);
            cancel(pc.item);
        } finally {
            if (resolved) {
                resolve(pc);
            }
        }
    }

    /** Removes the pending key and runs the callback. Runs once per claim, at the final outcome. */
    private void resolve(PendingClaim pc) {
        pendingKeys.remove(pc.key);
        if (pc.onResolved != null) {
            pc.onResolved.run();
        }
    }

    /**
     * Hands a parked claim back to the claimer pool. Runs on the
     * scheduler thread. A full work queue parks the claim for one more
     * second, and the claim's deadline bounds the repeat. The pending cap
     * equals the work queue's size, so a full queue is a guard only: it
     * occurs only if the two sizes differ.
     */
    private void dispatch(PendingClaim pc) {
        try {
            executor.execute(() -> runAttempt(pc));
        } catch (RejectedExecutionException e) {
            if (executor.isShutdown()) {
                resolve(pc);
                return;
            }
            if (clock.nowMillis() >= pc.deadline) {
                giveUp(pc);
                resolve(pc);
                return;
            }
            if (!park(pc, HANDBACK_RETRY_MILLIS)) {
                resolve(pc);
            }
        }
    }

    /** Parks the claim for {@code delayMillis}. Answers false when the scheduler is shut down. */
    private boolean park(PendingClaim pc, long delayMillis) {
        try {
            scheduler.schedule(() -> dispatch(pc), delayMillis);
            return true;
        } catch (RejectedExecutionException e) {
            return false;
        }
    }

    private void giveUp(PendingClaim pc) {
        LOGGER.log(Level.WARNING,
                "VarroaClaimQueue: giving up on job {0} after one hour; home slot {1}, run {2}; last answer {3}",
                new Object[] {pc.job.getFullName(), homeSlotLabel(pc.job), pc.action.getRunId(), pc.last.text});
        cancel(pc.item);
    }

    /**
     * Runs one attempt of the claim. Answers true when the claim is
     * resolved, and false when the claim is parked for its next attempt.
     * An item that left the queue resolves the claim with no request.
     */
    private boolean attemptOnce(PendingClaim pc) {
        Queue.Item item = pc.item;
        Job<?, ?> job = pc.job;
        VarroaIntentAction action = pc.action;
        if (action == null) {
            // The dispatcher stamps an action on every item before it
            // offers a claim (ruling p), so this never runs in
            // production. It leaves the item blocked rather than
            // cancelling it for a condition the claimer did not cause.
            LOGGER.log(Level.WARNING, "VarroaClaimQueue: item carries no VarroaIntentAction; leaving it blocked");
            return true;
        }
        if (!stillQueued(item)) {
            LOGGER.log(Level.FINE, "VarroaClaimQueue: the item of job {0}, run {1} left the queue; claiming stops",
                    new Object[] {job.getFullName(), action.getRunId()});
            return true;
        }
        long now = clock.nowMillis();
        if (!pc.started) {
            pc.started = true;
            pc.deadline = now + LIFETIME_MILLIS;
            pc.revisionDeadline = now + IntentKey.WAIT_DEADLINE_MILLIS;
        }
        IntentKey.Result derived = deriver.derive(item, job, action, now, pc.revisionDeadline);
        if (derived == null) {
            if (now >= pc.deadline) {
                giveUp(pc);
                return true;
            }
            return !park(pc, IntentKey.WAIT_INTERVAL_MILLIS);
        }
        pc.attempt++;
        LastAnswer last = pc.last;
        action.setOrigin(derived.origin);
        action.setIntentKey(derived.intentKey);
        action.setRevision(derived.revision);
        action.setForgeRevision(derived.forgeRevision);

        // A refused forward proves the cached map disagrees with the
        // recorder. The next attempt claims, so the fence names the home.
        OptionalInt home = last.forwardRefused || pinnedToThisCell(item) ? OptionalInt.empty()
                : homeCache.forwardSlotFor(job.getFullName());
        last.forwardRefused = false;
        last.notReady = false;
        if (home.isPresent()) {
            if (forwardOnce(item, job, action, derived, home.getAsInt(), last)) {
                return true;
            }
        } else if (claimOnce(item, job, action, derived, pc.attempt, last)) {
            return true;
        }

        if (last.capped) {
            // A cap holds the run, so the wait restarts on every answer. The
            // run retries at a fixed interval and never gives up.
            last.capped = false;
            pc.attempt = 0;
            pc.deadline = clock.nowMillis() + LIFETIME_MILLIS;
            LOGGER.log(Level.FINE, "VarroaClaimQueue: job {0}, run {1} waits on a concurrency cap: {2}",
                    new Object[] {job.getFullName(), action.getRunId(), last.text});
            return !park(pc, CAPPED_RETRY_MILLIS);
        }

        if (clock.nowMillis() >= pc.deadline) {
            giveUp(pc);
            return true;
        }
        long delay = backoffFor(pc.attempt);
        if (last.notReady) {
            delay = Math.min(delay, NOT_READY_RETRY_CAP_MILLIS);
            last.notReady = false;
        }
        return !park(pc, delay);
    }

    /**
     * Posts one claim and applies the decision table. Answers true when
     * the submission is resolved, and false when the caller must retry.
     *
     * <p>A {@code NOT_HOME} veto proves the home cache was stale. It
     * refreshes the cache and forwards at once, and it cancels no item:
     * the one-hour budget bounds a repeat. It forwards to the slot the
     * veto names, else to the refreshed cache's slot. With neither, the
     * caller retries. A replay or a restart item is the exception: it
     * never forwards, so a {@code NOT_HOME} veto cancels it.
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
                long granted = decision.buildNumber;
                String intentKey = derived.intentKey;
                action.setApplyPending(true);
                gate.submit(job.getFullName(), action.getRunId(),
                        () -> applyGrant(item, job, action, intentKey, granted));
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
                if (pinnedToThisCell(item)) {
                    LOGGER.log(Level.WARNING,
                            "VarroaClaimQueue: the home of job {0} is another slot; run {1} replays or restarts on this cell only; cancelled the queue item",
                            new Object[] {job.getFullName(), action.getRunId()});
                    cancel(item);
                    return true;
                }
                OptionalInt target = decision.homeSlot.isPresent() ? decision.homeSlot
                        : homeCache.forwardSlotFor(job.getFullName());
                return target.isPresent() && forwardOnce(item, job, action, derived, target.getAsInt(), last);
            case VETOED_NOT_READY:
                last.notReady = true;
                return false;
            case RETRY:
                return false;
            case VETOED_CAPPED:
                last.capped = true;
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

    /**
     * True when the item must run on this cell or not at all. A replay or
     * a restart holds actions that a forward cannot carry, and a restart
     * needs the source build on this cell.
     */
    private static boolean pinnedToThisCell(Queue.Item item) {
        for (Cause cause : item.getCauses()) {
            if (cause instanceof VarroaReplayCause || cause instanceof VarroaRestartCause) {
                return true;
            }
        }
        return false;
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
     * Applies one grant on the gate's thread. Answers true when the run
     * keeps the gate until it starts, and false when the gate opens at
     * once: the item left the queue, or the number did not apply. A number
     * below the counter keeps the item blocked; the dispatcher then offers a
     * new claim that names the number as {@code number_conflict}. A failed
     * save cancels the item, because a new claim cannot fix the disk.
     */
    boolean applyGrant(Queue.Item item, Job<?, ?> job, VarroaIntentAction action, String intentKey, long granted) {
        boolean marking = false;
        try {
            if (!stillQueued(item)) {
                return false;
            }
            ApplyResult applied = applyNumber(job, granted, action.getRunId());
            if (applied == ApplyResult.PERSIST_FAILED) {
                cancel(item);
                return false;
            }
            if (applied == ApplyResult.NUMBER_CONFLICT) {
                action.setNumberConflict(granted);
                return false;
            }
            action.setGrantedNumber(granted);
            action.setNumberConflict(0);
            marking = submitStartMark(item, job, action, intentKey);
            if (!marking) {
                deferNextClaim(action);
            }
            return marking;
        } finally {
            if (!marking) {
                action.setApplyPending(false);
                scheduleMaintenance();
            }
        }
    }

    // --- Start mark ---

    /**
     * Hands the start mark to the claimer pool. Answers false when the
     * pool refuses the task; the caller then opens the gate.
     */
    private boolean submitStartMark(Queue.Item item, Job<?, ?> job, VarroaIntentAction action, String intentKey) {
        try {
            executor.execute(() -> markStart(item, job, action, intentKey));
            return true;
        } catch (RejectedExecutionException e) {
            LOGGER.log(Level.FINE, "VarroaClaimQueue: the claimer pool refused the start mark of run {0}",
                    action.getRunId());
            return false;
        }
    }

    /**
     * Posts the start mark of a run whose number applied, on a claimer
     * thread. Only a {@code granted} answer that arrives while this run
     * still holds the apply gate sets the granted flag; the gate then stays
     * held until the run starts. A {@code duplicate} answer
     * cancels the item. Every other outcome opens the gate, leaves the item
     * blocked, and defers the next claim.
     */
    void markStart(Queue.Item item, Job<?, ?> job, VarroaIntentAction action, String intentKey) {
        boolean marked = false;
        try {
            if (!stillQueued(item)) {
                return;
            }
            MiteLoopbackClient.Response resp = client.post(CLAIM_PATH, startMarkBody(job, action, intentKey).toString());
            Decision decision = parseDecision(resp);
            switch (decision.kind) {
                case GRANTED:
                    // The flag is set under the gate's lock, and only while
                    // this run still holds the gate. An expired gate can
                    // already have applied the next grant of the job, so
                    // the item must claim again for a number it can use.
                    marked = gate.runWhileHolding(job.getFullName(), action.getRunId(), () -> {
                        action.clearMarkFailures();
                        ClaimRegistry.get().record(intentKey, action.getRunId());
                        action.setGranted(true);
                    });
                    if (!marked) {
                        LOGGER.log(Level.WARNING,
                                "VarroaClaimQueue: the apply gate of job {0} expired before the start mark of run {1} answered; claiming again",
                                new Object[] {job.getFullName(), action.getRunId()});
                        deferNextClaim(action);
                    }
                    return;
                case DUPLICATE:
                    DuplicateRegistry.get().record(action.getRunId());
                    LOGGER.log(Level.INFO,
                            "VarroaClaimQueue: the start mark of run {0} answered duplicate; cancelled the queue item",
                            action.getRunId());
                    cancel(item);
                    return;
                default:
                    LOGGER.log(Level.WARNING,
                            "VarroaClaimQueue: the start mark of job {0}, run {1} failed: {2}; claiming again",
                            new Object[] {job.getFullName(), action.getRunId(), decision.describe()});
                    deferNextClaim(action);
                    return;
            }
        } catch (RuntimeException e) {
            LOGGER.log(Level.WARNING, "VarroaClaimQueue: the start mark of run " + action.getRunId()
                    + " failed; claiming again", e);
            deferNextClaim(action);
        } finally {
            // The gate opens before the item can claim again, and the open
            // keeps a waiting apply of this run, so no retry is lost.
            if (!marked) {
                gate.reopen(job.getFullName(), action.getRunId());
            }
            action.setApplyPending(false);
            scheduleMaintenance();
        }
    }

    /** Defers the item's next claim by the backoff step of its new start-mark failure count. */
    private void deferNextClaim(VarroaIntentAction action) {
        int failures = action.addMarkFailure();
        action.setClaimNotBeforeMillis(clock.nowMillis() + backoffFor(failures));
    }

    private static JSONObject startMarkBody(Job<?, ?> job, VarroaIntentAction action, String intentKey) {
        JSONObject body = new JSONObject();
        body.put("origin", action.getOrigin());
        body.put("intent_key", intentKey);
        body.put("run_id", action.getRunId());
        body.put("job_path", job.getFullName());
        body.put("start_mark", true);
        return body;
    }

    /** True unless the item already left the queue. With no Jenkins, the answer is true. */
    private static boolean stillQueued(Queue.Item item) {
        try {
            Queue.Item live = Queue.getInstance().getItem(item.getId());
            return live != null && !(live instanceof Queue.LeftItem);
        } catch (Exception e) {
            return true;
        }
    }

    private static void scheduleMaintenance() {
        try {
            Queue.getInstance().scheduleMaintenance();
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaClaimQueue: failed to schedule a queue pass", e);
        }
    }

    /**
     * Applies the granted build number under the job's monitor (D4's five
     * steps). Never called from {@code canRun}: it takes the job monitor
     * and writes a file.
     */
    boolean applyBuildNumber(Job<?, ?> job, long grantedNumber) {
        return applyNumber(job, grantedNumber, "") == ApplyResult.APPLIED;
    }

    /** The outcome of one apply. */
    enum ApplyResult {
        APPLIED,
        /** The job's counter passed the grant; a new claim with {@code number_conflict} fixes it. */
        NUMBER_CONFLICT,
        /** The {@code nextBuildNumber} file did not save; a new claim cannot fix it. */
        PERSIST_FAILED
    }

    /** As {@link #applyBuildNumber(Job, long)}, names {@code runId} in the log line, and tells the two failures apart. */
    ApplyResult applyNumber(Job<?, ?> job, long grantedNumber, String runId) {
        synchronized (job) {
            int before = job.getNextBuildNumber();
            if (before > grantedNumber) {
                LOGGER.log(Level.WARNING,
                        "VarroaClaimQueue: NUMBER_CONFLICT, job {0} run {1} next build number {2} already exceeds the granted {3}; claiming again",
                        new Object[] {job.getFullName(), runId, before, grantedNumber});
                return ApplyResult.NUMBER_CONFLICT;
            }
            try {
                job.updateNextBuildNumber((int) grantedNumber);
            } catch (IOException e) {
                LOGGER.log(Level.WARNING, "VarroaClaimQueue: failed to persist the granted build number for run "
                        + runId + "; cancelled the queue item", e);
                return ApplyResult.PERSIST_FAILED;
            }
            int after = job.getNextBuildNumber();
            if (after != grantedNumber) {
                LOGGER.log(Level.WARNING,
                        "VarroaClaimQueue: NUMBER_CONFLICT, updateNextBuildNumber was a no-op for job {0} run {1}: stayed {2}, wanted {3}; claiming again",
                        new Object[] {job.getFullName(), runId, after, grantedNumber});
                return ApplyResult.NUMBER_CONFLICT;
            }
            return ApplyResult.APPLIED;
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
        if (action.getNumberConflict() > 0) {
            body.put("number_conflict", action.getNumberConflict());
        }
        return body;
    }

    // --- Decision parsing ---

    private enum Kind {
        GRANTED, DUPLICATE, VETOED_NOT_READY, VETOED_NOT_HOME, VETOED_CAPPED, VETOED_OTHER, RETRY, UNKNOWN
    }

    /**
     * The newest answer that did not grant a claim, as the give-up line
     * shows it. One attempt of a claim runs at a time, and each hand-back
     * goes through an executor, so the field needs no lock.
     */
    private static final class LastAnswer {
        String text = "none";
        /** True when the newest forward got a reason code back. */
        boolean forwardRefused;
        /** True when the newest claim answered CAPPED. */
        boolean capped;
        /** True when the newest claim answered NOT_READY. Reset on every attempt. */
        boolean notReady;
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
                if (REASON_CAPPED.equals(reason)) {
                    return Decision.of(Kind.VETOED_CAPPED, 0, body);
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

    /** The production scheduler: one daemon thread that only hands a parked claim back to the pool. */
    private static Scheduler retryScheduler() {
        ScheduledThreadPoolExecutor timer = new ScheduledThreadPoolExecutor(1, runnable -> {
            Thread thread = new Thread(runnable, "varroa-claim-retry");
            thread.setDaemon(true);
            return thread;
        });
        timer.setRemoveOnCancelPolicy(true);
        return new Scheduler() {
            @Override
            public void schedule(Runnable task, long delayMillis) {
                timer.schedule(task, delayMillis, TimeUnit.MILLISECONDS);
            }

            @Override
            public void shutdownNow() {
                timer.shutdownNow();
            }
        };
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
