package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import hudson.model.FreeStyleProject;
import hudson.model.Queue;
import hudson.model.queue.ScheduleResult;

import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.function.IntFunction;

import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests for {@link VarroaClaimQueue} against a stub loopback server: the
 * request fields, the decision table, and the retry rule (D4).
 */
public class VarroaClaimQueueTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private static final int QUIET_PERIOD = 300;

    private HttpServer server;
    private final List<String> requestBodies = new CopyOnWriteArrayList<>();
    private final AtomicInteger calls = new AtomicInteger();
    private volatile IntFunction<Reply> responder;

    private static final class Reply {
        final int status;
        final String body;

        Reply(int status, String body) {
            this.status = status;
            this.body = body;
        }
    }

    @Before
    public void setUp() throws Exception {
        DuplicateRegistry.get().resetForTesting();
        ClaimRegistry.get().resetForTesting();
        requestBodies.clear();
        calls.set(0);
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/v1/claim", this::handle);
        server.start();
    }

    @After
    public void tearDown() {
        if (server != null) {
            server.stop(0);
        }
    }

    private void handle(HttpExchange exchange) throws java.io.IOException {
        byte[] reqBytes = exchange.getRequestBody().readAllBytes();
        requestBodies.add(new String(reqBytes, StandardCharsets.UTF_8));
        int n = calls.incrementAndGet();
        Reply reply = responder.apply(n);
        byte[] resp = reply.body.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "application/json");
        exchange.sendResponseHeaders(reply.status, resp.length == 0 ? -1 : resp.length);
        if (resp.length > 0) {
            try (OutputStream os = exchange.getResponseBody()) {
                os.write(resp);
            }
        }
        exchange.close();
    }

    private MiteLoopbackClient newClient() throws Exception {
        String base = "http://127.0.0.1:" + server.getAddress().getPort();
        Path tokenFile = tmp.getRoot().toPath().resolve("token");
        Files.writeString(tokenFile, "test-token");
        LoopbackTokenFile tf = new LoopbackTokenFile(tokenFile, System::currentTimeMillis);
        return new MiteLoopbackClient(base, tf, Duration.ofSeconds(5));
    }

    private Queue.Item scheduleItem(FreeStyleProject project) {
        VarroaIntentAction action = new VarroaIntentAction("run-1", "", null, 1_000_000L);
        ScheduleResult result = Queue.getInstance().schedule2(project, QUIET_PERIOD, action);
        assertTrue(result.isAccepted());
        return result.getItem();
    }

    private static void awaitTrue(java.util.function.BooleanSupplier condition, String message) throws Exception {
        long deadline = System.currentTimeMillis() + 10_000L;
        while (System.currentTimeMillis() < deadline) {
            if (condition.getAsBoolean()) {
                return;
            }
            Thread.sleep(20L);
        }
        fail("timed out waiting for: " + message);
    }

    private static boolean isCancelled(Queue.Item item) {
        // Queue.getItem(id) can still resolve a recently-cancelled item
        // (Queue.LeftItem bookkeeping); the future is the reliable signal.
        return item.getFuture().isCancelled();
    }

    // ---- Request fields ----

    @Test
    public void everyRequestFieldIsSent() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("field-test-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, "{\"decision\":\"granted\",\"run_id\":\"run-1\",\"build_number\":1}");

        VarroaClaimQueue.Clock clock = () -> 42_000L;
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), clock, millis -> { });
        assertTrue(queue.offer(item, project));

        awaitTrue(() -> !requestBodies.isEmpty(), "a claim request");
        String body = requestBodies.get(0);
        for (String field : new String[] {
                "\"origin\"", "\"intent_key\"", "\"run_id\":\"run-1\"", "\"job_path\":\"field-test-job\"",
                "\"job_display_name\"", "\"branch\":\"\"", "\"actor\"", "\"cause\"", "\"params_digest\"",
                "\"attempt\":1", "\"requested_at_millis\":42000", "\"jenkins_next_build_number\":1"
        }) {
            assertTrue("body should contain " + field + ": " + body, body.contains(field));
        }
        assertFalse("request_id must stay unset by the plugin", body.contains("request_id"));
        assertFalse("cell_instance must stay unset by the plugin", body.contains("cell_instance"));
    }

    // ---- Decision table ----

    @Test
    public void grantedAppliesTheBuildNumberAndSetsTheFlag() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("granted-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, "{\"decision\":\"granted\",\"run_id\":\"run-1\",\"build_number\":5}");

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, millis -> { });
        queue.offer(item, project);

        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);
        awaitTrue(action::isGranted, "the action to be granted");
        assertEquals(5, project.getNextBuildNumber());
        assertEquals("run-1", ClaimRegistry.get().runIdFor(action.getIntentKey()));
    }

    @Test
    public void duplicateRecordsTheRunIdAndCancels() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("duplicate-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, "{\"decision\":\"duplicate\",\"run_id\":\"run-1\"}");

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, millis -> { });
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled");
        assertTrue(DuplicateRegistry.get().contains("run-1"));
    }

    @Test
    public void fencedCancelsWithNoSecondClaim() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("vetoed-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, "{\"decision\":\"vetoed\",\"reason_code\":\"FENCED\"}");

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, millis -> { });
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled");
        assertEquals("a FENCED veto must not retry", 1, calls.get());
        assertFalse("a veto must write no DuplicateRegistry entry", DuplicateRegistry.get().contains("run-1"));
    }

    @Test
    public void crossTenantCancelsWithNoSecondClaim() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("cross-tenant-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, "{\"decision\":\"vetoed\",\"reason_code\":\"CROSS_TENANT\"}");

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, millis -> { });
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled");
        assertEquals("a CROSS_TENANT veto must not retry", 1, calls.get());
        assertFalse("a veto must write no DuplicateRegistry entry", DuplicateRegistry.get().contains("run-1"));
    }

    @Test
    public void anEmptyDecisionWithACodeRetries() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("retry-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> n == 1
                ? new Reply(503, "{\"decision\":\"\",\"code\":\"UNAVAILABLE\"}")
                : new Reply(200, "{\"decision\":\"granted\",\"run_id\":\"run-1\",\"build_number\":1}");

        List<Long> slept = new CopyOnWriteArrayList<>();
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, slept::add);
        queue.offer(item, project);

        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);
        awaitTrue(action::isGranted, "eventual grant after a retry");
        assertEquals(2, calls.get());
        assertEquals(List.of(1000L), slept);
    }

    @Test
    public void anUnknownDecisionCancelsWithNoRetry() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("unknown-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, "{\"decision\":\"bogus\"}");

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, millis -> { });
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled");
        assertEquals(1, calls.get());
    }

    // ---- Retry rule (5.7) ----

    @Test
    public void notReadyRetriesWithTheSameIntentKeyAndRunId() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("not-ready-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> n == 1
                ? new Reply(200, "{\"decision\":\"vetoed\",\"reason_code\":\"NOT_READY\"}")
                : new Reply(200, "{\"decision\":\"granted\",\"run_id\":\"run-1\",\"build_number\":1}");

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, millis -> { });
        queue.offer(item, project);

        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);
        awaitTrue(action::isGranted, "eventual grant after a NOT_READY retry");
        assertEquals(2, calls.get());
        // The second attempt carries the same intent key and run id as the first.
        String firstIntentKey = extractField(requestBodies.get(0), "intent_key");
        String secondIntentKey = extractField(requestBodies.get(1), "intent_key");
        assertEquals(firstIntentKey, secondIntentKey);
        assertTrue(requestBodies.get(1).contains("\"run_id\":\"run-1\""));
    }

    @Test
    public void aTenMinuteOutageDoesNotDropThePendingClaim() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("outage-job");
        Queue.Item item = scheduleItem(project);
        // Fail for the equivalent of a 10-minute outage, then succeed.
        // The 1/2/5/10/30/60s (repeating) ladder needs 15 failures before
        // cumulative backoff passes 600_000 ms.
        responder = n -> n <= 15
                ? new Reply(503, "{\"decision\":\"\",\"code\":\"UNAVAILABLE\"}")
                : new Reply(200, "{\"decision\":\"granted\",\"run_id\":\"run-1\",\"build_number\":1}");

        long[] clockMillis = {0L};
        List<Long> slept = new CopyOnWriteArrayList<>();
        VarroaClaimQueue.Sleeper sleeper = millis -> {
            slept.add(millis);
            clockMillis[0] += millis;
        };
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), () -> clockMillis[0], sleeper);
        queue.offer(item, project);

        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);
        awaitTrue(action::isGranted, "eventual grant after a long outage");
        long totalSlept = slept.stream().mapToLong(Long::longValue).sum();
        assertTrue("expected over 10 minutes of cumulative backoff, got " + totalSlept, totalSlept > 600_000L);
        assertFalse("the pending claim must not be cancelled during the outage", isCancelled(item));
    }

    // ---- Shutdown (ruling r) ----

    @Test
    public void shutdownStopsTheExecutorAndRefusesFurtherOffers() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("shutdown-job");
        Queue.Item item = scheduleItem(project);
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, millis -> { });

        queue.shutdown();

        assertTrue(queue.isShutdownForTesting());
        assertFalse("a shut-down queue must refuse a new offer", queue.offer(item, project));
    }

    // ---- Unexpected claimer exception (ruling q) ----

    @Test
    public void anUnexpectedClaimerExceptionCancelsAndClearsBothKeys() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("exception-job");
        Queue.Item item = scheduleItem(project);
        MiteLoopbackClient client = mock(MiteLoopbackClient.class);
        when(client.post(any(), any())).thenThrow(new RuntimeException("boom"));
        java.util.concurrent.atomic.AtomicBoolean resolved = new java.util.concurrent.atomic.AtomicBoolean(false);

        VarroaClaimQueue queue = new VarroaClaimQueue(client, System::currentTimeMillis, millis -> { });
        queue.offer(item, project, () -> resolved.set(true));

        awaitTrue(() -> isCancelled(item), "the item to be cancelled after an unexpected error");
        awaitTrue(resolved::get, "the onResolved callback to run");
        assertFalse("the pending key must be cleared",
                queue.isPendingForTesting(project.getFullName() + "#run-1"));
    }

    // ---- Missing action never cancels (ruling p) ----

    @Test
    public void aMissingActionLeavesTheItemBlockedInsteadOfCancellingIt() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("no-action-job");
        ScheduleResult scheduled = Queue.getInstance().schedule2(project, QUIET_PERIOD);
        assertTrue(scheduled.isAccepted());
        Queue.Item item = scheduled.getItem();
        assertNull("fixture must start with no action", item.getAction(VarroaIntentAction.class));

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, millis -> { });
        java.util.concurrent.atomic.AtomicBoolean resolved = new java.util.concurrent.atomic.AtomicBoolean(false);
        queue.offer(item, project, () -> resolved.set(true));

        awaitTrue(resolved::get, "the offer to resolve");
        assertFalse("a missing action must never cancel the item", isCancelled(item));
        assertTrue("no claim request must have been sent for a missing action", requestBodies.isEmpty());
    }

    private static String extractField(String json, String field) {
        String marker = "\"" + field + "\":\"";
        int start = json.indexOf(marker);
        if (start < 0) {
            return null;
        }
        start += marker.length();
        int end = json.indexOf('"', start);
        return json.substring(start, end);
    }
}
