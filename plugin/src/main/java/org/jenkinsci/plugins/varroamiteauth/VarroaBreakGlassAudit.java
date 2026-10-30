package org.jenkinsci.plugins.varroamiteauth;

import java.util.HashMap;
import java.util.Map;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Emits break-glass audit events (D8) when {@link VarroaHeadlessFilter}'s
 * admin test passes.
 *
 * <p>A {@code controller.breakglass.session} event fires at most once for
 * each principal in a 30 minute window. A {@code controller.breakglass.script}
 * event fires, in addition, at most once for each principal and path in a
 * 60 second window, when the path is sensitive. Each dedupe map is capped
 * and clears itself rather than growing past the cap, so a cleared map
 * costs at most one repeated event and never a missing one.
 */
final class VarroaBreakGlassAudit {

    private static final Logger LOGGER = Logger.getLogger(VarroaBreakGlassAudit.class.getName());

    static final String SESSION_TYPE = "controller.breakglass.session";
    static final String SCRIPT_TYPE = "controller.breakglass.script";

    static final long SESSION_WINDOW_MILLIS = 30L * 60 * 1000;
    static final long SCRIPT_WINDOW_MILLIS = 60L * 1000;

    static final int SESSION_MAP_CAP = 64;
    static final int SCRIPT_MAP_CAP = 256;

    private static final String SCRIPT_KEY_SEPARATOR = "\u0000";

    private static final Map<String, Long> SESSION_LAST_SEEN = new HashMap<>();
    private static final Map<String, Long> SCRIPT_LAST_SEEN = new HashMap<>();

    private static final String[] SENSITIVE_PREFIXES = {
        "/script", "/manage", "/configureSecurity", "/pluginManager",
    };

    private VarroaBreakGlassAudit() {
    }

    /** Records an admin break-glass request against the real clock. */
    static void record(String actor, String path) {
        record(actor, path, System.currentTimeMillis());
    }

    /** Package-private overload with an injectable clock, for testing. */
    static void record(String actor, String path, long nowMillis) {
        String safeActor = actor == null ? "" : actor;
        String safePath = path == null ? "" : path;

        if (shouldEmit(SESSION_LAST_SEEN, SESSION_MAP_CAP, safeActor, SESSION_WINDOW_MILLIS, nowMillis)) {
            emit(SESSION_TYPE, safeActor, safePath,
                    "Break-glass session: " + safeActor + " reached the Jenkins UI on a headless controller");
        }

        if (isSensitivePath(safePath)) {
            String key = safeActor + SCRIPT_KEY_SEPARATOR + safePath;
            if (shouldEmit(SCRIPT_LAST_SEEN, SCRIPT_MAP_CAP, key, SCRIPT_WINDOW_MILLIS, nowMillis)) {
                emit(SCRIPT_TYPE, safeActor, safePath,
                        "Break-glass script access: " + safeActor + " requested " + safePath);
            }
        }
    }

    /** The sensitive-path set the {@code break-glass-access} delta states exactly. */
    static boolean isSensitivePath(String path) {
        if (path == null) {
            return false;
        }
        if ("/scriptText".equals(path)) {
            return true;
        }
        if (path.startsWith("/computer/") && path.endsWith("/script")) {
            return true;
        }
        for (String prefix : SENSITIVE_PREFIXES) {
            if (path.startsWith(prefix)) {
                return true;
            }
        }
        return false;
    }

    private static void emit(String type, String actor, String path, String message) {
        try {
            ActivityEvent event = ActivityEvent.builder()
                    .type(type)
                    .actor(actor)
                    .url(path)
                    .message(message)
                    .build();
            ActivitySink.get().offer(event);
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaBreakGlassAudit: failed to offer event", e);
        }
    }

    /**
     * Returns true when {@code key} should emit now: it is new, or its last
     * emission is outside the window. The cap check, the clear, and the
     * read-decide-write for one key all run under the map's monitor, so
     * two concurrent callers for the same key never both emit, and a clear
     * at the cap costs at most one repeated event, never a missing one.
     * Only an admin request on a headless controller reaches this path,
     * so the monitor sees no contention that matters.
     */
    private static boolean shouldEmit(
            Map<String, Long> map, int cap, String key, long windowMillis, long now) {
        synchronized (map) {
            Long last = map.get(key);
            if (last != null && now - last < windowMillis) {
                return false;
            }
            if (last == null && map.size() >= cap) {
                map.clear();
            }
            map.put(key, now);
            return true;
        }
    }

    // ---- Testing hooks ----

    static void resetForTesting() {
        synchronized (SESSION_LAST_SEEN) {
            SESSION_LAST_SEEN.clear();
        }
        synchronized (SCRIPT_LAST_SEEN) {
            SCRIPT_LAST_SEEN.clear();
        }
    }

    static int sessionMapSizeForTesting() {
        synchronized (SESSION_LAST_SEEN) {
            return SESSION_LAST_SEEN.size();
        }
    }

    static int scriptMapSizeForTesting() {
        synchronized (SCRIPT_LAST_SEEN) {
            return SCRIPT_LAST_SEEN.size();
        }
    }
}
