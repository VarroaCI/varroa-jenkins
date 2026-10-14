package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;

import java.util.Map;

import org.junit.Before;
import org.junit.Test;

/**
 * Unit tests for {@link PluginCounters}: every registered counter reports
 * under its stable snake_case name, and the two live-read counters track
 * the class that owns them.
 */
public class PluginCountersTest {

    @Before
    public void setUp() {
        PluginCounters.resetForTesting();
        VarroaRunRecorder.resetOversizedDroppedForTesting();
        LogChunkBuffer.resetStuckQueueWarningsForTesting();
    }

    @Test
    public void snapshotStartsEveryCounterAtZero() {
        Map<String, Long> snapshot = PluginCounters.snapshot();
        assertEquals(16, snapshot.size());
        for (Long value : snapshot.values()) {
            assertEquals(Long.valueOf(0L), value);
        }
    }

    @Test
    public void anOwnedCounterIncrementsUnderItsName() {
        PluginCounters.increment(PluginCounters.SPOOL_DROPPED);
        PluginCounters.increment(PluginCounters.SPOOL_DROPPED);
        PluginCounters.increment(PluginCounters.CLAIMS_NO_REVISION);

        Map<String, Long> snapshot = PluginCounters.snapshot();
        assertEquals(Long.valueOf(2L), snapshot.get(PluginCounters.SPOOL_DROPPED));
        assertEquals(Long.valueOf(1L), snapshot.get(PluginCounters.CLAIMS_NO_REVISION));
    }

    @Test
    public void theTwoLiveReadCountersTrackTheirOwningClass() {
        VarroaRunRecorder.applyCaps(oversizedEvent());
        LogChunkBuffer.resetStuckQueueWarningsForTesting();

        Map<String, Long> snapshot = PluginCounters.snapshot();
        assertEquals(Long.valueOf(VarroaRunRecorder.oversizedDroppedCount()),
                snapshot.get(PluginCounters.EVENTS_OVERSIZED));
        assertEquals(Long.valueOf(LogChunkBuffer.stuckQueueWarningCount()),
                snapshot.get(PluginCounters.LOG_QUEUES_STUCK));
    }

    // ---- The five forwarding counters ----

    @Test
    public void eachIntentKeySchemeCountsUnderItsOwnName() {
        PluginCounters.increment(PluginCounters.forwardedCounterFor("scm:payments/api:abc123"));
        PluginCounters.increment(PluginCounters.forwardedCounterFor("cron:payments/nightly:2026-09-17T03:00"));
        PluginCounters.increment(PluginCounters.forwardedCounterFor("api:0199-uuid"));
        PluginCounters.increment(PluginCounters.forwardedCounterFor("upstream:run-1:s1"));
        PluginCounters.increment(PluginCounters.forwardedCounterFor("no-scheme"));

        Map<String, Long> snapshot = PluginCounters.snapshot();
        assertEquals(Long.valueOf(1L), snapshot.get(PluginCounters.FORWARDED_SCM));
        assertEquals(Long.valueOf(1L), snapshot.get(PluginCounters.FORWARDED_CRON));
        assertEquals(Long.valueOf(1L), snapshot.get(PluginCounters.FORWARDED_API));
        assertEquals("a mite-supplied scheme and a key with no scheme both count as other",
                Long.valueOf(2L), snapshot.get(PluginCounters.FORWARDED_OTHER));
    }

    @Test
    public void aRefusedWaitingStepCountsUnderItsOwnName() {
        PluginCounters.increment(PluginCounters.FORWARD_REFUSED);

        assertEquals(Long.valueOf(1L), PluginCounters.snapshot().get(PluginCounters.FORWARD_REFUSED));
    }

    @Test
    public void noForwardingCounterNameCarriesATotalSuffix() {
        for (String name : PluginCounters.snapshot().keySet()) {
            assertFalse("counter names carry no _total suffix: " + name, name.endsWith("_total"));
        }
    }

    /** An event with an identifier field too large for the oversize ladder to shrink. */
    private static BuildEvent oversizedEvent() {
        return BuildEvent.builder()
                .phase(BuildEvent.PHASE_COMPLETED)
                .runId("run-oversized")
                .intentKey("intent-oversized")
                .jobPath("x".repeat(2 * 1024 * 1024))
                .buildNumber(1)
                .result("SUCCESS")
                .build();
    }
}
