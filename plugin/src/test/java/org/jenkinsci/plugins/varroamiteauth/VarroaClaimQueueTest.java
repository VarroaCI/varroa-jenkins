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
import java.util.logging.Handler;
import java.util.logging.Level;
import java.util.logging.LogRecord;
import java.util.logging.Logger;
import java.util.logging.SimpleFormatter;

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

    @Rule
    public HomeForwardHarness homes = new HomeForwardHarness();

    private static final int QUIET_PERIOD = 300;

    /**
     * The failure body the mite sends. The mite encodes a reply with an
     * empty decision through {@code omitempty}, so the body holds no
     * {@code decision} key.
     */
    private static final String UNAVAILABLE_BODY =
            "{\"request_id\":\"req-1\",\"error\":\"nats: no responders available for request\",\"code\":\"UNAVAILABLE\"}";

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

    /**
     * Captures every formatted line the claimer logs at {@code level} or
     * above until the test ends.
     */
    private List<String> captureClaimQueueLog(Level level) {
        List<String> lines = new CopyOnWriteArrayList<>();
        Logger logger = Logger.getLogger(VarroaClaimQueue.class.getName());
        SimpleFormatter formatter = new SimpleFormatter();
        Handler handler = new Handler() {
            @Override
            public void publish(LogRecord record) {
                if (record.getLevel().intValue() >= level.intValue()) {
                    lines.add(record.getLevel() + " " + formatter.formatMessage(record));
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
        logHandlers.add(handler);
        return lines;
    }

    private final List<Handler> logHandlers = new CopyOnWriteArrayList<>();

    @After
    public void removeLogHandlers() {
        Logger logger = Logger.getLogger(VarroaClaimQueue.class.getName());
        for (Handler handler : logHandlers) {
            logger.removeHandler(handler);
        }
    }

    @Test
    public void aRefusedClaimLogsTheJobTheRunTheReasonAndTheMessage() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("refused-job");
        Queue.Item item = scheduleItem(project);
        List<String> lines = captureClaimQueueLog(Level.WARNING);
        responder = n -> new Reply(200, "{\"decision\":\"vetoed\",\"reason_code\":\"FENCED\","
                + "\"message\":\"cell cell-old holds no member of tenant alpha\"}");

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, millis -> { });
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled");
        awaitTrue(() -> !lines.isEmpty(), "the refusal line");
        assertEquals("WARNING VarroaClaimQueue: claim refused for job refused-job, run run-1: "
                + "FENCED: cell cell-old holds no member of tenant alpha", lines.get(0));
    }

    @Test
    public void anUnknownDecisionLogsTheUnknownCode() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("unknown-refused-job");
        Queue.Item item = scheduleItem(project);
        List<String> lines = captureClaimQueueLog(Level.WARNING);
        responder = n -> new Reply(200, "{\"request_id\":\"req-1\"}");

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, millis -> { });
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled");
        awaitTrue(() -> !lines.isEmpty(), "the refusal line");
        assertTrue(lines.get(0), lines.get(0).startsWith(
                "WARNING VarroaClaimQueue: claim refused for job unknown-refused-job, run run-1: UNKNOWN"));
    }

    @Test
    public void theGiveUpLineNamesTheLastAnswer() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("give-up-job");
        Queue.Item item = scheduleItem(project);
        List<String> lines = captureClaimQueueLog(Level.WARNING);
        responder = n -> new Reply(200, "{\"decision\":\"vetoed\",\"reason_code\":\"NOT_READY\","
                + "\"message\":\"tenant alpha has no placement row\"}");

        long[] clockMillis = {0L};
        VarroaClaimQueue.Sleeper sleeper = millis -> clockMillis[0] += millis;
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), () -> clockMillis[0], sleeper);
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled at the one-hour budget");
        awaitTrue(() -> !lines.isEmpty(), "the give-up line");
        String line = lines.get(lines.size() - 1);
        assertTrue(line, line.contains("giving up on job give-up-job after one hour"));
        assertTrue(line, line.endsWith("last answer NOT_READY: tenant alpha has no placement row"));
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
    public void aMissingDecisionWithACodeRetries() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("retry-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> n == 1
                ? new Reply(503, UNAVAILABLE_BODY)
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
    public void anEmptyDecisionWithACodeRetries() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("empty-decision-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> n == 1
                ? new Reply(429, "{\"decision\":\"\",\"code\":\"RESOURCE_EXHAUSTED\"}")
                : new Reply(200, "{\"decision\":\"granted\",\"run_id\":\"run-1\",\"build_number\":1}");

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, millis -> { });
        queue.offer(item, project);

        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);
        awaitTrue(action::isGranted, "eventual grant after a retry");
        assertEquals(2, calls.get());
    }

    @Test
    public void aMissingDecisionWithNoCodeCancels() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("no-code-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, "{\"request_id\":\"req-1\"}");

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, millis -> { });
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled");
        assertEquals(1, calls.get());
    }

    @Test
    public void codeOnlyRepliesRetryUntilTheOneHourBudget() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("code-only-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(503, UNAVAILABLE_BODY);

        long[] clockMillis = {0L};
        List<Long> slept = new CopyOnWriteArrayList<>();
        long[] cancelledAtMillis = {-1L};
        VarroaClaimQueue.Sleeper sleeper = millis -> {
            slept.add(millis);
            clockMillis[0] += millis;
        };
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), () -> clockMillis[0], sleeper);
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled at the one-hour budget");
        cancelledAtMillis[0] = clockMillis[0];
        assertTrue("the item must not be cancelled before the one-hour budget, got " + cancelledAtMillis[0],
                cancelledAtMillis[0] >= VarroaClaimQueue.LIFETIME_MILLIS);
        assertTrue("a code-only reply must retry on the ladder", calls.get() > 5);
        long totalSlept = slept.stream().mapToLong(Long::longValue).sum();
        assertTrue("the budget is one hour, got " + totalSlept, totalSlept >= 60L * 60 * 1000);
    }

    @Test
    public void codeOnlyRepliesThenAGrantClaimOnceAndKeepTheItem() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("code-only-grant-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> n <= 3
                ? new Reply(503, UNAVAILABLE_BODY)
                : new Reply(200, "{\"decision\":\"granted\",\"run_id\":\"run-1\",\"build_number\":7}");

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, millis -> { });
        queue.offer(item, project);

        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);
        awaitTrue(action::isGranted, "a grant after three code-only replies");
        assertEquals(4, calls.get());
        assertEquals(7, project.getNextBuildNumber());
        assertFalse("a retried claim must keep its queue item", isCancelled(item));
        String firstIntentKey = extractField(requestBodies.get(0), "intent_key");
        String lastIntentKey = extractField(requestBodies.get(3), "intent_key");
        assertEquals(firstIntentKey, lastIntentKey);
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
                ? new Reply(503, UNAVAILABLE_BODY)
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

    // ---- The intra-tenant forward ----

    /** One job inside a tenant folder, so its full path opens with the tenant. */
    private FreeStyleProject tenantJob(String tenant, String name) throws Exception {
        com.cloudbees.hudson.plugins.folder.Folder folder = j.jenkins.createProject(
                com.cloudbees.hudson.plugins.folder.Folder.class, tenant);
        return folder.createProject(FreeStyleProject.class, name);
    }

    @Test
    public void aNonHomeJobIsForwardedAndNeverClaimed() throws Exception {
        FreeStyleProject project = tenantJob("payments", "deploy");
        homes.seedHomes("payments", 0, "active", java.util.Map.of("payments/deploy", 1));
        Queue.Item item = scheduleItem(project);

        VarroaClaimQueue queue = new VarroaClaimQueue(homes.client(), System::currentTimeMillis,
                millis -> { }, homes.cache());
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled after the forward");
        assertEquals("the claimer must post exactly one forward", 1, homes.stub().forwardCallCount());
        assertEquals("the claimer must post no claim", 0, homes.stub().claimCallCount());
    }

    @Test
    public void theForwardNamesOneTenantTwiceAndKeepsTheIntentKey() throws Exception {
        FreeStyleProject project = tenantJob("payments", "nightly");
        homes.seedHomes("payments", 0, "active", java.util.Map.of("payments/nightly", 1));
        Queue.Item item = scheduleItem(project);

        VarroaClaimQueue queue = new VarroaClaimQueue(homes.client(), System::currentTimeMillis,
                millis -> { }, homes.cache());
        queue.offer(item, project);

        awaitTrue(() -> homes.stub().forwardCallCount() == 1, "one forward");
        String body = homes.stub().bodiesFor(StubLoopback.FORWARD_PATH).get(0);
        assertEquals("build", extractField(body, "kind"));
        assertEquals("payments/nightly", extractField(body, "caller_job_path"));
        assertEquals("payments", extractField(body, "target_tenant"));
        assertEquals("nightly", extractField(body, "job_path"));
        assertEquals("s1", extractField(body, "seq"));
        assertEquals("the forward carries the key the claimer derived",
                item.getAction(VarroaIntentAction.class).getIntentKey(), extractField(body, "intent_key"));
    }

    @Test
    public void aHomeJobClaimsAndForwardsNothing() throws Exception {
        FreeStyleProject project = tenantJob("payments", "build");
        homes.seedHomes("payments", 0, "active", java.util.Map.of("payments/build", 0));
        homes.stub().setClaimResponder(
                n -> new StubLoopback.Reply(200, "{\"decision\":\"granted\",\"run_id\":\"run-1\",\"build_number\":1}"));
        Queue.Item item = scheduleItem(project);

        VarroaClaimQueue queue = new VarroaClaimQueue(homes.client(), System::currentTimeMillis,
                millis -> { }, homes.cache());
        queue.offer(item, project);

        awaitTrue(() -> item.getAction(VarroaIntentAction.class).isGranted(), "the claim to be granted");
        assertEquals("a home job must post no forward", 0, homes.stub().forwardCallCount());
    }

    @Test
    public void aStandbyForwardsEverySubmission() throws Exception {
        FreeStyleProject project = tenantJob("payments", "warm");
        homes.seedHomes("payments", 0, "standby", java.util.Map.of("payments/warm", 0));
        Queue.Item item = scheduleItem(project);

        VarroaClaimQueue queue = new VarroaClaimQueue(homes.client(), System::currentTimeMillis,
                millis -> { }, homes.cache());
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled after the forward");
        assertEquals(1, homes.stub().forwardCallCount());
        assertEquals(0, homes.stub().claimCallCount());
    }

    @Test
    public void aNotHomeVetoForwardsAndCancelsNoItemFirst() throws Exception {
        FreeStyleProject project = tenantJob("payments", "moved");
        // The cache is stale: it homes the job here, and the fence does not.
        homes.seedHomes("payments", 0, "active", java.util.Map.of("payments/moved", 0));
        homes.stub().setClaimResponder(
                n -> new StubLoopback.Reply(200, "{\"decision\":\"vetoed\",\"reason_code\":\"NOT_HOME\"}"));
        Queue.Item item = scheduleItem(project);

        // The refresh the veto triggers reads the new map.
        homes.stub().setHomesBody("{\"entries\":[{\"tenant\":\"payments\",\"slot\":0,\"role\":\"active\","
                + "\"job_homes\":{\"payments/moved\":1}}]}");

        VarroaClaimQueue queue = new VarroaClaimQueue(homes.client(), System::currentTimeMillis,
                millis -> { }, homes.cache());
        queue.offer(item, project);

        awaitTrue(() -> homes.stub().forwardCallCount() == 1, "a forward after the not-home veto");
        assertEquals("the veto must cost exactly one claim", 1, homes.stub().claimCallCount());
    }

    @Test
    public void aNotHomeVetoWithAHomeSlotForwardsThere() throws Exception {
        FreeStyleProject project = tenantJob("payments", "fresh");
        // The map holds no entry for a new job, so the cell claims first.
        homes.seedHomes("payments", 1, "active", java.util.Map.of("payments/other", 1));
        homes.stub().setClaimResponder(n -> new StubLoopback.Reply(200,
                "{\"decision\":\"vetoed\",\"reason_code\":\"NOT_HOME\",\"home_slot\":0}"));
        Queue.Item item = scheduleItem(project);

        VarroaClaimQueue queue = new VarroaClaimQueue(homes.client(), System::currentTimeMillis,
                millis -> { }, homes.cache());
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled after the forward");
        assertEquals("the veto must cost exactly one claim", 1, homes.stub().claimCallCount());
        assertEquals("the veto's home slot must drive one forward", 1, homes.stub().forwardCallCount());
        String body = homes.stub().bodiesFor(StubLoopback.FORWARD_PATH).get(0);
        assertEquals("the forward keeps the item's run id",
                item.getAction(VarroaIntentAction.class).getRunId(), extractField(body, "caller_run_id"));
    }

    @Test
    public void aNotHomeVetoWithNoSlotAndNoEntryRetries() throws Exception {
        FreeStyleProject project = tenantJob("payments", "orphan");
        homes.seedHomes("payments", 1, "active", java.util.Map.of("payments/other", 1));
        homes.stub().setClaimResponder(
                n -> new StubLoopback.Reply(200, "{\"decision\":\"vetoed\",\"reason_code\":\"NOT_HOME\"}"));
        Queue.Item item = scheduleItem(project);

        VarroaClaimQueue queue = new VarroaClaimQueue(homes.client(), System::currentTimeMillis,
                millis -> { }, homes.cache());
        queue.offer(item, project);

        awaitTrue(() -> homes.stub().claimCallCount() >= 3, "the claim to retry on the ladder");
        assertEquals("no slot and no entry must forward nothing", 0, homes.stub().forwardCallCount());
        assertFalse("no slot and no entry must keep the item", isCancelled(item));
        queue.shutdown();
    }

    @Test
    public void aNewJobWithNoEntryClaimsLocally() throws Exception {
        FreeStyleProject project = tenantJob("payments", "brand-new");
        homes.seedHomes("payments", 1, "active", java.util.Map.of("payments/other", 0));
        Queue.Item item = scheduleItem(project);

        VarroaClaimQueue queue = new VarroaClaimQueue(homes.client(), System::currentTimeMillis,
                millis -> { }, homes.cache());
        queue.offer(item, project);

        awaitTrue(() -> item.getAction(VarroaIntentAction.class).isGranted(), "the claim to be granted");
        assertEquals("a job with no entry must post no forward", 0, homes.stub().forwardCallCount());
        assertEquals(1, homes.stub().claimCallCount());
    }

    @Test
    public void aRefusedForwardClaimsOnTheNextAttempt() throws Exception {
        FreeStyleProject project = tenantJob("payments", "stale");
        // The cached map is stale: the recorder already homes the job here.
        homes.seedHomes("payments", 0, "active", java.util.Map.of("payments/stale", 1));
        homes.stub().setForwardResponder(n -> new StubLoopback.Reply(200,
                "{\"reason_code\":\"NOT_READY\",\"message\":\"the home of job payments/stale is the calling cell\"}"));
        Queue.Item item = scheduleItem(project);

        VarroaClaimQueue queue = new VarroaClaimQueue(homes.client(), System::currentTimeMillis,
                millis -> { }, homes.cache());
        queue.offer(item, project);

        awaitTrue(() -> item.getAction(VarroaIntentAction.class).isGranted(), "the claim to be granted");
        assertEquals("the refused forward costs one call", 1, homes.stub().forwardCallCount());
        assertEquals("the next attempt claims", 1, homes.stub().claimCallCount());
    }

    @Test
    public void aFailedForwardRetriesAndOnlyTheBudgetCancels() throws Exception {
        FreeStyleProject project = tenantJob("payments", "flaky");
        homes.seedHomes("payments", 0, "active", java.util.Map.of("payments/flaky", 1));
        homes.stub().setForwardResponder(
                n -> new StubLoopback.Reply(503, "{\"code\":\"UNAVAILABLE\"}"));
        Queue.Item item = scheduleItem(project);

        long[] clockMillis = {0L};
        List<Long> slept = new CopyOnWriteArrayList<>();
        VarroaClaimQueue.Sleeper sleeper = millis -> {
            slept.add(millis);
            clockMillis[0] += millis;
        };
        VarroaClaimQueue queue = new VarroaClaimQueue(homes.client(), () -> clockMillis[0], sleeper,
                homes.cache());
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled at the one-hour budget");
        assertTrue("a failed forward must retry on the ladder", homes.stub().forwardCallCount() > 5);
        assertEquals("the ladder starts at one second", Long.valueOf(1000L), slept.get(0));
        long totalSlept = slept.stream().mapToLong(Long::longValue).sum();
        assertTrue("the budget is one hour, got " + totalSlept, totalSlept >= 60L * 60 * 1000);
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
