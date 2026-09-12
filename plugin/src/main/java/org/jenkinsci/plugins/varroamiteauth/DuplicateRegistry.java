package org.jenkinsci.plugins.varroamiteauth;

import java.util.LinkedHashMap;
import java.util.Map;
import java.util.function.LongSupplier;

/**
 * Bounded, time-limited set of duplicate run ids (D4). Only a {@code
 * duplicate} decision writes here; {@link VarroaQueueDecisionHandler}
 * reads it to refuse a queued item that already lost a claim race.
 *
 * <p>Holds at most 4096 entries, each alive for 24 hours. The oldest
 * entry is evicted first when the bound is reached.
 */
public final class DuplicateRegistry {

    private static final int CAPACITY = 4096;
    private static final long TTL_MILLIS = 24L * 60 * 60 * 1000;

    private static final DuplicateRegistry INSTANCE = new DuplicateRegistry();

    private final LinkedHashMap<String, Long> expiresAtByRunId;
    private final LongSupplier clock;

    /** Returns the process-wide singleton. */
    public static DuplicateRegistry get() {
        return INSTANCE;
    }

    DuplicateRegistry() {
        this(System::currentTimeMillis);
    }

    /** Package-private constructor for testing: injects the clock. */
    DuplicateRegistry(LongSupplier clock) {
        this.clock = clock;
        this.expiresAtByRunId = new LinkedHashMap<>(16, 0.75f, false) {
            @Override
            protected boolean removeEldestEntry(Map.Entry<String, Long> eldest) {
                return size() > CAPACITY;
            }
        };
    }

    /** Records {@code runId} as a duplicate, alive for 24 hours. */
    public synchronized void record(String runId) {
        if (runId == null || runId.isEmpty()) {
            return;
        }
        expiresAtByRunId.remove(runId);
        expiresAtByRunId.put(runId, clock.getAsLong() + TTL_MILLIS);
    }

    /** Returns {@code true} when {@code runId} is a live duplicate entry. */
    public synchronized boolean contains(String runId) {
        Long expiresAt = expiresAtByRunId.get(runId);
        if (expiresAt == null) {
            return false;
        }
        if (clock.getAsLong() >= expiresAt) {
            expiresAtByRunId.remove(runId);
            return false;
        }
        return true;
    }

    /** Number of live and expired-but-unswept entries. Package-private for testing. */
    synchronized int sizeForTesting() {
        return expiresAtByRunId.size();
    }

    /** Clears every entry. Package-private for testing. */
    synchronized void resetForTesting() {
        expiresAtByRunId.clear();
    }
}
