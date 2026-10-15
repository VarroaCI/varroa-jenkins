package org.jenkinsci.plugins.varroamiteauth;

import java.util.ArrayList;
import java.util.Iterator;
import java.util.List;
import java.util.concurrent.ArrayBlockingQueue;
import java.util.concurrent.atomic.AtomicLong;

/**
 * Process-wide bounded, non-blocking queue of {@link BuildEvent}s, shaped
 * like {@link ActivitySink}.
 *
 * <p>Unlike {@link ActivitySink}, this sink evicts nothing to make room.
 * Contract F forbids a drop-oldest policy for build events. A full sink
 * drops a {@code HEARTBEAT} event, counts it in the dropped counter, and
 * refuses every other event. {@link #offer(BuildEvent)} tells the caller
 * whether the sink took the event, so a caller can spool a refused event
 * itself instead.
 */
public final class BuildEventSink {

    private static final int DEFAULT_CAPACITY = 4096;
    private static final int MIN_CAPACITY = 16;
    private static final int MAX_CAPACITY = 65536;

    private static final BuildEventSink INSTANCE = new BuildEventSink(readCapacity());

    private final ArrayBlockingQueue<BuildEvent> queue;
    private final AtomicLong dropped;

    /** Returns the process-wide singleton. */
    public static BuildEventSink get() {
        return INSTANCE;
    }

    /**
     * Constructs a sink with the given capacity. Package-private for
     * testing. The public singleton is created via {@link #get()}.
     */
    BuildEventSink(int capacity) {
        int clamped = Math.max(MIN_CAPACITY, Math.min(MAX_CAPACITY, capacity));
        this.queue = new ArrayBlockingQueue<>(clamped);
        this.dropped = new AtomicLong(0);
    }

    /**
     * Non-blocking enqueue. Returns {@code true} when the sink accepted the
     * event. When the sink is full and the event is a {@code HEARTBEAT},
     * the sink drops it and counts the drop. When the sink is full and the
     * event is anything else, the sink refuses it and evicts nothing.
     */
    public boolean offer(BuildEvent event) {
        if (event == null) {
            return false;
        }
        boolean accepted = queue.offer(event);
        if (!accepted && event.isHeartbeat()) {
            dropped.incrementAndGet();
            PluginCounters.increment(PluginCounters.EVENTS_DROPPED);
        }
        return accepted;
    }

    /**
     * Drains up to {@code max} events from the buffer and returns them
     * along with the current dropped count (which is reset to zero).
     */
    public DrainResult drain(int max) {
        int clamped = Math.max(1, Math.min(4096, max));
        List<BuildEvent> events = new ArrayList<>(clamped);
        queue.drainTo(events, clamped);
        long d = dropped.getAndSet(0);
        return new DrainResult(events, d);
    }

    /** Clears the queue and resets the dropped counter. Package-private for testing. */
    void resetForTesting() {
        queue.clear();
        dropped.set(0);
    }

    /**
     * Removes and returns every currently queued event of {@code runId},
     * in FIFO order, leaving every other run's events in place.
     *
     * <p>A caller uses this only on the overflow path: when a new event
     * of a run finds the sink full, it must first move any of that same
     * run's events still waiting here into the durable spool, in order,
     * before spooling the new one — otherwise a later spool replay could
     * post the new (newer) event while an older sibling still sits
     * unspooled in this queue (D11).
     */
    List<BuildEvent> drainRun(String runId) {
        List<BuildEvent> removed = new ArrayList<>();
        if (runId == null) {
            return removed;
        }
        // ArrayBlockingQueue's iterator is safe for concurrent removal
        // and walks the queue in FIFO (oldest-first) order.
        Iterator<BuildEvent> it = queue.iterator();
        while (it.hasNext()) {
            BuildEvent event = it.next();
            if (runId.equals(event.getRunId())) {
                removed.add(event);
                it.remove();
            }
        }
        return removed;
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

    // --- Result type ---

    /** Result of a {@link #drain(int)} call. */
    public static final class DrainResult {
        public final List<BuildEvent> events;
        public final long dropped;

        public DrainResult(List<BuildEvent> events, long dropped) {
            this.events = events;
            this.dropped = dropped;
        }
    }

    // --- Capacity from environment ---

    private static int readCapacity() {
        String raw = System.getenv("VARROA_BUILD_BUFFER");
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
