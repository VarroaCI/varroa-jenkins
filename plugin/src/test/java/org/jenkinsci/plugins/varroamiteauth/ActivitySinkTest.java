package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import java.util.List;
import org.junit.Test;

/**
 * Tests for {@link ActivitySink} — plain JUnit, no JenkinsRule.
 */
public class ActivitySinkTest {

    @Test
    public void offerPastCapacityDropsOldestAndIncrementsDropped() {
        ActivitySink sink = new ActivitySink(16);
        assertEquals(16, sink.remainingCapacity());

        // Offer 21 events (capacity + 5).
        for (int i = 0; i < 21; i++) {
            sink.offer(event("e" + i));
        }

        // Queue should be at capacity (16).
        assertEquals(16, sink.queueSize());
        // dropped ≥ 5 (may be slightly more under contention, but not in single-threaded test)
        assertTrue("dropped >= 5, was " + sink.droppedCount(), sink.droppedCount() >= 5);

        // The 5 oldest should have been dropped; the newest 16 remain.
        ActivitySink.DrainResult result = sink.drain(100);
        assertEquals(16, result.events.size());
        // The first event should be "e5" (e0-e4 were dropped).
        assertEquals("e5", result.events.get(0).getMessage());
        assertEquals("e20", result.events.get(15).getMessage());
    }

    @Test
    public void drainReturnsAtMostMaxAndResetsDropped() {
        ActivitySink sink = new ActivitySink(32);

        // Offer 40 events → 8 dropped.
        for (int i = 0; i < 40; i++) {
            sink.offer(event("e" + i));
        }
        assertTrue("dropped >= 8", sink.droppedCount() >= 8);
        assertEquals(32, sink.queueSize());

        // Drain with max=10.
        ActivitySink.DrainResult r1 = sink.drain(10);
        assertEquals(10, r1.events.size());
        assertTrue("dropped >= 8 in first drain", r1.dropped >= 8);

        // Remaining: 22 events in queue.
        assertEquals(22, sink.queueSize());

        // Second drain: dropped should be 0 (reset), get remaining.
        ActivitySink.DrainResult r2 = sink.drain(100);
        assertEquals(22, r2.events.size());
        assertEquals(0, r2.dropped);

        // Third drain: empty.
        ActivitySink.DrainResult r3 = sink.drain(100);
        assertEquals(0, r3.events.size());
        assertEquals(0, r3.dropped);
    }

    @Test
    public void offerNeverBlocks() {
        ActivitySink sink = new ActivitySink(16);

        // Fill the queue exactly.
        for (int i = 0; i < 16; i++) {
            sink.offer(event("e" + i));
        }
        assertEquals(16, sink.queueSize());

        // Offer one more — should not block, should drop oldest.
        sink.offer(event("overflow"));
        assertEquals(16, sink.queueSize());
        assertEquals(1, sink.droppedCount());
    }

    @Test
    public void resetForTestingClearsState() {
        ActivitySink sink = new ActivitySink(16);
        for (int i = 0; i < 20; i++) {
            sink.offer(event("e" + i));
        }
        assertTrue(sink.queueSize() > 0);
        assertTrue(sink.droppedCount() > 0);

        sink.resetForTesting();
        assertEquals(0, sink.queueSize());
        assertEquals(0, sink.droppedCount());
        assertEquals(16, sink.remainingCapacity());
    }

    @Test
    public void drainHonorsMaxClamping() {
        ActivitySink sink = new ActivitySink(64);
        for (int i = 0; i < 30; i++) {
            sink.offer(event("e" + i));
        }

        // max=0 should be clamped to 1.
        ActivitySink.DrainResult r1 = sink.drain(0);
        assertEquals(1, r1.events.size());

        // max > 1024 should be clamped to 1024, but we only have 29 left.
        ActivitySink.DrainResult r2 = sink.drain(9999);
        assertEquals(29, r2.events.size());
    }

    @Test
    public void emptyDrainReturnsEmptyListAndZeroDropped() {
        ActivitySink sink = new ActivitySink(16);
        ActivitySink.DrainResult result = sink.drain(100);
        assertEquals(0, result.events.size());
        assertEquals(0, result.dropped);
    }

    private static ActivityEvent event(String msg) {
        return ActivityEvent.builder().message(msg).type("test").build();
    }
}
