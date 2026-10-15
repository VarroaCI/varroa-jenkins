package org.jenkinsci.plugins.varroamiteauth;

import java.util.LinkedHashMap;
import java.util.Map;
import java.util.function.LongSupplier;

/**
 * Bounded, time-limited map from intent key to the run id a {@code
 * granted} decision returned for it (D1, D4). The claimer records an
 * entry after every grant, so a lost reply's retry — which repeats the
 * same intent key and run id — can be told apart from a genuinely new
 * claim on that key.
 *
 * <p>Holds at most 4096 entries, each alive for 24 hours. The oldest
 * entry is evicted first when the bound is reached.
 */
public final class ClaimRegistry {

    private static final int CAPACITY = 4096;
    private static final long TTL_MILLIS = 24L * 60 * 60 * 1000;

    private static final ClaimRegistry INSTANCE = new ClaimRegistry();

    private final LinkedHashMap<String, Entry> byIntentKey;
    private final LongSupplier clock;

    /** Returns the process-wide singleton. */
    public static ClaimRegistry get() {
        return INSTANCE;
    }

    ClaimRegistry() {
        this(System::currentTimeMillis);
    }

    /** Package-private constructor for testing: injects the clock. */
    ClaimRegistry(LongSupplier clock) {
        this.clock = clock;
        this.byIntentKey = new LinkedHashMap<>(16, 0.75f, false) {
            @Override
            protected boolean removeEldestEntry(Map.Entry<String, Entry> eldest) {
                return size() > CAPACITY;
            }
        };
    }

    /** Records that {@code intentKey} was granted {@code runId}. */
    public synchronized void record(String intentKey, String runId) {
        if (intentKey == null || intentKey.isEmpty()) {
            return;
        }
        byIntentKey.remove(intentKey);
        byIntentKey.put(intentKey, new Entry(runId, clock.getAsLong() + TTL_MILLIS));
    }

    /** Returns the run id granted for {@code intentKey}, or {@code null} when there is none live. */
    public synchronized String runIdFor(String intentKey) {
        Entry entry = byIntentKey.get(intentKey);
        if (entry == null) {
            return null;
        }
        if (clock.getAsLong() >= entry.expiresAtMillis) {
            byIntentKey.remove(intentKey);
            return null;
        }
        return entry.runId;
    }

    /** Number of live and expired-but-unswept entries. Package-private for testing. */
    synchronized int sizeForTesting() {
        return byIntentKey.size();
    }

    /** Clears every entry. Package-private for testing. */
    synchronized void resetForTesting() {
        byIntentKey.clear();
    }

    private static final class Entry {
        final String runId;
        final long expiresAtMillis;

        Entry(String runId, long expiresAtMillis) {
            this.runId = runId;
            this.expiresAtMillis = expiresAtMillis;
        }
    }
}
