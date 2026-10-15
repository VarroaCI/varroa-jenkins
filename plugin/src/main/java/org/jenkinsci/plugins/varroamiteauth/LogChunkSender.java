package org.jenkinsci.plugins.varroamiteauth;

import java.net.URI;
import java.net.URLEncoder;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.net.http.HttpTimeoutException;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.ArrayDeque;
import java.util.Deque;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.ThreadLocalRandom;
import java.util.concurrent.atomic.AtomicLong;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Shared flusher pool (D2, D3): posts every build's queued chunks to the
 * mite's {@code POST /v1/log}, with at most one post in flight per build.
 *
 * <p>A queued chunk is retried forever on a transport failure, a timeout, or
 * {@code 503}. It ends only on acceptance ({@code 202}) or a terminal
 * {@code 400}/{@code 413}. No cap and no timer discards a queued chunk (D2).
 *
 * <p>A {@code 503} that carries {@code X-Varroa-Pause-Ms} names the time left
 * in the mite's busy pause window. The sender waits that time, plus up to 10%
 * jitter, and does not grow its backoff. Every other retry uses the backoff.
 */
class LogChunkSender {

    private static final Logger LOGGER = Logger.getLogger(LogChunkSender.class.getName());

    static final String DEFAULT_CHUNK_BYTES_ENV = "VARROA_LOG_CHUNK_BYTES";
    static final long DEFAULT_CHUNK_BYTES = 262_144L;

    private static final long DEFAULT_POST_TIMEOUT_MS = 5000L;
    private static final long BACKOFF_MIN_MS = 250L;
    private static final long BACKOFF_MAX_MS = 5000L;
    /** The longest pause the sender honours. The gateway asks for at most 8000 ms. */
    static final long PAUSE_MAX_MS = 10_000L;
    static final String PAUSE_HEADER = "X-Varroa-Pause-Ms";
    private static final int STATUS_ACCEPTED = 202;
    private static final int STATUS_BAD_REQUEST = 400;
    private static final int STATUS_UNAUTHORIZED = 401;
    private static final int STATUS_TOO_LARGE = 413;
    private static final int STATUS_UNAVAILABLE = 503;

    /** Lowered by a {@code 413} response; never raised above the configured value. Shared across every build. */
    private static final AtomicLong CURRENT_CHUNK_CAP_BYTES = new AtomicLong(-1L);

    private final String logUrl;
    private final LoopbackTokenFile tokenFile;
    private final Transport transport;
    private final Sleeper sleeper;
    private final ExecutorService pool;

    private final ConcurrentHashMap<String, RunQueue> queues = new ConcurrentHashMap<>();

    LogChunkSender(String logUrl) {
        this(logUrl, new LoopbackTokenFile(), LogChunkSender::httpAttempt, LogChunkSender::threadSleep,
                Executors.newCachedThreadPool(r -> {
                    Thread t = new Thread(r, "varroa-log-chunk-sender");
                    t.setDaemon(true);
                    return t;
                }));
    }

    /** Package-private constructor for testing: injects every collaborator. */
    LogChunkSender(String logUrl, LoopbackTokenFile tokenFile, Transport transport, Sleeper sleeper, ExecutorService pool) {
        this.logUrl = logUrl;
        this.tokenFile = tokenFile;
        this.transport = transport;
        this.sleeper = sleeper;
        this.pool = pool;
    }

    /** The current chunk-size cap: the configured default, unless a {@code 413} lowered it. */
    static long currentChunkCapBytes() {
        long lowered = CURRENT_CHUNK_CAP_BYTES.get();
        return lowered > 0 ? lowered : configuredChunkCapBytes();
    }

    private static long configuredChunkCapBytes() {
        String raw = System.getenv(DEFAULT_CHUNK_BYTES_ENV);
        if (raw == null || raw.isEmpty()) {
            return DEFAULT_CHUNK_BYTES;
        }
        try {
            long parsed = Long.parseLong(raw);
            return parsed > 0 ? parsed : DEFAULT_CHUNK_BYTES;
        } catch (NumberFormatException e) {
            return DEFAULT_CHUNK_BYTES;
        }
    }

    /** Resets the shared lowered cap. Package-private for testing. */
    static void resetChunkCapForTesting() {
        CURRENT_CHUNK_CAP_BYTES.set(-1L);
    }

    /**
     * Queues {@code chunk} for its build. Starts a worker for that build's
     * queue when none is running. {@code owner} learns the outcome so it can
     * release the chunk's bytes from the outstanding-byte caps.
     */
    void enqueue(Owner owner, LogChunk chunk) {
        RunQueue rq = queues.computeIfAbsent(chunk.runId, k -> new RunQueue(owner));
        boolean start;
        synchronized (rq) {
            rq.pending.addLast(chunk);
            start = !rq.sending;
            if (start) {
                rq.sending = true;
            }
        }
        if (start) {
            pool.submit(() -> pump(chunk.runId, rq));
        }
    }

    private void pump(String runId, RunQueue rq) {
        while (true) {
            LogChunk chunk;
            synchronized (rq) {
                chunk = rq.pending.peekFirst();
                if (chunk == null) {
                    rq.sending = false;
                    queues.remove(runId, rq);
                    return;
                }
            }
            boolean resolved = postWithRetry(rq, chunk);
            if (!resolved) {
                // Sender is shutting down or the queue was abandoned; stop this worker.
                return;
            }
            synchronized (rq) {
                rq.pending.pollFirst();
            }
        }
    }

    /** Posts one chunk, retrying until acceptance or a terminal outcome. Returns {@code false} only if interrupted. */
    private boolean postWithRetry(RunQueue rq, LogChunk chunk) {
        int attempt = 0;
        String token = tokenFile.token();
        while (true) {
            Attempt result = transport.attempt(logUrl, chunk, token, postTimeoutMillis());
            if (!result.transportFailure && result.statusCode == STATUS_ACCEPTED) {
                rq.owner.onChunkAccepted(chunk);
                return true;
            }
            if (!result.transportFailure && result.statusCode == STATUS_BAD_REQUEST) {
                LOGGER.log(Level.WARNING, "LogChunkSender: mite rejected chunk seq=" + chunk.seq
                        + " for run " + chunk.runId + " with 400");
                rq.owner.onChunkTerminallyFailed(chunk);
                return true;
            }
            if (!result.transportFailure && result.statusCode == STATUS_TOO_LARGE) {
                LOGGER.log(Level.WARNING, "LogChunkSender: mite rejected chunk seq=" + chunk.seq
                        + " for run " + chunk.runId + " with 413");
                applyLoweredCap(result.maxChunkBytes);
                rq.owner.onChunkTerminallyFailed(chunk);
                return true;
            }
            long waitMillis;
            if (!result.transportFailure && result.statusCode == STATUS_UNAVAILABLE && result.pauseMillis > 0) {
                LOGGER.log(Level.FINE, "LogChunkSender: mite answered 503 with a "
                        + result.pauseMillis + " ms pause, retrying");
                waitMillis = pauseWaitMillis(result.pauseMillis);
            } else {
                if (!result.transportFailure && result.statusCode == STATUS_UNAUTHORIZED) {
                    token = tokenFile.onUnauthorized();
                } else if (!result.transportFailure && result.statusCode == STATUS_UNAVAILABLE) {
                    LOGGER.log(Level.FINE, "LogChunkSender: mite answered 503, retrying");
                } else {
                    LOGGER.log(Level.FINE, "LogChunkSender: post failed, status=" + result.statusCode);
                }
                attempt++;
                waitMillis = backoffMillis(attempt);
            }
            try {
                sleeper.sleep(waitMillis);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                return false;
            }
        }
    }

    private void applyLoweredCap(long headerValue) {
        if (headerValue <= 4095) {
            return;
        }
        long current = currentChunkCapBytes();
        if (headerValue >= current) {
            return;
        }
        CURRENT_CHUNK_CAP_BYTES.set(headerValue);
    }

    private static long postTimeoutMillis() {
        String raw = System.getenv("VARROA_LOG_POST_TIMEOUT_MS");
        if (raw == null || raw.isEmpty()) {
            return DEFAULT_POST_TIMEOUT_MS;
        }
        try {
            long parsed = Long.parseLong(raw);
            return parsed > 0 ? parsed : DEFAULT_POST_TIMEOUT_MS;
        } catch (NumberFormatException e) {
            return DEFAULT_POST_TIMEOUT_MS;
        }
    }

    /** Jittered backoff from 250 ms to 5000 ms. Package-private for testing. */
    static long backoffMillis(int attempt) {
        long base = Math.min(BACKOFF_MAX_MS, BACKOFF_MIN_MS * (1L << Math.min(attempt, 8)));
        long span = base - BACKOFF_MIN_MS;
        long jitter = span > 0 ? ThreadLocalRandom.current().nextLong(span + 1) : 0;
        return BACKOFF_MIN_MS + jitter;
    }

    /**
     * The wait for a {@code 503} that names a pause: the pause, clamped to
     * {@link #PAUSE_MAX_MS}, plus 0% to 10% jitter. The jitter only adds, so
     * the retry never lands before the window closes. Package-private for testing.
     */
    static long pauseWaitMillis(long pauseMillis) {
        long base = Math.min(pauseMillis, PAUSE_MAX_MS);
        long spread = base / 10;
        long jitter = spread > 0 ? ThreadLocalRandom.current().nextLong(spread + 1) : 0;
        return base + jitter;
    }

    /**
     * Parses {@code X-Varroa-Pause-Ms}. Answers -1 for a missing, malformed,
     * zero, or negative value, so the sender keeps its backoff. Package-private for testing.
     */
    static long parsePauseMillis(String raw) {
        if (raw == null) {
            return -1L;
        }
        try {
            long parsed = Long.parseLong(raw.trim());
            return parsed > 0 ? parsed : -1L;
        } catch (NumberFormatException e) {
            return -1L;
        }
    }

    private static void threadSleep(long millis) throws InterruptedException {
        Thread.sleep(millis);
    }

    // ---- default HTTP transport ----

    private static final HttpClient SHARED_CLIENT = HttpClient.newBuilder()
            .connectTimeout(Duration.ofSeconds(1))
            .build();

    private static Attempt httpAttempt(String url, LogChunk chunk, String token, long timeoutMillis) {
        try {
            HttpRequest.Builder b = HttpRequest.newBuilder()
                    .uri(URI.create(url))
                    .timeout(Duration.ofMillis(timeoutMillis))
                    .header("Content-Type", "application/octet-stream")
                    .header("X-Varroa-Run-Id", chunk.runId)
                    .header("X-Varroa-Job-Path", URLEncoder.encode(chunk.jobPath, StandardCharsets.UTF_8))
                    .header("X-Varroa-Build-Number", Long.toString(chunk.buildNumber))
                    .header("X-Varroa-Seq", Long.toString(chunk.seq))
                    .header("X-Varroa-Node-Id", chunk.nodeId)
                    .header("X-Varroa-Stage-Id", chunk.stageId)
                    .header("X-Varroa-First-Line-Unix-Ms", Long.toString(chunk.firstLineUnixMs))
                    .header("X-Varroa-Dropped-Bytes", Long.toString(chunk.droppedBytes))
                    .header("X-Varroa-Final", Boolean.toString(chunk.isFinal))
                    .POST(HttpRequest.BodyPublishers.ofByteArray(chunk.data));
            if (token != null) {
                b.header("Authorization", "Bearer " + token);
            }
            HttpResponse<byte[]> resp = SHARED_CLIENT.send(b.build(), HttpResponse.BodyHandlers.ofByteArray());
            long maxChunkBytes = parseMaxChunkBytesHeader(resp);
            long pauseMillis = parsePauseMillis(resp.headers().firstValue(PAUSE_HEADER).orElse(null));
            return new Attempt(resp.statusCode(), maxChunkBytes, pauseMillis, false);
        } catch (HttpTimeoutException e) {
            LOGGER.log(Level.FINE, "LogChunkSender: timeout posting chunk seq=" + chunk.seq, e);
            return new Attempt(0, -1, -1, true);
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "LogChunkSender: transport failure posting chunk seq=" + chunk.seq, e);
            return new Attempt(0, -1, -1, true);
        }
    }

    private static long parseMaxChunkBytesHeader(HttpResponse<?> resp) {
        return resp.headers().firstValue("X-Varroa-Max-Chunk-Bytes").map(v -> {
            try {
                return Long.parseLong(v.trim());
            } catch (NumberFormatException e) {
                return -1L;
            }
        }).orElse(-1L);
    }

    // ---- collaborator seams ----

    /** Learns the outcome of a chunk this sender posted. {@link LogChunkBuffer} implements this. */
    interface Owner {
        void onChunkAccepted(LogChunk chunk);

        void onChunkTerminallyFailed(LogChunk chunk);
    }

    /** Seam for a test to fake the HTTP layer. */
    interface Transport {
        Attempt attempt(String url, LogChunk chunk, String token, long timeoutMillis);
    }

    /** Seam for a test to skip real wall-clock waits. */
    interface Sleeper {
        void sleep(long millis) throws InterruptedException;
    }

    /** One HTTP attempt's outcome. {@code pauseMillis} is -1 when the response names no pause. */
    static final class Attempt {
        final int statusCode;
        final long maxChunkBytes;
        final long pauseMillis;
        final boolean transportFailure;

        Attempt(int statusCode, long maxChunkBytes, long pauseMillis, boolean transportFailure) {
            this.statusCode = statusCode;
            this.maxChunkBytes = maxChunkBytes;
            this.pauseMillis = pauseMillis;
            this.transportFailure = transportFailure;
        }
    }

    private static final class RunQueue {
        final Owner owner;
        final Deque<LogChunk> pending = new ArrayDeque<>();
        boolean sending;

        RunQueue(Owner owner) {
            this.owner = owner;
        }
    }
}
