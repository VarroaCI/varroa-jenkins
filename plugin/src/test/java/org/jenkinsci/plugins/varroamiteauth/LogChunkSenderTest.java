package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import java.util.ArrayList;
import java.util.List;
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
        LogChunkSender.Transport t = (url, chunk, token, timeout) -> new LogChunkSender.Attempt(202, -1, false);
        LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(), t,
                ms -> { }, directExecutor());
        RecordingOwner owner = new RecordingOwner();
        sender.enqueue(owner, chunk("r1", 0, false));

        assertEquals(1, owner.accepted.size());
        assertTrue(owner.terminallyFailed.isEmpty());
    }

    @Test
    public void a400IsTerminalAndCountsAsDropped() {
        LogChunkSender.Transport t = (url, chunk, token, timeout) -> new LogChunkSender.Attempt(400, -1, false);
        LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(), t,
                ms -> { }, directExecutor());
        RecordingOwner owner = new RecordingOwner();
        sender.enqueue(owner, chunk("r1", 0, false));

        assertTrue(owner.accepted.isEmpty());
        assertEquals(1, owner.terminallyFailed.size());
    }

    @Test
    public void a413IsTerminalAndLowersTheCap() {
        LogChunkSender.Transport t = (url, chunk, token, timeout) -> new LogChunkSender.Attempt(413, 8192, false);
        LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(), t,
                ms -> { }, directExecutor());
        RecordingOwner owner = new RecordingOwner();
        sender.enqueue(owner, chunk("r1", 0, false));

        assertEquals(1, owner.terminallyFailed.size());
        assertEquals(8192L, LogChunkSender.currentChunkCapBytes());
    }

    @Test
    public void aTooSmallCapHeaderIsIgnored() {
        LogChunkSender.Transport t = (url, chunk, token, timeout) -> new LogChunkSender.Attempt(413, 0, false);
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
            return n == 1 ? new LogChunkSender.Attempt(401, -1, false) : new LogChunkSender.Attempt(202, -1, false);
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
            return n < 3 ? new LogChunkSender.Attempt(503, -1, false) : new LogChunkSender.Attempt(202, -1, false);
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
            return n < 2 ? new LogChunkSender.Attempt(0, -1, true) : new LogChunkSender.Attempt(202, -1, false);
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
            return n <= 50 ? new LogChunkSender.Attempt(503, -1, false) : new LogChunkSender.Attempt(202, -1, false);
        };
        LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(), t,
                ms -> { }, directExecutor());
        RecordingOwner owner = new RecordingOwner();
        sender.enqueue(owner, chunk("r1", 0, false));

        assertEquals(51, calls.get());
        assertEquals(1, owner.accepted.size());
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
            return new LogChunkSender.Attempt(202, -1, false);
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
