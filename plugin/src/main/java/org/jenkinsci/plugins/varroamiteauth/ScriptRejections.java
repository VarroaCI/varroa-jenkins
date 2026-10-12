package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.Run;

import java.util.ArrayList;
import java.util.Collections;
import java.util.IdentityHashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;

import org.jenkinsci.plugins.scriptsecurity.sandbox.RejectedAccessException;
import org.jenkinsci.plugins.workflow.flow.FlowExecution;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;

/**
 * Remembers the script signatures the sandbox refused in completed builds, and
 * the tenant jobs that hit each one.
 *
 * <p>The pending view reads script-security's pending list and joins the
 * requesters from this map. A caught rejection has no requester, so it
 * appears in the pending list with none. The map is bounded and lives in
 * memory. A Jenkins restart clears it.
 */
final class ScriptRejections {

    /** The most signatures the map keeps. The oldest signature leaves first. */
    static final int MAX_SIGNATURES = 100;

    /** The most requesters kept for one signature, newest first. */
    static final int MAX_REQUESTERS = 3;

    /** The deepest cause chain {@link #rejectionIn} walks. */
    private static final int MAX_CAUSE_DEPTH = 8;

    /** Insertion order is the record order. The newest entry is last. */
    private static final Map<String, Entry> BY_SIGNATURE = new LinkedHashMap<>();

    private ScriptRejections() {
    }

    /** One refused signature, as the sandbox reported it. */
    static final class Rejection {
        private final String signature;
        private final boolean dangerous;

        Rejection(String signature, boolean dangerous) {
            this.signature = signature;
            this.dangerous = dangerous;
        }

        String getSignature() {
            return signature;
        }

        boolean isDangerous() {
            return dangerous;
        }
    }

    /** One tenant job that hit a refused signature. */
    static final class Requester {
        private final String jobPath;
        private final long buildNumber;
        private final String runId;

        Requester(String jobPath, long buildNumber, String runId) {
            this.jobPath = jobPath == null ? "" : jobPath;
            this.buildNumber = buildNumber;
            this.runId = runId == null ? "" : runId;
        }

        String getJobPath() {
            return jobPath;
        }

        long getBuildNumber() {
            return buildNumber;
        }

        String getRunId() {
            return runId;
        }
    }

    /** One refused signature with its requesters, newest first. */
    static final class Entry {
        private final String signature;
        private final boolean dangerous;
        private final List<Requester> requesters;

        Entry(String signature, boolean dangerous, List<Requester> requesters) {
            this.signature = signature;
            this.dangerous = dangerous;
            this.requesters = requesters;
        }

        String getSignature() {
            return signature;
        }

        boolean isDangerous() {
            return dangerous;
        }

        List<Requester> getRequesters() {
            return requesters;
        }
    }

    /**
     * Records that a job hit a refused signature. A job that already appears for the
     * signature moves to the front. The signature moves to the newest position.
     */
    static synchronized void record(Rejection rejection, String jobPath, long buildNumber, String runId) {
        if (rejection == null || rejection.getSignature() == null || rejection.getSignature().isEmpty()) {
            return;
        }
        String signature = rejection.getSignature();
        Entry previous = BY_SIGNATURE.remove(signature);
        List<Requester> list = previous == null ? new ArrayList<>() : previous.getRequesters();
        String path = jobPath == null ? "" : jobPath;
        list.removeIf(r -> r.getJobPath().equals(path));
        list.add(0, new Requester(path, buildNumber, runId));
        while (list.size() > MAX_REQUESTERS) {
            list.remove(list.size() - 1);
        }
        BY_SIGNATURE.put(signature, new Entry(signature, rejection.isDangerous(), list));
        while (BY_SIGNATURE.size() > MAX_SIGNATURES) {
            String oldest = BY_SIGNATURE.keySet().iterator().next();
            BY_SIGNATURE.remove(oldest);
        }
    }

    /** Answers a copy of the requesters of one signature, newest first. */
    static synchronized List<Requester> requestersOf(String signature) {
        Entry e = BY_SIGNATURE.get(signature);
        return e == null ? new ArrayList<>() : new ArrayList<>(e.getRequesters());
    }

    /** Answers a copy of every entry, newest signature first. */
    static synchronized List<Entry> entries() {
        List<Entry> out = new ArrayList<>(BY_SIGNATURE.size());
        for (Entry e : BY_SIGNATURE.values()) {
            out.add(0, new Entry(e.getSignature(), e.isDangerous(), new ArrayList<>(e.getRequesters())));
        }
        return out;
    }

    /** Answers the refused signature of a completed run, or {@code null}. */
    static Rejection rejectionOf(Run<?, ?> run) {
        if (!(run instanceof WorkflowRun)) {
            return null;
        }
        FlowExecution execution = ((WorkflowRun) run).getExecution();
        if (execution == null) {
            return null;
        }
        return rejectionIn(execution.getCauseOfFailure());
    }

    /**
     * Walks the cause chain of a failure and answers the first
     * {@link RejectedAccessException}, or {@code null}. The walk stops at a cycle and
     * after {@link #MAX_CAUSE_DEPTH} links.
     */
    static Rejection rejectionIn(Throwable failure) {
        Set<Throwable> seen = Collections.newSetFromMap(new IdentityHashMap<>());
        Throwable current = failure;
        for (int depth = 0; current != null && depth < MAX_CAUSE_DEPTH && seen.add(current); depth++) {
            if (current instanceof RejectedAccessException) {
                RejectedAccessException rejected = (RejectedAccessException) current;
                String signature = rejected.getSignature();
                if (signature == null || signature.isEmpty()) {
                    return null;
                }
                return new Rejection(signature, rejected.isDangerous());
            }
            current = current.getCause();
        }
        return null;
    }
}
