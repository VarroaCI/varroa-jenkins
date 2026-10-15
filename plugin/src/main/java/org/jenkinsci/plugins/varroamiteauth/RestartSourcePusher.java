package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.Run;

import java.io.File;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.LinkOption;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Base64;
import java.util.Collections;
import java.util.List;
import java.util.concurrent.Executor;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.RejectedExecutionException;
import java.util.concurrent.ThreadPoolExecutor;
import java.util.concurrent.TimeUnit;
import java.util.function.LongSupplier;
import java.util.logging.Level;
import java.util.logging.Logger;
import java.util.stream.Stream;

import net.sf.json.JSONObject;

import org.jenkinsci.plugins.workflow.job.WorkflowRun;

/**
 * Keeps one restart copy of a finalized declarative cell run.
 *
 * <p>The copy holds {@code build.xml}, every file under
 * {@code workflow-completed/}, and every file under {@code libs/}. The
 * mesh pusher never sends these files, and it stops at completion, before
 * Jenkins writes the final {@code build.xml}. The copy therefore starts at
 * finalization and posts once, on the run-state route, under its own key.
 *
 * <p>The copy has no disk spool. A Jenkins stop during the copy, or ten
 * minutes of failed posts, loses it. A restart then answers {@code
 * home-moved}, as it does without a copy.
 */
final class RestartSourcePusher {

    private static final Logger LOGGER = Logger.getLogger(RestartSourcePusher.class.getName());

    /** The file count one copy may hold. The gateway enforces the same cap. */
    static final int MAX_FILES = 512;

    /** The byte count one copy may hold. The gateway enforces the same cap. */
    static final long MAX_BYTES = 32L << 20;

    /** How long one copy may keep posting, measured from its start. */
    static final long COPY_DEADLINE_MILLIS = 10 * 60_000L;

    /** The first wait after a retryable refusal. Each further wait doubles it. */
    static final long RETRY_MIN = 2000L;

    /** The longest wait between two posts of one chunk. */
    static final long RETRY_MAX = 60_000L;

    /** The number of copies that may wait behind the one that runs. */
    static final int QUEUE_CAPACITY = 64;

    private static final String WORKFLOW_COMPLETED = "workflow-completed";
    private static final String LIBS = "libs";
    private static final String BUILD_XML = "build.xml";

    /** The gateway refusals that end a copy. Every other refusal is retried. */
    private static final String REASON_STATE_CAP = "state-cap";
    private static final String REASON_FOREIGN_RUN = "foreign-run";
    private static final String REASON_MALFORMED = "malformed";
    private static final String REASON_INCOMPLETE = "incomplete";

    private static volatile Executor executor = newExecutor();
    private static volatile RunStatePusher.RunStateSender sender = new RunStatePusher.HttpRunStateSender();
    private static volatile Boolean cellOverride;
    private static volatile LongSupplier clock = System::currentTimeMillis;
    private static volatile LongSleeper sleeper = RestartSourcePusher::sleepQuietly;
    private static volatile boolean loggedQueueFull;

    private RestartSourcePusher() {
    }

    /** Waits for a number of milliseconds. A test replaces it to skip the wait. */
    @FunctionalInterface
    interface LongSleeper {
        void sleep(long millis);
    }

    /**
     * Schedules the restart copy of {@code run}. It returns at once: it
     * reads no file and posts nothing. A run that does not qualify
     * schedules nothing.
     *
     * <p>A run qualifies when the gate names a loopback URL, the controller
     * is a cell, the run is a {@link WorkflowRun} that is not
     * template-defined, and the run has restartable stages.
     */
    public static void scheduleFor(Run<?, ?> run, String runId) {
        try {
            if (runId == null || runId.isEmpty()) {
                return;
            }
            String url = RunStatePusher.activationUrl();
            if (url == null || !onCell()) {
                return;
            }
            if (!(run instanceof WorkflowRun)) {
                return;
            }
            if (VarroaTemplateDefinitions.isTemplateDefined(run.getParent())) {
                return;
            }
            if (VarroaDeclarativeRestart.inspect(run).getRefusal() != null) {
                return;
            }
            String jobPath = run.getParent().getFullName();
            long number = run.getNumber();
            Path rootDir = run.getRootDir().toPath();
            try {
                executor.execute(() -> copy(rootDir, jobPath, number, runId, url));
            } catch (RejectedExecutionException e) {
                PluginCounters.increment(PluginCounters.RESTART_SOURCE_DROPPED);
                if (!loggedQueueFull) {
                    loggedQueueFull = true;
                    LOGGER.warning("RestartSourcePusher: the copy queue is full, so run " + runId + " keeps no copy");
                }
            }
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "RestartSourcePusher: failed to schedule a restart copy for run " + runId, e);
        }
    }

    /** Whether this controller is a cell. A test overrides the environment. */
    private static boolean onCell() {
        Boolean override = cellOverride;
        if (override != null) {
            return override;
        }
        return "true".equals(System.getenv(VarroaCellJobDslBlock.CELL_ENV));
    }

    /**
     * The files of a run's copy, as paths relative to {@code rootDir} with
     * {@code /} separators, sorted. It names no file when
     * {@code workflow-completed/} is not a directory, because a copy without
     * it cannot restart. Symbolic links are skipped.
     */
    static List<String> fileSet(Path rootDir) {
        List<String> files = new ArrayList<>();
        if (!Files.isDirectory(rootDir.resolve(WORKFLOW_COMPLETED), LinkOption.NOFOLLOW_LINKS)) {
            return files;
        }
        if (Files.isRegularFile(rootDir.resolve(BUILD_XML), LinkOption.NOFOLLOW_LINKS)) {
            files.add(BUILD_XML);
        }
        collect(rootDir, WORKFLOW_COMPLETED, files);
        collect(rootDir, LIBS, files);
        Collections.sort(files);
        return files;
    }

    private static void collect(Path rootDir, String dirName, List<String> out) {
        Path dir = rootDir.resolve(dirName);
        if (!Files.isDirectory(dir, LinkOption.NOFOLLOW_LINKS)) {
            return;
        }
        try (Stream<Path> walk = Files.walk(dir)) {
            walk.filter(p -> !p.equals(dir))
                    .filter(p -> Files.isRegularFile(p, LinkOption.NOFOLLOW_LINKS))
                    .forEach(p -> out.add(rootDir.relativize(p).toString().replace(File.separatorChar, '/')));
        } catch (IOException e) {
            throw new IllegalStateException("cannot list " + dirName, e);
        }
    }

    /**
     * Copies one run's files to the gateway. Every chunk of every file goes
     * out through the sender. A refusal that is not permanent is retried
     * until the copy's deadline. The copy counts as kept only when every
     * file is stored.
     */
    static void copy(Path rootDir, String jobPath, long number, String runId, String url) {
        try {
            long deadline = clock.getAsLong() + COPY_DEADLINE_MILLIS;
            List<String> rels = fileSet(rootDir);
            if (rels.isEmpty()) {
                dropped(runId, "no workflow-completed directory");
                return;
            }
            List<String> kept = new ArrayList<>();
            long bytes = 0L;
            for (String rel : rels) {
                long size = Files.size(rootDir.resolve(rel));
                if (size == 0L) {
                    // The gateway refuses a zero-byte object, so a copy
                    // that declared it could never complete.
                    continue;
                }
                bytes += size;
                kept.add(rel);
            }
            rels = kept;
            if (rels.isEmpty()) {
                dropped(runId, "no non-empty file to copy");
                return;
            }
            if (rels.size() > MAX_FILES || bytes > MAX_BYTES) {
                dropped(runId, "the copy is over the cap: " + rels.size() + " files, " + bytes + " bytes");
                return;
            }
            String prefix = RunStatePusher.buildRelPathPrefix(jobPath, number);
            // A refusal arrives on a later post, so an incomplete answer may
            // belong to an earlier file. The whole copy posts again. The
            // gateway answers STORED for every file it already holds.
            boolean again = true;
            while (again) {
                again = false;
                for (String rel : rels) {
                    byte[] content = Files.readAllBytes(rootDir.resolve(rel));
                    Push result = pushFile(url, runId, jobPath, number, prefix + rel, content,
                            rels.size(), bytes, deadline);
                    if (result == Push.STOP) {
                        return;
                    }
                    if (result == Push.RESTART) {
                        again = true;
                        break;
                    }
                }
                if (again) {
                    if (clock.getAsLong() + RETRY_MIN > deadline) {
                        dropped(runId, "the copy missed its deadline before every file was stored");
                        return;
                    }
                    sleeper.sleep(RETRY_MIN);
                }
            }
            PluginCounters.increment(PluginCounters.RESTART_SOURCE_KEPT);
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "RestartSourcePusher: the restart copy of run " + runId + " stopped", e);
            PluginCounters.increment(PluginCounters.RESTART_SOURCE_DROPPED);
        }
    }

    /** The outcome of one file: stored, the copy must stop, or the copy must post again. */
    private enum Push {
        ACCEPTED, STOP, RESTART
    }

    /**
     * Posts every chunk of one file. It answers ACCEPTED when the file is
     * stored, STOP when the copy must end, and RESTART when the gateway
     * reports an incomplete chunk.
     */
    private static Push pushFile(String url, String runId, String jobPath, long number,
            String relpath, byte[] content, long copyFiles, long copyBytes, long deadline) throws IOException {
        int chunks = (int) Math.max(1L, (content.length + RunStatePusher.CHUNK_BYTES - 1L) / RunStatePusher.CHUNK_BYTES);
        long backoff = RETRY_MIN;
        int seq = 0;
        while (seq < chunks) {
            int start = seq * RunStatePusher.CHUNK_BYTES;
            int end = Math.min(content.length, start + RunStatePusher.CHUNK_BYTES);
            byte[] part = Arrays.copyOfRange(content, start, end);
            JSONObject record = record(runId, jobPath, number, relpath, seq, chunks, part, content.length,
                    copyFiles, copyBytes);
            RunStatePusher.RunStateSender.Answer answer = sender.post(url + RunStatePusher.ROUTE, record);
            if (answer.accepted) {
                seq++;
                backoff = RETRY_MIN;
                continue;
            }
            String reason = answer.reason;
            if (REASON_INCOMPLETE.equals(reason)) {
                return Push.RESTART;
            }
            if (REASON_STATE_CAP.equals(reason) || REASON_FOREIGN_RUN.equals(reason)
                    || REASON_MALFORMED.equals(reason)) {
                dropped(runId, "the gateway refused a chunk: " + reason);
                return Push.STOP;
            }
            long wait = answer.pauseMillis > 0 ? answer.pauseMillis : backoff;
            if (clock.getAsLong() + wait > deadline) {
                dropped(runId, "the copy missed its deadline; last answer " + describe(answer));
                return Push.STOP;
            }
            sleeper.sleep(wait);
            backoff = Math.min(backoff * 2, RETRY_MAX);
        }
        return Push.ACCEPTED;
    }

    /** Names the last answer of a refused post: its status, and its reason when the gateway gave one. */
    static String describe(RunStatePusher.RunStateSender.Answer answer) {
        if (answer.status == 0) {
            return "no answer" + (answer.reason != null ? " (" + answer.reason + ")" : "");
        }
        return answer.status + (answer.reason != null ? " (" + answer.reason + ")" : "");
    }

    /** Builds the record one chunk posts. It has the mesh record's shape plus the copy's flag and counts. */
    static JSONObject record(String runId, String jobPath, long number, String relpath, int seq, int total,
            byte[] raw, long objectSize, long copyFiles, long copyBytes) throws IOException {
        byte[] gzip = RunStatePusher.gzip(raw);
        JSONObject body = new JSONObject();
        body.put("run_id", runId);
        body.put("job_path", jobPath);
        body.put("build_number", number);
        body.put("relpath", relpath);
        body.put("state_gen", 1L);
        body.put("seq", (long) seq);
        body.put("total", (long) total);
        body.put("raw_size", (long) raw.length);
        body.put("object_size", objectSize);
        body.put("pusher_epoch", RunStatePusher.PUSHER_EPOCH);
        body.put("created_at_millis", System.currentTimeMillis());
        body.put("gzip_base64", Base64.getEncoder().encodeToString(gzip));
        body.put("restart_source", true);
        body.put("copy_files", copyFiles);
        body.put("copy_bytes", copyBytes);
        return body;
    }

    private static void dropped(String runId, String why) {
        LOGGER.log(Level.WARNING, "RestartSourcePusher: no restart copy of run {0}: {1}", new Object[] {runId, why});
        PluginCounters.increment(PluginCounters.RESTART_SOURCE_DROPPED);
    }

    private static ThreadPoolExecutor newExecutor() {
        return new ThreadPoolExecutor(1, 1, 60L, TimeUnit.SECONDS,
                new LinkedBlockingQueue<>(QUEUE_CAPACITY), runnable -> {
                    Thread t = new Thread(runnable, "varroa-restart-source");
                    t.setDaemon(true);
                    return t;
                });
    }

    private static void sleepQuietly(long millis) {
        try {
            Thread.sleep(millis);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

    static void setExecutorForTesting(Executor value) {
        executor = value;
    }

    static void setSenderForTesting(RunStatePusher.RunStateSender value) {
        sender = value;
    }

    static void setCellOverrideForTesting(Boolean value) {
        cellOverride = value;
    }

    static void setClockForTesting(LongSupplier value) {
        clock = value;
    }

    static void setSleeperForTesting(LongSleeper value) {
        sleeper = value;
    }

    /** Restores every default. Package-private for testing. */
    static void resetForTesting() {
        executor = newExecutor();
        sender = new RunStatePusher.HttpRunStateSender();
        cellOverride = null;
        clock = System::currentTimeMillis;
        sleeper = RestartSourcePusher::sleepQuietly;
        loggedQueueFull = false;
    }
}
