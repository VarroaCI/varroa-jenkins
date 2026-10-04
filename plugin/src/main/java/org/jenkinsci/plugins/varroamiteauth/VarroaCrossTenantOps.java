package org.jenkinsci.plugins.varroamiteauth;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Shared cross-tenant network calls and reply-to-handle-map translation
 * (D1, D2, D4, D12). The four steps call this, and so does {@code
 * VarroaRunHandle.groovy}'s {@code refresh()}, which reads a record
 * directly: D1 lists no public status step, because a plain re-read needs
 * no workspace or channel access.
 */
final class VarroaCrossTenantOps {

    /** Handle state before a {@code run_intents} row exists (D8). */
    static final String STATE_PENDING = "pending";

    /** Overrides {@link #client()} in a test; {@code null} in production. */
    private static volatile MiteLoopbackClient testClient;

    private VarroaCrossTenantOps() {
    }

    static MiteLoopbackClient client() {
        MiteLoopbackClient override = testClient;
        return override != null ? override : MiteLoopbackClient.forCrossTenant();
    }

    /** Package-private test seam: points every cross-tenant call at a test double or stub server. */
    static void setClientForTesting(MiteLoopbackClient client) {
        testClient = client;
    }

    /**
     * Builds a run-handle map (D1's handle properties) from a {@code
     * build} or {@code status} reply. Fetches the artifact list once more
     * when the state is {@code completed}, because a run only carries
     * artifacts once it has finished. {@code callerJobPath} names the
     * caller of that fetch only: the map never carries it, because a
     * script can build a handle from any map.
     */
    static Map<String, Object> handleMap(VarroaCrossTenantReply reply, String tenant, String job,
            String callerJobPath, String intentKey) {
        return handleMap(reply, tenant, job, callerJobPath, intentKey, null);
    }

    /**
     * As {@link #handleMap(VarroaCrossTenantReply, String, String, String, String)},
     * with {@code stateOverride} standing in for a reply that carries no
     * {@code state} field of its own, such as {@code last_successful}'s
     * reply, which is always a completed run.
     */
    static Map<String, Object> handleMap(VarroaCrossTenantReply reply, String tenant, String job,
            String callerJobPath, String intentKey, String stateOverride) {
        Map<String, Object> m = new LinkedHashMap<>();
        String runId = reply.getString("run_id", "");
        String state = stateOverride != null ? stateOverride
                : reply.getString("state", runId.isEmpty() ? STATE_PENDING : "queued");
        m.put("runId", runId);
        m.put("tenant", tenant);
        m.put("job", job);
        long buildNumber = reply.getLong("build_number", 0L);
        m.put("buildNumber", buildNumber > 0 ? (int) buildNumber : null);
        m.put("state", state);
        String result = reply.getString("result", "");
        m.put("result", result.isEmpty() ? null : result);
        String startedAt = reply.getString("startedAt", reply.getString("started_at", ""));
        m.put("startedAt", startedAt.isEmpty() ? null : startedAt);
        String endedAt = reply.getString("endedAt", reply.getString("ended_at", ""));
        m.put("endedAt", endedAt.isEmpty() ? null : endedAt);
        m.put("url", reply.getString("url", ""));
        m.put("artifacts", "completed".equals(state) ? artifactList(callerJobPath, runId) : List.of());
        m.put("intentKey", intentKey != null ? intentKey : "");
        return m;
    }

    /**
     * Kind {@code artifact_list}: the run's artifacts, or an empty list on
     * any failure. Every entry is copied into a plain {@link
     * LinkedHashMap}: {@link groovy.json.JsonSlurper}'s own map and list
     * types are not {@link java.io.Serializable}, and this list ends up on
     * a CPS-persisted handle.
     */
    @SuppressWarnings("unchecked")
    static List<Map<String, Object>> artifactList(String callerJobPath, String runId) {
        if (runId == null || runId.isEmpty()) {
            return List.of();
        }
        VarroaCrossTenantReply reply = VarroaCrossTenantReply.parse(
                client().crossTenantArtifactList(callerJobPath, runId));
        if (reply.isFailure()) {
            return List.of();
        }
        Object raw = reply.fields.get("artifacts");
        List<Map<String, Object>> out = new ArrayList<>();
        if (raw instanceof List) {
            for (Object entry : (List<?>) raw) {
                if (entry instanceof Map) {
                    out.add(new LinkedHashMap<>((Map<String, Object>) entry));
                }
            }
        }
        return out;
    }

    /**
     * {@code refresh()}: re-reads the record by run id when one is known,
     * otherwise by intent key. Throws when the recorder does not answer.
     */
    static Map<String, Object> refresh(String callerJobPath, String tenant, String job,
            String runId, String intentKey) {
        MiteLoopbackClient.Response resp = (runId != null && !runId.isEmpty())
                ? client().crossTenantStatus(callerJobPath, runId)
                : client().crossTenantStatusByIntentKey(callerJobPath, intentKey);
        VarroaCrossTenantReply reply = VarroaCrossTenantReply.parse(resp);
        if (reply.isFailure()) {
            throw new IllegalStateException("varroa.refresh: " + reply.describe());
        }
        if (!reply.reasonCode().isEmpty()) {
            throw new IllegalStateException("varroa.refresh: " + reply.describeVeto());
        }
        return handleMap(reply, tenant, job, callerJobPath, intentKey);
    }
}
