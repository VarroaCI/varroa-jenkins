package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import org.junit.Test;

/** Tests for {@link DeliveryRegistry} — plain JUnit, no JenkinsRule. */
public class DeliveryRegistryTest {

    @Test
    public void theBoundEvictsTheOldestEntry() {
        long[] now = {0L};
        DeliveryRegistry registry = new DeliveryRegistry(() -> now[0]);
        for (int i = 0; i < DeliveryRegistry.CAPACITY; i++) {
            now[0] = i;
            registry.record("delivery-" + i);
        }
        assertEquals(DeliveryRegistry.CAPACITY, registry.sizeForTesting());

        now[0] = DeliveryRegistry.CAPACITY;
        registry.record("delivery-overflow");
        assertEquals(DeliveryRegistry.CAPACITY, registry.sizeForTesting());
    }

    @Test
    public void anEntryIsNoLongerTheNewestAfterFiveMinutes() {
        long[] now = {0L};
        DeliveryRegistry registry = new DeliveryRegistry(() -> now[0]);
        registry.record("delivery-1");
        assertEquals("delivery-1", registry.newest());

        now[0] = DeliveryRegistry.TTL_MILLIS;
        assertNull("expired at exactly five minutes", registry.newest());
    }

    @Test
    public void newestEntryRuleReturnsTheLatestArrival() {
        long[] now = {0L};
        DeliveryRegistry registry = new DeliveryRegistry(() -> now[0]);
        registry.record("delivery-old");
        now[0] = 1000L;
        registry.record("delivery-new");

        assertEquals("delivery-new", registry.newest());
    }

    @Test
    public void reRecordingAnEntryMakesItTheNewestAgain() {
        long[] now = {0L};
        DeliveryRegistry registry = new DeliveryRegistry(() -> now[0]);
        registry.record("delivery-a");
        now[0] = 1000L;
        registry.record("delivery-b");
        now[0] = 2000L;
        registry.record("delivery-a"); // re-arrives, now the newest

        assertEquals("delivery-a", registry.newest());
    }

    @Test
    public void emptyRegistryHasNoNewestEntry() {
        DeliveryRegistry registry = new DeliveryRegistry(() -> 0L);
        assertNull(registry.newest());
    }
}
