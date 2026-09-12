package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import org.junit.Test;

/** Tests for {@link ClaimRegistry} — plain JUnit, no JenkinsRule. */
public class ClaimRegistryTest {

    @Test
    public void recordedKeyReturnsItsRunId() {
        ClaimRegistry registry = new ClaimRegistry(() -> 0L);
        registry.record("scm:folder/job:abc", "run-1");
        assertEquals("run-1", registry.runIdFor("scm:folder/job:abc"));
        assertNull(registry.runIdFor("scm:folder/job:other"));
    }

    @Test
    public void theBoundEvictsTheOldestEntry() {
        ClaimRegistry registry = new ClaimRegistry(() -> 0L);
        for (int i = 0; i < 4096; i++) {
            registry.record("key-" + i, "run-" + i);
        }
        assertEquals(4096, registry.sizeForTesting());
        assertNotNull(registry.runIdFor("key-0"));

        registry.record("key-overflow", "run-overflow");
        assertEquals(4096, registry.sizeForTesting());
        assertNull("the oldest entry should have been evicted", registry.runIdFor("key-0"));
    }

    @Test
    public void anEntryExpiresAfter24Hours() {
        long[] now = {0L};
        ClaimRegistry registry = new ClaimRegistry(() -> now[0]);
        registry.record("key-1", "run-1");

        now[0] = 24L * 60 * 60 * 1000 - 1;
        assertEquals("run-1", registry.runIdFor("key-1"));

        now[0] = 24L * 60 * 60 * 1000;
        assertNull(registry.runIdFor("key-1"));
    }
}
