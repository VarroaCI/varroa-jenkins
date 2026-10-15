package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import org.junit.Test;

/** Tests for {@link DuplicateRegistry} — plain JUnit, no JenkinsRule. */
public class DuplicateRegistryTest {

    @Test
    public void recordedRunIdIsFound() {
        DuplicateRegistry registry = new DuplicateRegistry(() -> 0L);
        registry.record("run-1");
        assertTrue(registry.contains("run-1"));
        assertFalse(registry.contains("run-2"));
    }

    @Test
    public void theBoundEvictsTheOldestEntry() {
        DuplicateRegistry registry = new DuplicateRegistry(() -> 0L);
        for (int i = 0; i < 4096; i++) {
            registry.record("run-" + i);
        }
        assertEquals(4096, registry.sizeForTesting());
        assertTrue(registry.contains("run-0"));

        registry.record("run-overflow");
        assertEquals(4096, registry.sizeForTesting());
        assertFalse("the oldest entry should have been evicted", registry.contains("run-0"));
        assertTrue(registry.contains("run-overflow"));
    }

    @Test
    public void anEntryExpiresAfter24Hours() {
        long[] now = {0L};
        DuplicateRegistry registry = new DuplicateRegistry(() -> now[0]);
        registry.record("run-1");
        assertTrue(registry.contains("run-1"));

        now[0] = 24L * 60 * 60 * 1000 - 1;
        assertTrue("still alive just under 24h", registry.contains("run-1"));

        now[0] = 24L * 60 * 60 * 1000;
        assertFalse("expired at exactly 24h", registry.contains("run-1"));
    }

    @Test
    public void nullOrEmptyRunIdIsIgnored() {
        DuplicateRegistry registry = new DuplicateRegistry(() -> 0L);
        registry.record(null);
        registry.record("");
        assertEquals(0, registry.sizeForTesting());
    }
}
