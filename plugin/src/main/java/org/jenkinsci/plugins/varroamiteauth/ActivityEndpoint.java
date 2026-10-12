package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import com.cloudbees.hudson.plugins.folder.computed.FolderComputation;
import jenkins.branch.Branch;
import jenkins.branch.BranchProjectFactory;
import jenkins.branch.MultiBranchProject;
import jenkins.scm.api.SCMHead;
import jenkins.scm.api.SCMRevision;
import jenkins.scm.api.mixin.ChangeRequestSCMHead;
import jenkins.scm.api.mixin.TagSCMHead;
import net.sf.json.JSONNull;
import hudson.model.Computer;
import hudson.model.Executor;
import hudson.model.Item;
import hudson.model.Items;
import hudson.model.Job;
import hudson.model.Queue;
import hudson.model.Result;
import hudson.model.RootAction;
import hudson.scheduler.CronTab;
import hudson.scheduler.Hash;
import hudson.security.ACL;
import hudson.security.ACLContext;

import java.io.StringWriter;
import hudson.triggers.Trigger;
import hudson.triggers.TriggerDescriptor;

import java.nio.charset.StandardCharsets;
import java.time.Instant;
import java.time.format.DateTimeFormatter;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Calendar;
import java.util.Collections;
import java.util.Comparator;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TimeZone;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.logging.Level;
import java.util.logging.Logger;

import jenkins.model.Jenkins;
import jenkins.triggers.ReverseBuildTrigger;
import org.jenkinsci.plugins.scriptsecurity.scripts.ScriptApproval;
import org.kohsuke.accmod.restrictions.suppressions.SuppressRestrictedWarnings;
import net.sf.json.JSONArray;
import net.sf.json.JSONObject;

import org.kohsuke.stapler.StaplerRequest2;
import org.kohsuke.stapler.StaplerResponse2;
import org.kohsuke.stapler.WebMethod;

import org.springframework.security.core.GrantedAuthority;

/**
 * Protected {@link RootAction} that drains the {@link ActivitySink} buffer and
 * returns events plus idle gauges as JSON.
 *
 * <p>URL: {@code /varroa-activity/events} (GET only). Requires the authenticated
 * principal to hold the authority {@code ROLE:varroa:system-mite}.
 *
 * <p>The response now includes an {@code idle} object with the hibernation gauges
 * (D1): {@code lastHttpActivityUnix}, {@code lastEventUnix}, {@code runningBuilds},
 * {@code queueLength}, and {@code timerTriggerJobs}.
 *
 * <p>This class also serves {@code GET /varroa-activity/schedules}, the live job-schedule
 * export. It requires the same {@code ROLE:varroa:system-mite} authority and reports, for
 * every job, every trigger's type, specification, time zone, disabled state, and next three
 * scheduled occurrences, computed with Jenkins's own {@link CronTab}.
 */
@Extension
public class ActivityEndpoint implements RootAction {

    private static final Logger LOGGER = Logger.getLogger(ActivityEndpoint.class.getName());

    private static final int DEFAULT_MAX_BATCH = 256;
    private static final int MAX_BATCH_CAP = 1024;
    private static final int MIN_BATCH = 1;

    /** The schedule walk adds no row past this count. */
    private static final int MAX_SCHEDULE_ROWS = 5000;

    /** The schedule walk adds no row once the serialized rows reach this many bytes. */
    private static final int MAX_SCHEDULE_BYTES = 700 * 1024;

    /** The number of scheduled occurrences the endpoint reports per row. */
    private static final int OCCURRENCES_PER_ROW = 3;

    /** Multibranch projects per branch-inventory response. */
    static final int MAX_BRANCH_PROJECTS = 500;

    /** Heads per multibranch project in the branch-inventory response. */
    static final int MAX_BRANCH_HEADS = 500;

    /** Bytes per branch-inventory response. */
    static final int MAX_BRANCH_BYTES = 512 * 1024;

    // ---- Timer-triggered jobs cache (≤5 min lazy recompute) ----

    /** Cache TTL in milliseconds (5 minutes). */
    private static final long TIMER_CACHE_TTL_MS = 300_000L;

    /** Cached count of timer-triggered jobs. */
    private static final AtomicInteger cachedTimerTriggerJobs = new AtomicInteger(0);

    /** When the cache was last refreshed (epoch millis). */
    private static volatile long timerCacheLastRefreshedMs;

    @Override
    public String getUrlName() {
        return "varroa-activity";
    }

    @Override
    public String getIconFileName() {
        return null; // no UI surface
    }

    @Override
    public String getDisplayName() {
        return null; // no UI surface
    }

    /**
     * Handles {@code GET /varroa-activity/events}. Drains the sink and returns
     * {@code { "events": [...], "dropped": <n>, "idle": { ... } }} as JSON.
     */
    public void doEvents(StaplerRequest2 req, StaplerResponse2 rsp) {
        try {
            // Method gate: only GET.
            if (!"GET".equalsIgnoreCase(req.getMethod())) {
                PlainTextRefusal.refuse(rsp, 405, "Method Not Allowed: only GET is served");
                return;
            }

            // AuthZ: require ROLE:varroa:system-mite authority.
            if (!isMitePrincipal()) {
                PlainTextRefusal.refuse(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
                return;
            }

            // Parse max query param.
            int max = readMaxBatch(req.getParameter("max"));

            // Drain the sink.
            ActivitySink.DrainResult result = ActivitySink.get().drain(max);

            // Build JSON response with explicit field-by-field construction.
            JSONObject envelope = new JSONObject();
            envelope.put("events", buildEventsArray(result.events));
            envelope.put("dropped", result.dropped);
            envelope.put("idle", buildIdleObject());

            rsp.setContentType("application/json");
            rsp.setStatus(200);

            StringWriter writer = new StringWriter();
            envelope.write(writer);
            rsp.getWriter().write(writer.toString());
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "ActivityEndpoint.doEvents caught exception", e);
            try {
                PlainTextRefusal.refuse(rsp, 500, "Internal server error");
            } catch (Exception ignored) {
                // Best-effort error response.
            }
        }
    }

    /**
     * Handles {@code GET /varroa-activity/schedules}. Walks every job, reports one row per
     * trigger and one {@code none} row per untriggered job, and returns
     * {@code { "schedules": [...], "total": N, "truncated": B }} as JSON.
     */
    public void doSchedules(StaplerRequest2 req, StaplerResponse2 rsp) {
        try {
            // Method gate: only GET.
            if (!"GET".equalsIgnoreCase(req.getMethod())) {
                PlainTextRefusal.refuse(rsp, 405, "Method Not Allowed: only GET is served");
                return;
            }

            // AuthZ: require ROLE:varroa:system-mite authority.
            if (!isMitePrincipal()) {
                PlainTextRefusal.refuse(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
                return;
            }

            Jenkins jenkins = Jenkins.getInstanceOrNull();
            List<JSONObject> candidateRows = buildCandidateRows(jenkins);
            JSONObject envelope = applyScheduleBounds(candidateRows);

            rsp.setContentType("application/json");
            rsp.setStatus(200);

            StringWriter writer = new StringWriter();
            envelope.write(writer);
            rsp.getWriter().write(writer.toString());
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "ActivityEndpoint.doSchedules caught exception", e);
            try {
                PlainTextRefusal.refuse(rsp, 500, "Internal server error");
            } catch (Exception ignored) {
                // Best-effort error response.
            }
        }
    }

    /**
     * Handles {@code GET /varroa-activity/script-approvals}. Lists the script signatures the
     * sandbox refused in completed builds, with the tenant jobs that requested each. The
     * operator drops the signatures that a pool approves. Answers
     * {@code { "pending": [...], "total": N, "truncated": B }} as JSON.
     */
    @WebMethod(name = "script-approvals")
    public void doScriptApprovals(StaplerRequest2 req, StaplerResponse2 rsp) {
        try {
            if (!"GET".equalsIgnoreCase(req.getMethod())) {
                PlainTextRefusal.refuse(rsp, 405, "Method Not Allowed: only GET is served");
                return;
            }
            if (!isMitePrincipal()) {
                PlainTextRefusal.refuse(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
                return;
            }

            List<PendingRow> rows;
            try (ACLContext ignored = ACL.as2(ACL.SYSTEM2)) {
                rows = pendingRows();
            }
            JSONObject envelope = scriptApprovalEnvelope(rows);

            rsp.setContentType("application/json");
            rsp.setStatus(200);
            StringWriter writer = new StringWriter();
            envelope.write(writer);
            rsp.getWriter().write(writer.toString());
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "ActivityEndpoint.doScriptApprovals caught exception", e);
            try {
                PlainTextRefusal.refuse(rsp, 500, "Internal server error");
            } catch (Exception ignored) {
                // Best-effort error response.
            }
        }
    }

    /** The most pending signatures the script-approvals response lists. */
    static final int MAX_PENDING_LISTED = 25;

    /** One refused signature with the requesters the endpoint reports for it. */
    static final class PendingRow {
        final String signature;
        final boolean dangerous;
        final List<ScriptRejections.Requester> requesters;

        PendingRow(String signature, boolean dangerous, List<ScriptRejections.Requester> requesters) {
            this.signature = signature;
            this.dangerous = dangerous;
            this.requesters = requesters;
        }
    }

    /**
     * Builds the pending rows. The refusals observed in completed builds come first, each
     * with its requesters from {@link ScriptRejections}. Then the signatures that
     * script-security lists as pending and no build observed are added. A signature with no
     * requester falls back to the item of its pending approval context. The approved
     * signatures are dropped from both sources.
     *
     * <p>Two calls on this path are restricted APIs. The access check is suppressed here
     * for those calls only. The plugin pins the script-security version through the plugin
     * lock. The plugin never writes an approval: the operator renders approvals through the
     * pool, so this read only lists what the pool does not yet approve.
     */
    @SuppressRestrictedWarnings({ScriptApproval.class, ScriptApproval.PendingSignature.class})
    static List<PendingRow> pendingRows() {
        ScriptApproval approval = ScriptApproval.get();
        Set<String> approved = new HashSet<>();
        String[] approvedNow = approval.getApprovedSignatures();
        if (approvedNow != null) {
            approved.addAll(Arrays.asList(approvedNow));
        }
        Map<String, PendingRow> bySignature = new LinkedHashMap<>();
        for (ScriptRejections.Entry e : ScriptRejections.entries()) {
            if (!approved.contains(e.getSignature())) {
                bySignature.put(e.getSignature(),
                        new PendingRow(e.getSignature(), e.isDangerous(), e.getRequesters()));
            }
        }
        for (ScriptApproval.PendingSignature p : approval.getPendingSignatures()) {
            if (approved.contains(p.signature) || bySignature.containsKey(p.signature)) {
                continue;
            }
            List<ScriptRejections.Requester> requesters = new ArrayList<>();
            if (p.getContext() != null) {
                Item item = p.getContext().getItem();
                if (item != null) {
                    requesters.add(new ScriptRejections.Requester(item.getFullName(), 0, ""));
                }
            }
            bySignature.put(p.signature, new PendingRow(p.signature, p.dangerous, requesters));
        }
        return new ArrayList<>(bySignature.values());
    }

    /**
     * Builds the script-approvals envelope. The list keeps the first
     * {@link #MAX_PENDING_LISTED} rows, and {@code total} counts every row. Package-private
     * for testing.
     */
    static JSONObject scriptApprovalEnvelope(List<PendingRow> rows) {
        JSONArray pending = new JSONArray();
        for (PendingRow row : rows) {
            if (pending.size() >= MAX_PENDING_LISTED) {
                break;
            }
            JSONArray requesters = new JSONArray();
            for (ScriptRejections.Requester r : row.requesters) {
                JSONObject requester = new JSONObject();
                requester.put("tenant", tenantOfJobPath(r.getJobPath()));
                requester.put("jobPath", r.getJobPath());
                requester.put("buildNumber", r.getBuildNumber());
                requester.put("runId", r.getRunId());
                requesters.add(requester);
            }
            JSONObject item = new JSONObject();
            item.put("signature", row.signature);
            item.put("dangerous", row.dangerous);
            item.put("requesters", requesters);
            pending.add(item);
        }
        JSONObject envelope = new JSONObject();
        envelope.put("pending", pending);
        envelope.put("total", rows.size());
        envelope.put("truncated", rows.size() > pending.size());
        return envelope;
    }

    /**
     * The tenant of a job path: its first segment when the cell hosts that tenant, else
     * the empty string.
     */
    static String tenantOfJobPath(String jobPath) {
        if (jobPath == null || jobPath.isEmpty()) {
            return "";
        }
        int slash = jobPath.indexOf('/');
        String tenant = slash < 0 ? jobPath : jobPath.substring(0, slash);
        return VarroaTenantResolver.hostsTenant(tenant) ? tenant : "";
    }

    /**
     * Handles {@code GET /varroa-activity/branches}. Walks every multibranch project and
     * reports one entry per project with its discovered heads, and returns
     * {@code { "projects": [...], "total": N, "truncated": B }} as JSON.
     */
    public void doBranches(StaplerRequest2 req, StaplerResponse2 rsp) {
        try {
            if (!"GET".equalsIgnoreCase(req.getMethod())) {
                PlainTextRefusal.refuse(rsp, 405, "Method Not Allowed: only GET is served");
                return;
            }
            if (!isMitePrincipal()) {
                PlainTextRefusal.refuse(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
                return;
            }
            JSONObject envelope;
            try (ACLContext ignored = ACL.as2(ACL.SYSTEM2)) {
                envelope = buildBranchEnvelope(Jenkins.getInstanceOrNull());
            }
            rsp.setContentType("application/json");
            rsp.setStatus(200);
            StringWriter writer = new StringWriter();
            envelope.write(writer);
            rsp.getWriter().write(writer.toString());
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "ActivityEndpoint.doBranches caught exception", e);
            try {
                PlainTextRefusal.refuse(rsp, 500, "Internal server error");
            } catch (Exception ignored) {
                // Best-effort error response.
            }
        }
    }

    // ---- Multibranch inventory export ----

    /**
     * Builds the branch-inventory envelope. A project that fails to read is skipped, and
     * the response stays valid. Package-private for testing.
     */
    @SuppressWarnings({"rawtypes"})
    static JSONObject buildBranchEnvelope(Jenkins jenkins) {
        JSONArray projects = new JSONArray();
        int total = 0;
        boolean truncated = false;
        int bytes = 0;
        if (jenkins != null) {
            for (MultiBranchProject project : jenkins.getAllItems(MultiBranchProject.class)) {
                total++;
                if (projects.size() >= MAX_BRANCH_PROJECTS) {
                    truncated = true;
                    continue;
                }
                JSONObject row;
                try {
                    row = buildBranchProjectRow(project);
                } catch (RuntimeException | LinkageError e) {
                    LOGGER.log(Level.FINE, "ActivityEndpoint: the branch inventory read failed", e);
                    // A project that could not be read is not evidence it left the
                    // cell, so the envelope must not delete its stored rows.
                    truncated = true;
                    continue;
                }
                int size = row.toString().getBytes(StandardCharsets.UTF_8).length;
                if (bytes + size > MAX_BRANCH_BYTES) {
                    truncated = true;
                    continue;
                }
                bytes += size;
                projects.add(row);
            }
        }
        JSONObject envelope = new JSONObject();
        envelope.put("projects", projects);
        envelope.put("total", total);
        envelope.put("truncated", truncated);
        return envelope;
    }

    /**
     * Builds one project entry with its heads. The project path is the multibranch
     * project's full name. Each head carries the full name of its own child job.
     */
    @SuppressWarnings({"rawtypes", "unchecked"})
    static JSONObject buildBranchProjectRow(MultiBranchProject project) {
        BranchProjectFactory factory = project.getProjectFactory();
        JSONArray heads = new JSONArray();
        boolean headsTruncated = false;
        for (Object child : project.getItems()) {
            Job job = factory.asProject((Item) child);
            if (job == null) {
                continue;
            }
            Branch branch = factory.getBranch(job);
            if (branch == null) {
                continue;
            }
            if (heads.size() >= MAX_BRANCH_HEADS) {
                headsTruncated = true;
                break;
            }
            heads.add(buildBranchHeadRow(job.getFullName(), revisionOf(factory, job), branch));
        }
        FolderComputation computation = project.getComputation();
        String indexedAt = "";
        String indexResult = "";
        if (computation != null) {
            if (computation.getTimestamp() != null) {
                indexedAt = Instant.ofEpochMilli(computation.getTimestamp().getTimeInMillis()).toString();
            }
            if (computation.getResult() != null) {
                indexResult = computation.getResult().toString();
            }
        }
        JSONObject row = new JSONObject();
        row.put("jobPath", project.getFullName());
        row.put("indexedAt", indexedAt);
        row.put("indexResult", indexResult);
        row.put("heads", heads);
        row.put("headsTruncated", headsTruncated);
        row.put("lastScan", JSONNull.getInstance());
        return row;
    }

    /**
     * Builds one head entry: a branch, a change request, or a tag. The job path
     * and the revision come in as values, so the row is built from a branch alone.
     * Package-private for testing.
     */
    static JSONObject buildBranchHeadRow(String jobPath, String revision, Branch branch) {
        SCMHead head = branch.getHead();
        String kind = head instanceof ChangeRequestSCMHead ? "changeRequest"
                : head instanceof TagSCMHead ? "tag" : "branch";
        JSONObject row = new JSONObject();
        row.put("name", branch.getName() != null ? branch.getName() : "");
        row.put("jobPath", jobPath);
        row.put("kind", kind);
        row.put("orphaned", !branch.isBuildable());
        row.put("revision", revision);
        BuildEvent.ChangeRequestInfo request = VarroaRunRecorder.changeRequestOf(branch);
        if (request != null) {
            JSONObject change = new JSONObject();
            change.put("id", request.getId());
            change.put("title", request.getTitle());
            change.put("sourceBranch", request.getSourceBranch());
            change.put("targetBranch", request.getTargetBranch());
            change.put("url", request.getUrl());
            row.put("changeRequest", change);
        }
        return row;
    }

    @SuppressWarnings({"rawtypes", "unchecked"})
    private static String revisionOf(BranchProjectFactory factory, Job job) {
        SCMRevision revision = factory.getLastSeenRevision(job);
        if (revision == null) {
            return "";
        }
        Object hash = VarroaRunRecorder.invokeNoArg(revision, "getHash");
        return hash != null ? hash.toString() : "";
    }

    // ---- Job-schedule export (D8) ----

    /**
     * Walks every job in the instance and builds one candidate row per trigger, plus one
     * {@code none} row for a job with no trigger. Package-private for testing.
     */
    static List<JSONObject> buildCandidateRows(Jenkins jenkins) {
        List<JSONObject> rows = new ArrayList<>();
        if (jenkins == null) {
            return rows;
        }
        for (Job<?, ?> job : jenkins.getAllItems(Job.class)) {
            boolean disabled = isDisabledByReflection(job);
            List<Trigger<?>> triggers = getTriggersByReflection(job);
            if (triggers.isEmpty()) {
                rows.add(buildScheduleRow(job.getFullName(), "none", "", "", disabled,
                        Collections.emptyList()));
            } else {
                for (Trigger<?> trigger : triggers) {
                    rows.add(buildTriggerRow(job, trigger, disabled));
                }
            }
        }
        return rows;
    }

    /**
     * Builds the row for one job's trigger: maps the trigger type, splits the {@code TZ=}
     * line out of the specification, and computes the occurrences for a {@code cron} or
     * {@code scm-poll} row.
     */
    static JSONObject buildTriggerRow(Job<?, ?> job, Trigger<?> trigger, boolean disabled) {
        String jobPath = job.getFullName();
        String triggerType = mapTriggerType(trigger.getClass().getSimpleName());
        String rawSpec = trigger.getSpec();
        String[] specAndZone = splitTimezone(rawSpec == null ? "" : rawSpec);
        String spec = specAndZone[0];
        String timezone = specAndZone[1];

        List<String> occurrences = Collections.emptyList();
        if ("cron".equals(triggerType) || "scm-poll".equals(triggerType)) {
            occurrences = computeOccurrences(spec, jobPath, timezone, null);
        }
        if (trigger instanceof VarroaUpstreamTrigger) {
            VarroaUpstreamTrigger upstream = (VarroaUpstreamTrigger) trigger;
            return buildScheduleRow(jobPath, triggerType, spec, timezone, disabled, occurrences,
                    upstream.getTenant(), upstream.getJob(), upstream.getResult());
        }
        if (trigger instanceof ReverseBuildTrigger) {
            ReverseBuildTrigger reverse = (ReverseBuildTrigger) trigger;
            return buildScheduleRow(jobPath, triggerType, resolvedUpstreamPaths(job, reverse), "",
                    disabled, Collections.emptyList(), null, null, thresholdName(reverse));
        }
        return buildScheduleRow(jobPath, triggerType, spec, timezone, disabled, occurrences);
    }

    /**
     * The upstream job paths a native trigger names, resolved against the
     * job's parent and joined with a comma, in configuration order. A name
     * that resolves to no job is dropped.
     *
     * <p>A native trigger names several projects and names no tenant, so
     * the two single-valued upstream fields cannot hold the list. The
     * resolution runs as the system identity, because the trigger itself
     * resolves its own projects that way.
     */
    static String resolvedUpstreamPaths(Job<?, ?> job, ReverseBuildTrigger trigger) {
        String configured = trigger.getUpstreamProjects();
        if (configured == null || configured.isEmpty()) {
            return "";
        }
        List<String> paths = new ArrayList<>();
        try (ACLContext ignored = ACL.as2(ACL.SYSTEM2)) {
            for (Job<?, ?> upstream : Items.fromNameList(job.getParent(), configured, Job.class)) {
                paths.add(upstream.getFullName());
            }
        }
        return String.join(",", paths);
    }

    /** The native trigger's threshold: {@code SUCCESS}, {@code UNSTABLE}, or {@code FAILURE}. */
    private static String thresholdName(ReverseBuildTrigger trigger) {
        Result threshold = trigger.getThreshold();
        return threshold != null ? threshold.toString() : "";
    }

    /**
     * Maps a trigger's simple class name to the exported trigger type. Any class this table
     * does not name maps to {@code other}. {@code none} is not a trigger class and is never
     * produced by this method.
     */
    static String mapTriggerType(String simpleClassName) {
        switch (simpleClassName) {
            case "TimerTrigger":
                return "cron";
            case "SCMTrigger":
                return "scm-poll";
            case "ReverseBuildTrigger":
                return "upstream";
            case "VarroaUpstreamTrigger":
                return "varroa-upstream";
            default:
                return "other";
        }
    }

    /**
     * Splits a leading {@code TZ=} line out of a trigger specification. Returns a two-element
     * array: the remaining specification, and the time zone (empty when no {@code TZ=} line is
     * present). Mirrors the first-line check in {@code hudson.scheduler.CronTabList.create}.
     */
    static String[] splitTimezone(String spec) {
        if (spec == null || spec.isEmpty()) {
            return new String[] {"", ""};
        }
        String[] lines = spec.split("\\r?\\n", -1);
        String first = lines[0].trim();
        if (first.startsWith("TZ=")) {
            String timezone = first.substring("TZ=".length());
            String remaining = String.join("\n", Arrays.asList(lines).subList(1, lines.length));
            return new String[] {remaining, timezone};
        }
        return new String[] {spec, ""};
    }

    /**
     * Computes the next three occurrences of a cron specification, using Jenkins's own
     * {@link CronTab}. Builds one {@code CronTab} per specification line, skipping a blank
     * line and a comment line, exactly as {@code CronTabList.create} does — the caller has
     * already removed the leading {@code TZ=} line via {@link #splitTimezone}. Every
     * {@code CronTab} is seeded with {@code Hash.from(jobFullName)}, the seed
     * {@code Trigger.start} uses, so an {@code H} token resolves to the minute Jenkins fires.
     *
     * <p>{@code fixedNow} pins the start instant for a test. Production code passes
     * {@code null}, which starts from the current time.
     *
     * <p>{@code CronTab.ceil} throws an unchecked exception, annotated for no external use,
     * when a specification line reaches no date within two years. This method catches
     * {@code RuntimeException} per line and reports whatever occurrences the row already
     * holds, rather than naming that exception class.
     */
    static List<String> computeOccurrences(String specWithoutTz, String jobFullName, String timezone,
            Instant fixedNow) {
        List<Instant> candidates = new ArrayList<>();
        Hash hash = Hash.from(jobFullName);
        String timezoneArg = (timezone == null || timezone.isEmpty()) ? null : timezone;

        int lineNumber = 0;
        for (String rawLine : specWithoutTz.split("\\r?\\n", -1)) {
            lineNumber++;
            String line = rawLine.trim();
            if (line.isEmpty() || line.startsWith("#")) {
                continue; // ignorable line, same as CronTabList.create
            }
            try {
                CronTab tab = new CronTab(line, lineNumber, hash, timezoneArg);
                TimeZone tabZone = tab.getTimeZone();
                Calendar seed = tabZone == null ? Calendar.getInstance() : Calendar.getInstance(tabZone);
                if (fixedNow != null) {
                    seed.setTimeInMillis(fixedNow.toEpochMilli());
                }
                for (int i = 0; i < OCCURRENCES_PER_ROW; i++) {
                    Calendar candidate = (Calendar) seed.clone();
                    Calendar result = tab.ceil(candidate); // mutates and returns the same object
                    candidates.add(result.toInstant());
                    seed = (Calendar) result.clone();
                    seed.add(Calendar.MINUTE, 1);
                }
            } catch (RuntimeException e) {
                // A line that reaches no date in two years throws here. Keep whatever
                // occurrences this row already holds and move to the next line.
                LOGGER.log(Level.FINE, "ActivityEndpoint: cron line failed for job " + jobFullName, e);
            }
        }

        candidates.sort(Comparator.naturalOrder());
        List<String> result = new ArrayList<>();
        for (int i = 0; i < candidates.size() && i < OCCURRENCES_PER_ROW; i++) {
            result.add(DateTimeFormatter.ISO_INSTANT.format(candidates.get(i)));
        }
        return result;
    }

    /**
     * Applies the row bound and the byte bound to a candidate row list, keeping every row
     * counted regardless of whether it was kept. Package-private for testing.
     */
    static JSONObject applyScheduleBounds(List<JSONObject> candidateRows) {
        JSONArray kept = new JSONArray();
        long bytes = 0;
        int total = 0;
        boolean truncated = false;

        for (JSONObject row : candidateRows) {
            total++;
            if (kept.size() < MAX_SCHEDULE_ROWS && bytes < MAX_SCHEDULE_BYTES) {
                byte[] encoded = row.toString().getBytes(StandardCharsets.UTF_8);
                kept.add(row);
                bytes += encoded.length;
            } else {
                truncated = true;
            }
        }

        JSONObject envelope = new JSONObject();
        envelope.put("schedules", kept);
        envelope.put("total", total);
        envelope.put("truncated", truncated);
        return envelope;
    }

    /**
     * Builds one schedule row as JSON, field by field, so field names stay pinned to the
     * schema. Carries no upstream fields.
     */
    private static JSONObject buildScheduleRow(String jobPath, String triggerType, String spec,
            String timezone, boolean disabled, List<String> nextFireTimes) {
        return buildScheduleRow(jobPath, triggerType, spec, timezone, disabled, nextFireTimes,
                null, null, null);
    }

    /**
     * Builds one schedule row as JSON, field by field, so field names stay pinned to the
     * schema. Two trigger types carry upstream values, and each carries its own set. A
     * {@code varroa-upstream} row carries the tenant, the job, and the threshold. An
     * {@code upstream} row names several jobs and no tenant, so it carries the threshold
     * with an empty tenant and an empty job, and its paths ride the specification field.
     * Every other row omits all three keys.
     */
    private static JSONObject buildScheduleRow(String jobPath, String triggerType, String spec,
            String timezone, boolean disabled, List<String> nextFireTimes,
            String upstreamTenant, String upstreamJob, String upstreamResult) {
        JSONObject row = new JSONObject();
        row.put("jobPath", jobPath);
        row.put("triggerType", triggerType);
        row.put("spec", spec);
        row.put("timezone", timezone);
        row.put("disabled", disabled);
        JSONArray occurrences = new JSONArray();
        occurrences.addAll(nextFireTimes);
        row.put("nextFireTimes", occurrences);
        if (upstreamTenant != null) {
            row.put("upstreamTenant", upstreamTenant);
            row.put("upstreamJob", upstreamJob);
            row.put("upstreamResult", upstreamResult);
        } else if (upstreamResult != null) {
            row.put("upstreamTenant", "");
            row.put("upstreamJob", "");
            row.put("upstreamResult", upstreamResult);
        }
        return row;
    }

    /**
     * Reads a job's trigger map by reflection, because {@code Job.getTriggers()} was removed
     * in Jenkins 2.555.x. Reuses the pattern {@link #hasTriggerByReflection} applies to one
     * trigger name, generalized to every configured trigger.
     */
    @SuppressWarnings("unchecked")
    private static List<Trigger<?>> getTriggersByReflection(Job<?, ?> job) {
        List<Trigger<?>> result = new ArrayList<>();
        try {
            java.lang.reflect.Method m = job.getClass().getMethod("getTriggers");
            Object triggers = m.invoke(job);
            if (triggers instanceof Map) {
                for (Object value : ((Map<Object, Object>) triggers).values()) {
                    if (value instanceof Trigger) {
                        result.add((Trigger<?>) value);
                    }
                }
            }
        } catch (NoSuchMethodException e) {
            LOGGER.log(Level.FINE, "ActivityEndpoint: getTriggers() not available on Job", e);
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "ActivityEndpoint: reflection error reading job triggers", e);
        }
        return result;
    }

    /**
     * Reads a job's disabled state by reflection, since {@code isDisabled()} is not declared
     * on the {@code Job} base class.
     */
    private static boolean isDisabledByReflection(Job<?, ?> job) {
        try {
            java.lang.reflect.Method m = job.getClass().getMethod("isDisabled");
            Object result = m.invoke(job);
            return result instanceof Boolean && (Boolean) result;
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "ActivityEndpoint: reflection error reading job disabled state", e);
            return false;
        }
    }

    // ---- Idle gauge construction (D1) ----

    /**
     * Builds the {@code idle} JSON object with hibernation gauges.
     *
     * <p>The object shape:
     * <pre>
     * {
     *   "lastHttpActivityUnix": L,
     *   "lastEventUnix": E,
     *   "runningBuilds": R,
     *   "queueLength": Q,
     *   "timerTriggerJobs": T
     * }
     * </pre>
     */
    static JSONObject buildIdleObject() {
        JSONObject idle = new JSONObject();

        // Keys are snake_case to match the mite's mitev1.IdleGauges JSON tags,
        // which unmarshals this block directly (a camelCase mismatch would silently
        // zero every gauge — false idleness, e.g. hibernating a busy controller).

        // last_http_activity_unix: from the filter (epoch seconds).
        idle.put("last_http_activity_unix", HttpActivityFilter.getLastHttpActivityUnixMillis() / 1000L);

        // last_event_unix: from the sink (epoch seconds, survives drains).
        idle.put("last_event_unix", ActivitySink.get().getLastEventUnix());

        // running_builds: busy executors including one-off/flyweight.
        idle.put("running_builds", countRunningBuilds());

        // queue_length: pending queue items.
        idle.put("queue_length", countQueueLength());

        // timer_trigger_jobs: cached count of TimerTrigger jobs.
        idle.put("timer_trigger_jobs", getTimerTriggerJobs());

        return idle;
    }

    /**
     * Returns the number of busy executors, including one-off/flyweight
     * executors (covers Pipeline builds parked between node blocks).
     */
    static int countRunningBuilds() {
        int busy = 0;
        try {
            Jenkins jenkins = Jenkins.getInstanceOrNull();
            if (jenkins == null) {
                return 0;
            }
            for (Computer c : jenkins.getComputers()) {
                for (Executor e : c.getExecutors()) {
                    if (e.isBusy()) {
                        busy++;
                    }
                }
                for (Executor e : c.getOneOffExecutors()) {
                    if (e.isBusy()) {
                        busy++;
                    }
                }
            }
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "ActivityEndpoint: error counting running builds", e);
        }
        return busy;
    }

    /**
     * Returns the length of the Jenkins build queue.
     */
    static int countQueueLength() {
        try {
            Queue.Item[] items = Queue.getInstance().getItems();
            return items != null ? items.length : 0;
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "ActivityEndpoint: error reading queue length", e);
            return 0;
        }
    }

    /**
     * Returns the cached count of jobs whose trigger map includes a
     * {@code TimerTrigger}. The count is lazily recomputed at most every
     * 5 minutes, because walking the job graph is not free.
     */
    static int getTimerTriggerJobs() {
        long now = System.currentTimeMillis();
        if (now - timerCacheLastRefreshedMs > TIMER_CACHE_TTL_MS) {
            refreshTimerTriggerCache();
        }
        return cachedTimerTriggerJobs.get();
    }

    /**
     * Walks all jobs and counts those whose trigger map contains a
     * {@code TimerTrigger}. Visible for testing — call
     * {@link #getTimerTriggerJobs()} in production.
     */
    static int refreshTimerTriggerCache() {
        int count = 0;
        try {
            Jenkins jenkins = Jenkins.getInstanceOrNull();
            if (jenkins == null) {
                return 0;
            }
            // getAllItems(Job.class) returns all jobs recursively.
            for (Job<?, ?> job : jenkins.getAllItems(Job.class)) {
                if (hasTimerTrigger(job)) {
                    count++;
                }
            }
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "ActivityEndpoint: error refreshing timer trigger cache", e);
        }
        cachedTimerTriggerJobs.set(count);
        timerCacheLastRefreshedMs = System.currentTimeMillis();
        return count;
    }

    /**
     * Returns true if the job's trigger list includes a TimerTrigger.
     */
    private static boolean hasTimerTrigger(Job<?, ?> job) {
        try {
            // Try the modern Jenkins API: Trigger.forJob(Item) pattern
            // Iterate over Trigger.all() descriptors to find TimerTrigger
            for (TriggerDescriptor td : Trigger.all()) {
                if (td.getClass().getSimpleName().equals("TimerTriggerDescriptor")
                        || td.getClass().getSimpleName().equals("TimerTrigger")) {
                    // Check if this trigger is configured on this job by looking
                    // at the job's trigger map. Use reflection as a fallback
                    // since Job.getTriggers() was removed in Jenkins 2.555.x.
                    return hasTriggerByReflection(job, "TimerTrigger");
                }
            }
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "ActivityEndpoint: error checking job triggers", e);
        }
        return false;
    }

    /**
     * Uses reflection to check if a job has a trigger with the given simple class name.
     * This works across Jenkins versions even when the getTriggers() method signature changes.
     */
    @SuppressWarnings("unchecked")
    private static boolean hasTriggerByReflection(Job<?, ?> job, String triggerSimpleName) {
        try {
            java.lang.reflect.Method m = job.getClass().getMethod("getTriggers");
            Object result = m.invoke(job);
            if (result instanceof Map) {
                Map<Object, Object> triggers = (Map<Object, Object>) result;
                for (Object trigger : triggers.values()) {
                    if (trigger != null && trigger.getClass().getSimpleName().equals(triggerSimpleName)) {
                        return true;
                    }
                }
            }
        } catch (NoSuchMethodException e) {
            LOGGER.log(Level.FINE, "ActivityEndpoint: getTriggers() not available on Job", e);
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "ActivityEndpoint: reflection error checking job triggers", e);
        }
        return false;
    }

    /**
     * Resets the timer-trigger cache and forces a refresh on next read.
     * Package-private for testing.
     */
    static void resetTimerTriggerCache() {
        timerCacheLastRefreshedMs = 0;
        cachedTimerTriggerJobs.set(0);
    }

    // ---- Auth ----

    private boolean isMitePrincipal() {
        for (GrantedAuthority auth : Jenkins.getAuthentication2().getAuthorities()) {
            if ("ROLE:varroa:system-mite".equals(auth.getAuthority())) {
                return true;
            }
        }
        return false;
    }

    // ---- Batch size parsing ----

    private int readMaxBatch(String raw) {
        if (raw == null || raw.isEmpty()) {
            return defaultMaxBatch();
        }
        try {
            int parsed = Integer.parseInt(raw);
            return Math.max(MIN_BATCH, Math.min(MAX_BATCH_CAP, parsed));
        } catch (NumberFormatException e) {
            return defaultMaxBatch();
        }
    }

    private int defaultMaxBatch() {
        String raw = System.getenv("VARROA_ACTIVITY_MAX_BATCH");
        if (raw == null || raw.isEmpty()) {
            return DEFAULT_MAX_BATCH;
        }
        try {
            return Math.max(MIN_BATCH, Math.min(MAX_BATCH_CAP, Integer.parseInt(raw)));
        } catch (NumberFormatException e) {
            return DEFAULT_MAX_BATCH;
        }
    }

    // ---- JSON array builder ----

    /**
     * Builds a JSON array of event objects, each constructed field-by-field
     * (not via bean reflection) so field names are pinned to the schema.
     */
    private static JSONArray buildEventsArray(List<ActivityEvent> events) {
        JSONArray arr = new JSONArray();
        for (ActivityEvent e : events) {
            JSONObject obj = new JSONObject();
            obj.put("timestamp", e.getTimestamp());
            obj.put("type", e.getType());
            obj.put("source", e.getSource());
            obj.put("actor", e.getActor());
            obj.put("itemPath", e.getItemPath());
            obj.put("buildNumber", e.getBuildNumber());
            obj.put("result", e.getResult());
            obj.put("url", e.getUrl());
            obj.put("message", e.getMessage());
            obj.put("controller", e.getController());
            obj.put("namespace", e.getNamespace());
            obj.put("phase", e.getPhase());
            obj.put("reason", e.getReason());
            arr.add(obj);
        }
        return arr;
    }
}
