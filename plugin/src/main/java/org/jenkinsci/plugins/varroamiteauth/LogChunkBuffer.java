package org.jenkinsci.plugins.varroamiteauth;

import java.io.ByteArrayOutputStream;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicLong;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Ordered, capped chunk queue for one build (D1, D2).
 *
 * <p>Every write goes through {@link #append(String, String, byte[], int, int)}
 * under one lock, which is also where {@code seq} is assigned. A write for a
 * node id different from the open segment's closes that segment first, so a
 * chunk never mixes node ids. A background ticker also closes a stale open
 * segment once the applicable flush interval passes.
 *
 * <p>A finished chunk is written to the {@link LogReadCache} before it is
 * handed to the {@link LogChunkSender}, so read-back never depends on the
 * network.
 *
 * <p>No timer and no evictor ever discards a queued chunk (D2). The shared
 * ticker only observes a build whose queue is still present ten minutes
 * after {@link #finish()}: it logs one warning and counts one metric
 * increment for that build, naming the run and the queue depth, and takes
 * no other action.
 */
final class LogChunkBuffer implements LogChunkSender.Owner {

    private static final Logger LOGGER = Logger.getLogger(LogChunkBuffer.class.getName());

    /** Per-build byte cap default: 32 MiB. */
    static final long DEFAULT_BUILD_BUFFER_BYTES = 33_554_432L;

    /** JVM-wide byte cap default: 256 MiB. */
    static final long DEFAULT_TOTAL_BUFFER_BYTES = 268_435_456L;

    /** Unwatched flush interval default: 2000 ms. */
    static final long DEFAULT_FLUSH_MS = 2000L;

    /** Watched (tailed) flush interval default: 250 ms. */
    static final long DEFAULT_TAIL_FLUSH_MS = 250L;

    /** JVM-wide outstanding-byte counter: every build's not-yet-resolved chunk bytes. */
    private static final AtomicLong GLOBAL_OUTSTANDING_BYTES = new AtomicLong();

    /**
     * Delay before the stuck-queue observer warns about a finished build
     * whose sender queue has not drained: ten minutes (D2). The observer
     * only observes: it logs one warning and counts one metric increment
     * per build, and discards nothing.
     */
    static final long STUCK_QUEUE_WARN_MILLIS = 10L * 60 * 1000;

    /** Number of times the stuck-queue observer has warned. */
    private static final AtomicLong STUCK_QUEUE_WARNINGS = new AtomicLong();

    /** Live buffers with an open segment the shared ticker may need to close. */
    private static final Set<LogChunkBuffer> ACTIVE = ConcurrentHashMap.newKeySet();

    /**
     * Buffers past {@link #finish()} whose sender queue has not yet fully
     * drained (accepted or terminally failed every queued chunk,
     * including the byte-less final chunk). The stuck-queue observer
     * scans this set; a buffer leaves it the moment its queue empties.
     */
    private static final Set<LogChunkBuffer> FINISHED_PENDING = ConcurrentHashMap.newKeySet();

    /** Run ids with a build still in progress, so the read cache evictor never races a live writer. */
    private static final Set<String> LIVE_RUN_IDS = ConcurrentHashMap.newKeySet();

    private static volatile ScheduledExecutorService ticker;

    private final String runId;
    private final String jobPath;
    private final long buildNumber;
    private final long buildCapBytes;
    private final long totalCapBytes;
    private final LogChunkSender sender;
    private final ChunkCache cache;
    private final java.util.function.LongSupplier chunkCapBytes;
    private final java.util.function.LongSupplier clock;

    private final Object lock = new Object();

    private String openNodeId;
    private String openStageId = "";
    private ByteArrayOutputStream openData = new ByteArrayOutputStream();
    private long openStartedAtMillis;

    private long seq;

    /** Highest {@code seq} already recorded as resolved in the cache. Only the sender's worker for this build writes it. */
    private volatile long resolvedSeq = -1L;
    private long pendingDropped;
    private long totalDropped;
    private long buildOutstandingBytes;

    /** Chunks handed to the sender that have not yet been accepted or terminally failed. */
    private long chunksInFlight;
    private long finishedAtMillis;
    private boolean stuckWarned;

    private volatile boolean closed;
    private volatile boolean detached;
    private volatile boolean tailActive;
    private boolean capDropped;
    private boolean terminalResponse;
    private LogSummary finishedSummary;

    LogChunkBuffer(String runId, String jobPath, long buildNumber, LogChunkSender sender, ChunkCache cache,
            LogReadCache.ResumePoint resume) {
        this(runId, jobPath, buildNumber, sender, cache, readLongEnv("VARROA_LOG_BUILD_BUFFER_BYTES", DEFAULT_BUILD_BUFFER_BYTES),
                readLongEnv("VARROA_LOG_TOTAL_BUFFER_BYTES", DEFAULT_TOTAL_BUFFER_BYTES),
                LogChunkSender::currentChunkCapBytes, System::currentTimeMillis, resume);
    }

    /** Package-private constructor for testing: injects every cap and the clock, and starts at {@code seq} 0. */
    LogChunkBuffer(String runId, String jobPath, long buildNumber, LogChunkSender sender, ChunkCache cache,
            long buildCapBytes, long totalCapBytes, java.util.function.LongSupplier chunkCapBytes,
            java.util.function.LongSupplier clock) {
        this(runId, jobPath, buildNumber, sender, cache, buildCapBytes, totalCapBytes, chunkCapBytes, clock,
                LogReadCache.ResumePoint.FRESH);
    }

    /**
     * Package-private constructor for testing: injects every cap, the
     * clock, and the resume point.
     *
     * <p>A new JVM must not start a resumed build at {@code seq} 0. The
     * gateway ignores a chunk at or below the sequence it already stored,
     * so the first chunks after the resume would never reach the stored
     * log. The buffer therefore continues at {@code resume.nextSeq}, and it
     * posts {@code resume.replay} before any new chunk. The gateway indexes
     * only an unbroken run of sequences, so the replay must start at or
     * below the first chunk the mite did not accept.
     */
    LogChunkBuffer(String runId, String jobPath, long buildNumber, LogChunkSender sender, ChunkCache cache,
            long buildCapBytes, long totalCapBytes, java.util.function.LongSupplier chunkCapBytes,
            java.util.function.LongSupplier clock, LogReadCache.ResumePoint resume) {
        this.runId = runId;
        this.jobPath = jobPath;
        this.buildNumber = buildNumber;
        this.sender = sender;
        this.cache = cache;
        this.buildCapBytes = buildCapBytes;
        this.totalCapBytes = totalCapBytes;
        this.chunkCapBytes = chunkCapBytes;
        this.clock = clock;
        this.openStartedAtMillis = clock.getAsLong();
        this.seq = resume.nextSeq;
        // The summary of a resumed build must still report what the
        // earlier JVM dropped, or a truncated log reads as complete.
        this.totalDropped = resume.droppedBytes;
        this.capDropped = resume.droppedBytes > 0;
        ACTIVE.add(this);
        LIVE_RUN_IDS.add(runId);
        ensureTicker();
        synchronized (lock) {
            for (LogChunk chunk : resume.replay) {
                int bytes = chunk.accountedBytes();
                buildOutstandingBytes += bytes;
                GLOBAL_OUTSTANDING_BYTES.addAndGet(bytes);
                chunksInFlight++;
                sender.enqueue(this, chunk);
            }
        }
    }

    /** Switches the flush interval. {@link TailEndpoint} and its lease expiry call this. */
    void setTailActive(boolean active) {
        this.tailActive = active;
    }

    /**
     * Appends {@code len} bytes at {@code off} from {@code nodeId}/{@code stageId}.
     * Splits across a node change and the chunk-size cap. Drops incoming bytes,
     * and counts them, once either byte cap is full. Never blocks on the network.
     */
    void append(String nodeId, String stageId, byte[] data, int off, int len) {
        if (len <= 0) {
            return;
        }
        synchronized (lock) {
            if (closed || detached) {
                return;
            }
            String n = nodeId != null ? nodeId : "";
            String s = stageId != null ? stageId : "";
            int pos = off;
            int remaining = len;
            while (remaining > 0) {
                if (openNodeId == null) {
                    openNodeId = n;
                    openStageId = s;
                    openStartedAtMillis = clock.getAsLong();
                } else if (!openNodeId.equals(n)) {
                    closeSegmentLocked(false);
                    openNodeId = n;
                    openStageId = s;
                    openStartedAtMillis = clock.getAsLong();
                }
                long cap = Math.max(1L, chunkCapBytes.getAsLong());
                int room = (int) Math.min(Integer.MAX_VALUE, Math.max(0L, cap - openData.size()));
                if (room == 0) {
                    closeSegmentLocked(false);
                    continue;
                }
                int take = Math.min(room, remaining);
                if (buildOutstandingBytes + take > buildCapBytes
                        || GLOBAL_OUTSTANDING_BYTES.get() + take > totalCapBytes) {
                    // Discard the incoming bytes; keep whatever is already buffered.
                    long allowedBuild = Math.max(0L, buildCapBytes - buildOutstandingBytes);
                    long allowedTotal = Math.max(0L, totalCapBytes - GLOBAL_OUTSTANDING_BYTES.get());
                    int allowed = (int) Math.min(take, Math.min(allowedBuild, allowedTotal));
                    if (allowed > 0) {
                        openData.write(data, pos, allowed);
                        buildOutstandingBytes += allowed;
                        GLOBAL_OUTSTANDING_BYTES.addAndGet(allowed);
                    }
                    int dropped = take - allowed;
                    pendingDropped += dropped;
                    totalDropped += dropped;
                    capDropped = true;
                    pos += take;
                    remaining -= take;
                    continue;
                }
                openData.write(data, pos, take);
                buildOutstandingBytes += take;
                GLOBAL_OUTSTANDING_BYTES.addAndGet(take);
                pos += take;
                remaining -= take;
                if (openData.size() >= cap) {
                    closeSegmentLocked(false);
                }
            }
        }
    }

    /**
     * Runs the four-step end-of-build sequence (D2). Idempotent: a repeated
     * call returns the same summary without emitting a second final chunk.
     * Never waits on the network.
     */
    LogSummary finish() {
        synchronized (lock) {
            if (closed) {
                return finishedSummary;
            }
            if (openNodeId != null && openData.size() > 0) {
                closeSegmentLocked(false);
            }
            closeSegmentLocked(true);
            closed = true;
            finishedAtMillis = clock.getAsLong();
            ACTIVE.remove(this);
            LIVE_RUN_IDS.remove(runId);
            if (chunksInFlight > 0) {
                FINISHED_PENDING.add(this);
            }
            String reason = "";
            boolean incomplete = capDropped || terminalResponse;
            if (capDropped) {
                reason = LogSummary.REASON_PLUGIN_DROP;
            } else if (terminalResponse) {
                reason = LogSummary.REASON_TERMINAL_RESPONSE;
            }
            finishedSummary = LogSummary.builder()
                    .chunkCount(seq)
                    .droppedBytes(totalDropped)
                    .incomplete(incomplete)
                    .incompleteReason(reason)
                    .build();
            return finishedSummary;
        }
    }

    /**
     * Closes the open segment of every live buffer and stops each one
     * from taking more bytes. Jenkins calls this at shutdown, after it
     * pauses every build, so the pause line reaches the read cache and the
     * next JVM posts it again. A later write in this JVM would take a
     * {@code seq} that the next JVM also assigns, so it is discarded.
     */
    static void flushAndDetachAll() {
        for (LogChunkBuffer buffer : ACTIVE) {
            buffer.flushAndDetach();
        }
    }

    private void flushAndDetach() {
        synchronized (lock) {
            if (closed || detached) {
                return;
            }
            // A segment that holds only a dropped-byte count still closes,
            // so the next JVM can read the drop from the cache.
            if ((openNodeId != null && openData.size() > 0) || pendingDropped > 0) {
                closeSegmentLocked(false);
            }
            detached = true;
            ACTIVE.remove(this);
        }
    }

    /** {@code true} once {@link #flushAndDetachAll()} has detached this buffer. Package-private for testing. */
    boolean isDetachedForTesting() {
        return detached;
    }

    /** {@code true} once {@link #finish()} has run. */
    boolean isClosed() {
        return closed;
    }

    /** The dropped-byte total this build has discarded before {@code finish}. Package-private for testing. */
    long totalDroppedForTesting() {
        synchronized (lock) {
            return totalDropped;
        }
    }

    /** Forces the ticker's stale-segment check without waiting on wall-clock time. Package-private for testing. */
    void tickForTesting(long nowMillis) {
        tick(nowMillis);
    }

    /**
     * Forces the stuck-queue observer's check without waiting on the
     * shared ticker or ten real minutes. Package-private for testing.
     */
    void observeStuckQueueForTesting(long nowMillis) {
        checkStuckQueue(nowMillis);
    }

    /**
     * Number of times the stuck-queue observer has warned about a
     * finished build whose sender queue had not drained. Package-private
     * for testing, matching the accessor shape {@link BuildEventSink}
     * and {@link ActivitySink} use for their own dropped counters.
     */
    static long stuckQueueWarningCount() {
        return STUCK_QUEUE_WARNINGS.get();
    }

    /** Resets the stuck-queue warning counter. Package-private for testing. */
    static void resetStuckQueueWarningsForTesting() {
        STUCK_QUEUE_WARNINGS.set(0);
    }

    private void tick(long nowMillis) {
        synchronized (lock) {
            if (closed || detached || openNodeId == null) {
                return;
            }
            long interval = tailActive
                    ? readLongEnv("VARROA_LOG_TAIL_FLUSH_MS", DEFAULT_TAIL_FLUSH_MS)
                    : readLongEnv("VARROA_LOG_FLUSH_MS", DEFAULT_FLUSH_MS);
            if (nowMillis - openStartedAtMillis >= interval && openData.size() > 0) {
                closeSegmentLocked(false);
            }
        }
    }

    /** Called by {@link LogChunkSender} once a chunk is accepted (202). */
    @Override
    public void onChunkAccepted(LogChunk chunk) {
        release(chunk);
    }

    /** Called by {@link LogChunkSender} once a chunk ends on a terminal 400 or 413. */
    @Override
    public void onChunkTerminallyFailed(LogChunk chunk) {
        release(chunk);
        PluginCounters.increment(PluginCounters.LOG_CHUNKS_DROPPED);
        synchronized (lock) {
            if (!closed && !chunk.isFinal) {
                terminalResponse = true;
            }
        }
    }

    private void release(LogChunk chunk) {
        if (chunk.seq > resolvedSeq) {
            // The sender resolves one chunk of a build at a time, in seq
            // order, so this chunk's seq is also the build's watermark.
            resolvedSeq = chunk.seq;
            try {
                cache.recordResolved(runId, chunk.seq);
            } catch (Exception e) {
                LOGGER.log(Level.WARNING, "LogChunkBuffer: failed to record the resolved seq for run " + runId, e);
            }
        }
        int bytes = chunk.accountedBytes();
        boolean drained;
        synchronized (lock) {
            if (bytes > 0) {
                buildOutstandingBytes = Math.max(0L, buildOutstandingBytes - bytes);
            }
            chunksInFlight = Math.max(0L, chunksInFlight - 1);
            drained = closed && chunksInFlight == 0;
        }
        if (bytes > 0) {
            GLOBAL_OUTSTANDING_BYTES.addAndGet(-bytes);
        }
        if (drained) {
            // The final (byte-less) chunk always resolves through this
            // path, so a fully drained queue removes the buffer even
            // though it never touched buildOutstandingBytes.
            FINISHED_PENDING.remove(this);
        }
    }

    /** Closes the current open segment (or emits a zero-byte final chunk) and hands it off. Caller holds {@link #lock}. */
    private void closeSegmentLocked(boolean asFinal) {
        byte[] bytes = asFinal ? new byte[0] : openData.toByteArray();
        String nodeId = asFinal ? "" : (openNodeId != null ? openNodeId : "");
        String stageId = asFinal ? "" : openStageId;
        long firstLine = asFinal ? clock.getAsLong() : openStartedAtMillis;
        long chunkSeq = seq++;
        long dropped = pendingDropped;
        pendingDropped = 0;
        LogChunk chunk = new LogChunk(runId, jobPath, buildNumber, chunkSeq, nodeId, stageId, bytes,
                firstLine, dropped, asFinal);
        chunksInFlight++;
        openNodeId = null;
        openStageId = "";
        openData = new ByteArrayOutputStream();
        try {
            cache.write(chunk);
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "LogChunkBuffer: failed to write the read-back cache for run " + runId, e);
        }
        sender.enqueue(this, chunk);
    }

    private static void ensureTicker() {
        if (ticker != null) {
            return;
        }
        synchronized (LogChunkBuffer.class) {
            if (ticker != null) {
                return;
            }
            ScheduledExecutorService t = Executors.newSingleThreadScheduledExecutor(r -> {
                Thread th = new Thread(r, "varroa-log-chunk-ticker");
                th.setDaemon(true);
                return th;
            });
            t.scheduleWithFixedDelay(LogChunkBuffer::tickAll, 100, 100, TimeUnit.MILLISECONDS);
            ticker = t;
        }
    }

    private static void tickAll() {
        long now = System.currentTimeMillis();
        for (LogChunkBuffer buffer : ACTIVE) {
            try {
                buffer.tick(now);
            } catch (Exception e) {
                LOGGER.log(Level.FINE, "LogChunkBuffer: ticker pass failed for a build", e);
            }
        }
        for (LogChunkBuffer buffer : FINISHED_PENDING) {
            try {
                buffer.checkStuckQueue(now);
            } catch (Exception e) {
                LOGGER.log(Level.FINE, "LogChunkBuffer: stuck-queue observer pass failed for a build", e);
            }
        }
    }

    /**
     * Warns once, and counts once, the first time this buffer's queue is
     * still present ten minutes after {@link #finish()} (D2). Never
     * cancels the queue and never discards a chunk.
     */
    private void checkStuckQueue(long nowMillis) {
        boolean warn;
        long depth;
        synchronized (lock) {
            depth = chunksInFlight;
            warn = !stuckWarned && depth > 0 && (nowMillis - finishedAtMillis) >= STUCK_QUEUE_WARN_MILLIS;
            if (warn) {
                stuckWarned = true;
            }
        }
        if (warn) {
            LOGGER.log(Level.WARNING,
                    "LogChunkBuffer: run {0} still has {1} queued log chunk(s) ten minutes after finish",
                    new Object[] {runId, depth});
            STUCK_QUEUE_WARNINGS.incrementAndGet();
        }
    }

    /** {@code true} while a build for {@code runId} has not called {@link #finish()}. */
    static boolean isRunActive(String runId) {
        return LIVE_RUN_IDS.contains(runId);
    }

    /** Resets the JVM-wide outstanding-byte counter. Package-private for testing. */
    static void resetGlobalOutstandingForTesting() {
        GLOBAL_OUTSTANDING_BYTES.set(0);
    }

    static long globalOutstandingForTesting() {
        return GLOBAL_OUTSTANDING_BYTES.get();
    }

    private static long readLongEnv(String name, long defaultValue) {
        String raw = System.getenv(name);
        if (raw == null || raw.isEmpty()) {
            return defaultValue;
        }
        try {
            long parsed = Long.parseLong(raw);
            return parsed > 0 ? parsed : defaultValue;
        } catch (NumberFormatException e) {
            return defaultValue;
        }
    }

    /** Sink for a finished chunk, written before the chunk enters the sender queue. */
    interface ChunkCache {
        void write(LogChunk chunk);

        /** Records that the mite resolved every chunk of {@code runId} up to and including {@code seq}. */
        default void recordResolved(String runId, long seq) {
        }
    }
}
