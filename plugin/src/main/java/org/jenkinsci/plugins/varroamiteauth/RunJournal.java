package org.jenkinsci.plugins.varroamiteauth;

import jenkins.model.Jenkins;
import net.sf.json.JSONObject;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.nio.file.StandardCopyOption;
import java.nio.file.attribute.FileTime;
import java.util.ArrayList;
import java.util.List;
import java.util.UUID;
import java.util.logging.Level;
import java.util.logging.Logger;
import java.util.regex.Pattern;
import java.util.stream.Stream;

/**
 * The live-run journal under {@code $JENKINS_HOME/varroa/live/}.
 *
 * <p>One file holds one run that this process started and did not yet
 * complete, named {@code <run_id>.json}. {@link VarroaBootReconcile} reads
 * the directory at boot to find each build that a Jenkins stop ended
 * without a {@code RunListener.onCompleted} call.
 *
 * <p>A write goes to a temporary file first and is renamed into place, so
 * a reader never sees a partial file. The heartbeat tick sets each file's
 * modification time, so the time records the last moment the run was
 * live in this process.
 *
 * <p>The base directory resolves on every call, because a test harness
 * starts a new Jenkins home for each test while this class stays loaded.
 */
final class RunJournal {

    private static final Logger LOGGER = Logger.getLogger(RunJournal.class.getName());

    private static final String FILE_SUFFIX = ".json";
    private static final String TMP_SUFFIX = ".tmp";

    /**
     * A run id is a UUID. Any other value is never a file name, so a run
     * id can never name a path outside the journal directory.
     */
    private static final Pattern RUN_ID = Pattern.compile("[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}");

    private final Path fixedBaseDir;

    /** A journal over {@code $JENKINS_HOME/varroa/live}, resolved on every call. */
    RunJournal() {
        this(null);
    }

    /** Package-private constructor for testing: injects the base directory. */
    RunJournal(Path baseDir) {
        this.fixedBaseDir = baseDir;
    }

    /** True when {@code runId} has the UUID form a journal file name needs. */
    static boolean isValidRunId(String runId) {
        return runId != null && RUN_ID.matcher(runId).matches();
    }

    /** Writes the entry for one run. Replaces an earlier entry of the same run. */
    void write(Entry entry) {
        if (entry == null || !isValidRunId(entry.runId)) {
            return;
        }
        try {
            Path dir = baseDir();
            Files.createDirectories(dir);
            Path temp = dir.resolve("." + UUID.randomUUID() + TMP_SUFFIX);
            Files.write(temp, entry.toJson().toString().getBytes(StandardCharsets.UTF_8));
            Files.move(temp, dir.resolve(entry.runId + FILE_SUFFIX),
                    StandardCopyOption.ATOMIC_MOVE, StandardCopyOption.REPLACE_EXISTING);
        } catch (IOException e) {
            LOGGER.log(Level.WARNING, "RunJournal: failed to write the entry of run " + entry.runId, e);
        }
    }

    /** Deletes the entry of one run, if present. */
    void remove(String runId) {
        if (!isValidRunId(runId)) {
            return;
        }
        try {
            Files.deleteIfExists(baseDir().resolve(runId + FILE_SUFFIX));
        } catch (IOException e) {
            LOGGER.log(Level.WARNING, "RunJournal: failed to delete the entry of run " + runId, e);
        }
    }

    /** Sets the entry's modification time to now. Creates no entry when none exists. */
    void touch(String runId) {
        if (!isValidRunId(runId)) {
            return;
        }
        Path file = baseDir().resolve(runId + FILE_SUFFIX);
        try {
            if (Files.exists(file)) {
                Files.setLastModifiedTime(file, FileTime.fromMillis(System.currentTimeMillis()));
            }
        } catch (IOException e) {
            LOGGER.log(Level.FINE, "RunJournal: failed to touch the entry of run " + runId, e);
        }
    }

    /**
     * Returns the entry of one run, or {@code null} when none is present
     * or the file does not parse. A file that does not parse is deleted
     * with a warning.
     */
    Entry read(String runId) {
        if (!isValidRunId(runId)) {
            return null;
        }
        Path file = baseDir().resolve(runId + FILE_SUFFIX);
        if (!Files.isRegularFile(file)) {
            return null;
        }
        return readEntry(file);
    }

    /**
     * Returns every readable entry, in file-name order. A file that does
     * not parse is deleted with a warning, because no later read can
     * repair it.
     */
    List<Entry> readAll() {
        List<Entry> out = new ArrayList<>();
        Path dir = baseDir();
        if (!Files.isDirectory(dir)) {
            return out;
        }
        List<Path> files;
        try (Stream<Path> listing = Files.list(dir)) {
            files = listing.filter(p -> p.getFileName().toString().endsWith(FILE_SUFFIX))
                    .sorted()
                    .toList();
        } catch (IOException e) {
            LOGGER.log(Level.WARNING, "RunJournal: failed to list the journal directory", e);
            return out;
        }
        for (Path file : files) {
            Entry entry = readEntry(file);
            if (entry != null) {
                out.add(entry);
            }
        }
        return out;
    }

    private Entry readEntry(Path file) {
        try {
            String json = Files.readString(file, StandardCharsets.UTF_8);
            long modified = Files.getLastModifiedTime(file).toMillis();
            Entry entry = Entry.fromJson(JSONObject.fromObject(json), modified);
            String expected = fileName(file);
            if (!isValidRunId(entry.runId) || !expected.equals(entry.runId + FILE_SUFFIX)) {
                throw new IllegalArgumentException("the run id does not match the file name");
            }
            return entry;
        } catch (IOException | RuntimeException e) {
            LOGGER.log(Level.WARNING, "RunJournal: deleting an unreadable entry " + file, e);
            try {
                Files.deleteIfExists(file);
            } catch (IOException deleteError) {
                LOGGER.log(Level.FINE, "RunJournal: failed to delete " + file, deleteError);
            }
            return null;
        }
    }

    private Path baseDir() {
        if (fixedBaseDir != null) {
            return fixedBaseDir;
        }
        Jenkins jenkins = Jenkins.getInstanceOrNull();
        if (jenkins != null) {
            return jenkins.getRootDir().toPath().resolve("varroa").resolve("live");
        }
        String home = System.getenv("JENKINS_HOME");
        if (home == null || home.isEmpty()) {
            home = System.getProperty("user.dir");
        }
        return Paths.get(home, "varroa", "live");
    }

    /** One live run: its identity, its start time, and whether it is a pipeline. */
    static final class Entry {
        final String runId;
        final String intentKey;
        final String jobPath;
        final long buildNumber;
        final String deliveryId;
        final long startedAtMillis;
        final boolean pipeline;
        /** The file's modification time. Zero for an entry not read from disk. */
        final long lastSeenMillis;

        Entry(String runId, String intentKey, String jobPath, long buildNumber, String deliveryId,
                long startedAtMillis, boolean pipeline) {
            this(runId, intentKey, jobPath, buildNumber, deliveryId, startedAtMillis, pipeline, 0L);
        }

        private Entry(String runId, String intentKey, String jobPath, long buildNumber, String deliveryId,
                long startedAtMillis, boolean pipeline, long lastSeenMillis) {
            this.runId = runId;
            this.intentKey = intentKey != null ? intentKey : "";
            this.jobPath = jobPath != null ? jobPath : "";
            this.buildNumber = buildNumber;
            this.deliveryId = deliveryId != null ? deliveryId : "";
            this.startedAtMillis = startedAtMillis;
            this.pipeline = pipeline;
            this.lastSeenMillis = lastSeenMillis;
        }

        JSONObject toJson() {
            JSONObject o = new JSONObject();
            o.put("run_id", runId);
            o.put("intent_key", intentKey);
            o.put("job_path", jobPath);
            o.put("build_number", buildNumber);
            o.put("delivery_id", deliveryId);
            o.put("started_at_millis", startedAtMillis);
            o.put("pipeline", pipeline);
            return o;
        }

        static Entry fromJson(JSONObject o, long lastSeenMillis) {
            if (!o.has("run_id") || !o.has("job_path") || !o.has("build_number")) {
                throw new IllegalArgumentException("the entry misses an identity field");
            }
            return new Entry(
                    o.getString("run_id"),
                    o.optString("intent_key", ""),
                    o.getString("job_path"),
                    o.getLong("build_number"),
                    o.optString("delivery_id", ""),
                    o.optLong("started_at_millis", 0L),
                    o.optBoolean("pipeline", false),
                    lastSeenMillis);
        }
    }

    private static String fileName(Path p) {
        Path n = p.getFileName();
        return n == null ? "" : n.toString();
    }
}
