package org.jenkinsci.plugins.varroamiteauth;

import groovy.json.JsonSlurper;
import net.sf.json.JSONArray;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Deque;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ConcurrentLinkedDeque;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Daemon thread that drains {@link BuildEventSink}, spools each event
 * before posting it, and posts batches to the mite loopback
 * {@code /v1/events} (D11).
 *
 * <p>Durability for a retry comes from {@link RunSpool}, written before
 * every post, not from re-offering events to the bounded sink. On a
 * failed post the dispatcher drops any {@code HEARTBEAT} event from the
 * retry and keeps retrying the rest forever, with a backoff that grows
 * through 1, 2, 5, 10, and 20 seconds to 30 seconds.
 *
 * <p>{@link #noteSpooled(String)} announces a run whose spool holds an
 * event the dispatcher has not posted — written by {@link
 * VarroaRunRecorder} on the sink-full path, or seeded at startup by
 * {@link VarroaBuildEventLifecycle} from every spool directory that
 * already existed. Every pass posts the oldest announced run's backlog,
 * in spool order, before any newer event of that run (ruling r).
 */
public class BuildEventDispatcher implements Runnable {

    private static final Logger LOGGER = Logger.getLogger(BuildEventDispatcher.class.getName());

    static final String EVENTS_PATH = "/v1/events";

    /** Events drained per pass. */
    static final int DRAIN_SIZE = 64;

    /** Serialized-body cap per post: 512 KiB. */
    static final int POST_CAP_BYTES = 512 * 1024;

    /** Idle wait when the sink is empty. */
    static final long IDLE_WAIT_MILLIS = 200L;

    /** Retry backoff ladder; the last value repeats forever. */
    static final long[] BACKOFF_MILLIS = {1000L, 2000L, 5000L, 10_000L, 20_000L, 30_000L};

    /** {@code 202 Accepted}: the mite took the batch. */
    private static final int STATUS_ACCEPTED = 202;

    /** {@code 503}: the stream is down, or retention is full — retry later. */
    private static final int STATUS_UNAVAILABLE = 503;

    private final BuildEventSink sink;
    private final RunSpool spool;
    private final MiteLoopbackClient client;
    private final Sleeper sleeper;
    private final AtomicBoolean stopped = new AtomicBoolean(false);

    /** Runs with a spool backlog the dispatcher has not fully posted, oldest announced first. */
    private final Deque<String> announcedOrder = new ConcurrentLinkedDeque<>();
    private final Set<String> announcedSet = ConcurrentHashMap.newKeySet();

    private volatile Thread thread;

    public BuildEventDispatcher() {
        this(BuildEventSink.get(), new RunSpool(), new MiteLoopbackClient(), BuildEventDispatcher::threadSleep);
    }

    /** Package-private constructor for testing: injects every collaborator, including the sleeper. */
    BuildEventDispatcher(BuildEventSink sink, RunSpool spool, MiteLoopbackClient client, Sleeper sleeper) {
        this.sink = sink;
        this.spool = spool;
        this.client = client;
        this.sleeper = sleeper;
    }

    /** Starts the daemon thread. Idempotent: a second call is a no-op. */
    public synchronized void start() {
        if (thread != null) {
            return;
        }
        stopped.set(false);
        thread = new Thread(this, "varroa-build-event-dispatcher");
        thread.setDaemon(true);
        thread.start();
    }

    /** Signals the loop to stop after its current step and interrupts a wait. */
    public synchronized void stop() {
        stopped.set(true);
        if (thread != null) {
            thread.interrupt();
        }
        thread = null;
    }

    /** {@code true} once {@link #stop()} has run. Package-private for testing. */
    boolean isStoppedForTesting() {
        return stopped.get();
    }

    /** Snapshot of the announced runs, oldest first. Package-private for testing. */
    List<String> announcedForTesting() {
        return new ArrayList<>(announcedOrder);
    }

    @Override
    public void run() {
        while (!stopped.get()) {
            runOnce();
        }
    }

    /**
     * Records that {@code runId} has a spool file this dispatcher has not
     * yet posted. A later pass posts that run's whole backlog, in spool
     * order, before any newer event of the same run. Repeated calls for
     * the same run id, still announced, are a no-op.
     */
    void noteSpooled(String runId) {
        if (runId == null || runId.isEmpty()) {
            return;
        }
        if (announcedSet.add(runId)) {
            announcedOrder.addLast(runId);
        }
    }

    /**
     * Runs one drain-and-post pass. Package-private so a test can step the
     * loop deterministically instead of racing a background thread.
     */
    void runOnce() {
        if (postAnnouncedBacklog()) {
            return;
        }
        BuildEventSink.DrainResult result = sink.drain(DRAIN_SIZE);
        if (result.events.isEmpty()) {
            sleepQuietly(IDLE_WAIT_MILLIS);
            return;
        }
        List<PostableEvent> toPostNow = new ArrayList<>();
        for (BuildEvent event : result.events) {
            spool.append(event.getRunId(), event);
            if (!event.isHeartbeat() && announcedSet.contains(event.getRunId())) {
                // An older, unacknowledged spool backlog for this run is
                // still replaying. Leave this event in the spool; a later
                // pass over the backlog posts it in order.
                continue;
            }
            toPostNow.add(event);
        }
        for (List<PostableEvent> group : split(toPostNow)) {
            postWithRetry(group);
            if (stopped.get()) {
                return;
            }
        }
    }

    /**
     * Posts the oldest announced run's unacknowledged spool files, in
     * spool order. Returns {@code true} when this pass did that work —
     * posted a backlog, or found an announced run's backlog already
     * gone — so the caller skips the idle sink drain and loops again at
     * once.
     */
    private boolean postAnnouncedBacklog() {
        String runId = announcedOrder.peekFirst();
        if (runId == null) {
            return false;
        }
        List<RunSpool.Entry> entries = spool.readOrdered(runId);
        if (entries.isEmpty()) {
            clearAnnounced(runId);
            return true;
        }
        List<PostableEvent> events = new ArrayList<>(entries.size());
        for (RunSpool.Entry entry : entries) {
            events.add(new SpooledEvent(runId, entry));
        }
        for (List<PostableEvent> group : split(events)) {
            postWithRetry(group);
            if (stopped.get()) {
                return true;
            }
        }
        if (spool.readOrdered(runId).isEmpty()) {
            clearAnnounced(runId);
        }
        return true;
    }

    private void clearAnnounced(String runId) {
        announcedOrder.remove(runId);
        announcedSet.remove(runId);
    }

    /** Splits a drained batch into groups whose serialized body stays under {@link #POST_CAP_BYTES}. */
    static List<List<PostableEvent>> split(List<? extends PostableEvent> events) {
        List<List<PostableEvent>> groups = new ArrayList<>();
        List<PostableEvent> current = new ArrayList<>();
        int currentBytes = 2; // "[]"
        for (PostableEvent event : events) {
            int eventBytes = event.toJson().toString().getBytes(StandardCharsets.UTF_8).length;
            if (!current.isEmpty() && currentBytes + eventBytes + 1 > POST_CAP_BYTES) {
                groups.add(current);
                current = new ArrayList<>();
                currentBytes = 2;
            }
            current.add(event);
            currentBytes += eventBytes + 1;
        }
        if (!current.isEmpty()) {
            groups.add(current);
        }
        return groups;
    }

    /**
     * Posts one group, retrying forever on failure. A {@code HEARTBEAT}
     * event is dropped from the retry payload, never retried.
     */
    private void postWithRetry(List<PostableEvent> group) {
        List<PostableEvent> pending = new ArrayList<>(group);
        int attempt = 0;
        boolean firstAttempt = true;
        while (!stopped.get() && !pending.isEmpty()) {
            // A HEARTBEAT event rides the first attempt like any other
            // event, but never survives a retry: it carries no spool file
            // to make the retry durable, and it is not worth one.
            if (!firstAttempt) {
                pending.removeIf(PostableEvent::isHeartbeat);
                if (pending.isEmpty()) {
                    return;
                }
            }
            firstAttempt = false;
            JSONArray body = new JSONArray();
            for (PostableEvent e : pending) {
                body.add(e.toJson());
            }
            MiteLoopbackClient.Response resp = client.post(EVENTS_PATH, body.toString());
            if (resp.statusCode == STATUS_ACCEPTED && !resp.isTransportFailure()) {
                Set<AckKey> acked = parseAcks(resp.body);
                pending.removeIf(e -> acked.contains(new AckKey(e.getRunId(), e.getSeq())));
                if (pending.isEmpty()) {
                    return;
                }
            } else if (resp.statusCode == STATUS_UNAVAILABLE) {
                LOGGER.log(Level.FINE, "BuildEventDispatcher: mite reported 503, retrying later");
            } else {
                LOGGER.log(Level.FINE, "BuildEventDispatcher: post failed, status="
                        + resp.statusCode + " error=" + resp.error);
            }
            attempt++;
            sleepQuietly(backoffFor(attempt));
        }
    }

    /**
     * Acknowledges every event in the ack body against the spool, then
     * retires each acked run's spool directory once its own files are
     * gone. A run's directory is never removed on the strength of a
     * single acked event — including its {@code COMPLETED} event — only
     * once nothing unacknowledged is left on disk for it, so a sibling
     * still awaiting its own ack always survives to retry.
     */
    @SuppressWarnings("unchecked")
    private Set<AckKey> parseAcks(String body) {
        Set<AckKey> acked = new HashSet<>();
        if (body == null || body.isEmpty()) {
            return acked;
        }
        try {
            Object parsed = new JsonSlurper().parseText(body);
            if (!(parsed instanceof List)) {
                return acked;
            }
            Set<String> ackedRunIds = new HashSet<>();
            for (Object item : (List<Object>) parsed) {
                if (!(item instanceof Map)) {
                    continue;
                }
                Map<String, Object> ack = (Map<String, Object>) item;
                String runId = String.valueOf(ack.get("run_id"));
                long seq = toLong(ack.get("event_seq"));
                acked.add(new AckKey(runId, seq));
                ackedRunIds.add(runId);
                spool.acknowledge(runId, seq);
            }
            for (String runId : ackedRunIds) {
                spool.deleteRunIfEmpty(runId);
            }
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "BuildEventDispatcher: could not parse event-ack body", e);
        }
        return acked;
    }

    private static long toLong(Object v) {
        if (v instanceof Number) {
            return ((Number) v).longValue();
        }
        try {
            return Long.parseLong(String.valueOf(v));
        } catch (NumberFormatException e) {
            return -1;
        }
    }

    static long backoffFor(int attempt) {
        int index = Math.min(attempt - 1, BACKOFF_MILLIS.length - 1);
        return BACKOFF_MILLIS[Math.max(0, index)];
    }

    private void sleepQuietly(long millis) {
        try {
            sleeper.sleep(millis);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

    private static void threadSleep(long millis) throws InterruptedException {
        Thread.sleep(millis);
    }

    /** Seam for a test to skip real wall-clock waits. */
    interface Sleeper {
        void sleep(long millis) throws InterruptedException;
    }

    /** Wraps one {@link RunSpool.Entry} so a replayed spool file posts through the same path as a live event. */
    private static final class SpooledEvent implements PostableEvent {
        private final String runId;
        private final RunSpool.Entry entry;

        SpooledEvent(String runId, RunSpool.Entry entry) {
            this.runId = runId;
            this.entry = entry;
        }

        @Override
        public String getRunId() {
            return runId;
        }

        @Override
        public long getSeq() {
            return entry.seq;
        }

        @Override
        public boolean isHeartbeat() {
            return BuildEvent.PHASE_HEARTBEAT.equals(entry.phase);
        }

        @Override
        public boolean isCompletedPhase() {
            return BuildEvent.PHASE_COMPLETED.equals(entry.phase);
        }

        @Override
        public net.sf.json.JSONObject toJson() {
            return entry.toJsonObject();
        }
    }

    private static final class AckKey {
        private final String runId;
        private final long seq;

        AckKey(String runId, long seq) {
            this.runId = runId;
            this.seq = seq;
        }

        @Override
        public boolean equals(Object o) {
            if (!(o instanceof AckKey)) {
                return false;
            }
            AckKey other = (AckKey) o;
            return seq == other.seq && runId.equals(other.runId);
        }

        @Override
        public int hashCode() {
            return runId.hashCode() * 31 + Long.hashCode(seq);
        }
    }
}
