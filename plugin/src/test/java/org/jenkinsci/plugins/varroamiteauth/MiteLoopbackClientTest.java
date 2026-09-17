package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import com.sun.net.httpserver.HttpServer;

import java.io.ByteArrayOutputStream;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.function.Function;
import java.util.logging.Handler;
import java.util.logging.Level;
import java.util.logging.LogRecord;
import java.util.logging.Logger;

import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;

import net.sf.json.JSONObject;

/**
 * Tests for {@link MiteLoopbackClient}'s cross-tenant extension (task 9.11)
 * and its two timeout environment variables (task 5.4's plugin share).
 */
public class MiteLoopbackClientTest {

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private HttpServer server;
    private final List<String> receivedBodies = new ArrayList<>();

    @Before
    public void startServer() throws Exception {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/", exchange -> {
            ByteArrayOutputStream buf = new ByteArrayOutputStream();
            exchange.getRequestBody().transferTo(buf);
            synchronized (receivedBodies) {
                receivedBodies.add(buf.toString(StandardCharsets.UTF_8));
            }
            byte[] resp = "{\"ok\":true}".getBytes(StandardCharsets.UTF_8);
            exchange.getResponseHeaders().set("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, resp.length);
            try (OutputStream os = exchange.getResponseBody()) {
                os.write(resp);
            }
            exchange.close();
        });
        server.start();
    }

    @After
    public void stopServer() {
        server.stop(0);
    }

    private MiteLoopbackClient client() throws Exception {
        Path tokenFile = tmp.getRoot().toPath().resolve("token");
        Files.writeString(tokenFile, "test-token");
        LoopbackTokenFile tf = new LoopbackTokenFile(tokenFile, System::currentTimeMillis);
        return new MiteLoopbackClient("http://127.0.0.1:" + server.getAddress().getPort(), tf,
                Duration.ofSeconds(1), Duration.ofSeconds(5));
    }

    // ---- 9.11: every one of the eight kinds carries caller_job_path ----

    @Test
    public void everyCrossTenantKindCarriesCallerJobPath() throws Exception {
        MiteLoopbackClient client = client();
        String callerJobPath = "alpha/build-it";

        List<Function<MiteLoopbackClient, MiteLoopbackClient.Response>> calls = List.of(
                c -> c.crossTenantBuild(callerJobPath, "run-1", 1, "beta", "beta/deploy", Map.of(), ""),
                c -> c.crossTenantStatus(callerJobPath, "run-1"),
                c -> c.crossTenantLastSuccessful(callerJobPath, "beta", "beta/deploy"),
                c -> c.crossTenantArtifactList(callerJobPath, "run-1"),
                c -> c.crossTenantArtifactUrl(callerJobPath, "run-1", "out/app.jar"),
                c -> c.crossTenantLockAcquire(callerJobPath, "beta/deploy-prod", "run-1", 300L, "2026-09-11T00:00:00Z"),
                c -> c.crossTenantLockRenew(callerJobPath, "beta/deploy-prod", "run-1", 300L),
                c -> c.crossTenantLockRelease(callerJobPath, "beta/deploy-prod", "run-1"));

        assertEquals(8, calls.size());
        for (Function<MiteLoopbackClient, MiteLoopbackClient.Response> call : calls) {
            receivedBodies.clear();
            MiteLoopbackClient.Response resp = call.apply(client);
            assertTrue("must reach the loopback", resp.isSuccess(200));
            assertEquals(1, receivedBodies.size());
            JSONObject sent = JSONObject.fromObject(receivedBodies.get(0));
            assertEquals(callerJobPath, sent.getString("caller_job_path"));
        }
    }

    @Test
    public void everyRouteReceivesOnlyItsOwnClosedKindSet() throws Exception {
        MiteLoopbackClient client = client();
        client.crossTenantBuild("alpha/j", "run-1", 1, "beta", "beta/deploy", Map.of(), "");
        client.crossTenantLastSuccessful("alpha/j", "beta", "beta/deploy");
        client.crossTenantArtifactUrl("alpha/j", "run-1", "out/app.jar");
        client.crossTenantLockRenew("alpha/j", "beta/deploy-prod", "run-1", 300L);

        List<String> kinds = new ArrayList<>();
        for (String raw : receivedBodies) {
            kinds.add(JSONObject.fromObject(raw).getString("kind"));
        }
        assertEquals(List.of("build", "last_successful", "artifact_url", "lock_renew"), kinds);
    }

    // ---- D6: the build kind's seq travels as the string "s<n>", never a JSON number ----

    @Test
    public void crossTenantBuildSendsSeqAsAnSPrefixedString() throws Exception {
        MiteLoopbackClient client = client();
        client.crossTenantBuild("alpha/j", "run-1", 3, "beta", "deploy", Map.of(), "");

        assertEquals(1, receivedBodies.size());
        JSONObject sent = JSONObject.fromObject(receivedBodies.get(0));
        assertEquals("s3", sent.getString("seq"));
        assertTrue("seq must be a JSON string, not a number", sent.get("seq") instanceof String);
    }

    // ---- 9.11: a socket failure maps to the same failure code as a structured reply ----

    @Test
    public void transportFailureMapsToTheSameFailureCodeAsAStructuredReply() {
        MiteLoopbackClient.Response transportFailure = new MiteLoopbackClient.Response(0, null, "connection refused");
        MiteLoopbackClient.Response structuredFailure = new MiteLoopbackClient.Response(503,
                "{\"code\":\"UNAVAILABLE\"}", null);

        VarroaCrossTenantReply fromTransport = VarroaCrossTenantReply.parse(transportFailure);
        VarroaCrossTenantReply fromStructured = VarroaCrossTenantReply.parse(structuredFailure);

        assertTrue(fromTransport.isFailure());
        assertTrue(fromStructured.isFailure());
        assertEquals(fromStructured.code, fromTransport.code);
        assertEquals("UNAVAILABLE", fromTransport.code);
    }

    @Test
    public void aSuccessfulReplyIsNotAFailure() {
        MiteLoopbackClient.Response resp = new MiteLoopbackClient.Response(200,
                "{\"run_id\":\"run-1\",\"state\":\"completed\"}", null);
        VarroaCrossTenantReply reply = VarroaCrossTenantReply.parse(resp);
        assertFalse(reply.isFailure());
        assertEquals("run-1", reply.getString("run_id", ""));
        assertEquals("completed", reply.getString("state", ""));
    }

    // ---- 5.4 (plugin share): VARROA_CROSSTENANT_CONNECT_TIMEOUT_MS ----

    @Test
    public void connectTimeoutDefaultsWhenAbsent() {
        assertEquals(1000L, MiteLoopbackClient.crossTenantConnectTimeoutMillis(null));
        assertEquals(1000L, MiteLoopbackClient.crossTenantConnectTimeoutMillis(""));
    }

    @Test
    public void connectTimeoutHonorsAWholeNumberOverride() {
        assertEquals(2500L, MiteLoopbackClient.crossTenantConnectTimeoutMillis("2500"));
    }

    @Test
    public void connectTimeoutKeepsTheDefaultOnAMalformedValue() {
        for (String bad : List.of("abc", "-1", "0", "1000.5")) {
            assertEquals("input " + bad, 1000L, MiteLoopbackClient.crossTenantConnectTimeoutMillis(bad));
        }
    }

    // ---- 5.4 (plugin share): VARROA_CROSSTENANT_READ_TIMEOUT_MS ----

    @Test
    public void readTimeoutDefaultsWhenAbsent() {
        assertEquals(10_000L, MiteLoopbackClient.crossTenantReadTimeoutMillis(null));
        assertEquals(10_000L, MiteLoopbackClient.crossTenantReadTimeoutMillis(""));
    }

    @Test
    public void readTimeoutHonorsAWholeNumberOverride() {
        assertEquals(15_000L, MiteLoopbackClient.crossTenantReadTimeoutMillis("15000"));
    }

    @Test
    public void readTimeoutKeepsTheDefaultOnAMalformedValue() {
        for (String bad : List.of("abc", "-1", "0", "9000.5")) {
            assertEquals("input " + bad, 10_000L, MiteLoopbackClient.crossTenantReadTimeoutMillis(bad));
        }
    }

    @Test
    public void readTimeoutWarnsAtExactlyTheMiteHopsDefault() throws Exception {
        List<String> captured = captureWarnings(() -> MiteLoopbackClient.crossTenantReadTimeoutMillis("9500"));
        assertEquals(1, captured.size());
    }

    @Test
    public void readTimeoutDoesNotWarnAboveTheMiteHopsDefault() throws Exception {
        List<String> captured = captureWarnings(() -> MiteLoopbackClient.crossTenantReadTimeoutMillis("10000"));
        assertEquals(0, captured.size());
    }

    private static List<String> captureWarnings(Runnable action) {
        Logger logger = Logger.getLogger(MiteLoopbackClient.class.getName());
        List<String> captured = new ArrayList<>();
        Handler handler = new Handler() {
            @Override
            public void publish(LogRecord record) {
                if (record.getLevel() == Level.WARNING) {
                    captured.add(record.getMessage());
                }
            }

            @Override
            public void flush() {
            }

            @Override
            public void close() {
            }
        };
        logger.addHandler(handler);
        try {
            action.run();
        } finally {
            logger.removeHandler(handler);
        }
        return captured;
    }
}
