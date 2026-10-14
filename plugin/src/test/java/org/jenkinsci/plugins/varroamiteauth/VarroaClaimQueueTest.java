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
import java.util.function.Function;
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
    /** When set, answers each claim from its request body instead of {@link #responder}. */
    private volatile Function<String, Reply> bodyResponder;
    /** Every start-mark body, oldest first. A start mark never reaches {@link #requestBodies}. */
    private final List<String> markBodies = new CopyOnWriteArrayList<>();
    private final AtomicInteger markCalls = new AtomicInteger();
    /** Answers the n-th start mark. The default grants it. */
    private volatile IntFunction<Reply> markResponder;

    private static final String MARK_GRANTED = "{\"decision\":\"granted\",\"run_id\":\"run-1\"}";

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
        bodyResponder = null;
        markBodies.clear();
        markCalls.set(0);
        markResponder = n -> new Reply(200, MARK_GRANTED);
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
        String reqBody = new String(reqBytes, StandardCharsets.UTF_8);
        Reply reply;
        if (reqBody.contains("\"start_mark\":true")) {
            markBodies.add(reqBody);
            reply = markResponder.apply(markCalls.incrementAndGet());
        } else {
            requestBodies.add(reqBody);
            int n = calls.incrementAndGet();
            Function<String, Reply> byBody = bodyResponder;
            reply = byBody != null ? byBody.apply(reqBody) : responder.apply(n);
        }
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
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), clock, (task, d) -> task.run());
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

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> task.run());
        queue.offer(item, project);

        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);
        awaitTrue(action::isGranted, "the action to be granted");
        assertEquals(5, project.getNextBuildNumber());
        assertEquals("run-1", ClaimRegistry.get().runIdFor(action.getIntentKey()));
    }

    // ---- Start mark ----

    @Test
    public void aGrantReleasesTheItemOnlyAfterTheStartMark() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("mark-wait-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, "{\"decision\":\"granted\",\"run_id\":\"run-1\",\"build_number\":5}");
        java.util.concurrent.CountDownLatch answer = new java.util.concurrent.CountDownLatch(1);
        markResponder = n -> {
            try {
                answer.await(10, java.util.concurrent.TimeUnit.SECONDS);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            }
            return new Reply(200, MARK_GRANTED);
        };

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> task.run());
        queue.offer(item, project);
        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);

        awaitTrue(() -> markBodies.size() == 1, "a start mark");
        assertEquals("the number applies before the mark", 5, project.getNextBuildNumber());
        assertFalse("the item stays blocked until the mark answers", action.isGranted());
        assertTrue("the dispatcher offers no claim while the mark is out", action.isApplyPending());

        answer.countDown();
        awaitTrue(action::isGranted, "the grant after the start mark");
        assertFalse(action.isApplyPending());
        assertEquals(0, action.getMarkFailures());
        assertEquals("run-1", ClaimRegistry.get().runIdFor(action.getIntentKey()));
        String mark = markBodies.get(0);
        for (String field : new String[] {
                "\"start_mark\":true", "\"run_id\":\"run-1\"", "\"job_path\":\"mark-wait-job\"",
                "\"intent_key\":\"" + action.getIntentKey() + "\""
        }) {
            assertTrue("start mark should contain " + field + ": " + mark, mark.contains(field));
        }
        assertEquals("one claim and one mark", 1, calls.get());
    }

    @Test
    public void aDuplicateStartMarkCancelsTheItem() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("mark-duplicate-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, "{\"decision\":\"granted\",\"run_id\":\"run-1\",\"build_number\":5}");
        markResponder = n -> new Reply(200, "{\"decision\":\"duplicate\",\"run_id\":\"run-other\"}");

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> task.run());
        queue.offer(item, project);
        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled");
        assertFalse(action.isGranted());
        assertTrue(DuplicateRegistry.get().contains("run-1"));
        assertNull("a mark that did not grant records no claim", ClaimRegistry.get().runIdFor(action.getIntentKey()));
    }

    @Test
    public void aFailedStartMarkOpensTheGateAndDefersTheNextClaim() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("mark-fail-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, "{\"decision\":\"granted\",\"run_id\":\"run-1\",\"build_number\":5}");
        markResponder = n -> new Reply(200, UNAVAILABLE_BODY);

        VarroaClaimQueue.Clock clock = () -> 42_000L;
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), clock, (task, d) -> task.run());
        queue.offer(item, project);
        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);

        awaitTrue(() -> action.getMarkFailures() == 1 && !action.isApplyPending(), "the failed mark");
        assertFalse("a failed mark never releases the item", action.isGranted());
        assertFalse("a failed mark keeps the item", isCancelled(item));
        assertEquals(42_000L + VarroaClaimQueue.backoffFor(1), action.getClaimNotBeforeMillis());
        assertNull("a failed mark opens the gate", BuildNumberGate.get().holderForTesting("mark-fail-job"));
    }

    @Test
    public void aStartMarkAfterTheGateExpiredClaimsAgain() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("mark-expired-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, "{\"decision\":\"granted\",\"run_id\":\"run-1\",\"build_number\":5}");
        java.util.concurrent.CountDownLatch answer = new java.util.concurrent.CountDownLatch(1);
        markResponder = n -> {
            try {
                answer.await(10, java.util.concurrent.TimeUnit.SECONDS);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            }
            return new Reply(200, MARK_GRANTED);
        };

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> task.run());
        queue.offer(item, project);
        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);

        awaitTrue(() -> markBodies.size() == 1, "a start mark");
        // The release stands in for the gate's 60-second expiry.
        BuildNumberGate.get().release("mark-expired-job", "run-1");
        answer.countDown();

        awaitTrue(() -> action.getMarkFailures() == 1 && !action.isApplyPending(), "the refused late mark");
        assertFalse("a mark after the gate expired never releases the item", action.isGranted());
        assertFalse(isCancelled(item));
    }

    @Test
    public void aNumberConflictPostsNoStartMark() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("mark-conflict-job");
        project.updateNextBuildNumber(10);
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, "{\"decision\":\"granted\",\"run_id\":\"run-1\",\"build_number\":5}");

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> task.run());
        queue.offer(item, project);
        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);

        awaitTrue(() -> action.getNumberConflict() == 5 && !action.isApplyPending(), "the number conflict");
        assertEquals("a number that did not apply posts no mark", 0, markCalls.get());
        assertFalse(action.isGranted());
    }

    @Test
    public void duplicateRecordsTheRunIdAndCancels() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("duplicate-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, "{\"decision\":\"duplicate\",\"run_id\":\"run-1\"}");

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> task.run());
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled");
        assertTrue(DuplicateRegistry.get().contains("run-1"));
    }

    @Test
    public void fencedCancelsWithNoSecondClaim() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("vetoed-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, "{\"decision\":\"vetoed\",\"reason_code\":\"FENCED\"}");

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> task.run());
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

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> task.run());
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

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> task.run());
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
        VarroaClaimQueue.Scheduler scheduler = (task, d) -> {
            // A NOT_READY claim retries every 10 s. A one-minute step keeps the attempt count small.
            clockMillis[0] += Math.max(d, 60_000L);
            task.run();
        };
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), () -> clockMillis[0], scheduler);
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled at the one-hour budget");
        awaitTrue(() -> !lines.isEmpty(), "the give-up line");
        String line = lines.get(lines.size() - 1);
        assertTrue(line, line.contains("giving up on job give-up-job after one hour"));
        assertTrue(line, line.endsWith("last answer NOT_READY: tenant alpha has no placement row"));
        // The give-up runs at the first attempt past the deadline. One clock step is 60 s here.
        assertTrue("the give-up must come within one step after the one-hour budget, got " + clockMillis[0],
                clockMillis[0] >= VarroaClaimQueue.LIFETIME_MILLIS
                        && clockMillis[0] < VarroaClaimQueue.LIFETIME_MILLIS + 60_000L);
    }

    @Test
    public void crossTenantCancelsWithNoSecondClaim() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("cross-tenant-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, "{\"decision\":\"vetoed\",\"reason_code\":\"CROSS_TENANT\"}");

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> task.run());
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
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> {
            slept.add(d);
            task.run();
        });
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

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> task.run());
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

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> task.run());
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
        VarroaClaimQueue.Scheduler scheduler = (task, d) -> {
            slept.add(d);
            clockMillis[0] += d;
            task.run();
        };
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), () -> clockMillis[0], scheduler);
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

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> task.run());
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

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> task.run());
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

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> task.run());
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
    public void notReadyVetoRetriesAtMostEveryTenSeconds() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("not-ready-cap-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> n <= 6
                ? new Reply(200, NOT_READY_BODY)
                : new Reply(200, "{\"decision\":\"duplicate\"}");

        long[] clockMillis = {0L};
        List<Long> slept = new CopyOnWriteArrayList<>();
        VarroaClaimQueue.Scheduler scheduler = (task, d) -> {
            slept.add(d);
            clockMillis[0] += d;
            task.run();
        };
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), () -> clockMillis[0], scheduler);
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled by the duplicate answer");
        assertEquals(7, calls.get());
        assertEquals(List.of(1000L, 2000L, 5000L, 10_000L, 10_000L, 10_000L), slept);
    }

    @Test
    public void transportRetryKeepsFullLadder() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("transport-ladder-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> n <= 6
                ? new Reply(503, UNAVAILABLE_BODY)
                : new Reply(200, "{\"decision\":\"duplicate\"}");

        long[] clockMillis = {0L};
        List<Long> slept = new CopyOnWriteArrayList<>();
        VarroaClaimQueue.Scheduler scheduler = (task, d) -> {
            slept.add(d);
            clockMillis[0] += d;
            task.run();
        };
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), () -> clockMillis[0], scheduler);
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled by the duplicate answer");
        assertEquals(7, calls.get());
        assertEquals(List.of(1000L, 2000L, 5000L, 10_000L, 30_000L, 60_000L), slept);
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
        VarroaClaimQueue.Scheduler scheduler = (task, d) -> {
            slept.add(d);
            clockMillis[0] += d;
            task.run();
        };
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), () -> clockMillis[0], scheduler);
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
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> task.run());

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

        VarroaClaimQueue queue = new VarroaClaimQueue(client, System::currentTimeMillis, (task, d) -> task.run());
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

        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> task.run());
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
                (task, d) -> task.run(), homes.cache());
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
                (task, d) -> task.run(), homes.cache());
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
                (task, d) -> task.run(), homes.cache());
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
                (task, d) -> task.run(), homes.cache());
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
                (task, d) -> task.run(), homes.cache());
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
                (task, d) -> task.run(), homes.cache());
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
                (task, d) -> task.run(), homes.cache());
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
                (task, d) -> task.run(), homes.cache());
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
                (task, d) -> task.run(), homes.cache());
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
        VarroaClaimQueue.Scheduler scheduler = (task, d) -> {
            slept.add(d);
            clockMillis[0] += d;
            task.run();
        };
        VarroaClaimQueue queue = new VarroaClaimQueue(homes.client(), () -> clockMillis[0], scheduler,
                homes.cache());
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled at the one-hour budget");
        assertTrue("a failed forward must retry on the ladder", homes.stub().forwardCallCount() > 5);
        assertEquals("the ladder starts at one second", Long.valueOf(1000L), slept.get(0));
        long totalSlept = slept.stream().mapToLong(Long::longValue).sum();
        assertTrue("the budget is one hour, got " + totalSlept, totalSlept >= 60L * 60 * 1000);
    }

    // ---- A waiting claim holds no claimer thread ----

    private static final String NOT_READY_BODY =
            "{\"decision\":\"vetoed\",\"reason_code\":\"NOT_READY\"}";

    private static final String GRANT_BODY =
            "{\"decision\":\"granted\",\"run_id\":\"run-1\",\"build_number\":1}";

    @Test
    public void aNotReadyClaimReleasesItsThread() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("parked-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, NOT_READY_BODY);

        List<Runnable> parked = new CopyOnWriteArrayList<>();
        List<Long> delays = new CopyOnWriteArrayList<>();
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> {
            delays.add(d);
            parked.add(task);
        });
        queue.offer(item, project);

        awaitTrue(() -> parked.size() == 1, "the claim to park on the scheduler");
        awaitTrue(() -> queue.activeClaimersForTesting() == 0, "every claimer thread to be free");
        assertTrue("a parked claim stays pending", queue.isPendingForTesting("parked-job#run-1"));
        assertEquals("the first ladder step is one second", List.of(1000L), delays);
        assertEquals(1, calls.get());

        responder = n -> new Reply(200, GRANT_BODY);
        parked.remove(0).run();

        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);
        awaitTrue(action::isGranted, "the grant after the hand-back");
        assertEquals(2, calls.get());
        queue.shutdown();
    }

    @Test
    public void aReadyJobClaimsWhileFourClaimsWaitOnNotReady() throws Exception {
        bodyResponder = body -> body.contains("\"job_path\":\"ready-job\"")
                ? new Reply(200, GRANT_BODY)
                : new Reply(200, NOT_READY_BODY);
        // The scheduler never fires, so a refused claim waits forever.
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> { });

        for (int i = 0; i < VarroaClaimQueue.THREADS; i++) {
            FreeStyleProject waiting = j.createFreeStyleProject("waiting-job-" + i);
            queue.offer(scheduleItem(waiting), waiting);
        }
        awaitTrue(() -> calls.get() == VarroaClaimQueue.THREADS, "one refused claim per waiting job");

        FreeStyleProject ready = j.createFreeStyleProject("ready-job");
        Queue.Item item = scheduleItem(ready);
        long offeredAt = System.currentTimeMillis();
        queue.offer(item, ready);

        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);
        awaitTrue(action::isGranted, "the ready job's grant");
        long waited = System.currentTimeMillis() - offeredAt;
        assertTrue("the ready job must not wait behind refused claims, waited " + waited, waited < 5_000L);
        for (int i = 0; i < VarroaClaimQueue.THREADS; i++) {
            assertTrue(queue.isPendingForTesting("waiting-job-" + i + "#run-1"));
        }
        queue.shutdown();
    }

    @Test
    public void theBudgetStillExpiresOffThread() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("budget-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, NOT_READY_BODY);

        long[] clockMillis = {0L};
        List<String> schedulingThreads = new CopyOnWriteArrayList<>();
        VarroaClaimQueue.Scheduler scheduler = (task, d) -> {
            schedulingThreads.add(Thread.currentThread().getName());
            // A NOT_READY claim retries every 10 s. A one-minute step keeps the attempt count small.
            clockMillis[0] += Math.max(d, 60_000L);
            task.run();
        };
        AtomicInteger resolved = new AtomicInteger();
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), () -> clockMillis[0], scheduler);
        queue.offer(item, project, resolved::incrementAndGet);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled at the one-hour budget");
        awaitTrue(() -> resolved.get() == 1, "one resolution");
        // The give-up runs at the first attempt past the deadline. One clock step is 60 s here.
        assertTrue("the cancel must come within one step after the one-hour budget, got " + clockMillis[0],
                clockMillis[0] >= VarroaClaimQueue.LIFETIME_MILLIS
                        && clockMillis[0] < VarroaClaimQueue.LIFETIME_MILLIS + 60_000L);
        assertFalse(queue.isPendingForTesting("budget-job#run-1"));
        assertEquals("the claim resolves exactly once", 1, resolved.get());
        assertTrue("only a claimer thread parks a claim",
                schedulingThreads.stream().allMatch(name -> name.startsWith("varroa-claim-queue-")));
    }

    @Test
    public void aCancelledItemStopsClaiming() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("cancelled-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, NOT_READY_BODY);

        List<Runnable> parked = new CopyOnWriteArrayList<>();
        java.util.concurrent.atomic.AtomicBoolean resolved = new java.util.concurrent.atomic.AtomicBoolean(false);
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis,
                (task, d) -> parked.add(task));
        queue.offer(item, project, () -> resolved.set(true));

        awaitTrue(() -> parked.size() == 1, "the claim to park on the scheduler");
        assertTrue(Queue.getInstance().cancel(item));
        parked.remove(0).run();

        awaitTrue(resolved::get, "the claim to resolve");
        assertEquals("an item that left the queue sends no claim", 1, calls.get());
        assertFalse(queue.isPendingForTesting("cancelled-job#run-1"));
        assertTrue("the claim parks no new attempt", parked.isEmpty());
    }

    @Test
    public void thePendingCapCountsParkedClaims() throws Exception {
        responder = n -> new Reply(200, NOT_READY_BODY);
        List<Runnable> parked = new CopyOnWriteArrayList<>();
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis,
                (task, d) -> parked.add(task), VarroaHomeCache.get(), BuildNumberGate.get(), 2);

        FreeStyleProject first = j.createFreeStyleProject("cap-job-0");
        FreeStyleProject second = j.createFreeStyleProject("cap-job-1");
        Queue.Item firstItem = scheduleItem(first);
        assertTrue(queue.offer(firstItem, first));
        assertTrue(queue.offer(scheduleItem(second), second));
        awaitTrue(() -> parked.size() == 2, "both claims to park");
        awaitTrue(() -> queue.activeClaimersForTesting() == 0, "every claimer thread to be free");

        FreeStyleProject third = j.createFreeStyleProject("cap-job-2");
        assertFalse("parked claims count toward the cap", queue.offer(scheduleItem(third), third));
        assertTrue("a claim that is already pending is accepted", queue.offer(firstItem, first));
        assertEquals(2, calls.get());
        queue.shutdown();
        assertFalse("a shutdown drops the parked claims", queue.isPendingForTesting("cap-job-0#run-1"));
        assertFalse("a shutdown cancels no queue item", isCancelled(firstItem));
    }

    // ---- A replay or a restart never forwards ----

    /** Schedules an item with the given cause, such as a replay or a restart. */
    private Queue.Item scheduleItemWithCause(FreeStyleProject project, hudson.model.Cause cause) {
        VarroaIntentAction action = new VarroaIntentAction("run-1", "", null, 1_000_000L);
        ScheduleResult result = Queue.getInstance().schedule2(project, QUIET_PERIOD, action,
                new hudson.model.CauseAction(cause));
        assertTrue(result.isAccepted());
        return result.getItem();
    }

    @Test
    public void aReplayNotHomeVetoCancelsAndNeverForwards() throws Exception {
        FreeStyleProject project = tenantJob("payments", "replayed");
        homes.seedHomes("payments", 0, "active", java.util.Map.of("payments/replayed", 0));
        homes.stub().setClaimResponder(n -> new StubLoopback.Reply(200,
                "{\"decision\":\"vetoed\",\"reason_code\":\"NOT_HOME\",\"home_slot\":1}"));
        Queue.Item item = scheduleItemWithCause(project, new VarroaReplayCause("src"));

        VarroaClaimQueue queue = new VarroaClaimQueue(homes.client(), System::currentTimeMillis,
                (task, d) -> task.run(), homes.cache());
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the replay to be cancelled on the not-home veto");
        assertEquals("the veto must cost exactly one claim", 1, homes.stub().claimCallCount());
        assertEquals("a replay must never forward", 0, homes.stub().forwardCallCount());
    }

    @Test
    public void aRestartNotHomeVetoCancelsAndNeverForwards() throws Exception {
        FreeStyleProject project = tenantJob("payments", "restarted");
        homes.seedHomes("payments", 0, "active", java.util.Map.of("payments/restarted", 0));
        homes.stub().setClaimResponder(n -> new StubLoopback.Reply(200,
                "{\"decision\":\"vetoed\",\"reason_code\":\"NOT_HOME\",\"home_slot\":1}"));
        Queue.Item item = scheduleItemWithCause(project, new VarroaRestartCause("src", "deploy"));

        VarroaClaimQueue queue = new VarroaClaimQueue(homes.client(), System::currentTimeMillis,
                (task, d) -> task.run(), homes.cache());
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the restart to be cancelled on the not-home veto");
        assertEquals("the veto must cost exactly one claim", 1, homes.stub().claimCallCount());
        assertEquals("a restart must never forward", 0, homes.stub().forwardCallCount());
    }

    @Test
    public void aReplayOfANonHomeJobClaimsInsteadOfForwarding() throws Exception {
        FreeStyleProject project = tenantJob("payments", "deploy");
        homes.seedHomes("payments", 0, "active", java.util.Map.of("payments/deploy", 1));
        homes.stub().setClaimResponder(
                n -> new StubLoopback.Reply(200, "{\"decision\":\"granted\",\"run_id\":\"run-1\",\"build_number\":1}"));
        Queue.Item item = scheduleItemWithCause(project, new VarroaReplayCause("src"));

        VarroaClaimQueue queue = new VarroaClaimQueue(homes.client(), System::currentTimeMillis,
                (task, d) -> task.run(), homes.cache());
        queue.offer(item, project);

        awaitTrue(() -> item.getAction(VarroaIntentAction.class).isGranted(), "the replay to be granted");
        assertEquals("a replay must never forward", 0, homes.stub().forwardCallCount());
        assertEquals("a replay must claim", 1, homes.stub().claimCallCount());
    }

    @Test
    public void aReplayOfANonHomeJobVetoedNotHomeCancels() throws Exception {
        FreeStyleProject project = tenantJob("payments", "moved-replay");
        homes.seedHomes("payments", 0, "active", java.util.Map.of("payments/moved-replay", 1));
        homes.stub().setClaimResponder(
                n -> new StubLoopback.Reply(200, "{\"decision\":\"vetoed\",\"reason_code\":\"NOT_HOME\"}"));
        Queue.Item item = scheduleItemWithCause(project, new VarroaReplayCause("src"));

        VarroaClaimQueue queue = new VarroaClaimQueue(homes.client(), System::currentTimeMillis,
                (task, d) -> task.run(), homes.cache());
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the replay to be cancelled");
        assertEquals("the veto must cost exactly one claim", 1, homes.stub().claimCallCount());
        assertEquals("a replay must never forward", 0, homes.stub().forwardCallCount());
    }

    // ---- A revision wait holds no claimer thread ----

    /** Schedules an item with a branch event cause and the given admission revision. */
    private Queue.Item scheduleBranchItem(FreeStyleProject project, String admissionRevision) {
        VarroaIntentAction action = new VarroaIntentAction("run-1", "", admissionRevision, 1_000_000L);
        ScheduleResult result = Queue.getInstance().schedule2(project, QUIET_PERIOD, action,
                new hudson.model.CauseAction(new IntentKeyDispatchTest.BranchEventCause()));
        assertTrue(result.isAccepted());
        return result.getItem();
    }

    @Test
    public void aRevisionWaitReleasesItsThread() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("revision-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, GRANT_BODY);

        java.util.concurrent.atomic.AtomicBoolean accepted = new java.util.concurrent.atomic.AtomicBoolean(false);
        AtomicInteger derives = new AtomicInteger();
        VarroaClaimQueue.Deriver deriver = (i, jb, a, now, deadline) -> {
            derives.incrementAndGet();
            return accepted.get() ? IntentKey.derive(i, jb, a, now, deadline) : null;
        };
        List<Runnable> parked = new CopyOnWriteArrayList<>();
        List<Long> delays = new CopyOnWriteArrayList<>();
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> {
            delays.add(d);
            parked.add(task);
        }, VarroaHomeCache.get(), BuildNumberGate.get(), VarroaClaimQueue.CAPACITY, deriver);
        queue.offer(item, project);

        awaitTrue(() -> parked.size() == 1, "the revision wait to park on the scheduler");
        awaitTrue(() -> queue.activeClaimersForTesting() == 0, "every claimer thread to be free");
        assertTrue("a waiting claim stays pending", queue.isPendingForTesting("revision-job#run-1"));
        assertEquals("a revision poll parks for two seconds", List.of(2000L), delays);
        assertEquals("a revision wait sends no request", 0, calls.get());

        accepted.set(true);
        parked.remove(0).run();

        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);
        awaitTrue(action::isGranted, "the grant after the revision wait");
        assertEquals(1, calls.get());
        assertTrue("a revision poll is not a claim attempt: " + requestBodies.get(0),
                requestBodies.get(0).contains("\"attempt\":1"));
        assertEquals(2, derives.get());
        queue.shutdown();
    }

    @Test
    public void aReadyJobClaimsWhileFourClaimsWaitOnARevision() throws Exception {
        responder = n -> new Reply(200, GRANT_BODY);
        AtomicInteger waits = new AtomicInteger();
        VarroaClaimQueue.Deriver deriver = (i, jb, a, now, deadline) -> {
            if (jb.getFullName().startsWith("revision-wait-")) {
                waits.incrementAndGet();
                return null;
            }
            return IntentKey.derive(i, jb, a, now, deadline);
        };
        // The scheduler never fires, so a revision wait lasts forever.
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis, (task, d) -> { },
                VarroaHomeCache.get(), BuildNumberGate.get(), VarroaClaimQueue.CAPACITY, deriver);

        for (int i = 0; i < VarroaClaimQueue.THREADS; i++) {
            FreeStyleProject waiting = j.createFreeStyleProject("revision-wait-" + i);
            queue.offer(scheduleItem(waiting), waiting);
        }
        awaitTrue(() -> waits.get() == VarroaClaimQueue.THREADS, "one revision read per waiting job");

        FreeStyleProject ready = j.createFreeStyleProject("revision-ready");
        Queue.Item item = scheduleItem(ready);
        long offeredAt = System.currentTimeMillis();
        queue.offer(item, ready);

        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);
        awaitTrue(action::isGranted, "the ready job's grant");
        long waited = System.currentTimeMillis() - offeredAt;
        assertTrue("the ready job must not wait behind revision waits, waited " + waited, waited < 5_000L);
        assertEquals("only the ready job posts a claim", 1, calls.get());
        for (int i = 0; i < VarroaClaimQueue.THREADS; i++) {
            assertTrue(queue.isPendingForTesting("revision-wait-" + i + "#run-1"));
        }
        queue.shutdown();
    }

    @Test
    public void aRevisionWaitFallsBackAtThe30SecondBudget() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("revision-stuck");
        Queue.Item item = scheduleBranchItem(project, "rev-stuck");
        responder = n -> new Reply(200, GRANT_BODY);

        IntentKeyDispatchTest.MutableFactory factory = new IntentKeyDispatchTest.MutableFactory("rev-stuck");
        IntentKeyDispatchTest.MultiBranchProject parent = new IntentKeyDispatchTest.MultiBranchProject(factory);
        VarroaClaimQueue.Deriver deriver = (i, jb, a, now, deadline) ->
                IntentKey.derive(i, jb, a, now, deadline, () -> parent);
        long[] clockMillis = {0L};
        List<Long> delays = new CopyOnWriteArrayList<>();
        VarroaClaimQueue.Scheduler scheduler = (task, d) -> {
            delays.add(d);
            clockMillis[0] += d;
            task.run();
        };
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), () -> clockMillis[0], scheduler,
                VarroaHomeCache.get(), BuildNumberGate.get(), VarroaClaimQueue.CAPACITY, deriver);
        queue.offer(item, project);

        VarroaIntentAction action = item.getAction(VarroaIntentAction.class);
        awaitTrue(action::isGranted, "the grant after the fallback");
        assertEquals("fifteen revision polls park for two seconds each", java.util.Collections.nCopies(15, 2000L),
                delays);
        assertEquals("the factory is read at 0, 2, ..., 30 s", 16, factory.reads.get());
        assertEquals(1, calls.get());
        String body = requestBodies.get(0);
        assertEquals(IntentKey.ORIGIN_CELL_LOCAL, extractField(body, "origin"));
        assertTrue(body, extractField(body, "intent_key").startsWith("api:"));
        assertTrue(body, body.contains("\"attempt\":1"));
        queue.shutdown();
    }

    @Test
    public void aRevisionWaitStillEndsAtTheOneHourBudget() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("revision-forever");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, GRANT_BODY);

        VarroaClaimQueue.Deriver deriver = (i, jb, a, now, deadline) -> null;
        long[] clockMillis = {0L};
        VarroaClaimQueue.Scheduler scheduler = (task, d) -> {
            clockMillis[0] += d;
            task.run();
        };
        AtomicInteger resolved = new AtomicInteger();
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), () -> clockMillis[0], scheduler,
                VarroaHomeCache.get(), BuildNumberGate.get(), VarroaClaimQueue.CAPACITY, deriver);
        queue.offer(item, project, resolved::incrementAndGet);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled at the one-hour budget");
        awaitTrue(() -> resolved.get() == 1, "one resolution");
        assertTrue("the cancel must come at the one-hour budget or later, got " + clockMillis[0],
                clockMillis[0] >= VarroaClaimQueue.LIFETIME_MILLIS);
        assertEquals("a revision wait sends no claim", 0, calls.get());
        assertFalse(queue.isPendingForTesting("revision-forever#run-1"));
    }

    @Test
    public void aCancelledItemStopsTheRevisionWait() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("revision-cancelled");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, GRANT_BODY);

        AtomicInteger derives = new AtomicInteger();
        VarroaClaimQueue.Deriver deriver = (i, jb, a, now, deadline) -> {
            derives.incrementAndGet();
            return null;
        };
        List<Runnable> parked = new CopyOnWriteArrayList<>();
        java.util.concurrent.atomic.AtomicBoolean resolved = new java.util.concurrent.atomic.AtomicBoolean(false);
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), System::currentTimeMillis,
                (task, d) -> parked.add(task), VarroaHomeCache.get(), BuildNumberGate.get(),
                VarroaClaimQueue.CAPACITY, deriver);
        queue.offer(item, project, () -> resolved.set(true));

        awaitTrue(() -> parked.size() == 1, "the revision wait to park on the scheduler");
        assertTrue(Queue.getInstance().cancel(item));
        parked.remove(0).run();

        awaitTrue(resolved::get, "the claim to resolve");
        assertEquals("an item that left the queue reads no revision", 1, derives.get());
        assertEquals("an item that left the queue sends no claim", 0, calls.get());
        assertFalse(queue.isPendingForTesting("revision-cancelled#run-1"));
        assertTrue("the claim parks no new poll", parked.isEmpty());
        queue.shutdown();
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

    @Test
    public void cappedVetoRetriesAtFixedInterval() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("capped-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> n <= 3
                ? new Reply(200, "{\"decision\":\"vetoed\",\"reason_code\":\"CAPPED\","
                        + "\"message\":\"waiting on concurrency limit\"}")
                : new Reply(200, "{\"decision\":\"duplicate\"}");

        long[] clockMillis = {0L};
        List<Long> slept = new CopyOnWriteArrayList<>();
        VarroaClaimQueue.Scheduler scheduler = (task, d) -> {
            slept.add(d);
            clockMillis[0] += d;
            task.run();
        };
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), () -> clockMillis[0], scheduler);
        queue.offer(item, project);

        awaitTrue(() -> isCancelled(item), "the item to be cancelled by the duplicate answer");
        assertEquals(4, calls.get());
        assertTrue("every capped retry waits the fixed interval, got " + slept,
                slept.stream().filter(d -> d > 0).allMatch(d -> d == VarroaClaimQueue.CAPPED_RETRY_MILLIS));
    }

    @Test
    public void cappedVetoExtendsLifetimeAndNeverGivesUp() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("capped-forever-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> new Reply(200, "{\"decision\":\"vetoed\",\"reason_code\":\"CAPPED\","
                + "\"message\":\"waiting on concurrency limit\"}");

        long[] clockMillis = {0L};
        // Just past one lifetime.
        long horizon = VarroaClaimQueue.LIFETIME_MILLIS + 20_000L;
        VarroaClaimQueue.Scheduler scheduler = (task, d) -> {
            if (clockMillis[0] >= horizon) {
                return;
            }
            // A one-minute step keeps the retry count small. Each capped answer
            // still moves the deadline, so the one-hour budget never ends the claim.
            clockMillis[0] += 60_000L;
            task.run();
        };
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), () -> clockMillis[0], scheduler);
        queue.offer(item, project);

        awaitTrue(() -> clockMillis[0] >= horizon, "the clock to pass the one-hour budget");
        assertFalse("a capped claim must not give up at the one-hour budget", isCancelled(item));
    }

    @Test
    public void cappedVetoDoesNotCancelItem() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("capped-keeps-job");
        Queue.Item item = scheduleItem(project);
        responder = n -> n == 1
                ? new Reply(200, "{\"decision\":\"vetoed\",\"reason_code\":\"CAPPED\","
                        + "\"message\":\"waiting on concurrency limit\"}")
                : new Reply(200, "{\"decision\":\"duplicate\"}");

        VarroaClaimQueue.Scheduler scheduler = (task, d) -> task.run();
        VarroaClaimQueue queue = new VarroaClaimQueue(newClient(), () -> 0L, scheduler);
        queue.offer(item, project);

        awaitTrue(() -> calls.get() >= 2, "the claim to retry after the capped answer");
        assertFalse("a capped answer must not cancel the item before the retry", calls.get() < 2 && isCancelled(item));
        awaitTrue(() -> isCancelled(item), "the item to be cancelled by the later duplicate");
    }
}
