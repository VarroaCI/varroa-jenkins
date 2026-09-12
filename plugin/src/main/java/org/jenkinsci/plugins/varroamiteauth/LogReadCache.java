package org.jenkinsci.plugins.varroamiteauth;

import hudson.console.AnnotatedLargeText;

import java.io.IOException;
import java.net.URLDecoder;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicLong;
import java.util.function.LongSupplier;
import java.util.logging.Level;
import java.util.logging.Logger;
import java.util.stream.Stream;

import jenkins.model.Jenkins;
import org.kohsuke.stapler.framework.io.ByteBuffer;

/**
 * Cell-local mirror of every run's chunks, written at buffer time and read
 * back by {@code overallLog} and {@code stepLog} (D1).
 *
 * <p>Eviction removes a whole run, never part of one: the tombstone file is
 * written before any chunk file is deleted, under that run's own lock, so a
 * crash leaves a run absent or complete, never partial.
 *
 * <p>The byte accounting that drives both caps is rebuilt from whatever
 * chunk files already sit under {@code baseDir} at construction, not
 * assumed empty: a Jenkins restart finds this cache's files still on disk
 * (D1), and a fresh, zero accounting would let a run or the whole cache
 * grow past its configured cap before the JVM notices.
 */
final class LogReadCache implements LogChunkBuffer.ChunkCache {

    private static final Logger LOGGER = Logger.getLogger(LogReadCache.class.getName());

    /** Per-run tombstone threshold: 512 MiB. */
    static final long DEFAULT_PER_RUN_CAP_BYTES = 512L * 1024 * 1024;

    /** Whole-cache eviction target: 2 GiB. */
    static final long DEFAULT_TOTAL_TARGET_BYTES = 2L * 1024 * 1024 * 1024;

    private static final String TOMBSTONE_NAME = ".tombstone";
    private static final String CHUNK_SUFFIX = ".chunk";

    private final Path baseDir;
    private final long perRunCapBytes;
    private final long totalTargetBytes;
    private final LongSupplier clock;

    private final ConcurrentHashMap<String, Object> runLocks = new ConcurrentHashMap<>();
    private final ConcurrentHashMap<String, Long> runBytes = new ConcurrentHashMap<>();
    private final ConcurrentHashMap<String, Long> lastTouchedMillis = new ConcurrentHashMap<>();
    private final AtomicLong totalBytes = new AtomicLong();

    LogReadCache() {
        this(resolveBaseDir(), DEFAULT_PER_RUN_CAP_BYTES, DEFAULT_TOTAL_TARGET_BYTES, System::currentTimeMillis);
    }

    /** Package-private constructor for testing: injects the base directory, both caps, and the clock. */
    LogReadCache(Path baseDir, long perRunCapBytes, long totalTargetBytes, LongSupplier clock) {
        this.baseDir = baseDir;
        this.perRunCapBytes = perRunCapBytes;
        this.totalTargetBytes = totalTargetBytes;
        this.clock = clock;
        rebuildFromDisk();
    }

    @Override
    public void write(LogChunk chunk) {
        Object lock = lockFor(chunk.runId);
        synchronized (lock) {
            if (isTombstonedLocked(chunk.runId)) {
                return;
            }
            long newTotal = runBytes.getOrDefault(chunk.runId, 0L) + chunk.data.length;
            if (newTotal > perRunCapBytes) {
                tombstoneLocked(chunk.runId);
                return;
            }
            try {
                Path dir = runDir(chunk.runId);
                Files.createDirectories(dir);
                Path file = dir.resolve(fileName(chunk.seq, chunk.nodeId));
                Files.write(file, chunk.data);
                runBytes.put(chunk.runId, newTotal);
                totalBytes.addAndGet(chunk.data.length);
                lastTouchedMillis.put(chunk.runId, clock.getAsLong());
            } catch (IOException e) {
                LOGGER.log(Level.WARNING, "LogReadCache: failed to write chunk seq=" + chunk.seq
                        + " for run " + chunk.runId, e);
            }
        }
        maybeEvictTotal();
    }

    /** Returns the whole run's cached bytes in {@code seq} order, or the pointer text when absent. */
    <T> AnnotatedLargeText<T> overallLog(String runId, T context, String pointerMessage) {
        return readInto(runId, null, context, pointerMessage);
    }

    /** Returns only the cached chunks whose node id matches {@code nodeId}, or the pointer text when absent. */
    <T> AnnotatedLargeText<T> stepLog(String runId, String nodeId, T context, String pointerMessage) {
        return readInto(runId, nodeId != null ? nodeId : "", context, pointerMessage);
    }

    private <T> AnnotatedLargeText<T> readInto(String runId, String nodeIdFilterOrNull, T context, String pointerMessage) {
        Object lock = lockFor(runId);
        synchronized (lock) {
            Path dir = runDir(runId);
            if (isTombstonedLocked(runId) || !Files.isDirectory(dir)) {
                return pointerText(pointerMessage, context);
            }
            lastTouchedMillis.put(runId, clock.getAsLong());
            List<Path> files = listChunkFilesLocked(dir);
            ByteBuffer buf = new ByteBuffer();
            for (Path file : files) {
                ParsedName parsed = parseName(file.getFileName().toString());
                if (parsed == null) {
                    continue;
                }
                if (nodeIdFilterOrNull != null && !nodeIdFilterOrNull.equals(parsed.nodeId)) {
                    continue;
                }
                try {
                    byte[] bytes = Files.readAllBytes(file);
                    buf.write(bytes, 0, bytes.length);
                } catch (IOException e) {
                    LOGGER.log(Level.WARNING, "LogReadCache: failed to read " + file, e);
                }
            }
            boolean complete = !LogChunkBuffer.isRunActive(runId);
            return new AnnotatedLargeText<>(buf, StandardCharsets.UTF_8, complete, context);
        }
    }

    private <T> AnnotatedLargeText<T> pointerText(String pointerMessage, T context) {
        ByteBuffer buf = new ByteBuffer();
        byte[] bytes = pointerMessage.getBytes(StandardCharsets.UTF_8);
        try {
            buf.write(bytes, 0, bytes.length);
        } catch (IOException e) {
            // ByteBuffer.write never actually throws for an in-memory buffer.
            LOGGER.log(Level.FINE, "LogReadCache: unexpected failure building the pointer text", e);
        }
        return new AnnotatedLargeText<>(buf, StandardCharsets.UTF_8, true, context);
    }

    /** Files sorted by ascending {@code seq}, the run's chunks in delivery order. Caller holds the run's lock. */
    private List<Path> listChunkFilesLocked(Path dir) {
        List<Path> files = new ArrayList<>();
        try (Stream<Path> s = Files.list(dir)) {
            s.filter(p -> p.getFileName().toString().endsWith(CHUNK_SUFFIX))
                    .sorted(Comparator.comparing(p -> p.getFileName().toString()))
                    .forEach(files::add);
        } catch (IOException e) {
            LOGGER.log(Level.WARNING, "LogReadCache: failed to list " + dir, e);
        }
        return files;
    }

    private boolean isTombstonedLocked(String runId) {
        return Files.exists(runDir(runId).resolve(TOMBSTONE_NAME));
    }

    /** Writes the tombstone first, then deletes every chunk file. Caller holds the run's lock. */
    private void tombstoneLocked(String runId) {
        Path dir = runDir(runId);
        try {
            Files.createDirectories(dir);
            Files.write(dir.resolve(TOMBSTONE_NAME), new byte[0]);
        } catch (IOException e) {
            LOGGER.log(Level.WARNING, "LogReadCache: failed to write the tombstone for run " + runId, e);
            return;
        }
        long freed = 0L;
        try (Stream<Path> s = Files.list(dir)) {
            List<Path> chunkFiles = s.filter(p -> p.getFileName().toString().endsWith(CHUNK_SUFFIX)).toList();
            for (Path p : chunkFiles) {
                try {
                    freed += Files.size(p);
                    Files.deleteIfExists(p);
                } catch (IOException e) {
                    LOGGER.log(Level.FINE, "LogReadCache: failed to delete " + p, e);
                }
            }
        } catch (IOException e) {
            LOGGER.log(Level.WARNING, "LogReadCache: failed to list " + dir + " for tombstoning", e);
        }
        runBytes.remove(runId);
        lastTouchedMillis.remove(runId);
        totalBytes.addAndGet(-freed);
    }

    /** Tombstones least-recently-used runs, skipping any still active, until the total fits the target. */
    private void maybeEvictTotal() {
        if (totalBytes.get() <= totalTargetBytes) {
            return;
        }
        // A List, not a Map keyed by timestamp: two runs touched in the same
        // millisecond must not collapse into one entry.
        List<Map.Entry<String, Long>> byAge = new ArrayList<>(lastTouchedMillis.entrySet());
        byAge.sort(Map.Entry.comparingByValue());
        boolean sawOnlyActive = true;
        for (Map.Entry<String, Long> ageEntry : byAge) {
            String runId = ageEntry.getKey();
            if (totalBytes.get() <= totalTargetBytes) {
                return;
            }
            if (LogChunkBuffer.isRunActive(runId)) {
                continue;
            }
            sawOnlyActive = false;
            Object lock = lockFor(runId);
            synchronized (lock) {
                if (!isTombstonedLocked(runId)) {
                    tombstoneLocked(runId);
                }
            }
        }
        if (sawOnlyActive && totalBytes.get() > totalTargetBytes) {
            LOGGER.log(Level.WARNING, "LogReadCache: eviction target exceeded, but every cached run is still active");
        }
    }

    /**
     * Rebuilds {@link #runBytes}, {@link #totalBytes}, and {@link
     * #lastTouchedMillis} from chunk files already under {@link
     * #baseDir}. A tombstoned run contributes nothing: its chunk files
     * are already gone, and {@link #isTombstonedLocked} already treats
     * it as absent. Runs once, at construction, before any write reaches
     * this cache.
     */
    private void rebuildFromDisk() {
        if (!Files.isDirectory(baseDir)) {
            return;
        }
        try (Stream<Path> dirs = Files.list(baseDir)) {
            dirs.filter(Files::isDirectory).forEach(this::rebuildRunFromDisk);
        } catch (IOException e) {
            LOGGER.log(Level.WARNING, "LogReadCache: failed to list the cache directory for rebuild", e);
            return;
        }
        // A restart can find the total already past its target, if the
        // JVM ended before the previous session's eviction caught up.
        maybeEvictTotal();
    }

    /**
     * Sums one run's chunk file bytes into {@link #runBytes} and {@link
     * #totalBytes}, and records the newest chunk file's mtime as the
     * run's last-touched time, so a post-restart eviction pass orders
     * runs by the same recency an uninterrupted session would have used.
     */
    private void rebuildRunFromDisk(Path dir) {
        String runId = dir.getFileName().toString();
        if (Files.exists(dir.resolve(TOMBSTONE_NAME))) {
            return;
        }
        long sum = 0L;
        long newestMtimeMillis = 0L;
        try (Stream<Path> files = Files.list(dir)) {
            List<Path> chunkFiles = files.filter(p -> p.getFileName().toString().endsWith(CHUNK_SUFFIX)).toList();
            for (Path file : chunkFiles) {
                try {
                    sum += Files.size(file);
                    long mtimeMillis = Files.getLastModifiedTime(file).toMillis();
                    newestMtimeMillis = Math.max(newestMtimeMillis, mtimeMillis);
                } catch (IOException e) {
                    LOGGER.log(Level.FINE, "LogReadCache: failed to stat " + file + " during rebuild", e);
                }
            }
        } catch (IOException e) {
            LOGGER.log(Level.WARNING, "LogReadCache: failed to list " + dir + " during rebuild", e);
            return;
        }
        if (sum == 0L) {
            return;
        }
        runBytes.put(runId, sum);
        totalBytes.addAndGet(sum);
        lastTouchedMillis.put(runId, newestMtimeMillis);
    }

    private Object lockFor(String runId) {
        return runLocks.computeIfAbsent(runId, k -> new Object());
    }

    private Path runDir(String runId) {
        return baseDir.resolve(runId);
    }

    private static String fileName(long seq, String nodeId) {
        String encoded = URLEncoder.encode(nodeId != null ? nodeId : "", StandardCharsets.UTF_8);
        return String.format(Locale.ROOT, "%020d-%s%s", seq, encoded, CHUNK_SUFFIX);
    }

    private static ParsedName parseName(String name) {
        int dash = name.indexOf('-');
        if (dash < 0 || !name.endsWith(CHUNK_SUFFIX)) {
            return null;
        }
        String nodeIdEncoded = name.substring(dash + 1, name.length() - CHUNK_SUFFIX.length());
        return new ParsedName(URLDecoder.decode(nodeIdEncoded, StandardCharsets.UTF_8));
    }

    /** Total cached bytes across every run. Package-private for testing. */
    long totalBytesForTesting() {
        return totalBytes.get();
    }

    /** One run's cached bytes, or {@code 0} when the run is unknown. Package-private for testing. */
    long runBytesForTesting(String runId) {
        return runBytes.getOrDefault(runId, 0L);
    }

    /** {@code true} once a run's directory carries the tombstone file. Package-private for testing. */
    boolean isTombstonedForTesting(String runId) {
        return isTombstonedLocked(runId);
    }

    private static Path resolveBaseDir() {
        Jenkins jenkins = Jenkins.getInstanceOrNull();
        if (jenkins != null) {
            return jenkins.getRootDir().toPath().resolve("varroa-log-cache");
        }
        String home = System.getenv("JENKINS_HOME");
        if (home == null || home.isEmpty()) {
            home = System.getProperty("user.dir");
        }
        return Paths.get(home, "varroa-log-cache");
    }

    private static final class ParsedName {
        final String nodeId;

        ParsedName(String nodeId) {
            this.nodeId = nodeId;
        }
    }
}
