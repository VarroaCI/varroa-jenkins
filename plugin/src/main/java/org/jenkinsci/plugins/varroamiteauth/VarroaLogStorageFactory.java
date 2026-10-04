package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.ExtensionList;
import hudson.init.Terminator;
import hudson.model.Queue;
import hudson.model.Run;

import java.io.IOException;
import java.net.URI;
import java.util.concurrent.ConcurrentHashMap;
import java.util.logging.Level;
import java.util.logging.Logger;

import org.jenkinsci.plugins.workflow.flow.FlowExecutionList;
import org.jenkinsci.plugins.workflow.flow.FlowExecutionOwner;
import org.jenkinsci.plugins.workflow.log.LogStorage;
import org.jenkinsci.plugins.workflow.log.LogStorageFactory;

/**
 * {@code @Extension} that hands a Pipeline build a {@link VarroaLogStorage}
 * (D1). Returns {@code null}, so Jenkins keeps its own {@code
 * FileLogStorage}, unless both activation conditions hold: {@code
 * VARROA_MITE_LOG_URL} is set to an absolute {@code http://127.0.0.1} URL,
 * and the build carries a Varroa run id.
 */
@Extension
public class VarroaLogStorageFactory implements LogStorageFactory {

    private static final Logger LOGGER = Logger.getLogger(VarroaLogStorageFactory.class.getName());

    static final String LOG_URL_ENV = "VARROA_MITE_LOG_URL";

    private static final long JVM_START_MILLIS = java.lang.management.ManagementFactory.getRuntimeMXBean().getStartTime();

    /**
     * Overrides {@link #activationUrl()} in a test, where the process
     * environment cannot be changed. {@code null} in production, which
     * falls back to {@code VARROA_MITE_LOG_URL}. Mirrors {@link
     * VarroaBuildEventLifecycle#start(String)}'s testing seam for the same
     * problem.
     */
    private static volatile String activationUrlOverrideForTesting;

    private final ConcurrentHashMap<String, String> ownerUrlToRunId = new ConcurrentHashMap<>();
    private final ConcurrentHashMap<String, VarroaLogStorage> runIdToStorage = new ConcurrentHashMap<>();

    private volatile LogChunkSender sender;
    private volatile LogReadCache readCache;

    @Override
    public LogStorage forBuild(FlowExecutionOwner owner) {
        try {
            String url = activationUrl();
            if (url == null) {
                return null;
            }
            String ownerUrl;
            try {
                ownerUrl = owner.getUrl();
            } catch (IOException e) {
                return null;
            }
            String cachedRunId = ownerUrlToRunId.get(ownerUrl);
            if (cachedRunId != null) {
                VarroaLogStorage cached = runIdToStorage.get(cachedRunId);
                if (cached != null) {
                    return cached;
                }
            }

            Queue.Executable executable;
            try {
                executable = owner.getExecutable();
            } catch (IOException e) {
                return null;
            }
            if (!(executable instanceof Run<?, ?>)) {
                return null;
            }
            Run<?, ?> run = (Run<?, ?>) executable;
            VarroaIntentAction action = run.getAction(VarroaIntentAction.class);
            if (action == null || action.getRunId().isEmpty()) {
                return null;
            }
            String runId = action.getRunId();
            String jobPath = run.getParent().getFullName();
            long buildNumber = run.getNumber();

            VarroaLogStorage storage = runIdToStorage.computeIfAbsent(runId,
                    id -> newStorage(run, id, jobPath, buildNumber, url));
            ownerUrlToRunId.putIfAbsent(ownerUrl, runId);
            return storage;
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "VarroaLogStorageFactory: forBuild failed, falling back to FileLogStorage", e);
            return null;
        }
    }

    /**
     * Builds the run's storage at the point where this run's cached log
     * ends. A build that resumes in a new JVM continues its {@code seq}
     * from the read cache on disk, because the gateway ignores a chunk at
     * or below a sequence it already stored. A build that started before
     * this JVM and has no cached chunk can only start at 0, so its first
     * chunks after the resume can miss the stored log.
     */
    private VarroaLogStorage newStorage(Run<?, ?> run, String runId, String jobPath, long buildNumber, String url) {
        LogReadCache cache = readCache();
        // A finished build only reads its log, so it continues nothing.
        LogReadCache.ResumePoint resume = run.isBuilding()
                ? cache.resumePoint(runId, jobPath, buildNumber)
                : LogReadCache.ResumePoint.FRESH;
        if (resume.nextSeq > 0) {
            LOGGER.log(Level.INFO, "VarroaLogStorageFactory: run {0} continues its log at seq {1} and posts {2} cached chunk(s) again",
                    new Object[] {runId, resume.nextSeq, resume.replay.size()});
        } else if (run.isBuilding() && run.getStartTimeInMillis() > 0 && run.getStartTimeInMillis() < JVM_START_MILLIS) {
            LOGGER.log(Level.WARNING, "VarroaLogStorageFactory: run {0} started before this JVM and has no cached log;"
                    + " it starts its log at seq 0", runId);
        }
        return new VarroaLogStorage(runId, jobPath, buildNumber, sender(url), cache, resume);
    }

    /**
     * Runs {@link VarroaLogStorage#finish()} for {@code runId} and removes it
     * from this factory's cache (D2 step 4). Returns {@code null} when this
     * factory never captured the run — the activation condition never held,
     * or {@link VarroaLogStorage} was never asked for a listener.
     */
    LogSummary finish(String runId) {
        VarroaLogStorage storage = runIdToStorage.remove(runId);
        ownerUrlToRunId.values().remove(runId);
        if (storage == null) {
            return null;
        }
        return storage.finish();
    }

    /** Switches a run's flush interval. {@link TailEndpoint} calls this. */
    boolean setTailActive(String runId, boolean active) {
        VarroaLogStorage storage = runIdToStorage.get(runId);
        if (storage == null) {
            return false;
        }
        storage.setTailActive(active);
        return true;
    }

    /**
     * Writes every build's open log segment to the read cache when Jenkins
     * stops. It runs after Jenkins pauses every build, so the pause line
     * is part of the segment.
     */
    @Terminator(requires = FlowExecutionList.EXECUTIONS_SUSPENDED)
    public static void flushOnShutdown() {
        LogChunkBuffer.flushAndDetachAll();
    }

    /** Clears every cached entry. Package-private for testing. */
    void resetForTesting() {
        runIdToStorage.clear();
        ownerUrlToRunId.clear();
    }

    /** {@code true} when {@code runId} currently has a cached storage. Package-private for testing. */
    boolean isCachedForTesting(String runId) {
        return runIdToStorage.containsKey(runId);
    }

    private LogChunkSender sender(String url) {
        LogChunkSender s = sender;
        if (s == null) {
            synchronized (this) {
                s = sender;
                if (s == null) {
                    s = senderOverrideForTesting != null ? senderOverrideForTesting : new LogChunkSender(url);
                    sender = s;
                }
            }
        }
        return s;
    }

    private LogReadCache readCache() {
        LogReadCache c = readCache;
        if (c == null) {
            synchronized (this) {
                c = readCache;
                if (c == null) {
                    c = readCacheOverrideForTesting != null ? readCacheOverrideForTesting : new LogReadCache();
                    readCache = c;
                }
            }
        }
        return c;
    }

    private volatile LogChunkSender senderOverrideForTesting;
    private volatile LogReadCache readCacheOverrideForTesting;

    /** Injects the sender and the read cache a build uses, before its first listener is requested. Package-private for testing. */
    void setCollaboratorsForTesting(LogChunkSender sender, LogReadCache readCache) {
        this.senderOverrideForTesting = sender;
        this.readCacheOverrideForTesting = readCache;
    }

    static String activationUrl() {
        String override = activationUrlOverrideForTesting;
        return parseActivationUrl(override != null ? override : System.getenv(LOG_URL_ENV));
    }

    /** Sets the testing override. Package-private for testing. */
    static void setActivationUrlForTesting(String url) {
        activationUrlOverrideForTesting = url;
    }

    /**
     * Validates {@code raw} against the activation rule: an absolute {@code
     * http://127.0.0.1} URL. Returns {@code raw} unchanged when valid, or
     * {@code null}. A pure function, separated from {@link #activationUrl()}
     * so a test can drive every branch without touching the process
     * environment.
     */
    static String parseActivationUrl(String raw) {
        if (raw == null || raw.isEmpty()) {
            return null;
        }
        try {
            URI uri = URI.create(raw);
            if (!uri.isAbsolute() || uri.getHost() == null) {
                return null;
            }
            if (!"http".equals(uri.getScheme()) || !"127.0.0.1".equals(uri.getHost())) {
                return null;
            }
            return raw;
        } catch (IllegalArgumentException e) {
            return null;
        }
    }

    /** Returns the running extension, or {@code null} when the plugin has not started. */
    static VarroaLogStorageFactory get() {
        try {
            return ExtensionList.lookupSingleton(VarroaLogStorageFactory.class);
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaLogStorageFactory: extension not available", e);
            return null;
        }
    }
}
