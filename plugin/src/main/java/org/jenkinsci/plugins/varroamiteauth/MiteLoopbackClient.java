package org.jenkinsci.plugins.varroamiteauth;

import net.sf.json.JSONObject;

import java.io.IOException;
import java.net.URI;
import java.net.URLEncoder;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.net.http.HttpTimeoutException;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.Map;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * HTTP client for the mite loopback (D6, D7): {@code POST /v1/claim} and
 * {@code POST /v1/events} against {@code http://127.0.0.1:9095}. D2 adds
 * four cross-tenant routes, and {@code GET /v1/homes} reads the tenant
 * home map. Every route reuses the same token file and the same
 * {@code 401} retry.
 *
 * <p>The bearer token comes from {@link LoopbackTokenFile}. A {@code 401}
 * triggers one retry with a freshly re-read token.
 */
public class MiteLoopbackClient {

    private static final Logger LOGGER = Logger.getLogger(MiteLoopbackClient.class.getName());

    static final String DEFAULT_BASE_URL = "http://127.0.0.1:9095";

    /** Connect budget (D6): 1 s, fixed for the claim and events routes. */
    private static final Duration CONNECT_TIMEOUT = Duration.ofSeconds(1);

    /** Read budget (D6): 5 s by default, overridden by {@code VARROA_CLAIM_TIMEOUT_MS}. */
    private static final long DEFAULT_READ_TIMEOUT_MILLIS = 5000L;

    /** The most body bytes a run-state answer keeps for its log line. */
    static final int BODY_PREFIX_BYTES = 200;

    /** Cross-tenant connect budget (D14): 1 s by default. */
    private static final long DEFAULT_CROSSTENANT_CONNECT_TIMEOUT_MILLIS = 1000L;

    /** Cross-tenant read budget (D14): 10 s by default. */
    private static final long DEFAULT_CROSSTENANT_READ_TIMEOUT_MILLIS = 10_000L;

    /**
     * The mite's own wait-for-response budget (D14), 9 s by default. The
     * plugin cannot read the mite process's own environment, so the
     * nesting check below compares against this documented default, never
     * a live value.
     */
    private static final long CROSSTENANT_MITE_HOP_DEFAULT_MILLIS = 9000L;

    static final String HOMES_PATH = "/v1/homes";

    private static final String CROSSTENANT_BUILD_PATH = "/v1/cross-tenant/build";
    private static final String CROSSTENANT_STATUS_PATH = "/v1/cross-tenant/status";
    private static final String CROSSTENANT_ARTIFACT_PATH = "/v1/cross-tenant/artifact";
    private static final String CROSSTENANT_LOCK_PATH = "/v1/cross-tenant/lock";

    private final String baseUrl;
    private final LoopbackTokenFile tokenFile;
    private final Duration connectTimeout;
    private final Duration readTimeout;
    private volatile HttpClient httpClient;

    public MiteLoopbackClient() {
        this(DEFAULT_BASE_URL, new LoopbackTokenFile(), CONNECT_TIMEOUT,
                Duration.ofMillis(readTimeoutMillisFromEnv()));
    }

    /** Package-private constructor for testing: injects the base URL, token source, and read timeout. */
    MiteLoopbackClient(String baseUrl, LoopbackTokenFile tokenFile, Duration readTimeout) {
        this(baseUrl, tokenFile, CONNECT_TIMEOUT, readTimeout);
    }

    /** Package-private constructor for testing: injects every timeout as well. */
    MiteLoopbackClient(String baseUrl, LoopbackTokenFile tokenFile, Duration connectTimeout, Duration readTimeout) {
        this.baseUrl = baseUrl;
        this.tokenFile = tokenFile;
        this.connectTimeout = connectTimeout;
        this.readTimeout = readTimeout;
    }

    /**
     * Builds a client configured for the cross-tenant routes (D2, D14):
     * {@code VARROA_CROSSTENANT_CONNECT_TIMEOUT_MS} (default 1000) and
     * {@code VARROA_CROSSTENANT_READ_TIMEOUT_MS} (default 10000), both
     * whole-number milliseconds.
     */
    public static MiteLoopbackClient forCrossTenant() {
        return new MiteLoopbackClient(DEFAULT_BASE_URL, new LoopbackTokenFile(),
                Duration.ofMillis(crossTenantConnectTimeoutMillis(
                        System.getenv("VARROA_CROSSTENANT_CONNECT_TIMEOUT_MS"))),
                Duration.ofMillis(crossTenantReadTimeoutMillis(
                        System.getenv("VARROA_CROSSTENANT_READ_TIMEOUT_MS"))));
    }

    private HttpClient client() {
        HttpClient c = httpClient;
        if (c == null) {
            synchronized (this) {
                c = httpClient;
                if (c == null) {
                    c = HttpClient.newBuilder().connectTimeout(connectTimeout).build();
                    httpClient = c;
                }
            }
        }
        return c;
    }

    /**
     * Posts {@code jsonBody} to {@code path} on the loopback. Retries once,
     * with a re-read token, when the first attempt answers {@code 401}.
     */
    public Response post(String path, String jsonBody) {
        Response first = attempt(path, jsonBody, tokenFile.token());
        if (first.statusCode == 401) {
            String refreshed = tokenFile.onUnauthorized();
            return attempt(path, jsonBody, refreshed);
        }
        return first;
    }

    /**
     * Reads {@code path} on the loopback. Retries once, with a re-read
     * token, when the first attempt answers {@code 401}.
     */
    public Response get(String path) {
        Response first = attemptGet(path, tokenFile.token());
        if (first.statusCode == 401) {
            return attemptGet(path, tokenFile.onUnauthorized());
        }
        return first;
    }

    /**
     * Route {@code GET /v1/homes}: the tenant home map of the newest
     * desired state the mite applied.
     */
    public Response homes() {
        return get(HOMES_PATH);
    }

    private Response attemptGet(String path, String token) {
        try {
            HttpRequest.Builder builder = HttpRequest.newBuilder()
                    .uri(URI.create(baseUrl + path))
                    .timeout(readTimeout)
                    .GET();
            if (token != null) {
                builder.header("Authorization", "Bearer " + token);
            }
            HttpResponse<String> resp = client().send(builder.build(), HttpResponse.BodyHandlers.ofString());
            return new Response(resp.statusCode(), resp.body(), null);
        } catch (HttpTimeoutException e) {
            LOGGER.log(Level.FINE, "MiteLoopbackClient: timeout reading " + path, e);
            return new Response(0, null, "timeout");
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "MiteLoopbackClient: failed to read " + path, e);
            return new Response(0, null, e.getMessage() != null ? e.getMessage() : "transport error");
        }
    }

    private Response attempt(String path, String jsonBody, String token) {
        try {
            HttpRequest.Builder builder = HttpRequest.newBuilder()
                    .uri(URI.create(baseUrl + path))
                    .timeout(readTimeout)
                    .header("Content-Type", "application/json")
                    .POST(HttpRequest.BodyPublishers.ofString(jsonBody, StandardCharsets.UTF_8));
            if (token != null) {
                builder.header("Authorization", "Bearer " + token);
            }
            HttpResponse<String> resp = client().send(builder.build(), HttpResponse.BodyHandlers.ofString());
            return new Response(resp.statusCode(), resp.body(), null);
        } catch (HttpTimeoutException e) {
            LOGGER.log(Level.FINE, "MiteLoopbackClient: timeout posting to " + path, e);
            return new Response(0, null, "timeout");
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "MiteLoopbackClient: failed to post to " + path, e);
            return new Response(0, null, e.getMessage() != null ? e.getMessage() : "transport error");
        }
    }

    // ---- Run-state route: headers and a raw gzip body, never JSON ----

    /**
     * Posts one run-state chunk to {@code url}. The metadata rides nine
     * {@code X-Varroa-*} headers and the body is the raw gzip of that
     * chunk, because base64 inside a JSON body would add a third to
     * every byte of a multi-megabyte file.
     *
     * <p>A {@code 503} the mite raises from a gateway refusal carries
     * {@code X-Varroa-State-Reason}, and a busy answer also carries
     * {@code X-Varroa-Pause-Ms}.
     */
    public RunStateAnswer postRunState(String url, JSONObject record, byte[] gzipBody) {
        RunStateAnswer first = attemptRunState(url, record, gzipBody, tokenFile.token());
        if (first.statusCode == 401) {
            return attemptRunState(url, record, gzipBody, tokenFile.onUnauthorized());
        }
        return first;
    }

    private RunStateAnswer attemptRunState(String url, JSONObject record, byte[] gzipBody, String token) {
        try {
            HttpRequest.Builder b = HttpRequest.newBuilder()
                    .uri(URI.create(url))
                    .timeout(readTimeout)
                    .header("Content-Type", "application/octet-stream")
                    .header("X-Varroa-Run-Id", record.getString("run_id"))
                    .header("X-Varroa-Job-Path",
                            URLEncoder.encode(record.getString("job_path"), StandardCharsets.UTF_8))
                    .header("X-Varroa-Build-Number", Long.toString(record.getLong("build_number")))
                    .header("X-Varroa-Relpath",
                            URLEncoder.encode(record.getString("relpath"), StandardCharsets.UTF_8))
                    .header("X-Varroa-State-Gen", Long.toString(record.getLong("state_gen")))
                    .header("X-Varroa-Seq", Long.toString(record.getLong("seq")))
                    .header("X-Varroa-Total", Long.toString(record.getLong("total")))
                    .header("X-Varroa-Raw-Size", Long.toString(record.getLong("raw_size")))
                    .header("X-Varroa-Object-Size", Long.toString(record.getLong("object_size")))
                    .header("X-Varroa-Pusher-Epoch", Long.toString(record.getLong("pusher_epoch")))
                    .POST(HttpRequest.BodyPublishers.ofByteArray(gzipBody));
            // A restart copy names itself and its copy counts. A mesh chunk
            // carries none of the three headers.
            if (record.optBoolean("restart_source", false)) {
                b.header("X-Varroa-Restart-Source", "1")
                        .header("X-Varroa-Copy-Files", Long.toString(record.getLong("copy_files")))
                        .header("X-Varroa-Copy-Bytes", Long.toString(record.getLong("copy_bytes")));
            }
            if (token != null) {
                b.header("Authorization", "Bearer " + token);
            }
            HttpResponse<byte[]> resp = client().send(b.build(), HttpResponse.BodyHandlers.ofByteArray());
            String reason = resp.headers().firstValue("X-Varroa-State-Reason").orElse(null);
            long pause = resp.headers().firstValue("X-Varroa-Pause-Ms")
                    .map(MiteLoopbackClient::parseLongOrZero).orElse(0L);
            return new RunStateAnswer(resp.statusCode(), reason, pause, bodyPrefix(resp.body()));
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            LOGGER.log(Level.FINE, "MiteLoopbackClient: interrupted posting a run-state chunk", e);
            return new RunStateAnswer(0, null, 0, null);
        } catch (IOException | RuntimeException e) {
            LOGGER.log(Level.FINE, "MiteLoopbackClient: failed to post a run-state chunk", e);
            return new RunStateAnswer(0, null, 0, null);
        }
    }

    /** The first {@value #BODY_PREFIX_BYTES} bytes of an answer body as UTF-8, or null when it has none. */
    private static String bodyPrefix(byte[] body) {
        if (body == null || body.length == 0) {
            return null;
        }
        int end = Math.min(BODY_PREFIX_BYTES, body.length);
        // A cut inside a multi-byte sequence drops that partial sequence.
        if (end < body.length) {
            while (end > 0 && (body[end] & 0xC0) == 0x80) {
                end--;
            }
        }
        return new String(body, 0, end, StandardCharsets.UTF_8);
    }

    private static long parseLongOrZero(String raw) {
        try {
            return Long.parseLong(raw.trim());
        } catch (NumberFormatException e) {
            return 0L;
        }
    }

    /** One run-state post's outcome: a status code, the mite's reason, and the start of its body. */
    public static final class RunStateAnswer {
        public final int statusCode;
        public final String reason;
        public final long pauseMillis;
        /** The first bytes of the answer body as UTF-8, or null when the answer had none. */
        public final String body;

        public RunStateAnswer(int statusCode, String reason, long pauseMillis, String body) {
            this.statusCode = statusCode;
            this.reason = reason;
            this.pauseMillis = pauseMillis;
            this.body = body;
        }
    }

    // ---- Cross-tenant routes (D2, D3, D4): one method per kind ----

    /**
     * Kind {@code build}, route {@code /v1/cross-tenant/build}. {@code seq}
     * is a 1-based ordinal; the wire field carries it as the string
     * {@code "s" + seq} (design.md D6), never a JSON number.
     *
     * <p>{@code intentKey} is the key an intra-tenant forward preserves.
     * The responder reads it only when the caller tenant equals the
     * target tenant, and it refuses a cross-tenant call that carries one.
     * A cross-tenant caller therefore sends an empty value, and the
     * responder derives the key itself.
     */
    public Response crossTenantBuild(String callerJobPath, String callerRunId, int seq,
            String targetTenant, String jobPath, Map<String, Object> params, String intentKey) {
        JSONObject body = new JSONObject();
        body.put("kind", "build");
        body.put("caller_job_path", callerJobPath);
        body.put("caller_run_id", callerRunId);
        body.put("seq", "s" + seq);
        body.put("target_tenant", targetTenant);
        body.put("job_path", jobPath);
        body.put("params", params != null ? params : Map.of());
        if (intentKey != null && !intentKey.isEmpty()) {
            body.put("intent_key", intentKey);
        }
        return post(CROSSTENANT_BUILD_PATH, body.toString());
    }

    /** Kind {@code status}, route {@code /v1/cross-tenant/status}, resolved by run id. */
    public Response crossTenantStatus(String callerJobPath, String runId) {
        JSONObject body = new JSONObject();
        body.put("kind", "status");
        body.put("caller_job_path", callerJobPath);
        body.put("run_id", runId);
        return post(CROSSTENANT_STATUS_PATH, body.toString());
    }

    /**
     * Kind {@code status}, route {@code /v1/cross-tenant/status}, resolved
     * by intent key: the poll names the intent key while a run id is not
     * yet known (D8).
     */
    public Response crossTenantStatusByIntentKey(String callerJobPath, String intentKey) {
        JSONObject body = new JSONObject();
        body.put("kind", "status");
        body.put("caller_job_path", callerJobPath);
        body.put("intent_key", intentKey);
        return post(CROSSTENANT_STATUS_PATH, body.toString());
    }

    /** Kind {@code last_successful}, route {@code /v1/cross-tenant/status}. */
    public Response crossTenantLastSuccessful(String callerJobPath, String targetTenant, String jobPath) {
        JSONObject body = new JSONObject();
        body.put("kind", "last_successful");
        body.put("caller_job_path", callerJobPath);
        body.put("target_tenant", targetTenant);
        body.put("job_path", jobPath);
        return post(CROSSTENANT_STATUS_PATH, body.toString());
    }

    /** Kind {@code artifact_list}, route {@code /v1/cross-tenant/artifact}. */
    public Response crossTenantArtifactList(String callerJobPath, String runId) {
        JSONObject body = new JSONObject();
        body.put("kind", "artifact_list");
        body.put("caller_job_path", callerJobPath);
        body.put("run_id", runId);
        return post(CROSSTENANT_ARTIFACT_PATH, body.toString());
    }

    /** Kind {@code artifact_url}, route {@code /v1/cross-tenant/artifact}. */
    public Response crossTenantArtifactUrl(String callerJobPath, String runId, String path) {
        JSONObject body = new JSONObject();
        body.put("kind", "artifact_url");
        body.put("caller_job_path", callerJobPath);
        body.put("run_id", runId);
        body.put("path", path);
        return post(CROSSTENANT_ARTIFACT_PATH, body.toString());
    }

    /**
     * Kind {@code lock_acquire}, route {@code /v1/cross-tenant/lock}.
     * {@code name} is the bare lock name. {@code targetTenant} names the
     * tenant that owns the lock; an empty value names the caller's own
     * tenant, and the body then carries no {@code target_tenant}.
     */
    public Response crossTenantLockAcquire(String callerJobPath, String targetTenant, String name, String runId,
            long leaseSeconds, String waitingSince) {
        JSONObject body = new JSONObject();
        body.put("kind", "lock_acquire");
        body.put("caller_job_path", callerJobPath);
        putTargetTenant(body, targetTenant);
        body.put("name", name);
        body.put("run_id", runId);
        body.put("lease_seconds", leaseSeconds);
        body.put("waiting_since", waitingSince);
        return post(CROSSTENANT_LOCK_PATH, body.toString());
    }

    /** Kind {@code lock_renew}, route {@code /v1/cross-tenant/lock}; the arguments match the acquire's. */
    public Response crossTenantLockRenew(String callerJobPath, String targetTenant, String name, String runId,
            long leaseSeconds) {
        JSONObject body = new JSONObject();
        body.put("kind", "lock_renew");
        body.put("caller_job_path", callerJobPath);
        putTargetTenant(body, targetTenant);
        body.put("name", name);
        body.put("run_id", runId);
        body.put("lease_seconds", leaseSeconds);
        return post(CROSSTENANT_LOCK_PATH, body.toString());
    }

    /** Kind {@code lock_release}, route {@code /v1/cross-tenant/lock}; the arguments match the acquire's. */
    public Response crossTenantLockRelease(String callerJobPath, String targetTenant, String name, String runId) {
        JSONObject body = new JSONObject();
        body.put("kind", "lock_release");
        body.put("caller_job_path", callerJobPath);
        putTargetTenant(body, targetTenant);
        body.put("name", name);
        body.put("run_id", runId);
        return post(CROSSTENANT_LOCK_PATH, body.toString());
    }

    /** Adds {@code target_tenant} to a lock body only when the caller names another owner. */
    private static void putTargetTenant(JSONObject body, String targetTenant) {
        if (targetTenant != null && !targetTenant.isEmpty()) {
            body.put("target_tenant", targetTenant);
        }
    }

    // ---- Timeout env parsing ----

    private static long readTimeoutMillisFromEnv() {
        String raw = System.getenv("VARROA_CLAIM_TIMEOUT_MS");
        if (raw == null || raw.isEmpty()) {
            return DEFAULT_READ_TIMEOUT_MILLIS;
        }
        try {
            long parsed = Long.parseLong(raw);
            return parsed > 0 ? parsed : DEFAULT_READ_TIMEOUT_MILLIS;
        } catch (NumberFormatException e) {
            return DEFAULT_READ_TIMEOUT_MILLIS;
        }
    }

    /**
     * Parses {@code VARROA_CROSSTENANT_CONNECT_TIMEOUT_MS} from its raw
     * value. Package-private, and takes the raw value rather than reading
     * the environment itself, so a test can drive every input without
     * mutating the process environment.
     */
    static long crossTenantConnectTimeoutMillis(String raw) {
        return wholeMillisOrDefault(raw, DEFAULT_CROSSTENANT_CONNECT_TIMEOUT_MILLIS);
    }

    /**
     * Parses {@code VARROA_CROSSTENANT_READ_TIMEOUT_MS} from its raw value,
     * and logs one {@code WARNING} when the resolved budget is at most the
     * mite hop's own 9000 ms default (D14): the plugin's read budget is the
     * outermost budget in the chain, and must stay longer than every hop it
     * wraps.
     */
    static long crossTenantReadTimeoutMillis(String raw) {
        long resolved = wholeMillisOrDefault(raw, DEFAULT_CROSSTENANT_READ_TIMEOUT_MILLIS);
        if (resolved <= CROSSTENANT_MITE_HOP_DEFAULT_MILLIS) {
            LOGGER.log(Level.WARNING,
                    "MiteLoopbackClient: VARROA_CROSSTENANT_READ_TIMEOUT_MS ({0} ms) is at or below "
                            + "the mite hop''s own {1} ms default; raise the outer budget before the inner one.",
                    new Object[] {resolved, CROSSTENANT_MITE_HOP_DEFAULT_MILLIS});
        }
        return resolved;
    }

    /**
     * Whole-number milliseconds only: an absent, empty, non-integer, or
     * non-positive value keeps {@code def}.
     */
    private static long wholeMillisOrDefault(String raw, long def) {
        if (raw == null || raw.isEmpty()) {
            return def;
        }
        try {
            long parsed = Long.parseLong(raw);
            return parsed > 0 ? parsed : def;
        } catch (NumberFormatException e) {
            return def;
        }
    }

    /** Result of a loopback POST: a status code and a body, or a transport failure. */
    public static final class Response {
        public final int statusCode;
        public final String body;
        /** Non-null when the request never reached the server, or timed out. */
        public final String error;

        public Response(int statusCode, String body, String error) {
            this.statusCode = statusCode;
            this.body = body;
            this.error = error;
        }

        public boolean isTransportFailure() {
            return error != null;
        }

        public boolean isSuccess(int expectedStatus) {
            return error == null && statusCode == expectedStatus;
        }
    }
}
