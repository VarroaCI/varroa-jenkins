package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Job;
import hudson.model.Queue;
import hudson.model.Run;
import hudson.model.TaskListener;
import hudson.model.listeners.RunListener;
import hudson.model.queue.QueueListener;

import java.util.ArrayDeque;
import java.util.Deque;
import java.util.HashMap;
import java.util.Iterator;
import java.util.Map;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.function.LongSupplier;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Applies one granted build number of a job at a time.
 *
 * <p>A grant only moves the job's counter, and Jenkins numbers a
 * dispatched item from that counter. Two grants of one job that apply
 * before a queue pass would therefore give the first item the second
 * number. The gate holds, for each job full name, one run id: the run of
 * the grant that applied last. The next grant of the job waits until that
 * run starts ({@link RunListener#onStarted}), until its item leaves the
 * queue cancelled, or until {@link #HOLD_MILLIS} pass. A timeout logs a
 * {@code WARNING}.
 *
 * <p>The gate opens on {@code onStarted}: Jenkins assigns the number in
 * the {@code Run} constructor, before that call, and a Pipeline run never
 * fires {@code onInitialize}.
 *
 * <p>Every apply runs on the gate's own daemon thread, never under the
 * queue lock: an apply takes the job monitor and writes a file.
 */
public final class BuildNumberGate {

    private static final Logger LOGGER = Logger.getLogger(BuildNumberGate.class.getName());

    /** A holder that does not start within this time opens the gate. */
    static final long HOLD_MILLIS = 60_000L;

    /** How often the gate checks for a holder past its time. */
    static final long CHECK_MILLIS = 5_000L;

    /** One apply. Answers true when its run keeps the gate until it starts. */
    interface Apply {
        boolean run();
    }

    private static final BuildNumberGate INSTANCE = new BuildNumberGate(System::currentTimeMillis, true);

    /** Returns the process-wide gate. */
    public static BuildNumberGate get() {
        return INSTANCE;
    }

    private final LongSupplier clock;
    private final ScheduledExecutorService executor;
    private final Map<String, Holder> holders = new HashMap<>();
    private final Map<String, Deque<Pending>> waiting = new HashMap<>();

    /** Package-private constructor for testing: injects the clock; {@code checkTimeouts} starts the timer. */
    BuildNumberGate(LongSupplier clock, boolean checkTimeouts) {
        this.clock = clock;
        this.executor = Executors.newSingleThreadScheduledExecutor(runnable -> {
            Thread thread = new Thread(runnable, "varroa-build-number-gate");
            thread.setDaemon(true);
            return thread;
        });
        if (checkTimeouts) {
            executor.scheduleWithFixedDelay(this::expire, CHECK_MILLIS, CHECK_MILLIS, TimeUnit.MILLISECONDS);
        }
    }

    /** Queues {@code apply} for {@code runId} of {@code jobName}; it runs once the job has no holder. */
    void submit(String jobName, String runId, Apply apply) {
        synchronized (this) {
            waiting.computeIfAbsent(jobName, k -> new ArrayDeque<>()).add(new Pending(runId, apply));
            pump(jobName);
        }
    }

    /**
     * Opens the gate when {@code runId} holds it, and drops a waiting
     * apply of {@code runId}. Any other call changes nothing.
     */
    synchronized void release(String jobName, String runId) {
        Deque<Pending> queue = waiting.get(jobName);
        if (queue != null) {
            queue.removeIf(p -> p.runId.equals(runId));
        }
        Holder holder = holders.get(jobName);
        if (holder != null && holder.runId.equals(runId)) {
            holders.remove(jobName);
        }
        pump(jobName);
    }

    /** Opens every gate whose holder is past {@link #HOLD_MILLIS}. Package-private for testing. */
    synchronized void expire() {
        long now = clock.getAsLong();
        for (Iterator<Map.Entry<String, Holder>> it = holders.entrySet().iterator(); it.hasNext(); ) {
            Map.Entry<String, Holder> e = it.next();
            if (now - e.getValue().since >= HOLD_MILLIS) {
                LOGGER.log(Level.WARNING,
                        "BuildNumberGate: run {0} of job {1} did not start within {2} ms of its grant; the next grant applies",
                        new Object[] {e.getValue().runId, e.getKey(), HOLD_MILLIS});
                it.remove();
                pumpLater(e.getKey());
            }
        }
    }

    /** The run id that holds the gate of {@code jobName}, or null. Package-private for testing. */
    synchronized String holderForTesting(String jobName) {
        Holder holder = holders.get(jobName);
        return holder != null ? holder.runId : null;
    }

    private void pumpLater(String jobName) {
        executor.execute(() -> {
            synchronized (this) {
                pump(jobName);
            }
        });
    }

    private void pump(String jobName) {
        if (holders.containsKey(jobName)) {
            return;
        }
        Deque<Pending> queue = waiting.get(jobName);
        if (queue == null || queue.isEmpty()) {
            waiting.remove(jobName);
            return;
        }
        Pending next = queue.poll();
        holders.put(jobName, new Holder(next.runId, clock.getAsLong()));
        executor.execute(() -> runApply(jobName, next));
    }

    private void runApply(String jobName, Pending pending) {
        boolean keep;
        try {
            keep = pending.apply.run();
        } catch (Throwable t) {
            LOGGER.log(Level.WARNING, "BuildNumberGate: apply failed for run " + pending.runId, t);
            keep = false;
        }
        if (!keep) {
            open(jobName, pending.runId);
        }
    }

    /**
     * Opens the gate after an apply that kept nothing. It keeps a waiting
     * apply of the same run: a new claim can submit one before this call.
     */
    private synchronized void open(String jobName, String runId) {
        Holder holder = holders.get(jobName);
        if (holder != null && holder.runId.equals(runId)) {
            holders.remove(jobName);
        }
        pump(jobName);
    }

    private static final class Holder {
        final String runId;
        final long since;

        Holder(String runId, long since) {
            this.runId = runId;
            this.since = since;
        }
    }

    private static final class Pending {
        final String runId;
        final Apply apply;

        Pending(String runId, Apply apply) {
            this.runId = runId;
            this.apply = apply;
        }
    }

    /**
     * Opens the gate when the holder's run starts, and logs a {@code
     * WARNING} when the run's number differs from its grant.
     */
    @Extension
    public static class Starts extends RunListener<Run<?, ?>> {
        @Override
        public void onStarted(Run<?, ?> run, TaskListener listener) {
            VarroaIntentAction action = run.getAction(VarroaIntentAction.class);
            if (action == null || action.getRunId().isEmpty()) {
                return;
            }
            String jobName = run.getParent().getFullName();
            long granted = action.getGrantedNumber();
            if (granted > 0 && run.getNumber() != granted) {
                LOGGER.log(Level.WARNING,
                        "BuildNumberGate: run {0} of job {1} started as #{2}, but its grant was {3}",
                        new Object[] {action.getRunId(), jobName, run.getNumber(), granted});
            }
            get().release(jobName, action.getRunId());
        }
    }

    /** Opens the gate, and drops a waiting apply, when a cancelled item leaves the queue. */
    @Extension
    public static class Leaves extends QueueListener {
        @Override
        public void onLeft(Queue.LeftItem li) {
            if (!li.isCancelled() || !(li.task instanceof Job)) {
                return;
            }
            VarroaIntentAction action = li.getAction(VarroaIntentAction.class);
            if (action == null || action.getRunId().isEmpty()) {
                return;
            }
            get().release(((Job<?, ?>) li.task).getFullName(), action.getRunId());
        }
    }
}
