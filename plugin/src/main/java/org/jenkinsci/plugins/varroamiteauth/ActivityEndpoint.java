package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Computer;
import hudson.model.Executor;
import hudson.model.Job;
import hudson.model.Queue;
import hudson.model.RootAction;

import java.io.StringWriter;
import hudson.triggers.Trigger;
import hudson.triggers.TriggerDescriptor;

import java.util.List;
import java.util.Map;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.logging.Level;
import java.util.logging.Logger;

import jenkins.model.Jenkins;
import net.sf.json.JSONArray;
import net.sf.json.JSONObject;

import org.kohsuke.stapler.StaplerRequest2;
import org.kohsuke.stapler.StaplerResponse2;

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
 */
@Extension
public class ActivityEndpoint implements RootAction {

    private static final Logger LOGGER = Logger.getLogger(ActivityEndpoint.class.getName());

    private static final int DEFAULT_MAX_BATCH = 256;
    private static final int MAX_BATCH_CAP = 1024;
    private static final int MIN_BATCH = 1;

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
                rsp.sendError(405, "Method Not Allowed: only GET is served");
                return; // Coordinator §9.1: return after sendError
            }

            // AuthZ: require ROLE:varroa:system-mite authority.
            if (!isMitePrincipal()) {
                rsp.sendError(403, "Forbidden: requires ROLE:varroa:system-mite authority");
                return; // Coordinator §9.1: return after sendError
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
                rsp.sendError(500, "Internal server error");
            } catch (Exception ignored) {
                // Best-effort error response.
            }
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
