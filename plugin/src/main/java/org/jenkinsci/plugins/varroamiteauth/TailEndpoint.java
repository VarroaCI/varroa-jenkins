package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.RootAction;

import java.io.BufferedReader;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.logging.Level;
import java.util.logging.Logger;
import java.util.stream.Collectors;

import jenkins.model.Jenkins;
import net.sf.json.JSONObject;

import org.kohsuke.stapler.StaplerRequest2;
import org.kohsuke.stapler.StaplerResponse2;
import org.springframework.security.core.GrantedAuthority;

/**
 * Protected {@link RootAction} at {@code /varroa-log/tail}, the mite's local
 * call when the gateway pushes {@code TailControl} (D1, log-live-tail).
 *
 * <p>Requires the authority {@code ROLE:varroa:system-mite}, like {@link
 * ActivityEndpoint}. On {@code start} it switches the named build to the
 * fast flush interval and records the plugin's own lease expiry. It returns
 * the build to the slow interval once that expiry passes with no renewal,
 * so a disconnected mite never strands a build in the fast interval.
 *
 * <p>The request body is {@code {"runId", "jobPath", "buildNumber",
 * "action", "leaseSeconds"}} in camelCase, the shape the shipped mite
 * sends ({@code cmd/mite/loopback.go}'s {@code tailControlRequest}).
 * This endpoint reads {@code runId}, {@code action} ({@code start} or
 * {@code stop}), and {@code leaseSeconds}; it needs neither {@code
 * jobPath} nor {@code buildNumber}.
 *
 * <p>{@link #doCounters} serves the same root action's second route,
 * {@code GET /varroa-log/counters}: one JSON document of the plugin's
 * operational counters (see {@link PluginCounters}), gated by the same
 * authority.
 */
@Extension
public class TailEndpoint implements RootAction {

    private static final Logger LOGGER = Logger.getLogger(TailEndpoint.class.getName());

    static final String ACTION_START = "start";
    static final String ACTION_STOP = "stop";

    private final ConcurrentHashMap<String, Long> leaseExpiryMillis = new ConcurrentHashMap<>();

    private static volatile ScheduledExecutorService ticker;

    @Override
    public String getUrlName() {
        return "varroa-log";
    }

    @Override
    public String getIconFileName() {
        return null; // no UI surface
    }

    @Override
    public String getDisplayName() {
        return null; // no UI surface
    }

    /** Handles {@code POST /varroa-log/tail}. */
    public void doTail(StaplerRequest2 req, StaplerResponse2 rsp) {
        try {
            if (!"POST".equalsIgnoreCase(req.getMethod())) {
                rsp.sendError(405, "Method Not Allowed: only POST is served");
                return;
            }
            if (!isMitePrincipal()) {
                rsp.sendError(403, "Forbidden: requires ROLE:varroa:system-mite authority");
                return;
            }
            JSONObject body = readBody(req);
            String runId = body.optString("runId", "");
            String action = body.optString("action", "");
            long leaseSeconds = body.optLong("leaseSeconds", 0L);
            if (runId.isEmpty() || (!ACTION_START.equals(action) && !ACTION_STOP.equals(action))) {
                rsp.sendError(400, "Bad Request: runId and a start/stop action are required");
                return;
            }
            ensureTicker();
            VarroaLogStorageFactory factory = VarroaLogStorageFactory.get();
            boolean applied = factory != null && factory.setTailActive(runId, ACTION_START.equals(action));
            if (ACTION_START.equals(action)) {
                leaseExpiryMillis.put(runId, System.currentTimeMillis() + Math.max(0L, leaseSeconds) * 1000L);
            } else {
                leaseExpiryMillis.remove(runId);
            }

            JSONObject out = new JSONObject();
            out.put("ok", applied);
            rsp.setContentType("application/json");
            rsp.setStatus(200);
            rsp.getWriter().write(out.toString());
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "TailEndpoint.doTail caught exception", e);
            try {
                rsp.sendError(500, "Internal server error");
            } catch (Exception ignored) {
                // Best-effort error response.
            }
        }
    }

    /**
     * Handles {@code GET /varroa-log/counters}: one JSON document holding
     * every plugin operational counter, one integer per stable
     * snake_case name (see {@link PluginCounters}). Requires the same
     * authority as {@link #doTail}.
     */
    public void doCounters(StaplerRequest2 req, StaplerResponse2 rsp) {
        try {
            if (!"GET".equalsIgnoreCase(req.getMethod())) {
                rsp.sendError(405, "Method Not Allowed: only GET is served");
                return;
            }
            if (!isMitePrincipal()) {
                rsp.sendError(403, "Forbidden: requires ROLE:varroa:system-mite authority");
                return;
            }
            JSONObject out = new JSONObject();
            for (java.util.Map.Entry<String, Long> counter : PluginCounters.snapshot().entrySet()) {
                out.put(counter.getKey(), counter.getValue());
            }
            rsp.setContentType("application/json");
            rsp.setStatus(200);
            rsp.getWriter().write(out.toString());
        } catch (Exception e) {
            LOGGER.log(Level.WARNING, "TailEndpoint.doCounters caught exception", e);
            try {
                rsp.sendError(500, "Internal server error");
            } catch (Exception ignored) {
                // Best-effort error response.
            }
        }
    }

    private JSONObject readBody(StaplerRequest2 req) throws Exception {
        try (BufferedReader reader = req.getReader()) {
            String text = reader.lines().collect(Collectors.joining("\n"));
            if (text.isEmpty()) {
                return new JSONObject();
            }
            return JSONObject.fromObject(text);
        }
    }

    private boolean isMitePrincipal() {
        for (GrantedAuthority auth : Jenkins.getAuthentication2().getAuthorities()) {
            if ("ROLE:varroa:system-mite".equals(auth.getAuthority())) {
                return true;
            }
        }
        return false;
    }

    /** Forces the lease-expiry sweep without waiting on wall-clock time. Package-private for testing. */
    void tickForTesting(long nowMillis) {
        tick(nowMillis);
    }

    private void tick(long nowMillis) {
        for (java.util.Map.Entry<String, Long> e : leaseExpiryMillis.entrySet()) {
            if (nowMillis >= e.getValue()) {
                String runId = e.getKey();
                leaseExpiryMillis.remove(runId, e.getValue());
                VarroaLogStorageFactory factory = VarroaLogStorageFactory.get();
                if (factory != null) {
                    factory.setTailActive(runId, false);
                }
            }
        }
    }

    /** {@code true} while {@code runId} carries an unexpired lease. Package-private for testing. */
    boolean hasLeaseForTesting(String runId) {
        return leaseExpiryMillis.containsKey(runId);
    }

    private static void ensureTicker() {
        if (ticker != null) {
            return;
        }
        synchronized (TailEndpoint.class) {
            if (ticker != null) {
                return;
            }
            ScheduledExecutorService t = Executors.newSingleThreadScheduledExecutor(r -> {
                Thread th = new Thread(r, "varroa-log-tail-lease-ticker");
                th.setDaemon(true);
                return th;
            });
            t.scheduleWithFixedDelay(() -> {
                try {
                    hudson.ExtensionList.lookupSingleton(TailEndpoint.class).tick(System.currentTimeMillis());
                } catch (Exception e) {
                    LOGGER.log(Level.FINE, "TailEndpoint: lease sweep failed", e);
                }
            }, 1, 1, TimeUnit.SECONDS);
            ticker = t;
        }
    }
}
