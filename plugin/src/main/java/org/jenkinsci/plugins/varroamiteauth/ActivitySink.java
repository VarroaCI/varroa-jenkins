package org.jenkinsci.plugins.varroamiteauth;

import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.ArrayBlockingQueue;
import java.util.concurrent.atomic.AtomicLong;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Process-wide bounded, non-blocking event sink backed by an
 * {@link ArrayBlockingQueue}. Listeners enqueue events via {@link #offer(ActivityEvent)};
 * the mite-side drain endpoint retrieves them via {@link #drain(int)}.
 *
 * <p>On overflow the oldest event is dropped and a {@code dropped} counter is
 * incremented. The enqueue is O(1), pure in-memory, and never blocks the caller.
 */
public final class ActivitySink {

    private static final Logger LOGGER = Logger.getLogger(ActivitySink.class.getName());

    private static final int DEFAULT_CAPACITY = 1024;
    private static final int MIN_CAPACITY = 16;
    private static final int MAX_CAPACITY = 65536;

    private static final ActivitySink INSTANCE = new ActivitySink(readCapacity());

    private final ArrayBlockingQueue<ActivityEvent> queue;
    private final AtomicLong dropped;

    /**
     * Unix epoch millis of the newest event ever offered to this sink.
     * Updated on every successful (non-dropped) offer. Survives drains —
     * never reset. Volatile for lock-free reads from the mite poller.
     */
    private volatile long lastEventUnixMillis;

    /** Returns the process-wide singleton. */
    public static ActivitySink get() {
        return INSTANCE;
    }

    /**
     * Constructs a sink with the given capacity. Package-private for testing.
     * The public singleton is created via {@link #get()}.
     */
    ActivitySink(int capacity) {
        int clamped = Math.max(MIN_CAPACITY, Math.min(MAX_CAPACITY, capacity));
        this.queue = new ArrayBlockingQueue<>(clamped);
        this.dropped = new AtomicLong(0);
    }

    /**
     * Non-blocking enqueue. If the buffer is full, the oldest event is dropped
     * and the {@code dropped} counter is incremented. Never throws; exceptions
     * are logged at {@link Level#FINE}.
     */
    public void offer(ActivityEvent event) {
        if (event == null) {
            return;
        }
        try {
            boolean accepted = queue.offer(event);
            if (!accepted) {
                queue.poll();
                dropped.incrementAndGet();
                accepted = queue.offer(event);
            }
            // Record the timestamp only when the event was actually buffered, so
            // lastEventUnix reflects real event activity — advancing it on a
            // dropped/failed offer would make the operator's idle-gauge logic see
            // phantom "recent activity" and keep an idle controller awake.
            // We use System.currentTimeMillis() rather than parsing the event's
            // RFC3339 timestamp to avoid formatting and parse overhead on the
            // hot enqueue path.
            if (accepted) {
                lastEventUnixMillis = System.currentTimeMillis();
            }
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "ActivitySink.offer caught unexpected exception", e);
        }
    }

    /**
     * Drains up to {@code max} events from the buffer and returns them along
     * with the current dropped count (which is reset to zero).
     */
    public DrainResult drain(int max) {
        int clamped = Math.max(1, Math.min(1024, max));
        List<ActivityEvent> events = new ArrayList<>(clamped);
        queue.drainTo(events, clamped);
        long d = dropped.getAndSet(0);
        return new DrainResult(events, d);
    }

    /**
     * Clears the queue and resets the dropped counter. Package-private for
     * testing; called on hot-reload.
     */
    void resetForTesting() {
        queue.clear();
        dropped.set(0);
        lastEventUnixMillis = 0;
    }

    /**
     * Returns the Unix epoch seconds of the newest event ever offered,
     * or 0 if no event has been offered since construction/reset.
     */
    public long getLastEventUnix() {
        return lastEventUnixMillis / 1000L;
    }

    // --- Package-private accessors for testing ---

    int queueSize() {
        return queue.size();
    }

    int remainingCapacity() {
        return queue.remainingCapacity();
    }

    long droppedCount() {
        return dropped.get();
    }

    /**
     * Returns the raw millis timestamp for testing precision.
     */
    long getLastEventUnixMillis() {
        return lastEventUnixMillis;
    }

    // --- Result type ---

    /**
     * Result of a {@link #drain(int)} call: the drained events and the number
     * of events dropped since the last successful drain.
     */
    public static final class DrainResult {
        public final List<ActivityEvent> events;
        public final long dropped;

        public DrainResult(List<ActivityEvent> events, long dropped) {
            this.events = events;
            this.dropped = dropped;
        }
    }

    // --- Capacity from environment ---

    private static int readCapacity() {
        String raw = System.getenv("VARROA_ACTIVITY_BUFFER");
        if (raw == null || raw.isEmpty()) {
            return DEFAULT_CAPACITY;
        }
        try {
            return Math.max(MIN_CAPACITY, Math.min(MAX_CAPACITY, Integer.parseInt(raw)));
        } catch (NumberFormatException e) {
            return DEFAULT_CAPACITY;
        }
    }
}
