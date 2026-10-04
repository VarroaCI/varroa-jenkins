package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Path;

import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;

/**
 * Unit tests for {@link LogReadCache} (task 6.7): a live read, a
 * {@code stepLog} filter, a tombstoned run, and the pointer text.
 */
public class LogReadCacheTest {

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private LogChunk chunk(String runId, long seq, String nodeId, byte[] data) {
        return new LogChunk(runId, "folder/job", 1, seq, nodeId, "", data, 100L, 0L, false);
    }

    @Test
    public void aLiveReadReturnsFlushedBytesInSeqOrder() throws IOException {
        Path base = tmp.getRoot().toPath();
        LogReadCache cache = new LogReadCache(base, LogReadCache.DEFAULT_PER_RUN_CAP_BYTES,
                LogReadCache.DEFAULT_TOTAL_TARGET_BYTES, System::currentTimeMillis);
        cache.write(chunk("run-1", 0, "", "hello ".getBytes(StandardCharsets.UTF_8)));
        cache.write(chunk("run-1", 1, "", "world".getBytes(StandardCharsets.UTF_8)));

        hudson.console.AnnotatedLargeText<Object> text = cache.overallLog("run-1", new Object(), "pointer");
        assertEquals("hello world", readAll(text));
    }

    @Test
    public void stepLogFiltersByNodeId() throws IOException {
        Path base = tmp.getRoot().toPath();
        LogReadCache cache = new LogReadCache(base, LogReadCache.DEFAULT_PER_RUN_CAP_BYTES,
                LogReadCache.DEFAULT_TOTAL_TARGET_BYTES, System::currentTimeMillis);
        cache.write(chunk("run-2", 0, "node-a", "AAA".getBytes(StandardCharsets.UTF_8)));
        cache.write(chunk("run-2", 1, "node-b", "BBB".getBytes(StandardCharsets.UTF_8)));
        cache.write(chunk("run-2", 2, "node-a", "aaa".getBytes(StandardCharsets.UTF_8)));

        hudson.console.AnnotatedLargeText<Object> text = cache.stepLog("run-2", "node-a", new Object(), "pointer");
        assertEquals("AAAaaa", readAll(text));
    }

    @Test
    public void aTombstonedRunReadsAsAbsent() throws IOException {
        Path base = tmp.getRoot().toPath();
        LogReadCache cache = new LogReadCache(base, 4L, LogReadCache.DEFAULT_TOTAL_TARGET_BYTES,
                System::currentTimeMillis);
        cache.write(chunk("run-3", 0, "", "abcd".getBytes(StandardCharsets.UTF_8))); // fills the 4-byte per-run cap
        cache.write(chunk("run-3", 1, "", "e".getBytes(StandardCharsets.UTF_8))); // tombstones the run

        assertTrue(cache.isTombstonedForTesting("run-3"));
        hudson.console.AnnotatedLargeText<Object> text = cache.overallLog("run-3", new Object(), "pointer message");
        assertTrue(readAll(text).contains("pointer message"));
    }

    @Test
    public void anAbsentRunReturnsThePointerText() throws IOException {
        Path base = tmp.getRoot().toPath();
        LogReadCache cache = new LogReadCache(base, LogReadCache.DEFAULT_PER_RUN_CAP_BYTES,
                LogReadCache.DEFAULT_TOTAL_TARGET_BYTES, System::currentTimeMillis);

        hudson.console.AnnotatedLargeText<Object> text = cache.overallLog("never-cached", new Object(), "read it from Varroa");
        String content = readAll(text);
        assertTrue(content.contains("read it from Varroa"));
        assertTrue(text.isComplete());
    }

    @Test
    public void wholeCacheEvictionSkipsActiveRuns() throws IOException {
        Path base = tmp.getRoot().toPath();
        LogReadCache cache = new LogReadCache(base, 1_000_000L, 10L, System::currentTimeMillis);
        LogChunkBuffer.resetGlobalOutstandingForTesting();

        // run-active stays "active" for the whole test (never calls finish()).
        LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(),
                (url, c, token, timeout) -> new LogChunkSender.Attempt(202, -1, false),
                ms -> { }, java.util.concurrent.Executors.newSingleThreadExecutor());
        LogChunkBuffer activeBuffer = new LogChunkBuffer("run-active", "j", 1, sender, cache, LogReadCache.ResumePoint.FRESH);

        cache.write(chunk("run-active", 0, "", "0123456789".getBytes(StandardCharsets.UTF_8)));
        cache.write(chunk("run-old", 0, "", "abcdefghij".getBytes(StandardCharsets.UTF_8)));

        // The total (20 bytes) is now over the 10-byte target. run-old is
        // the only evictable (inactive) run, so eviction must tombstone it
        // and must never touch the still-active run.
        assertTrue("the inactive run over the target must be tombstoned", cache.isTombstonedForTesting("run-old"));
        assertFalse("an active run must never be evicted", cache.isTombstonedForTesting("run-active"));

        activeBuffer.finish();
    }

    // ---- Restart accounting rebuild (D1, review defect 6) ----

    @Test
    public void constructionRebuildsByteAccountingFromExistingFiles() throws IOException {
        Path base = tmp.getRoot().toPath();
        LogReadCache original = new LogReadCache(base, LogReadCache.DEFAULT_PER_RUN_CAP_BYTES,
                LogReadCache.DEFAULT_TOTAL_TARGET_BYTES, System::currentTimeMillis);
        original.write(chunk("run-1", 0, "", "hello ".getBytes(StandardCharsets.UTF_8))); // 6 bytes
        original.write(chunk("run-1", 1, "", "world".getBytes(StandardCharsets.UTF_8))); // 5 bytes, 11 total

        // A fresh instance over the same directory, as a restarted JVM
        // would construct: it must not start from zero.
        LogReadCache restarted = new LogReadCache(base, LogReadCache.DEFAULT_PER_RUN_CAP_BYTES,
                LogReadCache.DEFAULT_TOTAL_TARGET_BYTES, System::currentTimeMillis);

        assertEquals(11L, restarted.runBytesForTesting("run-1"));
        assertEquals(11L, restarted.totalBytesForTesting());
        hudson.console.AnnotatedLargeText<Object> text = restarted.overallLog("run-1", new Object(), "pointer");
        assertEquals("hello world", readAll(text));
    }

    @Test
    public void aRestartedCacheStillEnforcesThePerRunCapOnTheNextWrite() throws IOException {
        Path base = tmp.getRoot().toPath();
        LogReadCache original = new LogReadCache(base, 4L, LogReadCache.DEFAULT_TOTAL_TARGET_BYTES,
                System::currentTimeMillis);
        original.write(chunk("run-1", 0, "", "abcd".getBytes(StandardCharsets.UTF_8))); // fills the 4-byte cap

        LogReadCache restarted = new LogReadCache(base, 4L, LogReadCache.DEFAULT_TOTAL_TARGET_BYTES,
                System::currentTimeMillis);
        assertFalse("the restored accounting must not already read as over cap",
                restarted.isTombstonedForTesting("run-1"));

        // Without the rebuild this write would see an empty runBytes map
        // and accept a fifth byte well past the 4-byte cap.
        restarted.write(chunk("run-1", 1, "", "e".getBytes(StandardCharsets.UTF_8)));

        assertTrue("the per-run cap must still apply on the write after a restart",
                restarted.isTombstonedForTesting("run-1"));
    }

    @Test
    public void constructionEvictsWhenTheRestoredTotalAlreadyExceedsTheTarget() throws IOException {
        Path base = tmp.getRoot().toPath();
        LogReadCache original = new LogReadCache(base, 1_000_000L, LogReadCache.DEFAULT_TOTAL_TARGET_BYTES,
                System::currentTimeMillis);
        // Neither run is ever registered with a LogChunkBuffer, so both
        // read as inactive and are eviction-eligible.
        original.write(chunk("run-old", 0, "", "0123456789".getBytes(StandardCharsets.UTF_8))); // 10 bytes
        original.write(chunk("run-new", 0, "", "0123456789".getBytes(StandardCharsets.UTF_8))); // 10 more, 20 total

        // A fresh instance with a 10-byte target: the restored total (20
        // bytes) already exceeds it, and eviction must run at
        // construction, not wait for the next write.
        LogReadCache restarted = new LogReadCache(base, 1_000_000L, 10L, System::currentTimeMillis);

        assertTrue("eviction must apply against the restored total at construction",
                restarted.totalBytesForTesting() <= 10L);
    }

    // ---- Resume point after a JVM restart ----

    private LogReadCache newCache(Path base) {
        return new LogReadCache(base, LogReadCache.DEFAULT_PER_RUN_CAP_BYTES,
                LogReadCache.DEFAULT_TOTAL_TARGET_BYTES, System::currentTimeMillis);
    }

    @Test
    public void anUncachedRunStartsAtZero() {
        LogReadCache cache = newCache(tmp.getRoot().toPath());
        LogReadCache.ResumePoint resume = cache.resumePoint("never-cached", "folder/job", 1);
        assertEquals(0L, resume.nextSeq);
        assertTrue(resume.replay.isEmpty());
    }

    @Test
    public void aRestartedCacheReplaysOnlyChunksAboveTheResolvedWatermark() {
        Path base = tmp.getRoot().toPath();
        LogReadCache original = newCache(base);
        original.write(chunk("run-r", 0, "", "zero".getBytes(StandardCharsets.UTF_8)));
        original.write(chunk("run-r", 1, "5", "one".getBytes(StandardCharsets.UTF_8)));
        original.write(chunk("run-r", 2, "7", "two".getBytes(StandardCharsets.UTF_8)));
        original.write(chunk("run-r", 3, "", "three".getBytes(StandardCharsets.UTF_8)));
        original.recordResolved("run-r", 0);
        original.recordResolved("run-r", 1);

        LogReadCache.ResumePoint resume = newCache(base).resumePoint("run-r", "folder/job", 9);

        assertEquals(4L, resume.nextSeq);
        assertEquals(2, resume.replay.size());
        LogChunk first = resume.replay.get(0);
        assertEquals(2L, first.seq);
        assertEquals("7", first.nodeId);
        assertEquals("two", new String(first.data, StandardCharsets.UTF_8));
        assertEquals("folder/job", first.jobPath);
        assertEquals(9L, first.buildNumber);
        assertFalse(first.isFinal);
        assertEquals(3L, resume.replay.get(1).seq);
    }

    @Test
    public void aReplayChunkKeepsItsStageDroppedBytesAndFirstLineTime() {
        Path base = tmp.getRoot().toPath();
        LogReadCache original = newCache(base);
        original.write(new LogChunk("run-s", "folder/job", 1, 0, "12", "a~b c", "x".getBytes(StandardCharsets.UTF_8),
                1234L, 56L, false));

        LogChunk replayed = newCache(base).resumePoint("run-s", "folder/job", 1).replay.get(0);

        assertEquals("12", replayed.nodeId);
        assertEquals("a~b c", replayed.stageId);
        assertEquals(56L, replayed.droppedBytes);
        assertEquals(1234L, replayed.firstLineUnixMs);
    }

    @Test
    public void theResumePointSumsTheDroppedBytesOfEveryCachedChunk() {
        Path base = tmp.getRoot().toPath();
        LogReadCache original = newCache(base);
        original.write(new LogChunk("run-x", "folder/job", 1, 0, "", "", "a".getBytes(StandardCharsets.UTF_8),
                1L, 10L, false));
        original.write(new LogChunk("run-x", "folder/job", 1, 1, "", "", "b".getBytes(StandardCharsets.UTF_8),
                1L, 5L, false));
        original.recordResolved("run-x", 0);

        assertEquals(15L, newCache(base).resumePoint("run-x", "folder/job", 1).droppedBytes);
    }

    @Test
    public void aRunWithoutAWatermarkReplaysEveryCachedChunk() {
        Path base = tmp.getRoot().toPath();
        LogReadCache original = newCache(base);
        original.write(chunk("run-w", 0, "", "a".getBytes(StandardCharsets.UTF_8)));
        original.write(chunk("run-w", 1, "", "b".getBytes(StandardCharsets.UTF_8)));

        LogReadCache.ResumePoint resume = newCache(base).resumePoint("run-w", "folder/job", 1);

        assertEquals(2L, resume.nextSeq);
        assertEquals(2, resume.replay.size());
        assertEquals(0L, resume.replay.get(0).seq);
    }

    @Test
    public void aFullyResolvedRunContinuesAboveTheWatermarkWithNoReplay() {
        Path base = tmp.getRoot().toPath();
        LogReadCache original = newCache(base);
        original.write(chunk("run-f", 0, "", "a".getBytes(StandardCharsets.UTF_8)));
        original.write(chunk("run-f", 1, "", "b".getBytes(StandardCharsets.UTF_8)));
        original.recordResolved("run-f", 1);

        LogReadCache.ResumePoint resume = newCache(base).resumePoint("run-f", "folder/job", 1);

        assertEquals(2L, resume.nextSeq);
        assertTrue(resume.replay.isEmpty());
    }

    @Test
    public void seedResolvedCreatesTheRunAndContinuesAboveIt() {
        Path base = tmp.getRoot().toPath();
        newCache(base).seedResolved("run-moved", 41);

        LogReadCache.ResumePoint resume = newCache(base).resumePoint("run-moved", "folder/job", 1);

        assertEquals(42L, resume.nextSeq);
        assertTrue(resume.replay.isEmpty());
    }

    @Test
    public void seedResolvedNeverLowersTheWatermark() {
        Path base = tmp.getRoot().toPath();
        LogReadCache cache = newCache(base);
        cache.recordResolved("run-s", 9);
        cache.seedResolved("run-s", 3);

        assertEquals(10L, newCache(base).resumePoint("run-s", "folder/job", 1).nextSeq);

        cache.seedResolved("run-s", 20);
        assertEquals(21L, newCache(base).resumePoint("run-s", "folder/job", 1).nextSeq);
    }

    @Test
    public void seedResolvedStaysBelowACachedChunkAboveIt() {
        Path base = tmp.getRoot().toPath();
        LogReadCache cache = newCache(base);
        cache.write(chunk("run-c", 7, "", "a".getBytes(StandardCharsets.UTF_8)));
        cache.seedResolved("run-c", 4);

        LogReadCache.ResumePoint resume = newCache(base).resumePoint("run-c", "folder/job", 1);

        assertEquals(8L, resume.nextSeq);
    }

    @Test
    public void aTombstonedRunContinuesAboveTheWatermark() {
        Path base = tmp.getRoot().toPath();
        LogReadCache original = new LogReadCache(base, 4L, LogReadCache.DEFAULT_TOTAL_TARGET_BYTES,
                System::currentTimeMillis);
        original.write(new LogChunk("run-t", "folder/job", 1, 0, "", "", "abcd".getBytes(StandardCharsets.UTF_8),
                1L, 3L, false));
        original.recordResolved("run-t", 5);
        // Tombstones the run: the per-run cap is 4 bytes.
        original.write(new LogChunk("run-t", "folder/job", 1, 6, "", "", "e".getBytes(StandardCharsets.UTF_8),
                1L, 4L, false));
        original.write(new LogChunk("run-t", "folder/job", 1, 7, "", "", "f".getBytes(StandardCharsets.UTF_8),
                1L, 5L, false));
        assertTrue(original.isTombstonedForTesting("run-t"));

        LogReadCache.ResumePoint resume = original.resumePoint("run-t", "folder/job", 1);

        assertEquals(6L, resume.nextSeq);
        assertTrue(resume.replay.isEmpty());
        assertEquals("the drops of deleted and later chunks survive the tombstone", 12L, resume.droppedBytes);
    }

    @Test
    public void aFinishedRunReplaysNothing() {
        Path base = tmp.getRoot().toPath();
        LogReadCache original = newCache(base);
        original.write(chunk("run-d", 0, "", "a".getBytes(StandardCharsets.UTF_8)));
        original.write(new LogChunk("run-d", "folder/job", 1, 1, "", "", new byte[0], 100L, 0L, true));

        LogReadCache.ResumePoint resume = newCache(base).resumePoint("run-d", "folder/job", 1);

        assertEquals(2L, resume.nextSeq);
        assertTrue(resume.replay.isEmpty());
    }

    @Test
    public void theWatermarkFileDoesNotChangeTheReadBack() throws IOException {
        Path base = tmp.getRoot().toPath();
        LogReadCache cache = newCache(base);
        cache.write(chunk("run-m", 0, "", "hello".getBytes(StandardCharsets.UTF_8)));
        cache.recordResolved("run-m", 0);

        assertEquals("hello", readAll(newCache(base).overallLog("run-m", new Object(), "pointer")));
        assertEquals(5L, newCache(base).runBytesForTesting("run-m"));
    }

    private static String readAll(hudson.console.AnnotatedLargeText<?> text) throws IOException {
        try (java.io.Reader r = text.readAll()) {
            StringBuilder sb = new StringBuilder();
            int ch;
            while ((ch = r.read()) != -1) {
                sb.append((char) ch);
            }
            return sb.toString();
        }
    }
}
