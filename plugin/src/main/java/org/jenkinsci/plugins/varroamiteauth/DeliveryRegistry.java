package org.jenkinsci.plugins.varroamiteauth;

import java.util.LinkedHashMap;
import java.util.Map;
import java.util.function.LongSupplier;

/**
 * Bounded, time-limited record of {@code X-Varroa-Delivery} header values
 * and their arrival time (D5). {@link VarroaDeliveryFilter} writes an
 * entry on every request; the claim gate reads the newest live entry to
 * attach a {@code delivery_id} to a branch or SCM-triggered item.
 *
 * <p>Holds at most 256 entries, each alive for five minutes. The oldest
 * entry is evicted first when the bound is reached.
 */
public final class DeliveryRegistry {

    static final int CAPACITY = 256;
    static final long TTL_MILLIS = 5L * 60 * 1000;

    private static final DeliveryRegistry INSTANCE = new DeliveryRegistry();

    private final LinkedHashMap<String, Long> arrivalByValue;
    private final LongSupplier clock;

    /** Returns the process-wide singleton. */
    public static DeliveryRegistry get() {
        return INSTANCE;
    }

    DeliveryRegistry() {
        this(System::currentTimeMillis);
    }

    /** Package-private constructor for testing: injects the clock. */
    DeliveryRegistry(LongSupplier clock) {
        this.clock = clock;
        this.arrivalByValue = new LinkedHashMap<>(16, 0.75f, false) {
            @Override
            protected boolean removeEldestEntry(Map.Entry<String, Long> eldest) {
                return size() > CAPACITY;
            }
        };
    }

    /** Records {@code value}'s arrival at the current time. */
    public synchronized void record(String value) {
        if (value == null || value.isEmpty()) {
            return;
        }
        arrivalByValue.remove(value);
        arrivalByValue.put(value, clock.getAsLong());
    }

    /**
     * Returns the most recently recorded value still inside the five-minute
     * window, or {@code null} when none is live.
     */
    public synchronized String newest() {
        long now = clock.getAsLong();
        String result = null;
        for (Map.Entry<String, Long> entry : arrivalByValue.entrySet()) {
            if (now - entry.getValue() < TTL_MILLIS) {
                // Insertion order: the last live entry seen is the newest.
                result = entry.getKey();
            }
        }
        return result;
    }

    /** Number of entries currently held. Package-private for testing. */
    synchronized int sizeForTesting() {
        return arrivalByValue.size();
    }

    /** Clears every entry. Package-private for testing. */
    synchronized void resetForTesting() {
        arrivalByValue.clear();
    }
}
