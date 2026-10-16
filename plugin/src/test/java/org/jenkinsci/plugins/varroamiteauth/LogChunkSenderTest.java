package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import java.util.ArrayList;
import java.util.List;
import java.util.logging.Handler;
import java.util.logging.Level;
import java.util.logging.LogRecord;
import java.util.logging.Logger;
import java.util.concurrent.AbstractExecutorService;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;

import org.junit.After;
import org.junit.Before;
import org.junit.Test;

/**
 * Unit tests for {@link LogChunkSender} (task 6.3): every response code,
 * and the guarantee that two chunks of one build never overlap.
 */
public class LogChunkSenderTest {

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

    @Before
    public void setUp() {
        LogChunkSender.resetChunkCapForTesting();
    }

    @After
    public void tearDown() {
        LogChunkSender.resetChunkCapForTesting();
    }

    private static LogChunk chunk(String runId, long seq, boolean isFinal) {
        return new LogChunk(runId, "folder/job", 3, seq, "n1", "s1", new byte[] {1, 2, 3}, 100L, 0L, isFinal);
    }

    private static final class RecordingOwner implements LogChunkSender.Owner {
        final List<LogChunk> accepted = new CopyOnWriteArrayList<>();
        final List<LogChunk> terminallyFailed = new CopyOnWriteArrayList<>();

        @Override
        public void onChunkAccepted(LogChunk chunk) {
            accepted.add(chunk);
        }

        @Override
        public void onChunkTerminallyFailed(LogChunk chunk) {
            terminallyFailed.add(chunk);
        }
    }

    @Test
    public void a202AcceptsAndDropsTheChunk() {
        LogChunkSender.Transport t = (url, chunk, token, timeout) -> new LogChunkSender.Attempt(202, -1, -1, false);
        LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(), t,
                ms -> { }, directExecutor());
        RecordingOwner owner = new RecordingOwner();
        sender.enqueue(owner, chunk("r1", 0, false));

        assertEquals(1, owner.accepted.size());
        assertTrue(owner.terminallyFailed.isEmpty());
    }

    @Test
    public void a400IsTerminalAndCountsAsDropped() {
        LogChunkSender.Transport t = (url, chunk, token, timeout) -> new LogChunkSender.Attempt(400, -1, -1, false);
        LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(), t,
                ms -> { }, directExecutor());
        RecordingOwner owner = new RecordingOwner();
        sender.enqueue(owner, chunk("r1", 0, false));

        assertTrue(owner.accepted.isEmpty());
        assertEquals(1, owner.terminallyFailed.size());
    }

    @Test
    public void a413IsTerminalAndLowersTheCap() {
        LogChunkSender.Transport t = (url, chunk, token, timeout) -> new LogChunkSender.Attempt(413, 8192, -1, false);
        LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(), t,
                ms -> { }, directExecutor());
        RecordingOwner owner = new RecordingOwner();
        sender.enqueue(owner, chunk("r1", 0, false));

        assertEquals(1, owner.terminallyFailed.size());
        assertEquals(8192L, LogChunkSender.currentChunkCapBytes());
    }

    @Test
    public void a400LogsTheRefusalBodyPrefix() {
        List<LogRecord> records = new CopyOnWriteArrayList<>();
        Handler handler = captureWarnings(records);
        try {
            LogChunkSender.Transport t = (url, chunk, token, timeout) ->
                    new LogChunkSender.Attempt(400, -1, -1, false, "malformed log headers");
            LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(), t,
                    ms -> { }, directExecutor());
            RecordingOwner owner = new RecordingOwner();
            sender.enqueue(owner, chunk("r1", 0, false));

            assertEquals(1, owner.terminallyFailed.size());
            assertEquals(1, records.size());
            assertEquals(Level.WARNING, records.get(0).getLevel());
            assertTrue(records.get(0).getMessage().contains("malformed log headers"));
        } finally {
            Logger.getLogger(LogChunkSender.class.getName()).removeHandler(handler);
        }
    }

    @Test
    public void a413LogsTheRefusalBodyPrefix() {
        List<LogRecord> records = new CopyOnWriteArrayList<>();
        Handler handler = captureWarnings(records);
        try {
            LogChunkSender.Transport t = (url, chunk, token, timeout) ->
                    new LogChunkSender.Attempt(413, 8192, -1, false, "log chunk over the cap");
            LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(), t,
                    ms -> { }, directExecutor());
            RecordingOwner owner = new RecordingOwner();
            sender.enqueue(owner, chunk("r1", 0, false));

            assertEquals(1, owner.terminallyFailed.size());
            assertEquals(1, records.size());
            assertEquals(Level.WARNING, records.get(0).getLevel());
            assertTrue(records.get(0).getMessage().contains("log chunk over the cap"));
        } finally {
            Logger.getLogger(LogChunkSender.class.getName()).removeHandler(handler);
        }
    }

    /** Adds a handler that collects WARNING records from the sender's logger. The caller removes it. */
    private static Handler captureWarnings(List<LogRecord> records) {
        Handler handler = new Handler() {
            @Override
            public void publish(LogRecord r) {
                if (r.getLevel().intValue() >= Level.WARNING.intValue()) {
                    records.add(r);
                }
            }

            @Override
            public void flush() {
            }

            @Override
            public void close() {
            }
        };
        Logger.getLogger(LogChunkSender.class.getName()).addHandler(handler);
        return handler;
    }

    @Test
    public void aTooSmallCapHeaderIsIgnored() {
        LogChunkSender.Transport t = (url, chunk, token, timeout) -> new LogChunkSender.Attempt(413, 0, -1, false);
        LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(), t,
                ms -> { }, directExecutor());
        RecordingOwner owner = new RecordingOwner();
        long before = LogChunkSender.currentChunkCapBytes();
        sender.enqueue(owner, chunk("r1", 0, false));

        assertEquals(before, LogChunkSender.currentChunkCapBytes());
    }

    @Test
    public void a401RereadsTheTokenAndRetries() {
        AtomicInteger calls = new AtomicInteger();
        LoopbackTokenFile tokenFile = new LoopbackTokenFile(java.nio.file.Paths.get("/nonexistent-token-file"),
                System::currentTimeMillis);
        LogChunkSender.Transport t = (url, chunk, token, timeout) -> {
            int n = calls.incrementAndGet();
            return n == 1 ? new LogChunkSender.Attempt(401, -1, -1, false) : new LogChunkSender.Attempt(202, -1, -1, false);
        };
        LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", tokenFile, t,
                ms -> { }, directExecutor());
        RecordingOwner owner = new RecordingOwner();
        sender.enqueue(owner, chunk("r1", 0, false));

        assertEquals(2, calls.get());
        assertEquals(1, owner.accepted.size());
    }

    @Test
    public void a503KeepsTheChunkAndRetries() {
        AtomicInteger calls = new AtomicInteger();
        LogChunkSender.Transport t = (url, chunk, token, timeout) -> {
            int n = calls.incrementAndGet();
            return n < 3 ? new LogChunkSender.Attempt(503, -1, -1, false) : new LogChunkSender.Attempt(202, -1, -1, false);
        };
        LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(), t,
                ms -> { }, directExecutor());
        RecordingOwner owner = new RecordingOwner();
        sender.enqueue(owner, chunk("r1", 0, false));

        assertEquals(3, calls.get());
        assertEquals(1, owner.accepted.size());
    }

    @Test
    public void aTransportFailureKeepsTheChunkAndRetries() {
        AtomicInteger calls = new AtomicInteger();
        LogChunkSender.Transport t = (url, chunk, token, timeout) -> {
            int n = calls.incrementAndGet();
            return n < 2 ? new LogChunkSender.Attempt(0, -1, -1, true) : new LogChunkSender.Attempt(202, -1, -1, false);
        };
        LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(), t,
                ms -> { }, directExecutor());
        RecordingOwner owner = new RecordingOwner();
        sender.enqueue(owner, chunk("r1", 0, false));

        assertEquals(2, calls.get());
        assertEquals(1, owner.accepted.size());
    }

    @Test
    public void anOutageOfManyRetriesLosesNoQueuedChunk() {
        // Simulates a long outage: no timer or evictor may discard a queued
        // chunk (D2). A fake Sleeper skips the real wall-clock wait.
        AtomicInteger calls = new AtomicInteger();
        LogChunkSender.Transport t = (url, chunk, token, timeout) -> {
            int n = calls.incrementAndGet();
            return n <= 50 ? new LogChunkSender.Attempt(503, -1, -1, false) : new LogChunkSender.Attempt(202, -1, -1, false);
        };
        LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(), t,
                ms -> { }, directExecutor());
        RecordingOwner owner = new RecordingOwner();
        sender.enqueue(owner, chunk("r1", 0, false));

        assertEquals(51, calls.get());
        assertEquals(1, owner.accepted.size());
    }

    @Test
    public void a503WithPauseHeaderSleepsThePause() {
        // 120 ms is below the 250 ms backoff floor, so a sleep from the
        // growing backoff can never pass this check.
        AtomicInteger calls = new AtomicInteger();
        LogChunkSender.Transport t = (url, chunk, token, timeout) -> {
            int n = calls.incrementAndGet();
            return n <= 4 ? new LogChunkSender.Attempt(503, -1, 120, false)
                    : new LogChunkSender.Attempt(202, -1, -1, false);
        };
        List<Long> sleeps = new ArrayList<>();
        LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(), t,
                sleeps::add, directExecutor());
        RecordingOwner owner = new RecordingOwner();
        sender.enqueue(owner, chunk("r1", 0, false));

        assertEquals(1, owner.accepted.size());
        assertEquals(4, sleeps.size());
        for (long ms : sleeps) {
            assertTrue("sleep " + ms + " ms is outside 120 to 132 ms", ms >= 120 && ms <= 132);
        }
    }

    @Test
    public void aPauseDoesNotGrowTheBackoff() {
        // Four paused 503s, then one 503 with no header. The header-free
        // retry is the first backoff step, 250 to 500 ms.
        AtomicInteger calls = new AtomicInteger();
        LogChunkSender.Transport t = (url, chunk, token, timeout) -> {
            int n = calls.incrementAndGet();
            if (n <= 4) {
                return new LogChunkSender.Attempt(503, -1, 120, false);
            }
            return n == 5 ? new LogChunkSender.Attempt(503, -1, -1, false)
                    : new LogChunkSender.Attempt(202, -1, -1, false);
        };
        List<Long> sleeps = new ArrayList<>();
        LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(), t,
                sleeps::add, directExecutor());
        sender.enqueue(new RecordingOwner(), chunk("r1", 0, false));

        assertEquals(5, sleeps.size());
        long last = sleeps.get(4);
        assertTrue("first backoff step was " + last + " ms", last >= 250 && last <= 500);
    }

    @Test
    public void a503WithoutPauseHeaderKeepsTheGrowingBackoff() {
        AtomicInteger calls = new AtomicInteger();
        LogChunkSender.Transport t = (url, chunk, token, timeout) -> {
            int n = calls.incrementAndGet();
            return n <= 6 ? new LogChunkSender.Attempt(503, -1, -1, false)
                    : new LogChunkSender.Attempt(202, -1, -1, false);
        };
        List<Long> sleeps = new ArrayList<>();
        LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(), t,
                sleeps::add, directExecutor());
        sender.enqueue(new RecordingOwner(), chunk("r1", 0, false));

        assertEquals(6, sleeps.size());
        for (long ms : sleeps) {
            assertTrue("backoff " + ms + " ms is outside 250 to 5000 ms", ms >= 250 && ms <= 5000);
        }
    }

    @Test
    public void aPauseIsClampedTo10Seconds() {
        for (int i = 0; i < 100; i++) {
            long ms = LogChunkSender.pauseWaitMillis(60_000L);
            assertTrue("wait " + ms + " ms is outside 10000 to 11000 ms", ms >= 10_000L && ms <= 11_000L);
        }
    }

    @Test
    public void aMalformedPauseHeaderParsesAsAbsent() {
        assertEquals(-1L, LogChunkSender.parsePauseMillis(null));
        assertEquals(-1L, LogChunkSender.parsePauseMillis("abc"));
        assertEquals(-1L, LogChunkSender.parsePauseMillis("-5"));
        assertEquals(-1L, LogChunkSender.parsePauseMillis("0"));
        assertEquals(1200L, LogChunkSender.parsePauseMillis(" 1200 "));
    }

    @Test
    public void twoChunksOfOneBuildNeverOverlap() throws InterruptedException {
        List<String> order = new CopyOnWriteArrayList<>();
        CountDownLatch firstStarted = new CountDownLatch(1);
        CountDownLatch releaseFirst = new CountDownLatch(1);
        LogChunkSender.Transport t = (url, chunk, token, timeout) -> {
            order.add("start-" + chunk.seq);
            if (chunk.seq == 0) {
                firstStarted.countDown();
                try {
                    releaseFirst.await(5, TimeUnit.SECONDS);
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                }
            }
            order.add("end-" + chunk.seq);
            return new LogChunkSender.Attempt(202, -1, -1, false);
        };
        ExecutorService pool = java.util.concurrent.Executors.newCachedThreadPool();
        LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(), t,
                ms -> { }, pool);
        RecordingOwner owner = new RecordingOwner();

        sender.enqueue(owner, chunk("run-overlap", 0, false));
        assertTrue(firstStarted.await(5, TimeUnit.SECONDS));
        sender.enqueue(owner, chunk("run-overlap", 1, false));
        // Give the pool a moment; chunk 1 must not start until chunk 0 ends.
        Thread.sleep(200);
        assertFalse(order.contains("start-1"));
        releaseFirst.countDown();

        long deadline = System.currentTimeMillis() + 5000;
        while (owner.accepted.size() < 2 && System.currentTimeMillis() < deadline) {
            Thread.sleep(20);
        }
        pool.shutdown();
        assertEquals(List.of("start-0", "end-0", "start-1", "end-1"), order);
    }
}
