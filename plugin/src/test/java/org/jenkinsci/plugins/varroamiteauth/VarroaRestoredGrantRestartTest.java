package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import hudson.model.FreeStyleBuild;
import hudson.model.FreeStyleProject;
import hudson.model.Queue;
import hudson.model.queue.CauseOfBlockage;
import hudson.model.queue.QueueTaskDispatcher;
import hudson.model.queue.ScheduleResult;

import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.time.Duration;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.function.BooleanSupplier;
import java.util.function.Function;

import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.jvnet.hudson.test.JenkinsSessionRule;
import org.jvnet.hudson.test.TestExtension;

/**
 * Proves on a real second boot over the same Jenkins home that a queue
 * item restored from disk claims again before it runs. The first session
 * gets a grant and a start mark for one item, and it holds the item in the
 * queue with zero executors. The second session loads that item from
 * {@code queue.xml}.
 */
public class VarroaRestoredGrantRestartTest {

    @Rule
    public JenkinsSessionRule sessions = new JenkinsSessionRule();

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private static final String JOB = "restored";

    private HttpServer server;
    private ExecutorService serverThreads;
    /** Every claim body, oldest first. A start mark never reaches this list. */
    private final List<String> claims = new CopyOnWriteArrayList<>();
    /** Every start-mark body, oldest first. */
    private final List<String> marks = new CopyOnWriteArrayList<>();
    /** Answers one claim or start-mark body. */
    private volatile Function<String, String> responder;

    /**
     * Turns the claim gate on for this test class alone. A null delegate
     * blocks nothing, so each session installs one before it adds an
     * executor.
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
    public void startLedger() throws Exception {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/v1/claim", this::handle);
        serverThreads = Executors.newCachedThreadPool();
        server.setExecutor(serverThreads);
        server.start();
    }

    @After
    public void stopLedger() {
        GateOn.delegate = null;
        if (server != null) {
            server.stop(0);
        }
        if (serverThreads != null) {
            serverThreads.shutdownNow();
        }
    }

    private void handle(HttpExchange exchange) throws java.io.IOException {
        String body = new String(exchange.getRequestBody().readAllBytes(), StandardCharsets.UTF_8);
        if (body.contains("\"start_mark\":true")) {
            marks.add(body);
        } else {
            claims.add(body);
        }
        byte[] resp = responder.apply(body).getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "application/json");
        exchange.sendResponseHeaders(200, resp.length);
        try (OutputStream os = exchange.getResponseBody()) {
            os.write(resp);
        }
        exchange.close();
    }

    private static String grant(String runId, long number) {
        return "{\"decision\":\"granted\",\"run_id\":\"" + runId + "\",\"build_number\":" + number + "}";
    }

    private static String duplicate(String runId) {
        return "{\"decision\":\"duplicate\",\"run_id\":\"" + runId + "\"}";
    }

    /**
     * Installs a fresh dispatcher with a fresh apply gate. A real restart
     * starts with an empty gate, and the process-wide gate outlives a test
     * session.
     */
    private VarroaClaimDispatcher installGate() throws Exception {
        String base = "http://127.0.0.1:" + server.getAddress().getPort();
        Path tokenFile = tmp.getRoot().toPath().resolve("token");
        Files.writeString(tokenFile, "test-token");
        LoopbackTokenFile tf = new LoopbackTokenFile(tokenFile, System::currentTimeMillis);
        MiteLoopbackClient client = new MiteLoopbackClient(base, tf, Duration.ofSeconds(5));
        VarroaClaimQueue queue = new VarroaClaimQueue(client, System::currentTimeMillis, (task, d) -> task.run(),
                VarroaHomeCache.get(), new BuildNumberGate(System::currentTimeMillis, true));
        VarroaClaimDispatcher dispatcher = new VarroaClaimDispatcher(queue);
        GateOn.delegate = dispatcher;
        return dispatcher;
    }

    private static void uninstallGate(VarroaClaimDispatcher dispatcher) {
        GateOn.delegate = null;
        dispatcher.shutdown();
    }

    private static void await(BooleanSupplier condition, String message) throws Exception {
        long deadline = System.currentTimeMillis() + 60_000L;
        while (System.currentTimeMillis() < deadline) {
            if (condition.getAsBoolean()) {
                return;
            }
            Queue.getInstance().scheduleMaintenance();
            Thread.sleep(50L);
        }
        fail("timed out waiting for: " + message);
    }

    /**
     * Schedules one item of {@code JOB} with zero executors, and waits for
     * its grant and its start mark. Answers the item's action.
     */
    private VarroaIntentAction grantHeldItem(org.jvnet.hudson.test.JenkinsRule j, String runId) throws Exception {
        j.jenkins.setNumExecutors(0);
        FreeStyleProject project = j.createFreeStyleProject(JOB);
        VarroaIntentAction action = new VarroaIntentAction(runId, "", null, System.currentTimeMillis());
        ScheduleResult result = Queue.getInstance().schedule2(project, 0, action);
        assertTrue(result.isAccepted());
        await(action::isGranted, "the grant and its start mark");
        assertEquals(1, action.getGrantedNumber());
        assertEquals(1, marks.size());
        return action;
    }

    /** Answers the only queued item, which Jenkins restored from disk. */
    private static VarroaIntentAction restoredAction(String runId) {
        Queue.Item[] items = Queue.getInstance().getItems();
        assertEquals("Jenkins restores the saved item", 1, items.length);
        VarroaIntentAction restored = items[0].getAction(VarroaIntentAction.class);
        assertNotNull(restored);
        assertEquals(runId, restored.getRunId());
        return restored;
    }

    /**
     * The build of a granted item starts, and the pod stops hard before the
     * queue saves again. The next boot restores the item with the run id of
     * a build that is already on disk. That item never builds: the claimer
     * finds the build, cancels the item, and sends no claim.
     */
    @Test
    public void aRestoredItemOfABuiltRunNeverBuildsAgain() throws Throwable {
        String runId = Uuid7.next();
        Path savedQueue = tmp.getRoot().toPath().resolve("queue-granted.xml");
        responder = body -> grant(runId, 1);
        sessions.then(j -> {
            DuplicateRegistry.get().resetForTesting();
            ClaimRegistry.get().resetForTesting();
            VarroaClaimDispatcher dispatcher = installGate();
            grantHeldItem(j, runId);

            // The queue saver writes the granted item, as it does a minute
            // after the item entered the queue.
            Queue.getInstance().save();
            Files.copy(j.jenkins.getRootDir().toPath().resolve("queue.xml"), savedQueue,
                    StandardCopyOption.REPLACE_EXISTING);
            assertTrue(Files.readString(savedQueue, StandardCharsets.UTF_8).contains(runId));

            j.jenkins.setNumExecutors(1);
            FreeStyleProject project = j.jenkins.getItemByFullName(JOB, FreeStyleProject.class);
            await(() -> project.getBuildByNumber(1) != null, "build 1");
            j.waitUntilNoActivity();
            FreeStyleBuild built = project.getBuildByNumber(1);
            assertEquals(runId, built.getAction(VarroaIntentAction.class).getRunId());
            j.jenkins.setNumExecutors(0);
            uninstallGate(dispatcher);
        });

        // A hard stop loses the queue save that follows the start of build 1.
        Files.copy(savedQueue, sessions.getHome().toPath().resolve("queue.xml"), StandardCopyOption.REPLACE_EXISTING);
        int claimsBefore = claims.size();
        responder = body -> duplicate(runId);

        sessions.then(j -> {
            DuplicateRegistry.get().resetForTesting();
            ClaimRegistry.get().resetForTesting();
            VarroaIntentAction restored = restoredAction(runId);
            assertFalse("a restored item holds no grant", restored.isGranted());
            assertEquals("no executor runs before the gate is on", 0, j.jenkins.getNumExecutors());

            VarroaClaimDispatcher dispatcher = installGate();
            j.jenkins.setNumExecutors(1);
            await(() -> Queue.getInstance().getItems().length == 0, "the restored item to leave the queue");
            j.waitUntilNoActivity();

            FreeStyleProject project = j.jenkins.getItemByFullName(JOB, FreeStyleProject.class);
            assertNull("the run builds once", project.getBuildByNumber(2));
            assertEquals(2, project.getNextBuildNumber());
            assertEquals("the claimer sends no claim for a run that built", claimsBefore, claims.size());
            assertTrue(DuplicateRegistry.get().contains(runId));
            uninstallGate(dispatcher);
        });
    }

    /**
     * A granted item waits in the queue, and Jenkins stops before its build
     * starts. The next boot restores the item with no grant. The item claims
     * again under its saved intent key and builds once, under its granted
     * number.
     */
    @Test
    public void aRestoredItemOfAnUnstartedRunClaimsAgainAndBuildsOnce() throws Throwable {
        String runId = Uuid7.next();
        String[] intentKey = new String[1];
        responder = body -> grant(runId, 1);
        sessions.then(j -> {
            DuplicateRegistry.get().resetForTesting();
            ClaimRegistry.get().resetForTesting();
            VarroaClaimDispatcher dispatcher = installGate();
            intentKey[0] = grantHeldItem(j, runId).getIntentKey();
            assertFalse(intentKey[0].isEmpty());
            uninstallGate(dispatcher);
        });

        int claimsBefore = claims.size();
        int marksBefore = marks.size();

        sessions.then(j -> {
            DuplicateRegistry.get().resetForTesting();
            ClaimRegistry.get().resetForTesting();
            VarroaIntentAction restored = restoredAction(runId);
            assertFalse("a restored item holds no grant", restored.isGranted());
            assertEquals("no executor runs before the gate is on", 0, j.jenkins.getNumExecutors());

            VarroaClaimDispatcher dispatcher = installGate();
            j.jenkins.setNumExecutors(1);
            FreeStyleProject project = j.jenkins.getItemByFullName(JOB, FreeStyleProject.class);
            await(() -> project.getBuildByNumber(1) != null, "build 1");
            j.waitUntilNoActivity();

            assertEquals(runId, project.getBuildByNumber(1).getAction(VarroaIntentAction.class).getRunId());
            assertNull("the run builds once", project.getBuildByNumber(2));
            assertEquals("the restored item claims again", claimsBefore + 1, claims.size());
            String claim = claims.get(claimsBefore);
            assertTrue(claim, claim.contains("\"intent_key\":\"" + intentKey[0] + "\""));
            assertTrue(claim, claim.contains("\"jenkins_next_build_number\":1"));
            assertEquals("the restored item marks its start again", marksBefore + 1, marks.size());
            uninstallGate(dispatcher);
        });
    }
}
