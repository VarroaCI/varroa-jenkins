package org.jenkinsci.plugins.varroamiteauth;

import java.util.Map;
import java.util.TreeMap;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicLong;

/**
 * Process-wide, thread-safe registry of the plugin's operational counters.
 *
 * <p>{@link TailEndpoint} serves a snapshot of every counter as one JSON
 * document at {@code /varroa-log/counters}. Each name is stable and
 * snake_case, with one integer value.
 *
 * <p>Two counters read a value that another class already keeps for
 * itself and never resets outside a test: {@link
 * VarroaRunRecorder#oversizedDroppedCount()} and {@link
 * LogChunkBuffer#stuckQueueWarningCount()}. This class registers no
 * duplicate state for those two. The remaining four counters live here,
 * because the class that produces each one either resets its own local
 * count on drain ({@link BuildEventSink}) or keeps it per instance across
 * several live instances ({@link RunSpool}), and neither shape serves a
 * lifetime total on its own.
 */
final class PluginCounters {

    static final String EVENTS_OVERSIZED = "varroa_plugin_events_oversized";
    static final String EVENTS_DROPPED = "varroa_plugin_events_dropped";
    static final String SPOOL_DROPPED = "varroa_plugin_spool_dropped";
    static final String LOG_CHUNKS_DROPPED = "varroa_plugin_log_chunks_dropped";
    static final String LOG_QUEUES_STUCK = "varroa_plugin_log_queues_stuck";
    static final String CLAIMS_NO_REVISION = "varroa_plugin_claims_no_revision";
    static final String RUN_STATE_PUSHED = "varroa_plugin_run_state_pushed";
    static final String RUN_STATE_REFUSED = "varroa_plugin_run_state_refused";

    private static final ConcurrentHashMap<String, AtomicLong> OWNED = new ConcurrentHashMap<>();

    private PluginCounters() {
    }

    /** Adds one to the named counter. Creates the counter at zero on first use. */
    static void increment(String name) {
        OWNED.computeIfAbsent(name, k -> new AtomicLong()).incrementAndGet();
    }

    /**
     * Every counter's current value, keyed by its stable snake_case name,
     * sorted for a deterministic response. Reads {@link
     * VarroaRunRecorder} and {@link LogChunkBuffer} live, so their values
     * never drift out of step with this snapshot.
     */
    static Map<String, Long> snapshot() {
        Map<String, Long> out = new TreeMap<>();
        out.put(EVENTS_OVERSIZED, VarroaRunRecorder.oversizedDroppedCount());
        out.put(EVENTS_DROPPED, ownedValue(EVENTS_DROPPED));
        out.put(SPOOL_DROPPED, ownedValue(SPOOL_DROPPED));
        out.put(LOG_CHUNKS_DROPPED, ownedValue(LOG_CHUNKS_DROPPED));
        out.put(LOG_QUEUES_STUCK, LogChunkBuffer.stuckQueueWarningCount());
        out.put(CLAIMS_NO_REVISION, ownedValue(CLAIMS_NO_REVISION));
        out.put(RUN_STATE_PUSHED, ownedValue(RUN_STATE_PUSHED));
        out.put(RUN_STATE_REFUSED, ownedValue(RUN_STATE_REFUSED));
        return out;
    }

    private static long ownedValue(String name) {
        AtomicLong counter = OWNED.get(name);
        return counter != null ? counter.get() : 0L;
    }

    /** Clears every counter this class owns. Package-private for testing. */
    static void resetForTesting() {
        OWNED.clear();
    }
}
