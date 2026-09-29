package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import hudson.model.FreeStyleBuild;
import hudson.model.FreeStyleProject;
import hudson.model.Queue;
import hudson.model.Run;
import hudson.model.queue.CauseOfBlockage;
import hudson.model.queue.QueueTaskDispatcher;
import hudson.model.queue.ScheduleResult;

import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.function.BiFunction;
import java.util.function.BooleanSupplier;

import jenkins.model.Jenkins;

import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.jvnet.hudson.test.JenkinsRule;
import org.jvnet.hudson.test.TestExtension;

/**
 * Tests the claim gate under a burst of claims into one job: the number a
 * run starts with is the number its claim was granted, a grant that
 * arrives after a higher one is claimed again instead of cancelled, and a
 * job reload keeps an applied grant.
 */
public class ClaimBurstBuildNumberTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private static final int QUIET_PERIOD = 0;

    private HttpServer server;
    private final List<String> bodies = new CopyOnWriteArrayList<>();
    /** Answers one claim body for a run id; the answer is the reply JSON. */
    private volatile BiFunction<String, String, String> responder;

    /**
     * Turns the claim gate on for this test class alone: it hands every
     * dispatch decision to a real {@link VarroaClaimDispatcher} with the gate
     * value "1", so an item that holds no grant stays blocked.
     */
    @TestExtension
    public static class GateOn extends QueueTaskDispatcher {
        static volatile VarroaClaimDispatcher delegate;

        @Override
        public CauseOfBlockage canRun(Queue.Item item) {
            VarroaClaimDispatcher d = delegate;
            return d == null ? null : d.canRun(item, "1");
        }
    }

    @Before
    public void setUp() throws Exception {
        DuplicateRegistry.get().resetForTesting();
        ClaimRegistry.get().resetForTesting();
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/v1/claim", this::handle);
        server.setExecutor(java.util.concurrent.Executors.newCachedThreadPool());
        server.start();
    }

    @After
    public void tearDown() {
        GateOn.delegate = null;
        if (server != null) {
            server.stop(0);
        }
    }

    private void handle(HttpExchange exchange) throws java.io.IOException {
        String body = new String(exchange.getRequestBody().readAllBytes(), StandardCharsets.UTF_8);
        bodies.add(body);
        String reply = responder.apply(field(body, "run_id"), body);
        byte[] resp = reply.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "application/json");
        exchange.sendResponseHeaders(200, resp.length);
        try (OutputStream os = exchange.getResponseBody()) {
            os.write(resp);
        }
        exchange.close();
    }

    private static String field(String body, String name) {
        String marker = "\"" + name + "\":\"";
        int start = body.indexOf(marker);
        if (start < 0) {
            return "";
        }
        start += marker.length();
        return body.substring(start, body.indexOf('"', start));
    }

    private static String grant(String runId, long number) {
        return "{\"decision\":\"granted\",\"run_id\":\"" + runId + "\",\"build_number\":" + number + "}";
    }

    private void installGate() throws Exception {
        String base = "http://127.0.0.1:" + server.getAddress().getPort();
        Path tokenFile = tmp.getRoot().toPath().resolve("token");
        Files.writeString(tokenFile, "test-token");
        LoopbackTokenFile tf = new LoopbackTokenFile(tokenFile, System::currentTimeMillis);
        MiteLoopbackClient client = new MiteLoopbackClient(base, tf, Duration.ofSeconds(5));
        VarroaClaimQueue queue = new VarroaClaimQueue(client, System::currentTimeMillis, millis -> Thread.sleep(50L));
        GateOn.delegate = new VarroaClaimDispatcher(queue);
    }

    private static Queue.Item schedule(FreeStyleProject project, String runId) {
        VarroaIntentAction action = new VarroaIntentAction(runId, "", null, System.currentTimeMillis());
        ScheduleResult result = Queue.getInstance().schedule2(project, QUIET_PERIOD, action);
        assertTrue(result.isAccepted());
        return result.getItem();
    }

    private static void await(BooleanSupplier condition, String message) throws Exception {
        long deadline = System.currentTimeMillis() + 30_000L;
        while (System.currentTimeMillis() < deadline) {
            if (condition.getAsBoolean()) {
                return;
            }
            Queue.getInstance().scheduleMaintenance();
            Thread.sleep(50L);
        }
        StringBuilder seen = new StringBuilder();
        for (hudson.model.Job<?, ?> job : Jenkins.get().getAllItems(hudson.model.Job.class)) {
            for (Run<?, ?> r : job.getBuilds()) {
                VarroaIntentAction a = r.getAction(VarroaIntentAction.class);
                seen.append(' ').append(r.getFullDisplayName()).append('=')
                        .append(a == null ? "no-action" : a.getRunId() + "/granted=" + a.isGranted());
            }
        }
        fail("timed out waiting for: " + message + "; builds:" + seen);
    }

    /** Answers the build of {@code project} whose intent action names {@code runId}, or null. */
    private static FreeStyleBuild buildOf(FreeStyleProject project, String runId) {
        for (FreeStyleBuild b : project.getBuilds()) {
            VarroaIntentAction a = b.getAction(VarroaIntentAction.class);
            if (a != null && runId.equals(a.getRunId())) {
                return b;
            }
        }
        return null;
    }

    /**
     * Two grants of one job arrive from four claimer threads in any order.
     * Every run must start with the number its own grant applied, and no
     * item is cancelled: a grant below the counter claims again, and the
     * stub ledger then answers a fresh number, as the recorder does.
     */
    @Test
    public void twoGrantsBeforeADispatchEachStartWithItsOwnNumber() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("burst-two");
        project.setConcurrentBuild(true);
        Map<String, Long> grants = new ConcurrentHashMap<>(Map.of("run-a", 5L, "run-b", 6L));
        java.util.concurrent.atomic.AtomicLong fresh = new java.util.concurrent.atomic.AtomicLong(7L);
        responder = (runId, body) -> body.contains("\"number_conflict\"")
                ? grant(runId, grants.merge(runId, fresh.getAndIncrement(), (old, n) -> n))
                : grant(runId, grants.get(runId));
        installGate();

        Queue.Item a = schedule(project, "run-a");
        Queue.Item b = schedule(project, "run-b");

        await(() -> buildOf(project, "run-a") != null && buildOf(project, "run-b") != null, "both builds");
        j.waitUntilNoActivity();
        assertFalse(a.getFuture().isCancelled());
        assertFalse(b.getFuture().isCancelled());
        for (String runId : new String[] {"run-a", "run-b"}) {
            FreeStyleBuild build = buildOf(project, runId);
            long applied = build.getAction(VarroaIntentAction.class).getGrantedNumber();
            assertEquals(runId + " starts with the grant it applied", applied, build.getNumber());
            assertEquals(runId + " applied the ledger's newest answer", (long) grants.get(runId), applied);
        }
    }

    /**
     * Two grants of one job apply in claim order before the queue runs a
     * dispatch pass. Each run must still start with its own number: a grant
     * binds its number to its own item, not to the next dispatch.
     */
    @Test
    public void grantsAppliedInOrderBeforeADispatchStartWithTheirOwnNumbers() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("burst-ordered");
        project.setConcurrentBuild(true);
        Map<String, Queue.Item> items = new ConcurrentHashMap<>();
        responder = (runId, body) -> {
            if ("run-b".equals(runId)) {
                // Answer run-b only once run-a holds its grant, so the two
                // applies run in claim order.
                long deadline = System.currentTimeMillis() + 20_000L;
                while (System.currentTimeMillis() < deadline
                        && !items.get("run-a").getAction(VarroaIntentAction.class).isGranted()) {
                    try {
                        Thread.sleep(5L);
                    } catch (InterruptedException e) {
                        Thread.currentThread().interrupt();
                        break;
                    }
                }
                return grant(runId, 6);
            }
            return grant(runId, 5);
        };
        installGate();

        items.put("run-a", schedule(project, "run-a"));
        items.put("run-b", schedule(project, "run-b"));

        await(() -> buildOf(project, "run-a") != null && buildOf(project, "run-b") != null, "both builds");
        j.waitUntilNoActivity();
        assertEquals("run-a starts with its grant", 5, buildOf(project, "run-a").getNumber());
        assertEquals("run-b starts with its grant", 6, buildOf(project, "run-b").getNumber());
    }

    /**
     * A grant that reaches the job after a higher grant started a build is
     * below the job's counter. The item claims again and names the number
     * it could not apply, and it builds under the fresh grant. It is never
     * cancelled.
     */
    @Test
    public void aGrantBelowTheCounterClaimsAgainInsteadOfCancelling() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("burst-late");
        project.setConcurrentBuild(true);
        responder = (runId, body) -> {
            if ("run-b".equals(runId)) {
                return grant(runId, 6);
            }
            if (body.contains("\"number_conflict\":5")) {
                return grant(runId, 8);
            }
            // run-a: hold the first answer until run-b started with 6.
            long deadline = System.currentTimeMillis() + 20_000L;
            while (System.currentTimeMillis() < deadline && buildOf(project, "run-b") == null) {
                try {
                    Thread.sleep(20L);
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                    break;
                }
            }
            return grant(runId, 5);
        };
        installGate();

        Queue.Item a = schedule(project, "run-a");
        schedule(project, "run-b");

        await(() -> buildOf(project, "run-b") != null, "run-b to start");
        await(() -> buildOf(project, "run-a") != null || a.getFuture().isCancelled(), "run-a to start or cancel");
        assertFalse("a grant below the counter must not cancel the item", a.getFuture().isCancelled());
        j.waitUntilNoActivity();
        assertEquals(6, buildOf(project, "run-b").getNumber());
        assertEquals("run-a builds under the fresh grant", 8, buildOf(project, "run-a").getNumber());
        assertTrue("the second claim names the number it could not apply",
                bodies.stream().anyMatch(b -> b.contains("\"run_id\":\"run-a\"") && b.contains("\"number_conflict\":5")));
    }

    /**
     * The seed reload re-reads the job from disk. An applied grant is on
     * disk, so the reload keeps it.
     */
    @Test
    public void aReloadAfterTheApplyKeepsTheGrant() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("burst-reload");
        j.buildAndAssertSuccess(project);
        VarroaClaimQueue queue = new VarroaClaimQueue(new MiteLoopbackClient(), System::currentTimeMillis, m -> { });

        assertTrue(queue.applyBuildNumber(project, 7));
        project.doReload();

        assertEquals(7, project.getNextBuildNumber());
        FreeStyleBuild next = j.buildAndAssertSuccess(project);
        assertEquals(7, next.getNumber());
        Run<?, ?> first = project.getBuildByNumber(1);
        assertNotNull("the reload keeps the earlier build", first);
    }
}
