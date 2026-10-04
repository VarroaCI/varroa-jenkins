package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.AbstractExecutorService;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicLong;

import org.junit.After;
import org.junit.Before;
import org.junit.Test;

/**
 * Unit tests for {@link LogChunkBuffer} (task 6.2, 6.6): ordering, the drop
 * counter, the node-change split, and the {@code finish} sequence.
 */
public class LogChunkBufferTest {

    private final List<LogChunk> posted = new CopyOnWriteArrayList<>();
    private final AtomicLong clockMillis = new AtomicLong(1_000_000L);
    private LogChunkSender sender;
    private List<LogChunk> cached;

    @Before
    public void setUp() {
        posted.clear();
        cached = new ArrayList<>();
        LogChunkBuffer.resetGlobalOutstandingForTesting();
        LogChunkSender.resetChunkCapForTesting();
        LogChunkSender.Transport alwaysAccept = (url, chunk, token, timeout) -> {
            posted.add(chunk);
            return new LogChunkSender.Attempt(202, -1, false);
        };
        sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(),
                alwaysAccept, ms -> { }, directExecutor());
    }

    @After
    public void tearDown() {
        LogChunkBuffer.resetGlobalOutstandingForTesting();
        LogChunkSender.resetChunkCapForTesting();
    }

    private LogChunkBuffer.ChunkCache recordingCache() {
        return chunk -> cached.add(chunk);
    }

    private LogChunkBuffer newBuffer(long buildCap, long totalCap, long chunkCap) {
        return new LogChunkBuffer("run-1", "folder/job", 7, sender, recordingCache(),
                buildCap, totalCap, () -> chunkCap, clockMillis::get);
    }

    private static ExecutorService directExecutor() {
        return new AbstractExecutorService() {
            @Override
            public void execute(Runnable command) {
                command.run();
            }

            @Override
            public void shutdown() {
            }

            @Override
            public List<Runnable> shutdownNow() {
                return List.of();
            }

            @Override
            public boolean isShutdown() {
                return false;
            }

            @Override
            public boolean isTerminated() {
                return false;
            }

            @Override
            public boolean awaitTermination(long timeout, TimeUnit unit) {
                return true;
            }
        };
    }

    private static byte[] bytes(String s) {
        return s.getBytes(StandardCharsets.UTF_8);
    }

    @Test
    public void chunksArriveInAscendingSeqOrder() {
        LogChunkBuffer buffer = newBuffer(1_000_000, 1_000_000, 8);
        buffer.append("", "", bytes("abcdefgh"), 0, 8); // exactly the chunk cap: closes immediately
        buffer.append("", "", bytes("ijklmnop"), 0, 8);
        buffer.finish();

        assertEquals(3, posted.size()); // two full chunks + the final chunk
        assertEquals(0, posted.get(0).seq);
        assertEquals(1, posted.get(1).seq);
        assertEquals(2, posted.get(2).seq);
        assertTrue(posted.get(2).isFinal);
    }

    @Test
    public void dropCounterTracksIncomingOverflowAndResetsPerChunk() {
        LogChunkBuffer buffer = newBuffer(4, 1_000_000, 100);
        buffer.append("", "", bytes("abcd"), 0, 4); // fills the 4-byte build cap
        buffer.append("", "", bytes("EFGH"), 0, 4); // all four bytes discarded
        buffer.finish();

        // The accepted "abcd" bytes flush as their own chunk at finish, carrying
        // the pending drop count; the final chunk then carries none (it reset).
        assertEquals(2, posted.size());
        LogChunk dataChunk = posted.get(0);
        assertEquals("abcd", new String(dataChunk.data, StandardCharsets.UTF_8));
        assertEquals(4, dataChunk.droppedBytes);
        LogChunk finalChunk = posted.get(1);
        assertTrue(finalChunk.isFinal);
        assertEquals(0, finalChunk.droppedBytes);
        assertEquals(4, buffer.totalDroppedForTesting());
    }

    @Test
    public void aNodeChangeClosesTheOpenSegment() {
        LogChunkBuffer buffer = newBuffer(1_000_000, 1_000_000, 1_000);
        buffer.append("node-a", "stage-1", bytes("from-a"), 0, 6);
        buffer.append("node-b", "stage-1", bytes("from-b"), 0, 6);
        buffer.finish();

        assertEquals(3, posted.size());
        assertEquals("node-a", posted.get(0).nodeId);
        assertEquals("node-b", posted.get(1).nodeId);
        assertFalse("no chunk should mix the two node ids",
                new String(posted.get(0).data, StandardCharsets.UTF_8).contains("from-b"));
        assertFalse(new String(posted.get(1).data, StandardCharsets.UTF_8).contains("from-a"));
    }

    @Test
    public void finishIsIdempotent() {
        LogChunkBuffer buffer = newBuffer(1_000_000, 1_000_000, 1_000);
        buffer.append("", "", bytes("hello"), 0, 5);
        LogSummary first = buffer.finish();
        LogSummary second = buffer.finish();

        assertEquals(first.getChunkCount(), second.getChunkCount());
        long finalChunks = posted.stream().filter(c -> c.isFinal).count();
        assertEquals("a repeated finish must not post a second final chunk", 1, finalChunks);
    }

    @Test
    public void chunkCountIsExact() {
        LogChunkBuffer buffer = newBuffer(1_000_000, 1_000_000, 4);
        buffer.append("", "", bytes("aaaa"), 0, 4);
        buffer.append("", "", bytes("bbbb"), 0, 4);
        buffer.append("", "", bytes("cc"), 0, 2);
        LogSummary summary = buffer.finish();

        // Two full 4-byte chunks, one 2-byte chunk on finish's flush, one final chunk: 4.
        assertEquals(4, summary.getChunkCount());
    }

    @Test
    public void aCapDropMarksTheSummaryIncomplete() {
        LogChunkBuffer buffer = newBuffer(2, 1_000_000, 100);
        buffer.append("", "", bytes("ab"), 0, 2);
        buffer.append("", "", bytes("CD"), 0, 2); // discarded: build cap already full
        LogSummary summary = buffer.finish();

        assertTrue(summary.isIncomplete());
        assertEquals(LogSummary.REASON_PLUGIN_DROP, summary.getIncompleteReason());
    }

    @Test
    public void aTerminalResponseBeforeFinishMarksTheSummaryIncomplete() {
        LogChunkSender.Transport terminal400 = (url, chunk, token, timeout) ->
                new LogChunkSender.Attempt(400, -1, false);
        LogChunkSender terminalSender = new LogChunkSender("http://127.0.0.1:9095/v1/log",
                new LoopbackTokenFile(), terminal400, ms -> { }, directExecutor());
        LogChunkBuffer buffer = new LogChunkBuffer("run-2", "folder/job", 1, terminalSender, recordingCache(),
                1_000_000, 1_000_000, () -> 4, clockMillis::get);
        buffer.append("", "", bytes("aaaa"), 0, 4); // closes immediately at the 4-byte cap, posts, gets a 400
        LogSummary summary = buffer.finish();

        assertTrue(summary.isIncomplete());
        assertEquals(LogSummary.REASON_TERMINAL_RESPONSE, summary.getIncompleteReason());
    }

    // ---- Stuck-queue observer (D2, review defect 5) ----

    /** A pool that accepts a submission but never runs it: the mite that never accepts. */
    private static ExecutorService neverRunsExecutor() {
        return new AbstractExecutorService() {
            @Override
            public void execute(Runnable command) {
                // Deliberately never runs the pump: simulates a mite that
                // never accepts, so the chunk stays queued forever.
            }

            @Override
            public void shutdown() {
            }

            @Override
            public List<Runnable> shutdownNow() {
                return List.of();
            }

            @Override
            public boolean isShutdown() {
                return false;
            }

            @Override
            public boolean isTerminated() {
                return false;
            }

            @Override
            public boolean awaitTermination(long timeout, TimeUnit unit) {
                return true;
            }
        };
    }

    @Test
    public void aQueueStillPresentTenMinutesAfterFinishWarnsOnceAndCountsOnce() {
        LogChunkBuffer.resetStuckQueueWarningsForTesting();
        LogChunkSender.Transport neverAsked = (url, chunk, token, timeout) -> {
            throw new AssertionError("a chunk the pool never runs must never reach the transport");
        };
        LogChunkSender stuckSender = new LogChunkSender("http://127.0.0.1:9095/v1/log",
                new LoopbackTokenFile(), neverAsked, ms -> { }, neverRunsExecutor());
        LogChunkBuffer buffer = new LogChunkBuffer("run-stuck", "folder/job", 1, stuckSender, recordingCache(),
                1_000_000, 1_000_000, () -> 1_000, clockMillis::get);
        buffer.append("", "", bytes("hello"), 0, 5);
        buffer.finish();

        // Just under ten minutes: no warning yet.
        clockMillis.addAndGet(LogChunkBuffer.STUCK_QUEUE_WARN_MILLIS - 1);
        buffer.observeStuckQueueForTesting(clockMillis.get());
        assertEquals(0, LogChunkBuffer.stuckQueueWarningCount());

        // At the ten-minute mark: exactly one warning, one count.
        clockMillis.addAndGet(1);
        buffer.observeStuckQueueForTesting(clockMillis.get());
        assertEquals(1, LogChunkBuffer.stuckQueueWarningCount());

        // A repeated check well past the threshold never warns again, and
        // never posts anything — the observer only observes.
        clockMillis.addAndGet(LogChunkBuffer.STUCK_QUEUE_WARN_MILLIS);
        buffer.observeStuckQueueForTesting(clockMillis.get());
        assertEquals(1, LogChunkBuffer.stuckQueueWarningCount());
        assertTrue("the observer must never post or resolve the stuck chunk itself", posted.isEmpty());
    }

    @Test
    public void aDrainedQueueNeverWarnsEvenLongAfterFinish() {
        LogChunkBuffer.resetStuckQueueWarningsForTesting();
        // setUp()'s sender accepts every chunk immediately, so this
        // build's queue is already empty by the time finish() returns.
        LogChunkBuffer buffer = newBuffer(1_000_000, 1_000_000, 1_000);
        buffer.append("", "", bytes("hello"), 0, 5);
        buffer.finish();

        clockMillis.addAndGet(LogChunkBuffer.STUCK_QUEUE_WARN_MILLIS * 2);
        buffer.observeStuckQueueForTesting(clockMillis.get());

        assertEquals(0, LogChunkBuffer.stuckQueueWarningCount());
    }

    // ---- Resume after a JVM restart ----

    private final List<Long> resolved = new CopyOnWriteArrayList<>();

    private LogChunkBuffer.ChunkCache watermarkCache() {
        return new LogChunkBuffer.ChunkCache() {
            @Override
            public void write(LogChunk chunk) {
                cached.add(chunk);
            }

            @Override
            public void recordResolved(String runId, long seq) {
                resolved.add(seq);
            }
        };
    }

    private LogChunkBuffer resumedBuffer(LogReadCache.ResumePoint resume) {
        return new LogChunkBuffer("run-1", "folder/job", 7, sender, watermarkCache(),
                1_000_000, 1_000_000, () -> 1_000, clockMillis::get, resume);
    }

    private static LogChunk cachedChunk(long seq, String text) {
        return new LogChunk("run-1", "folder/job", 7, seq, "", "", bytes(text), 1L, 0L, false);
    }

    @Test
    public void aResumedBufferContinuesAtTheResumeSeq() {
        LogChunkBuffer buffer = resumedBuffer(new LogReadCache.ResumePoint(5L, List.of(), 0L));
        buffer.append("", "", bytes("Resuming build"), 0, 14);
        LogSummary summary = buffer.finish();

        assertEquals(2, posted.size());
        assertEquals(5L, posted.get(0).seq);
        assertEquals(6L, posted.get(1).seq);
        assertTrue(posted.get(1).isFinal);
        assertEquals("chunk_count is the final seq plus 1", 7L, summary.getChunkCount());
    }

    @Test
    public void aResumedBufferKeepsTheEarlierDropsInItsSummary() {
        LogChunkBuffer buffer = resumedBuffer(new LogReadCache.ResumePoint(2L, List.of(), 40L));
        LogSummary summary = buffer.finish();

        assertEquals(40L, summary.getDroppedBytes());
        assertTrue(summary.isIncomplete());
        assertEquals(LogSummary.REASON_PLUGIN_DROP, summary.getIncompleteReason());
    }

    @Test
    public void replayChunksArePostedBeforeNewChunks() {
        LogChunkBuffer buffer = resumedBuffer(new LogReadCache.ResumePoint(4L,
                List.of(cachedChunk(2, "two"), cachedChunk(3, "three")), 0L));
        buffer.append("", "", bytes("new"), 0, 3);
        buffer.finish();

        assertEquals(4, posted.size());
        assertEquals(2L, posted.get(0).seq);
        assertEquals("two", new String(posted.get(0).data, StandardCharsets.UTF_8));
        assertEquals(3L, posted.get(1).seq);
        assertEquals(4L, posted.get(2).seq);
        assertEquals("new", new String(posted.get(2).data, StandardCharsets.UTF_8));
        assertEquals(5L, posted.get(3).seq);
    }

    @Test
    public void replayChunksAreNotWrittenToTheCacheAgain() {
        LogChunkBuffer buffer = resumedBuffer(new LogReadCache.ResumePoint(3L, List.of(cachedChunk(2, "two")), 0L));
        buffer.finish();

        for (LogChunk c : cached) {
            assertTrue("only the new final chunk reaches the cache", c.isFinal);
        }
    }

    @Test
    public void anAcceptedReplayChunkReleasesItsBytes() {
        LogChunkBuffer buffer = resumedBuffer(new LogReadCache.ResumePoint(3L, List.of(cachedChunk(2, "two")), 0L));
        assertEquals(0L, LogChunkBuffer.globalOutstandingForTesting());
        buffer.finish();
        assertEquals(0L, LogChunkBuffer.globalOutstandingForTesting());
    }

    @Test
    public void everyResolvedChunkRecordsTheWatermark() {
        LogChunkBuffer buffer = resumedBuffer(new LogReadCache.ResumePoint(3L, List.of(cachedChunk(2, "two")), 0L));
        buffer.append("", "", bytes("abc"), 0, 3);
        buffer.finish();

        assertEquals(List.of(2L, 3L, 4L), resolved);
    }

    @Test
    public void shutdownFlushesTheOpenSegmentAndDropsLaterWrites() {
        LogChunkBuffer buffer = newBuffer(1_000_000, 1_000_000, 1_000_000);
        buffer.append("", "", bytes("Pausing (shutting down)"), 0, 23);
        assertEquals(0, posted.size());

        LogChunkBuffer.flushAndDetachAll();

        assertTrue(buffer.isDetachedForTesting());
        assertEquals(1, posted.size());
        assertEquals(1, cached.size());
        assertEquals("Pausing (shutting down)", new String(cached.get(0).data, StandardCharsets.UTF_8));

        buffer.append("", "", bytes("late"), 0, 4);
        clockMillis.addAndGet(LogChunkBuffer.DEFAULT_FLUSH_MS + 1);
        buffer.tickForTesting(clockMillis.get());
        assertEquals("a write after the shutdown flush takes no seq", 1, cached.size());
    }

    @Test
    public void shutdownRecordsAPendingDropWithNoBufferedBytes() {
        LogChunkSender stuck = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(),
                (url, chunk, token, timeout) -> new LogChunkSender.Attempt(503, -1, false),
                ms -> { throw new InterruptedException("stop"); }, neverRunsExecutor());
        LogChunkBuffer buffer = new LogChunkBuffer("run-drop", "folder/job", 1, stuck, recordingCache(),
                4, 1_000_000, () -> 4, clockMillis::get);
        buffer.append("", "", bytes("abcd"), 0, 4); // closes at the 4-byte chunk cap and fills the build cap
        buffer.append("", "", bytes("EFG"), 0, 3); // dropped: the open segment stays empty

        LogChunkBuffer.flushAndDetachAll();

        assertEquals(2, cached.size());
        assertEquals(0, cached.get(1).data.length);
        assertEquals(3L, cached.get(1).droppedBytes);
    }

    @Test
    public void aStaleOpenSegmentClosesOnTheTimeTrigger() {
        LogChunkBuffer buffer = newBuffer(1_000_000, 1_000_000, 1_000_000);
        buffer.append("", "", bytes("partial"), 0, 7);
        assertEquals(0, posted.size());

        clockMillis.addAndGet(LogChunkBuffer.DEFAULT_FLUSH_MS + 1);
        buffer.tickForTesting(clockMillis.get());

        assertEquals(1, posted.size());
        assertEquals("partial", new String(posted.get(0).data, StandardCharsets.UTF_8));
    }
}
