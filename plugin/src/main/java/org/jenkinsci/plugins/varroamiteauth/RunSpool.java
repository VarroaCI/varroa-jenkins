package org.jenkinsci.plugins.varroamiteauth;

import jenkins.model.Jenkins;
import net.sf.json.JSONObject;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.nio.file.StandardCopyOption;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;
import java.util.Locale;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.logging.Level;
import java.util.logging.Logger;
import java.util.stream.Stream;

/**
 * Per-run disk spool under {@code $JENKINS_HOME/varroa/spool/<run_id>/}.
 *
 * <p>This is the durable record, not an overflow buffer (D11): every event
 * that is not a {@code HEARTBEAT} gets a file here before the plugin posts
 * it, so a mite restart cannot lose an event the cell already accepted.
 *
 * <p>One file holds one event, named by its sequence number and its phase,
 * so a directory listing sorted by name is the replay order and needs no
 * index file. A byte cap per run evicts the oldest file whose phase is not
 * {@code COMPLETED}; a {@code COMPLETED} file is never evicted by the cap.
 *
 * <p>{@link #append}, {@link #readOrdered}, {@link #acknowledge}, and
 * {@link #deleteRun} for one run id take that run's own monitor, so a
 * recorder's write and a dispatcher's read or write never race on the
 * same run's directory listing, file contents, or eviction count — even
 * across separate {@code RunSpool} instances. The lock table is
 * process-wide, keyed by the run's resolved directory path, because
 * {@link VarroaRunRecorder} and {@link BuildEventDispatcher} each hold
 * their own {@code RunSpool} instance over the same directory.
 *
 * <p>Every monitor in that table is reference-counted: a caller holds a
 * reservation on it from the moment it resolves the monitor to the
 * moment it releases it, and the table entry is retired only when its
 * count drops to zero. This keeps two callers from ever operating under
 * two different monitor objects for the same run at once — one caller
 * holding a monitor obtained just before {@link #deleteRun} retires the
 * table entry, another obtaining a fresh monitor for the same run right
 * after, both believing they hold exclusive access.
 *
 * <p>{@link #append} writes to a temporary file in the run's directory
 * and renames it into place, so a concurrent {@link #readOrdered} never
 * observes a partially written file.
 */
public class RunSpool {

    private static final Logger LOGGER = Logger.getLogger(RunSpool.class.getName());

    /** Default per-run byte cap: 64 MiB. */
    static final long DEFAULT_CAP_BYTES = 64L * 1024 * 1024;

    private static final String FILE_SUFFIX = ".json";
    private static final String TMP_SUFFIX = ".tmp";

    /**
     * The phase a run-state record carries, and the name prefix that
     * keeps its sequence space apart from a build event's. A build
     * event's acknowledge deletes by the bare decimal prefix, so a
     * run-state record sharing a sequence would be deleted with it.
     * The prefix sorts after every digit, so a replay still reads every
     * build event first.
     */
    static final String PHASE_RUN_STATE = "RUNSTATE";
    private static final String RUN_STATE_PREFIX = "S";

    /**
     * Process-wide, keyed by a run's resolved directory path, so every
     * {@code RunSpool} instance over the same directory shares the same
     * monitor for that run.
     */
    private static final ConcurrentHashMap<String, RunLock> RUN_LOCKS = new ConcurrentHashMap<>();

    private final Path baseDir;
    private final long capBytesPerRun;
    /**
     * The live-run journal whose entry a spooled {@code COMPLETED} event
     * supersedes, or {@code null} for none. The entry goes only once the
     * completion is on disk, because until then the entry is the only
     * durable record of the run.
     */
    private final RunJournal journal;
    private final java.util.concurrent.atomic.AtomicLong droppedCount = new java.util.concurrent.atomic.AtomicLong();

    public RunSpool() {
        this(resolveBaseDir(), readCapBytes(), new RunJournal());
    }

    /** Package-private constructor for testing: injects the base directory and the cap, with no journal. */
    RunSpool(Path baseDir, long capBytesPerRun) {
        this(baseDir, capBytesPerRun, null);
    }

    /** Package-private constructor for testing: injects the base directory, the cap, and the journal. */
    RunSpool(Path baseDir, long capBytesPerRun, RunJournal journal) {
        this.baseDir = baseDir;
        this.capBytesPerRun = capBytesPerRun;
        this.journal = journal;
    }

    /**
     * Writes a spool file for {@code event}. A {@code HEARTBEAT} event is
     * never written. Evicts the oldest non-{@code COMPLETED} file of the
     * run when the write would put the run's directory above the byte cap.
     * Returns true only when the event's file is on disk. A {@code
     * COMPLETED} event on disk deletes the run's journal entry.
     */
    public boolean append(String runId, BuildEvent event) {
        if (event == null || event.isHeartbeat()) {
            return false;
        }
        RunLock lock = acquireLock(runId);
        try {
            synchronized (lock) {
                // The cap eviction never removes the file just written, so
                // an eviction failure after the rename still leaves it.
                boolean written = false;
                try {
                    Path dir = runDir(runId);
                    Files.createDirectories(dir);
                    Path file = dir.resolve(fileName(event.getSeq(), event.getPhase()));
                    // Write to a temp name first, then rename into place: a
                    // concurrent readOrdered() (another RunSpool instance
                    // over this directory, sharing this run's lock) never
                    // sees a half-written file.
                    Path temp = dir.resolve("." + UUID.randomUUID() + TMP_SUFFIX);
                    byte[] body = event.toJson().toString().getBytes(StandardCharsets.UTF_8);
                    Files.write(temp, body);
                    Files.move(temp, file, StandardCopyOption.ATOMIC_MOVE, StandardCopyOption.REPLACE_EXISTING);
                    written = true;
                    evictToCap(dir, file);
                } catch (IOException e) {
                    LOGGER.log(Level.WARNING, "RunSpool: failed to write spool file for run " + runId, e);
                }
                if (written && journal != null && BuildEvent.PHASE_COMPLETED.equals(event.getPhase())) {
                    journal.remove(runId);
                }
                return written;
            }
        } finally {
            releaseLock(runId, lock);
        }
    }

    /**
     * Writes a spool file for one run-state record. The record holds the
     * run-state route's own headers and its gzip body, in place of a
     * build event.
     */
    public void appendRunState(String runId, long seq, JSONObject record) {
        if (record == null) {
            return;
        }
        RunLock lock = acquireLock(runId);
        try {
            synchronized (lock) {
                try {
                    Path dir = runDir(runId);
                    Files.createDirectories(dir);
                    Path file = dir.resolve(runStateFileName(seq));
                    Path temp = dir.resolve("." + UUID.randomUUID() + TMP_SUFFIX);
                    Files.write(temp, record.toString().getBytes(StandardCharsets.UTF_8));
                    Files.move(temp, file, StandardCopyOption.ATOMIC_MOVE, StandardCopyOption.REPLACE_EXISTING);
                    evictToCap(dir, file);
                } catch (IOException e) {
                    LOGGER.log(Level.WARNING, "RunSpool: failed to write a run-state record for run " + runId, e);
                }
            }
        } finally {
            releaseLock(runId, lock);
        }
    }

    /** Returns the run's spooled run-state records in replay order. */
    public List<Entry> readRunStateOrdered(String runId) {
        List<Entry> out = new ArrayList<>();
        for (Entry entry : readOrdered(runId)) {
            if (entry.runState) {
                out.add(entry);
            }
        }
        return out;
    }

    /** Deletes the spooled run-state record for {@code (runId, seq)}, if present. */
    public void acknowledgeRunState(String runId, long seq) {
        RunLock lock = acquireLock(runId);
        try {
            synchronized (lock) {
                Path dir = runDir(runId);
                if (!Files.isDirectory(dir)) {
                    return;
                }
                String prefix = RUN_STATE_PREFIX + seqPrefix(seq);
                try (Stream<Path> files = Files.list(dir)) {
                    files.filter(p -> p.getFileName().toString().startsWith(prefix))
                            .forEach(this::deleteQuietly);
                } catch (IOException e) {
                    LOGGER.log(Level.WARNING,
                            "RunSpool: failed to acknowledge a run-state record for run " + runId, e);
                }
            }
        } finally {
            releaseLock(runId, lock);
        }
    }

    /**
     * Deletes every run-state record of {@code runId}, then removes the
     * run's directory when no event file is left.
     *
     * <p>It deletes the run-state records alone, never the directory
     * outright. A build event of the same run lives in that directory,
     * and the run's own {@code COMPLETED} event can still be waiting
     * there for the dispatcher, so an unconditional delete would lose
     * it.
     */
    public void deleteRunStateRecords(String runId) {
        RunLock lock = acquireLock(runId);
        try {
            synchronized (lock) {
                Path dir = runDir(runId);
                if (!Files.isDirectory(dir)) {
                    return;
                }
                try (Stream<Path> files = Files.list(dir)) {
                    files.filter(p -> p.getFileName().toString().startsWith(RUN_STATE_PREFIX))
                            .forEach(this::deleteQuietly);
                } catch (IOException e) {
                    LOGGER.log(Level.WARNING,
                            "RunSpool: failed to delete the run-state records of run " + runId, e);
                }
            }
        } finally {
            releaseLock(runId, lock);
        }
        deleteRunIfEmpty(runId);
    }

    /**
     * Returns every run id that has a spool directory, in ascending
     * lexicographic order. A run id is a UUIDv7, so this order is also
     * oldest first, which {@link VarroaBuildEventLifecycle} relies on to
     * replay the runs it finds at startup in the order they ran.
     */
    public List<String> listRuns() {
        List<String> runs = new ArrayList<>();
        if (!Files.isDirectory(baseDir)) {
            return runs;
        }
        try (Stream<Path> dirs = Files.list(baseDir)) {
            dirs.filter(Files::isDirectory)
                    .map(p -> p.getFileName().toString())
                    .sorted()
                    .forEach(runs::add);
        } catch (IOException e) {
            LOGGER.log(Level.WARNING, "RunSpool: failed to list the spool directory", e);
        }
        return runs;
    }

    /**
     * Returns the run's spooled events in replay order (ascending
     * sequence), each carrying its sequence, its phase, and its raw JSON
     * body.
     */
    public List<Entry> readOrdered(String runId) {
        List<Entry> entries = new ArrayList<>();
        RunLock lock = acquireLock(runId);
        try {
            synchronized (lock) {
                Path dir = runDir(runId);
                if (!Files.isDirectory(dir)) {
                    return entries;
                }
                try (Stream<Path> files = Files.list(dir)) {
                    List<Path> sorted = files.filter(p -> p.getFileName().toString().endsWith(FILE_SUFFIX))
                            .sorted(Comparator.comparing(p -> p.getFileName().toString()))
                            .toList();
                    for (Path file : sorted) {
                        Entry entry = readEntry(file);
                        if (entry != null) {
                            entries.add(entry);
                        }
                    }
                } catch (IOException e) {
                    LOGGER.log(Level.WARNING, "RunSpool: failed to list spool directory for run " + runId, e);
                }
            }
        } finally {
            releaseLock(runId, lock);
        }
        return entries;
    }

    /** Deletes the spooled file for {@code (runId, seq)}, if present. */
    public void acknowledge(String runId, long seq) {
        RunLock lock = acquireLock(runId);
        try {
            synchronized (lock) {
                Path dir = runDir(runId);
                if (!Files.isDirectory(dir)) {
                    return;
                }
                String prefix = seqPrefix(seq);
                try (Stream<Path> files = Files.list(dir)) {
                    files.filter(p -> p.getFileName().toString().startsWith(prefix))
                            .forEach(this::deleteQuietly);
                } catch (IOException e) {
                    LOGGER.log(Level.WARNING, "RunSpool: failed to acknowledge seq " + seq + " for run " + runId, e);
                }
            }
        } finally {
            releaseLock(runId, lock);
        }
    }

    /** Deletes the run's whole spool directory. Call after acking its {@code COMPLETED} event. */
    public void deleteRun(String runId) {
        RunLock lock = acquireLock(runId);
        try {
            synchronized (lock) {
                Path dir = runDir(runId);
                if (Files.isDirectory(dir)) {
                    try (Stream<Path> files = Files.walk(dir)) {
                        files.sorted(Comparator.reverseOrder()).forEach(this::deleteQuietly);
                    } catch (IOException e) {
                        LOGGER.log(Level.WARNING, "RunSpool: failed to delete spool directory for run " + runId, e);
                    }
                }
            }
        } finally {
            releaseLock(runId, lock);
        }
    }

    /**
     * Deletes the run's spool directory only when it holds no event
     * file. An unacknowledged sibling still on disk keeps the directory
     * in place. A temporary file that an interrupted {@link #append}
     * left behind is not an event and does not keep it. Safe to call
     * after any {@link #acknowledge}: a later {@link #append} recreates
     * the directory.
     */
    public void deleteRunIfEmpty(String runId) {
        RunLock lock = acquireLock(runId);
        try {
            synchronized (lock) {
                Path dir = runDir(runId);
                if (!Files.isDirectory(dir)) {
                    return;
                }
                List<Path> residual;
                try (Stream<Path> files = Files.list(dir)) {
                    residual = files.toList();
                } catch (IOException e) {
                    LOGGER.log(Level.WARNING, "RunSpool: failed to check spool directory for run " + runId, e);
                    return;
                }
                if (residual.stream().anyMatch(p -> p.getFileName().toString().endsWith(FILE_SUFFIX))) {
                    return;
                }
                residual.forEach(this::deleteQuietly);
                deleteQuietly(dir);
            }
        } finally {
            releaseLock(runId, lock);
        }
    }

    /**
     * Resolves {@code runId}'s monitor and reserves it: the table entry
     * for this run is retired only once every reservation on it,
     * including this one, has been released. The lock table is keyed by
     * the resolved directory path, not the bare run id, so every
     * {@code RunSpool} instance over the same base directory shares the
     * same monitor for a given run.
     */
    private RunLock acquireLock(String runId) {
        return RUN_LOCKS.compute(lockKey(runId), (key, existing) -> {
            RunLock lock = existing != null ? existing : new RunLock();
            lock.users++;
            return lock;
        });
    }

    /**
     * Releases a reservation obtained from {@link #acquireLock}. Retires
     * the table entry only when this was the last outstanding
     * reservation, so a concurrent caller that already resolved the
     * same monitor never gets stranded on a retired entry while a fresh
     * caller mints a second, unsynchronized monitor for the same run.
     */
    private void releaseLock(String runId, RunLock lock) {
        RUN_LOCKS.compute(lockKey(runId), (key, existing) -> {
            lock.users--;
            return lock.users > 0 ? lock : null;
        });
    }

    private String lockKey(String runId) {
        return runDir(runId).toAbsolutePath().normalize().toString();
    }

    /** A per-run monitor, reference-counted so the lock table retires it only when unused. */
    private static final class RunLock {
        private int users;
    }

    /** Number of spool files dropped by cap eviction. Package-private for testing. */
    long droppedCountForTesting() {
        return droppedCount.get();
    }

    /** Reserves {@code runId}'s monitor without entering it. Package-private for testing. */
    Object acquireLockForTesting(String runId) {
        return acquireLock(runId);
    }

    /** Releases a reservation from {@link #acquireLockForTesting}. Package-private for testing. */
    void releaseLockForTesting(String runId, Object lock) {
        releaseLock(runId, (RunLock) lock);
    }

    // --- Internals ---

    private Path runDir(String runId) {
        return baseDir.resolve(runId);
    }

    private static String fileName(long seq, String phase) {
        return seqPrefix(seq) + phase + FILE_SUFFIX;
    }

    private static String runStateFileName(long seq) {
        return RUN_STATE_PREFIX + seqPrefix(seq) + PHASE_RUN_STATE + FILE_SUFFIX;
    }

    private static String seqPrefix(long seq) {
        return String.format(Locale.ROOT, "%020d-", seq);
    }

    private Entry readEntry(Path file) {
        String name = fileName(file);
        boolean runState = name.startsWith(RUN_STATE_PREFIX);
        String body = runState ? name.substring(RUN_STATE_PREFIX.length()) : name;
        int dash = body.indexOf('-');
        if (dash < 0) {
            return null;
        }
        long seq;
        try {
            seq = Long.parseLong(body.substring(0, dash));
        } catch (NumberFormatException e) {
            return null;
        }
        name = body;
        String phase = name.substring(dash + 1, name.length() - FILE_SUFFIX.length());
        String json;
        try {
            json = Files.readString(file, StandardCharsets.UTF_8);
        } catch (IOException e) {
            LOGGER.log(Level.FINE, "RunSpool: failed to read spool file " + file, e);
            return null;
        }
        try {
            // Validates the body parses as JSON before handing it back.
            // A truncated or otherwise corrupt file must never reach the
            // dispatcher: a raw JSON parse failure there is an unchecked
            // exception that would kill its daemon thread.
            JSONObject.fromObject(json);
        } catch (RuntimeException e) {
            LOGGER.log(Level.WARNING, "RunSpool: skipping an unparsable spool file " + file, e);
            return null;
        }
        return new Entry(seq, phase, json, runState);
    }

    private void evictToCap(Path dir, Path justWritten) throws IOException {
        long total = directorySize(dir);
        if (total <= capBytesPerRun) {
            return;
        }
        List<Path> ordered;
        try (Stream<Path> files = Files.list(dir)) {
            ordered = files.filter(p -> p.getFileName().toString().endsWith(FILE_SUFFIX))
                    .filter(p -> !p.equals(justWritten))
                    .sorted(Comparator.comparing(p -> p.getFileName().toString()))
                    .toList();
        }
        for (Path candidate : ordered) {
            if (total <= capBytesPerRun) {
                break;
            }
            if (isCompletedFile(candidate)) {
                continue;
            }
            long size = Files.size(candidate);
            deleteQuietly(candidate);
            droppedCount.incrementAndGet();
            PluginCounters.increment(PluginCounters.SPOOL_DROPPED);
            total -= size;
        }
    }

    private static boolean isCompletedFile(Path file) {
        String name = fileName(file);
        return name.contains("-" + BuildEvent.PHASE_COMPLETED + FILE_SUFFIX);
    }

    private static long directorySize(Path dir) throws IOException {
        try (Stream<Path> files = Files.list(dir)) {
            long total = 0;
            for (Path p : (Iterable<Path>) files::iterator) {
                total += Files.size(p);
            }
            return total;
        }
    }

    private void deleteQuietly(Path path) {
        try {
            Files.deleteIfExists(path);
        } catch (IOException e) {
            LOGGER.log(Level.FINE, "RunSpool: failed to delete " + path, e);
        }
    }

    private static Path resolveBaseDir() {
        Jenkins jenkins = Jenkins.getInstanceOrNull();
        if (jenkins != null) {
            return jenkins.getRootDir().toPath().resolve("varroa").resolve("spool");
        }
        String home = System.getenv("JENKINS_HOME");
        if (home == null || home.isEmpty()) {
            home = System.getProperty("user.dir");
        }
        return Paths.get(home, "varroa", "spool");
    }

    private static long readCapBytes() {
        String raw = System.getenv("VARROA_SPOOL_BYTES");
        if (raw == null || raw.isEmpty()) {
            return DEFAULT_CAP_BYTES;
        }
        try {
            long parsed = Long.parseLong(raw);
            return parsed > 0 ? parsed : DEFAULT_CAP_BYTES;
        } catch (NumberFormatException e) {
            return DEFAULT_CAP_BYTES;
        }
    }

    /** One spooled event: its sequence, its phase, and its raw JSON body. */
    public static final class Entry {
        public final long seq;
        public final String phase;
        public final String json;
        /**
         * True for a run-state record. A replay reader tells the two
         * variants apart by this flag: a run-state record carries the
         * run-state route's headers and a gzip body, never a build
         * event, so the build-event dispatcher must skip it.
         */
        public final boolean runState;

        public Entry(long seq, String phase, String json) {
            this(seq, phase, json, false);
        }

        public Entry(long seq, String phase, String json, boolean runState) {
            this.seq = seq;
            this.phase = phase;
            this.json = json;
            this.runState = runState;
        }

        /** Parses {@link #json} into a {@link JSONObject}. */
        public JSONObject toJsonObject() {
            return JSONObject.fromObject(json);
        }
    }

    private static String fileName(Path p) {
        Path n = p.getFileName();
        return n == null ? "" : n.toString();
    }
}
