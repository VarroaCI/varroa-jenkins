package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.Run;

import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Base64;
import java.util.HashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicLong;
import java.util.logging.Level;
import java.util.logging.Logger;
import java.util.zip.GZIPOutputStream;

import jenkins.model.Jenkins;
import jenkins.util.Timer;

import net.sf.json.JSONObject;

import org.jenkinsci.plugins.workflow.actions.WorkspaceAction;
import org.jenkinsci.plugins.workflow.flow.FlowDurabilityHint;
import org.jenkinsci.plugins.workflow.graph.FlowNode;

/**
 * Replicates a running Pipeline build's own state files off its cell, so
 * another Jenkins process can continue the build when the cell dies.
 *
 * <p>One instance serves one run. {@link VarroaRunRecorder.Graph} calls
 * {@link #scheduleFor} for every flow node, and the run's completion
 * calls {@link #remove}.
 *
 * <p>A push is scheduled, never performed, on the caller's thread: the
 * caller runs on the flow's own thread, and a file read plus a network
 * post there would slow every step of every build. The first schedule
 * after an idle period arms one task two seconds out, and every further
 * schedule inside that window arms nothing. The task then posts the
 * whole current file set once, serially.
 *
 * <p>A push failure of any kind leaves the build running. Every call
 * path catches its own exception, logs it, and emits no build event.
 */
public final class RunStatePusher {

    private static final Logger LOGGER = Logger.getLogger(RunStatePusher.class.getName());

    /** The coalescing window. A constant, not a setting. */
    static final long WINDOW_MILLIS = 2000L;

    /**
     * The inflated bytes of one chunk. The loopback caps one body at
     * 1 MiB, and gzip of incompressible bytes grows the body by a
     * fraction of a percent, so this value stays under the cap for any
     * content.
     */
    static final int CHUNK_BYTES = 768 * 1024;

    /**
     * The longest wait between two retries of an undrained spool. The
     * retry doubles from the window up to this bound, so a mite that
     * stays down costs one post a minute for each run instead of one
     * every two seconds.
     */
    static final long RETRY_DELAY_MAX_MILLIS = 60_000L;

    /**
     * The wait between two refresh windows of a build that runs but
     * fires no flow node. A node-driven window is the only window a
     * successful push arms, so a long step would leave the run's stored
     * state as old as that step is long. The build record and the flow
     * program both carry the step in progress, so a refresh of them
     * bounds how old a resume's copy can be.
     */
    static final long REFRESH_INTERVAL_MILLIS = 30_000L;

    /**
     * The age past which a retained record is dropped. It is the
     * recorder's own arm window: a run the recorder can no longer
     * resume has no use for state older than this, so holding the
     * record only costs spool bytes.
     */
    static final long RETAINED_MAX_MILLIS = 15L * 60L * 1000L;

    /** The gate. An unset or invalid value pushes nothing. */
    static final String URL_ENV = "VARROA_MITE_RUN_STATE_URL";

    static final String ROUTE = "/v1/run-state";

    /** The two files that change on every step, pushed on every push. */
    static final String BUILD_XML = "build.xml";
    static final String PROGRAM_DAT = "program.dat";

    /** Refusal reasons the mite names on a 503. */
    static final String REASON_STATE_CAP = "state-cap";
    static final String REASON_FOREIGN_RUN = "foreign-run";
    static final String REASON_BUDGET_FULL = "gateway-budget-full";
    static final String REASON_INCOMPLETE = "incomplete";
    static final String REASON_MALFORMED = "malformed";

    /**
     * Names this Jenkins process to the gateway. The push generation
     * counts inside one process and starts again at 1 after a restart,
     * so the gateway needs a second value to tell a retry of a push
     * from the first push of a restarted process. The JVM start time
     * changes on every restart and stays fixed for the process's life.
     */
    static final long PUSHER_EPOCH = java.lang.management.ManagementFactory.getRuntimeMXBean().getStartTime();

    private static final ConcurrentHashMap<String, RunStatePusher> PUSHERS = new ConcurrentHashMap<>();

    private static volatile String urlOverrideForTesting;

    private final String runId;
    private final String jobPath;
    private final long buildNumber;
    private final Path rootDir;
    private final String baseUrl;
    private final RunStateSender sender;
    private final RunSpool spool;

    private final AtomicBoolean armed = new AtomicBoolean();

    /**
     * Whether a refresh window waits on the timer. It is a flag of its
     * own, never the window's, so a flow node that fires inside the
     * refresh interval still takes the two-second window.
     */
    private final AtomicBoolean refreshArmed = new AtomicBoolean();

    /**
     * Whether a window of this run is posting. The window clears the
     * armed flag when it starts, so a flow node or a refresh can arm the
     * next one while it still posts, and the shared timer runs several
     * tasks at once. Two windows of one run would then post the same
     * files under two generations at the same time, and the gateway
     * fences the pushing process, not the generation: the older
     * generation can land last and hold the run's stored state.
     */
    private final AtomicBoolean running = new AtomicBoolean();
    private final AtomicBoolean stopped = new AtomicBoolean();
    private final AtomicBoolean loggedRefusal = new AtomicBoolean();
    private final AtomicLong stateGen = new AtomicLong();
    private final AtomicLong spoolSeq = new AtomicLong();
    private final AtomicLong retryDelayMillis = new AtomicLong(WINDOW_MILLIS);

    /**
     * Orders a window's append against the stop that ends the run. A
     * window can pass its own stopped test and then append, so the stop
     * and the append take this monitor: the stop either lands first,
     * and the window appends nothing, or it waits and then deletes the
     * record the window wrote. Without it a record survives with no
     * replay and no delete path.
     *
     * <p>It is held across an append and across the stop's own delete,
     * never across a post, so a run's completion never waits on the
     * network.
     */
    private final Object spoolLock = new Object();

    /** Size and modification time of each workflow file at its last push. */
    private final Map<String, long[]> seenWorkflowFiles = new ConcurrentHashMap<>();

    /** Node names an executor step has named, and the names already pushed. */
    private final Set<String> namedNodes = ConcurrentHashMap.newKeySet();
    private final Set<String> pushedNodes = ConcurrentHashMap.newKeySet();

    /** The build's durability, read once at the first schedule. */
    private volatile Boolean durable;

    RunStatePusher(String runId, String jobPath, long buildNumber, Path rootDir,
            String baseUrl, RunStateSender sender, RunSpool spool) {
        this.runId = runId;
        this.jobPath = jobPath;
        this.buildNumber = buildNumber;
        this.rootDir = rootDir;
        this.baseUrl = baseUrl;
        this.sender = sender;
        this.spool = spool;
    }

    /**
     * Schedules one push for {@code run}. It returns at once: it reads no
     * file and posts nothing. A run whose gate is off, or whose flow
     * durability is below the most durable level, schedules nothing.
     */
    public static void scheduleFor(Run<?, ?> run, String runId, FlowNode node) {
        try {
            if (run == null || runId == null || runId.isEmpty()) {
                return;
            }
            String url = activationUrl();
            if (url == null) {
                return;
            }
            RunStatePusher pusher = PUSHERS.computeIfAbsent(runId, id -> new RunStatePusher(
                    id, run.getParent().getFullName(), run.getNumber(), run.getRootDir().toPath(),
                    url, new HttpRunStateSender(), new RunSpool()));
            pusher.schedule(node);
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "RunStatePusher: failed to schedule a push for run " + runId, e);
        }
    }

    /**
     * Removes the run's pusher and stops it. The run's completion ends
     * its pushes.
     *
     * <p>Dropping the map entry alone ends nothing: a task already on
     * the timer holds the pusher itself, so it would post and arm
     * another window after the run finished. Stopping it is what the
     * window and the re-arm both read.
     *
     * <p>It also clears the run's own run-state records. A stopped
     * pusher replays nothing, so a retained record would otherwise sit
     * on disk for the life of the installation.
     */
    public static void remove(String runId) {
        if (runId == null) {
            return;
        }
        RunStatePusher pusher = PUSHERS.remove(runId);
        if (pusher == null) {
            return;
        }
        // The run is over, so no record can reach the gateway and no
        // reader can use one. A record left here would survive every
        // restart, because the stopped pusher replays nothing.
        synchronized (pusher.spoolLock) {
            pusher.stopped.set(true);
            try {
                pusher.spool.deleteRunStateRecords(runId);
            } catch (RuntimeException e) {
                LOGGER.log(Level.WARNING, "RunStatePusher: failed to clear the spool of run " + runId, e);
            }
        }
    }

    /** Registers a pusher directly, bypassing a real build. Package-private for testing. */
    static void putForTesting(String runId, RunStatePusher pusher) {
        PUSHERS.put(runId, pusher);
    }

    /** Number of live pushers. Package-private for testing. */
    static int sizeForTesting() {
        return PUSHERS.size();
    }

    /** Clears every pusher. Package-private for testing. */
    static void resetForTesting() {
        PUSHERS.clear();
    }

    /** Forces the gate value for a test. */
    static void setActivationUrlForTesting(String url) {
        urlOverrideForTesting = url;
    }

    /**
     * Reads and validates the gate, with the same absolute
     * {@code http://127.0.0.1} rule the log-storage gate applies.
     */
    static String activationUrl() {
        String override = urlOverrideForTesting;
        String raw = override != null ? override : System.getenv(URL_ENV);
        return VarroaLogStorageFactory.parseActivationUrl(raw);
    }

    /**
     * Records the node an executor step names and arms the window. The
     * first schedule after an idle period arms one task; every further
     * schedule inside that window arms nothing.
     */
    void schedule(FlowNode node) {
        if (stopped.get()) {
            return;
        }
        if (!durableEnough(node)) {
            return;
        }
        recordNodeName(node);
        armWindow();
    }

    /**
     * Arms one window, one window ahead. Every caller takes this path,
     * so several schedules inside one window make one window.
     */
    private void armWindow() {
        if (stopped.get()) {
            return;
        }
        if (armed.compareAndSet(false, true)) {
            Timer.get().schedule(this::runWindow, WINDOW_MILLIS, TimeUnit.MILLISECONDS);
        }
    }

    /**
     * Reads the run's flow durability once. A build below the most
     * durable level pushes nothing and logs the refusal once: Jenkins
     * writes its flow program at a checkpoint under a lower level, so a
     * copied set can be stale by whole stages, and a resume from such a
     * set repeats finished work under the same build number.
     */
    private boolean durableEnough(FlowNode node) {
        Boolean known = durable;
        if (known != null) {
            return known;
        }
        boolean ok = true;
        FlowDurabilityHint hint = null;
        try {
            if (node != null && node.getExecution() != null) {
                hint = node.getExecution().getDurabilityHint();
            }
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "RunStatePusher: could not read the durability hint for run " + runId, e);
        }
        if (hint != null && hint != FlowDurabilityHint.MAX_SURVIVABILITY) {
            ok = false;
            if (loggedRefusal.compareAndSet(false, true)) {
                LOGGER.log(Level.WARNING,
                        "RunStatePusher: run {0} has the flow durability {1}; the pipeline mesh needs "
                                + "MAX_SURVIVABILITY, so this build replicates no state.",
                        new Object[] {runId, hint});
            }
        }
        durable = ok;
        return ok;
    }

    /** Reads the node name an executor step's workspace action reports. */
    private void recordNodeName(FlowNode node) {
        if (node == null) {
            return;
        }
        WorkspaceAction workspace = node.getAction(WorkspaceAction.class);
        if (workspace == null) {
            return;
        }
        String name = workspace.getNode();
        if (name == null || name.isEmpty() || "master".equals(name) || "built-in".equals(name)) {
            return;
        }
        namedNodes.add(name);
    }

    /** Runs one window: one serial push of the whole current file set. */
    void runWindow() {
        if (stopped.get()) {
            armed.set(false);
            return;
        }
        // One run posts one window at a time. The generation counts once
        // for each push that reaches the gateway, and no two windows
        // write one path together.
        if (!running.compareAndSet(false, true)) {
            // This task stays the run's one armed window: the armed flag
            // is left set, so no other schedule adds a second task
            // beside it, and this task tries again one window later.
            Timer.get().schedule(this::runWindow, WINDOW_MILLIS, TimeUnit.MILLISECONDS);
            return;
        }
        // The window owns the run from here, so the next flow node arms
        // the window that follows this one.
        armed.set(false);
        try {
            // The replay runs before the generation increments, so a
            // record this pusher kept from the last window is not below
            // the current generation and is posted again. A window that
            // cannot drain the spool posts nothing new: the kept record
            // is the newest copy of its file, and a later generation
            // must never reach the gateway before it.
            if (!replaySpooled()) {
                rearm();
                return;
            }
            long gen = stateGen.incrementAndGet();
            for (String relPath : currentFileSet()) {
                if (stopped.get()) {
                    return;
                }
                if (!pushFile(relPath, gen)) {
                    rearm();
                    return;
                }
            }
            // The window drained the spool and posted its whole set, so
            // the next retry starts from the ordinary window again.
            retryDelayMillis.set(WINDOW_MILLIS);
            armRefresh();
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "RunStatePusher: push window failed for run " + runId, e);
        } finally {
            running.set(false);
        }
    }

    /**
     * Arms one more window for a run whose spool did not drain. A push
     * is otherwise scheduled only by a later flow node, so a transport
     * failure on the last node of a long step would leave the spooled
     * record undelivered until the build moved again.
     */
    private void rearm() {
        if (stopped.get()) {
            return;
        }
        long delay = retryDelayMillis.get();
        if (armed.compareAndSet(false, true)) {
            Timer.get().schedule(this::runWindow, delay, TimeUnit.MILLISECONDS);
        }
        retryDelayMillis.set(Math.min(delay * 2, RETRY_DELAY_MAX_MILLIS));

    }

    /**
     * Arms the next refresh window of a build that is still running. A
     * failed window arms its own retry, so only a successful one needs
     * this.
     */
    private void armRefresh() {
        if (stopped.get()) {
            return;
        }
        if (refreshArmed.compareAndSet(false, true)) {
            Timer.get().schedule(this::runRefresh, REFRESH_INTERVAL_MILLIS, TimeUnit.MILLISECONDS);
        }
    }

    /**
     * Arms one ordinary window from the refresh timer. It schedules
     * rather than pushes, so a refresh and a flow node that fire
     * together still make one push. Package-private for testing.
     */
    void runRefresh() {
        refreshArmed.set(false);
        schedule(null);
    }

    /**
     * Replays every spooled record of this run and reports whether the
     * spool drained. It drops a record only when the spool itself holds
     * a newer generation of the same path: that newer record supersedes
     * it, and a superseded record is never replayed.
     *
     * <p>A generation counter that moved is not supersession. Only a
     * newer push of the same path supersedes an older one, so a record
     * the last window kept after a transport failure is posted again
     * here. Its file may be unreadable by now, which makes the record
     * the last durable copy of that file.
     */
    private boolean replaySpooled() {
        List<RunSpool.Entry> entries = spool.readRunStateOrdered(runId);
        Map<String, Long> newestByPath = new HashMap<>();
        for (RunSpool.Entry entry : entries) {
            JSONObject body = parseSpooled(entry);
            if (body == null) {
                continue;
            }
            String path = body.optString("relpath", "");
            long recordGen = body.optLong("state_gen", 0L);
            Long seen = newestByPath.get(path);
            if (seen == null || recordGen > seen) {
                newestByPath.put(path, recordGen);
            }
        }

        long now = System.currentTimeMillis();
        for (RunSpool.Entry entry : entries) {
            JSONObject body = parseSpooled(entry);
            if (body == null) {
                spool.acknowledgeRunState(runId, entry.seq);
                continue;
            }
            Long newest = newestByPath.get(body.optString("relpath", ""));
            if (newest != null && body.optLong("state_gen", 0L) < newest) {
                spool.acknowledgeRunState(runId, entry.seq);
                continue;
            }
            if (now - body.optLong("created_at_millis", 0L) > RETAINED_MAX_MILLIS) {
                spool.acknowledgeRunState(runId, entry.seq);
                LOGGER.log(Level.WARNING,
                        "RunStatePusher: dropping a run-state record of run {0} older than the resume window.",
                        runId);
                continue;
            }
            if (!postSpooled(entry.seq, body)) {
                return false;
            }
        }
        return true;
    }

    /** Parses one spooled record, or null when its body does not decode. */
    private JSONObject parseSpooled(RunSpool.Entry entry) {
        try {
            return entry.toJsonObject();
        } catch (RuntimeException e) {
            LOGGER.log(Level.FINE, "RunStatePusher: skipping an unparsable spooled record", e);
            return null;
        }
    }

    /**
     * Builds the push's file set. The build record and the flow program
     * change on every step, so a change test would save nothing. A flow
     * node file is immutable once its node ends, so a size and time test
     * skips nearly all of them after the first push.
     */
    List<String> currentFileSet() {
        List<String> files = new ArrayList<>();
        String buildPrefix = buildRelPathPrefix();

        Path workflow = rootDir.resolve("workflow");
        if (Files.isDirectory(workflow)) {
            List<String> changed = new ArrayList<>();
            try (java.util.stream.Stream<Path> entries = Files.list(workflow)) {
                for (Path p : entries.toList()) {
                    // A path's file name can be null, so read it once and
                    // skip a path that has none.
                    Path fileName = p.getFileName();
                    if (fileName == null) {
                        continue;
                    }
                    String name = fileName.toString();
                    if (!name.endsWith(".xml")) {
                        continue;
                    }
                    if (workflowFileChanged(p, name)) {
                        changed.add(buildPrefix + "workflow/" + name);
                    }
                }
            } catch (IOException e) {
                LOGGER.log(Level.FINE, "RunStatePusher: could not list the workflow directory of run " + runId, e);
            }
            changed.sort(null);
            files.addAll(changed);
        }

        if (Files.isRegularFile(rootDir.resolve(PROGRAM_DAT))) {
            files.add(buildPrefix + PROGRAM_DAT);
        }

        Set<String> nodes = new LinkedHashSet<>(namedNodes);
        for (String name : nodes) {
            if (pushedNodes.contains(name)) {
                continue;
            }
            if (Files.isRegularFile(nodesDir().resolve(name).resolve("config.xml"))) {
                files.add("nodes/" + name + "/config.xml");
            }
        }

        // The build record goes last. Jenkins reads it to rebuild the
        // run, so a reader never sees a run whose flow program is older.
        if (Files.isRegularFile(rootDir.resolve(BUILD_XML))) {
            files.add(buildPrefix + BUILD_XML);
        }
        return files;
    }

    /**
     * Reports whether a workflow file's size or modification time changed
     * since its last stored push. It records nothing: a file counts as
     * replicated only once the gateway has stored it, so a failed push
     * leaves the file selected for the next window.
     */
    private boolean workflowFileChanged(Path file, String name) {
        long[] current = sizeAndTime(file);
        if (current == null) {
            return false;
        }
        long[] seen = seenWorkflowFiles.get(name);
        return seen == null || seen[0] != current[0] || seen[1] != current[1];
    }

    /** The file's size and modification time, or null when it cannot be read. */
    private static long[] sizeAndTime(Path file) {
        try {
            return new long[] {Files.size(file), Files.getLastModifiedTime(file).toMillis()};
        } catch (IOException e) {
            return null;
        }
    }

    /** The path prefix of this build's own directory under the Jenkins home. */
    String buildRelPathPrefix() {
        StringBuilder sb = new StringBuilder();
        for (String seg : jobPath.split("/")) {
            if (!seg.isEmpty()) {
                sb.append("jobs/").append(seg).append('/');
            }
        }
        sb.append("builds/").append(buildNumber).append('/');
        return sb.toString();
    }

    private Path nodesDir() {
        Jenkins jenkins = Jenkins.getInstanceOrNull();
        if (jenkins != null) {
            return jenkins.getRootDir().toPath().resolve("nodes");
        }
        return rootDir.resolve("..").normalize().resolve("nodes");
    }

    /**
     * Posts one file, in chunks, in ascending sequence order. It reports
     * whether the window may continue: a chunk the spool keeps stops the
     * window, so no later generation of any path overtakes it.
     */
    private boolean pushFile(String relPath, long gen) {
        Path absolute = resolveLocalPath(relPath);
        if (absolute == null) {
            return true;
        }
        byte[] content;
        try {
            content = Files.readAllBytes(absolute);
        } catch (IOException e) {
            LOGGER.log(Level.FINE, "RunStatePusher: could not read " + absolute, e);
            return true;
        }

        long total = Math.max(1L, (content.length + CHUNK_BYTES - 1L) / CHUNK_BYTES);
        for (long seq = 0; seq < total; seq++) {
            int start = (int) (seq * CHUNK_BYTES);
            int end = Math.min(content.length, start + CHUNK_BYTES);
            byte[] part = java.util.Arrays.copyOfRange(content, start, end);
            byte[] gzip;
            try {
                gzip = gzip(part);
            } catch (IOException e) {
                LOGGER.log(Level.FINE, "RunStatePusher: could not compress a chunk of " + relPath, e);
                return true;
            }
            JSONObject record = record(relPath, gen, seq, total, part.length, content.length, gzip);
            long spoolId;
            synchronized (spoolLock) {
                // The stop takes this monitor too, so a run that ended
                // while this window ran leaves no record behind.
                if (stopped.get()) {
                    return false;
                }
                spoolId = spoolSeq.incrementAndGet();
                spool.appendRunState(runId, spoolId, record);
            }
            if (!postSpooled(spoolId, record)) {
                return false;
            }
        }
        // The file is replicated only now. A marker written before the
        // push would make a failed push look like a stored file, and the
        // next window would skip it.
        if (relPath.startsWith("nodes/")) {
            pushedNodes.add(nodeNameOf(relPath));
        } else if (relPath.contains("/workflow/")) {
            long[] pushedAt = sizeAndTime(absolute);
            if (pushedAt != null) {
                seenWorkflowFiles.put(workflowNameOf(relPath), pushedAt);
            }
        }
        return true;
    }

    /** The bare file name of a workflow path, which keys the marker map. */
    private static String workflowNameOf(String relPath) {
        int slash = relPath.lastIndexOf('/');
        return slash >= 0 ? relPath.substring(slash + 1) : relPath;
    }

    /**
     * Posts one spooled record and acts on the answer. It reports whether
     * the caller may continue with the run's remaining chunks.
     *
     * <p>Only an answer a retry cannot change drops the record. A
     * permanent refusal drops it and stops every further push for the
     * build. A refusal of the pushed bytes themselves drops it, because
     * the next window sends a whole set instead. Every other answer, and
     * every transport failure, keeps the record for a later window: a
     * loopback outage, a timeout, and a closed stream are all states
     * that pass.
     */
    private boolean postSpooled(long spoolId, JSONObject record) {
        if (stopped.get()) {
            return false;
        }
        RunStateSender.Answer answer = sender.post(baseUrl + ROUTE, record);
        if (answer.accepted) {
            spool.acknowledgeRunState(runId, spoolId);
            PluginCounters.increment(PluginCounters.RUN_STATE_PUSHED);
            return true;
        }
        if (REASON_BUDGET_FULL.equals(answer.reason)) {
            sleepQuietly(answer.pauseMillis > 0 ? answer.pauseMillis : 1000L);
            return false;
        }
        if (REASON_STATE_CAP.equals(answer.reason) || REASON_FOREIGN_RUN.equals(answer.reason)) {
            spool.acknowledgeRunState(runId, spoolId);
            PluginCounters.increment(PluginCounters.RUN_STATE_REFUSED);
            stopped.set(true);
            LOGGER.log(Level.WARNING, "RunStatePusher: run {0} stops replicating state ({1}).",
                    new Object[] {runId, answer.reason});
            return false;
        }
        if (REASON_INCOMPLETE.equals(answer.reason) || REASON_MALFORMED.equals(answer.reason)) {
            spool.acknowledgeRunState(runId, spoolId);
            PluginCounters.increment(PluginCounters.RUN_STATE_REFUSED);
            return true;
        }
        // A transport failure, a timeout, and a 503 that names no reason
        // all reach here. The record stays on disk, and the next window
        // replays it before it posts the current set.
        return false;
    }

    private static void sleepQuietly(long millis) {
        try {
            Thread.sleep(millis);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

    /** Builds the record the spool holds and the sender posts. */
    JSONObject record(String relPath, long gen, long seq, long total, long rawSize, long objectSize, byte[] gzip) {
        JSONObject body = new JSONObject();
        body.put("run_id", runId);
        body.put("job_path", jobPath);
        body.put("build_number", buildNumber);
        body.put("relpath", relPath);
        body.put("state_gen", gen);
        body.put("seq", seq);
        body.put("total", total);
        body.put("raw_size", rawSize);
        body.put("object_size", objectSize);
        body.put("pusher_epoch", PUSHER_EPOCH);
        body.put("created_at_millis", System.currentTimeMillis());
        body.put("gzip_base64", Base64.getEncoder().encodeToString(gzip));
        return body;
    }

    /** Maps a path under the Jenkins home back onto a local file. */
    private Path resolveLocalPath(String relPath) {
        if (relPath.startsWith("nodes/")) {
            String name = nodeNameOf(relPath);
            return name.isEmpty() ? null : nodesDir().resolve(name).resolve("config.xml");
        }
        String prefix = buildRelPathPrefix();
        if (!relPath.startsWith(prefix)) {
            return null;
        }
        String rest = relPath.substring(prefix.length());
        Path local = rootDir;
        for (String seg : rest.split("/")) {
            if (!seg.isEmpty()) {
                local = local.resolve(seg);
            }
        }
        return local;
    }

    private static String nodeNameOf(String relPath) {
        String[] parts = relPath.split("/");
        return parts.length >= 2 ? parts[1] : "";
    }

    static byte[] gzip(byte[] raw) throws IOException {
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        try (GZIPOutputStream gz = new GZIPOutputStream(out)) {
            gz.write(raw);
        }
        return out.toByteArray();
    }

    /** Posts one record to the mite. {@link HttpRunStateSender} implements it. */
    interface RunStateSender {

        Answer post(String url, JSONObject record);

        /** One post's outcome: accepted, or refused with a reason. */
        final class Answer {
            final boolean accepted;
            final String reason;
            final long pauseMillis;

            Answer(boolean accepted, String reason, long pauseMillis) {
                this.accepted = accepted;
                this.reason = reason;
                this.pauseMillis = pauseMillis;
            }

            static Answer accepted() {
                return new Answer(true, null, 0);
            }

            static Answer refused(String reason, long pauseMillis) {
                return new Answer(false, reason, pauseMillis);
            }
        }
    }

    /** Posts a record's headers and its raw gzip body over the loopback. */
    static final class HttpRunStateSender implements RunStateSender {

        private final MiteLoopbackClient client = new MiteLoopbackClient();

        @Override
        public Answer post(String url, JSONObject record) {
            byte[] gzip = Base64.getDecoder().decode(record.getString("gzip_base64"));
            MiteLoopbackClient.RunStateAnswer answer = client.postRunState(url, record, gzip);
            if (answer.statusCode == 202) {
                return Answer.accepted();
            }
            String reason = answer.reason != null ? answer.reason : "transport";
            return Answer.refused(reason, answer.pauseMillis);
        }
    }

    /** Renders a long in the plain decimal form every header uses. */
    static String decimal(long value) {
        return String.format(Locale.ROOT, "%d", value);
    }

    /** The run's root directory, for a test that builds files under it. */
    File rootDirForTesting() {
        return rootDir.toFile();
    }

    /** Reports whether the pusher stopped pushing. Package-private for testing. */
    boolean stoppedForTesting() {
        return stopped.get();
    }

    /** The delay the next re-arm applies. Package-private for testing. */
    long retryDelayForTesting() {
        return retryDelayMillis.get();
    }

    /** Whether a refresh window waits on the timer. Package-private for testing. */
    boolean refreshArmedForTesting() {
        return refreshArmed.get();
    }

    /** Whether a window of this run is posting. Package-private for testing. */
    boolean windowRunningForTesting() {
        return running.get();
    }

    /** The monitor the stop and the append share. Package-private for testing. */
    Object spoolLockForTesting() {
        return spoolLock;
    }

    /** The record body every spooled entry holds, as UTF-8. Package-private for testing. */
    static byte[] utf8(String s) {
        return s.getBytes(StandardCharsets.UTF_8);
    }
}
