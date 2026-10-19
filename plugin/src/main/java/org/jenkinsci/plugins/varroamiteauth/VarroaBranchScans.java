package org.jenkinsci.plugins.varroamiteauth;

import com.cloudbees.hudson.plugins.folder.computed.ComputedFolder;
import com.cloudbees.hudson.plugins.folder.computed.FolderComputation;
import hudson.model.Item;
import hudson.security.ACL;
import hudson.security.ACLContext;
import hudson.model.Job;
import hudson.model.Result;
import java.time.Instant;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.Iterator;
import java.util.Collections;
import java.util.Comparator;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Objects;
import java.util.TreeSet;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.TimeUnit;
import java.util.logging.Level;
import java.util.logging.Logger;
import jenkins.branch.Branch;
import jenkins.branch.BranchProjectFactory;
import jenkins.branch.MultiBranchProject;
import jenkins.branch.OrganizationFolder;
import jenkins.util.Timer;
import net.sf.json.JSONArray;
import net.sf.json.JSONNull;
import net.sf.json.JSONObject;

/**
 * Tracks the branch scans the mite requests on a multibranch job or an
 * organization folder. A scan records the heads of the job before it starts.
 * For an organization folder, a head is one repository. A timer compares the heads
 * after the folder computation ends with that record. The result answers
 * the {@code lastScan} key of the branch inventory row. Each head in the
 * result carries its job path, so a head that the scan deleted stays
 * attributable to its job.
 *
 * <p>The tracker holds its state in memory. A restart of the controller
 * drops every record.
 */
public final class VarroaBranchScans {

    private static final Logger LOGGER = Logger.getLogger(VarroaBranchScans.class.getName());

    /** The interval between two checks of the running scans. */
    static final long POLL_MILLIS = 3_000L;

    /** The longest a scan may run before it is reported as timed out. */
    static final long TIMEOUT_MILLIS = 30L * 60L * 1000L;

    /** The most names one result list holds. */
    static final int LIST_CAP = 200;

    /** The most accepted scan identifiers one job keeps for redelivery checks. */
    static final int ACCEPTED_CAP = 64;

    private static final ConcurrentHashMap<String, Scan> RECORDS = new ConcurrentHashMap<>();
    /** Per job full name: scan identifiers, mapped to whether the queue accepted them. */
    private static final Map<String, Map<String, Boolean>> ACCEPTED = new HashMap<>();
    private static final Object TIMER_LOCK = new Object();
    private static boolean timerStarted;

    private VarroaBranchScans() {
    }

    /** One head of a multibranch job at one moment. */
    static final class Head {
        final String name;
        final String hash;
        final boolean buildable;

        Head(String name, String hash, boolean buildable) {
            this.name = name;
            this.hash = hash;
            this.buildable = buildable;
        }
    }

    /** One head that a scan names: the branch name and the job path of its job. */
    static final class Ref {
        final String name;
        final String jobPath;

        Ref(String name, String jobPath) {
            this.name = name;
            this.jobPath = jobPath;
        }
    }

    /** The heads a scan adds, orphans, or changes. Each list is capped. */
    static final class Diff {
        final List<Ref> newHeads;
        final List<Ref> changedHeads;
        final List<Ref> orphanedHeads;

        Diff(List<Ref> newHeads, List<Ref> changedHeads, List<Ref> orphanedHeads) {
            this.newHeads = newHeads;
            this.changedHeads = changedHeads;
            this.orphanedHeads = orphanedHeads;
        }
    }

    /** One requested scan and its outcome. */
    static final class Scan {
        final String scanId;
        final String actor;
        final long requestedAt;
        final ComputedFolder<?> project;
        final Map<String, Head> before;
        volatile String state;
        volatile String result = "";
        volatile String message = "";
        volatile long endedAt;
        volatile List<Ref> newHeads = Collections.emptyList();
        volatile List<Ref> changedHeads = Collections.emptyList();
        volatile List<Ref> orphanedHeads = Collections.emptyList();

        Scan(String scanId, String actor, long requestedAt, ComputedFolder<?> project,
                Map<String, Head> before) {
            this.scanId = scanId;
            this.actor = actor;
            this.requestedAt = requestedAt;
            this.project = project;
            this.before = before;
            this.state = "running";
        }
    }

    /**
     * Records that a scan of {@code project} starts now. The record holds the
     * heads of the job before the scan. The timer then checks the job until
     * the folder computation that the scan starts ends.
     */
    static void arm(ComputedFolder<?> project, String scanId, String actor) {
        Scan scan = new Scan(scanId, actor, System.currentTimeMillis(), project, snapshot(project));
        RECORDS.put(project.getFullName(), scan);
        ensureTimer();
    }


    /** Records a scan that the job or the queue refused. */
    static void recordRefused(String fullName, String scanId, String actor, String message) {
        Scan scan = new Scan(scanId, actor, System.currentTimeMillis(), null, Collections.emptyMap());
        scan.state = "refused";
        scan.message = message;
        scan.endedAt = scan.requestedAt;
        RECORDS.put(fullName, scan);
        forgetAccepted(fullName, scanId);
    }

    /** Records a completed scan with the given diff. Tests use it to fill the result. */
    static void recordCompletedForTest(String fullName, String scanId, Diff diff) {
        Scan scan = new Scan(scanId, "ada", System.currentTimeMillis(), null, Collections.emptyMap());
        scan.state = "completed";
        scan.endedAt = scan.requestedAt;
        scan.newHeads = diff.newHeads;
        scan.changedHeads = diff.changedHeads;
        scan.orphanedHeads = diff.orphanedHeads;
        RECORDS.put(fullName, scan);
    }

    /** Answers the {@code lastScan} value of a job's inventory row, or JSON null. */
    static Object lastScanJson(String fullName) {
        Scan scan = RECORDS.get(fullName);
        if (scan == null) {
            return JSONNull.getInstance();
        }
        JSONObject out = new JSONObject();
        out.put("scanId", scan.scanId);
        out.put("state", scan.state);
        out.put("actor", scan.actor);
        out.put("requestedAt", iso(scan.requestedAt));
        out.put("endedAt", scan.endedAt == 0 ? "" : iso(scan.endedAt));
        out.put("result", scan.result);
        out.put("newHeadRefs", refs(scan.newHeads));
        out.put("changedHeadRefs", refs(scan.changedHeads));
        out.put("orphanedHeadRefs", refs(scan.orphanedHeads));
        out.put("message", scan.message);
        return out;
    }

    /**
     * Answers the difference between two head snapshots. Heads are keyed by
     * job path, so two heads that share a branch name stay separate. A new
     * head is in {@code after} only. An orphaned head is missing from
     * {@code after}, or was buildable before and is not buildable now. A
     * changed head is still buildable and holds a different revision hash.
     */
    static Diff diff(Map<String, Head> before, Map<String, Head> after) {
        TreeSet<Ref> added = new TreeSet<>(REF_ORDER);
        TreeSet<Ref> changed = new TreeSet<>(REF_ORDER);
        TreeSet<Ref> orphaned = new TreeSet<>(REF_ORDER);
        for (Map.Entry<String, Head> entry : after.entrySet()) {
            if (!before.containsKey(entry.getKey())) {
                added.add(refOf(entry.getKey(), entry.getValue()));
            }
        }
        for (Map.Entry<String, Head> entry : before.entrySet()) {
            String path = entry.getKey();
            Head was = entry.getValue();
            Head now = after.get(path);
            if (now == null) {
                orphaned.add(refOf(path, was));
            } else if (was.buildable && !now.buildable) {
                orphaned.add(refOf(path, now));
            } else if (now.buildable && !Objects.equals(was.hash, now.hash)) {
                changed.add(refOf(path, now));
            }
        }
        return new Diff(cap(added), cap(changed), cap(orphaned));
    }

    /** Orders refs by branch name, then by job path. */
    private static final Comparator<Ref> REF_ORDER = Comparator
            .comparing((Ref ref) -> ref.name)
            .thenComparing(ref -> ref.jobPath);

    private static Ref refOf(String jobPath, Head head) {
        return new Ref(head.name, jobPath);
    }

    private static List<Ref> cap(TreeSet<Ref> refs) {
        List<Ref> out = new ArrayList<>();
        for (Ref ref : refs) {
            if (out.size() >= LIST_CAP) {
                break;
            }
            out.add(ref);
        }
        return out;
    }

    /**
     * Checks every running scan once. A scan whose folder computation has ended
     * takes its result. A scan that runs past {@link #TIMEOUT_MILLIS} becomes a
     * timeout.
     */
    static void tick() {
        long now = System.currentTimeMillis();
        for (Scan scan : RECORDS.values()) {
            if (!"running".equals(scan.state) || scan.project == null) {
                continue;
            }
            try {
                if (now - scan.requestedAt > TIMEOUT_MILLIS) {
                    finish(scan, "timeout", "", "the scan did not end within 30 minutes", now);
                    continue;
                }
                FolderComputation computation = scan.project.getComputation();
                if (computation == null || computation.getTimestamp() == null
                        || computation.getTimestamp().getTimeInMillis() < scan.requestedAt
                        || computation.isBuilding()) {
                    continue;
                }
                Result outcome = computation.getResult();
                String resultText = outcome == null ? "" : outcome.toString();
                Diff diff = diff(scan.before, snapshot(scan.project));
                scan.newHeads = diff.newHeads;
                scan.changedHeads = diff.changedHeads;
                scan.orphanedHeads = diff.orphanedHeads;
                if (Result.SUCCESS.equals(outcome)) {
                    finish(scan, "completed", resultText, "", now);
                } else {
                    finish(scan, "failed", resultText, "the folder computation ended with " + resultText, now);
                }
            } catch (RuntimeException e) {
                LOGGER.log(Level.WARNING, "branch scan check failed for " + scan.project.getFullName(), e);
            }
        }
    }

    private static void finish(Scan scan, String state, String result, String message, long now) {
        scan.result = result;
        scan.message = message;
        scan.endedAt = now;
        scan.state = state;
    }

    /** {@link #claim} answer: this request holds the new reservation. */
    static final int CLAIM_NEW = 0;
    /** {@link #claim} answer: another request holds the reservation and has not finished. */
    static final int CLAIM_IN_FLIGHT = 1;
    /** {@link #claim} answer: the queue accepted this scan earlier. */
    static final int CLAIM_ACCEPTED = 2;

    /**
     * Reserves {@code scanId} for {@code fullName}. The first request claims it
     * and must then call {@link #markAccepted} once the queue takes the scan, or
     * {@link #recordRefused} when it does not. A duplicate of a reservation still
     * in flight answers {@link #CLAIM_IN_FLIGHT}, so no request is acknowledged
     * before the owner decides. A duplicate of an accepted scan answers
     * {@link #CLAIM_ACCEPTED}, and no second computation starts. Every access
     * holds the class monitor.
     */
    static synchronized int claim(String fullName, String scanId) {
        Map<String, Boolean> ids = ACCEPTED.computeIfAbsent(fullName, k -> new LinkedHashMap<>());
        Boolean accepted = ids.get(scanId);
        if (accepted != null) {
            return accepted ? CLAIM_ACCEPTED : CLAIM_IN_FLIGHT;
        }
        ids.put(scanId, Boolean.FALSE);
        evictAccepted(ids);
        return CLAIM_NEW;
    }

    /** Marks a claimed reservation accepted by the queue. */
    static synchronized void markAccepted(String fullName, String scanId) {
        Map<String, Boolean> ids = ACCEPTED.get(fullName);
        if (ids != null && ids.containsKey(scanId)) {
            // Re-inserting moves the entry to the end, so eviction follows the
            // order of acceptance, not the order of the claim.
            ids.remove(scanId);
            ids.put(scanId, Boolean.TRUE);
            evictAccepted(ids);
        }
    }

    /**
     * Drops the oldest accepted entries while more than {@link #ACCEPTED_CAP}
     * accepted entries remain. An in-flight reservation is never dropped, so its
     * redelivery still finds it.
     */
    private static void evictAccepted(Map<String, Boolean> ids) {
        int acceptedCount = 0;
        for (Boolean value : ids.values()) {
            if (value) {
                acceptedCount++;
            }
        }
        Iterator<Map.Entry<String, Boolean>> entries = ids.entrySet().iterator();
        while (acceptedCount > ACCEPTED_CAP && entries.hasNext()) {
            if (entries.next().getValue()) {
                entries.remove();
                acceptedCount--;
            }
        }
    }

    private static synchronized void forgetAccepted(String fullName, String scanId) {
        Map<String, Boolean> ids = ACCEPTED.get(fullName);
        if (ids != null) {
            ids.remove(scanId);
        }
    }

    /**
     * Answers the heads of a job now, keyed by the full name of each head's job.
     * The snapshot runs as the system identity, so the before and after records
     * hold the same heads whichever identity requested the scan.
     */
    static Map<String, Head> snapshot(ComputedFolder<?> folder) {
        try (ACLContext ignored = ACL.as2(ACL.SYSTEM2)) {
            if (folder instanceof OrganizationFolder) {
                return snapshotOrganizationFolder((OrganizationFolder) folder);
            }
            return snapshotAsCurrentIdentity((MultiBranchProject) folder);
        }
    }

    /**
     * Answers one head per repository of an organization folder. The head
     * name is the repository name, and its job path is the repository's
     * full name. The branch heads of a repository stay in its own record.
     */
    private static Map<String, Head> snapshotOrganizationFolder(OrganizationFolder folder) {
        Map<String, Head> out = new LinkedHashMap<>();
        for (Object child : folder.getItems()) {
            if (!(child instanceof MultiBranchProject)) {
                continue;
            }
            MultiBranchProject repository = (MultiBranchProject) child;
            out.put(repository.getFullName(), new Head(repository.getName(), "",
                    !repository.isDisabled()));
        }
        return out;
    }

    private static Map<String, Head> snapshotAsCurrentIdentity(MultiBranchProject project) {
        BranchProjectFactory factory = project.getProjectFactory();
        Map<String, Head> out = new LinkedHashMap<>();
        for (Object child : project.getItems()) {
            Job job = factory.asProject((Item) child);
            if (job == null) {
                continue;
            }
            Branch branch = factory.getBranch(job);
            if (branch == null) {
                continue;
            }
            String name = branch.getName() != null ? branch.getName() : "";
            out.put(job.getFullName(), new Head(name, ActivityEndpoint.revisionOf(factory, job),
                    branch.isBuildable()));
        }
        return out;
    }

    private static void ensureTimer() {
        synchronized (TIMER_LOCK) {
            if (timerStarted) {
                return;
            }
            timerStarted = true;
            Timer.get().scheduleWithFixedDelay(() -> {
                try {
                    tick();
                } catch (RuntimeException e) {
                    LOGGER.log(Level.WARNING, "branch scan tick failed", e);
                }
            }, POLL_MILLIS, POLL_MILLIS, TimeUnit.MILLISECONDS);
        }
    }

    private static String iso(long millis) {
        return Instant.ofEpochMilli(millis).toString();
    }

    private static JSONArray refs(List<Ref> refs) {
        JSONArray out = new JSONArray();
        for (Ref ref : refs) {
            JSONObject item = new JSONObject();
            item.put("name", ref.name);
            item.put("jobPath", ref.jobPath);
            out.add(item);
        }
        return out;
    }
}
