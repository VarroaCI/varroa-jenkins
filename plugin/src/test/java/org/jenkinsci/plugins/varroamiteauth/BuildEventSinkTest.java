package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import java.util.List;
import org.junit.Test;

/** Tests for {@link BuildEventSink} — plain JUnit, no JenkinsRule. */
public class BuildEventSinkTest {

    /** The sink clamps capacity to at least 16, mirroring ActivitySink. */
    private static final int CAPACITY = 16;

    private static BuildEvent event(String phase, String runId) {
        return BuildEvent.builder().phase(phase).runId(runId).build();
    }

    private static BuildEvent event(String phase, String runId, long seq) {
        return BuildEvent.builder().phase(phase).runId(runId).seq(seq).build();
    }

    @Test
    public void fullSinkDropsAHeartbeatAndCountsIt() {
        BuildEventSink sink = new BuildEventSink(CAPACITY);
        for (int i = 0; i < CAPACITY; i++) {
            assertTrue(sink.offer(event(BuildEvent.PHASE_STARTED, "run-" + i)));
        }
        assertEquals(CAPACITY, sink.queueSize());

        boolean accepted = sink.offer(event(BuildEvent.PHASE_HEARTBEAT, "run-hb"));
        assertFalse(accepted);
        assertEquals(1, sink.droppedCount());
        // The heartbeat is dropped, not evicted into the queue.
        assertEquals(CAPACITY, sink.queueSize());
    }

    @Test
    public void fullSinkRefusesANonHeartbeatAndEvictsNothing() {
        BuildEventSink sink = new BuildEventSink(CAPACITY);
        for (int i = 0; i < CAPACITY; i++) {
            assertTrue(sink.offer(event(BuildEvent.PHASE_STARTED, "run-" + i)));
        }

        boolean accepted = sink.offer(event(BuildEvent.PHASE_COMPLETED, "run-overflow"));
        assertFalse(accepted);
        // A refused non-heartbeat event is not counted as dropped: the
        // caller owns spooling it, not the sink.
        assertEquals(0, sink.droppedCount());
        assertEquals(CAPACITY, sink.queueSize());

        // Every original entry survives, in order — nothing was evicted.
        BuildEventSink.DrainResult result = sink.drain(100);
        assertEquals(CAPACITY, result.events.size());
        assertEquals("run-0", result.events.get(0).getRunId());
        assertEquals("run-" + (CAPACITY - 1), result.events.get(CAPACITY - 1).getRunId());
    }

    @Test
    public void offerBelowCapacitySucceeds() {
        BuildEventSink sink = new BuildEventSink(CAPACITY);
        assertTrue(sink.offer(event(BuildEvent.PHASE_STARTED, "run-1")));
        assertEquals(1, sink.queueSize());
        assertEquals(0, sink.droppedCount());
    }

    @Test
    public void drainResetsDroppedCounter() {
        BuildEventSink sink = new BuildEventSink(CAPACITY);
        for (int i = 0; i < CAPACITY; i++) {
            sink.offer(event(BuildEvent.PHASE_STARTED, "run-" + i));
        }
        sink.offer(event(BuildEvent.PHASE_HEARTBEAT, "hb"));
        assertEquals(1, sink.droppedCount());

        BuildEventSink.DrainResult r1 = sink.drain(CAPACITY);
        assertEquals(CAPACITY, r1.events.size());
        assertEquals(1, r1.dropped);

        BuildEventSink.DrainResult r2 = sink.drain(CAPACITY);
        assertEquals(0, r2.dropped);
    }

    @Test
    public void nullOfferIsIgnored() {
        BuildEventSink sink = new BuildEventSink(CAPACITY);
        assertFalse(sink.offer(null));
        assertEquals(0, sink.queueSize());
    }

    // ---- drainRun (D11 sink-full ordering) ----

    @Test
    public void drainRunRemovesOnlyThatRunsEventsInOrder() {
        BuildEventSink sink = new BuildEventSink(CAPACITY);
        sink.offer(event(BuildEvent.PHASE_STARTED, "run-1", 0));
        sink.offer(event(BuildEvent.PHASE_STAGE, "run-other", 0));
        sink.offer(event(BuildEvent.PHASE_STAGE, "run-1", 1));
        sink.offer(event(BuildEvent.PHASE_STAGE, "run-other", 1));

        List<BuildEvent> removed = sink.drainRun("run-1");

        assertEquals(2, removed.size());
        assertEquals(0, removed.get(0).getSeq());
        assertEquals(1, removed.get(1).getSeq());
        // Every removed event carries the requested run id.
        assertTrue(removed.stream().allMatch(e -> "run-1".equals(e.getRunId())));

        // "run-other"'s events, and their relative order, are untouched.
        BuildEventSink.DrainResult remaining = sink.drain(100);
        assertEquals(2, remaining.events.size());
        assertTrue(remaining.events.stream().allMatch(e -> "run-other".equals(e.getRunId())));
        assertEquals(0, remaining.events.get(0).getSeq());
        assertEquals(1, remaining.events.get(1).getSeq());
    }

    @Test
    public void drainRunOfAnAbsentRunReturnsEmpty() {
        BuildEventSink sink = new BuildEventSink(CAPACITY);
        sink.offer(event(BuildEvent.PHASE_STARTED, "run-1", 0));

        assertTrue(sink.drainRun("run-does-not-exist").isEmpty());
        assertEquals(1, sink.queueSize());
    }

    @Test
    public void drainRunOfNullReturnsEmpty() {
        BuildEventSink sink = new BuildEventSink(CAPACITY);
        sink.offer(event(BuildEvent.PHASE_STARTED, "run-1", 0));

        assertTrue(sink.drainRun(null).isEmpty());
        assertEquals(1, sink.queueSize());
    }
}
