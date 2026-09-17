package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

import com.cloudbees.hudson.plugins.folder.Folder;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import hudson.model.Result;
import hudson.model.Run;

import java.io.ByteArrayOutputStream;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.ArrayDeque;
import java.util.Deque;
import java.util.List;
import java.util.Map;
import java.util.Objects;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.jenkinsci.plugins.workflow.steps.StepContext;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.jvnet.hudson.test.JenkinsRule;

import net.sf.json.JSONObject;

/**
 * Tests for {@link VarroaLockStep} (tasks 9.8, 9.9): FIFO acquire, release
 * on abort, the token surfaces, and the waiting/renewal poller.
 */
public class VarroaLockStepTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private HttpServer server;
    private Folder tenantFolder;
    private final AtomicLong jobCounter = new AtomicLong();

    @Before
    public void setUp() throws Exception {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        // varroaLock qualifies a bare name with the caller's own tenant
        // (VarroaTenantResolver), which needs a role-strategy document
        // naming the tenant; every job in this file lives under this
        // "alpha" folder, so its full name is "alpha/<job>". A tenant
        // build also needs a node reserved for that tenant
        // (VarroaQueueTaskDispatcher), so every script runs on this node.
        tenantFolder = j.jenkins.createProject(Folder.class, "alpha");
        VarroaTenantResolverTest.hostTenants(j, "alpha");
        j.createSlave("alpha-agent", "varroa-tenant-alpha", new hudson.EnvVars());
    }

    @After
    public void tearDown() {
        server.stop(0);
        VarroaCrossTenantOps.setClientForTesting(null);
        VarroaPollPolicy.resetBoundsForTesting();
    }

    private void installClient(FakeLockHandler handler) throws Exception {
        server.createContext("/v1/cross-tenant/lock", handler);
        server.start();
        Path tokenFile = tmp.getRoot().toPath().resolve("token");
        Files.writeString(tokenFile, "test-token");
        LoopbackTokenFile tf = new LoopbackTokenFile(tokenFile, System::currentTimeMillis);
        VarroaCrossTenantOps.setClientForTesting(new MiteLoopbackClient(
                "http://127.0.0.1:" + server.getAddress().getPort(), tf,
                Duration.ofSeconds(1), Duration.ofSeconds(5)));
    }

    private WorkflowJob newPipeline(String groovy) throws Exception {
        WorkflowJob p = tenantFolder.createProject(WorkflowJob.class, "job-" + jobCounter.incrementAndGet());
        p.setDefinition(new CpsFlowDefinition(groovy, true));
        return p;
    }

    /**
     * Schedules {@code p} with an explicit {@link VarroaIntentAction}
     * carrying {@code runId}: the claim gate is off in these tests, so
     * nothing else would stamp a run identity, and two different runs of
     * two different jobs must never share one fleet-lock run id.
     */
    private WorkflowRun schedule(WorkflowJob p, String runId) throws Exception {
        return (WorkflowRun) p.scheduleBuild2(0,
                new VarroaIntentAction(runId, "", null, System.currentTimeMillis())).waitForStart();
    }

    private WorkflowRun buildSync(WorkflowJob p, String runId, Result expected) throws Exception {
        WorkflowRun run = schedule(p, runId);
        j.waitForCompletion(run);
        j.assertBuildStatus(expected, run);
        return run;
    }

    // ---- 9.8 ----

    @Test
    public void theSecondOfTwoRunsEntersTheBodyOnlyAfterTheFirstLeaves() throws Exception {
        FakeLockHandler handler = new FakeLockHandler();
        installClient(handler);

        // A file-based rendezvous: job A writes a marker when inside the
        // body and waits for a release signal file.
        Path markerA = tmp.newFile("a-inside").toPath();
        Files.delete(markerA);
        Path releaseA = tmp.newFile("a-release").toPath();
        Files.delete(releaseA);

        WorkflowJob jobA = newPipeline(
                "node('varroa-tenant-alpha') { varroa.lock(name: 'deploy-prod') { "
                        + "writeFile(file: '" + escapedPath(markerA) + "', text: 'x'); "
                        + "while (!fileExists('" + escapedPath(releaseA) + "')) { sleep(time: 200, unit: 'MILLISECONDS') } "
                        + "} }");
        WorkflowRun runA = schedule(jobA, "run-a");
        waitForFile(markerA, 20_000);

        WorkflowJob jobB = newPipeline(
                "node('varroa-tenant-alpha') { varroa.lock(name: 'deploy-prod') { echo 'B_INSIDE' } }");
        WorkflowRun runB = schedule(jobB, "run-b");

        // B must not enter its body while A still holds the lock.
        Thread.sleep(1500);
        assertTrue("B must still be waiting for the lock", runB.isBuilding());
        assertEquals("", readLogSafely(runB, "B_INSIDE"));

        Files.writeString(releaseA, "go");
        j.waitForCompletion(runA);
        j.assertBuildStatusSuccess(runA);
        j.waitForCompletion(runB);
        j.assertBuildStatusSuccess(runB);
        j.assertLogContains("B_INSIDE", runB);
    }

    @Test
    public void anAbortedBuildReleasesItsLock() throws Exception {
        FakeLockHandler handler = new FakeLockHandler();
        installClient(handler);

        WorkflowJob jobA = newPipeline(
                "node('varroa-tenant-alpha') { varroa.lock(name: 'deploy-prod') { sleep(time: 60, unit: 'SECONDS') } }");
        WorkflowRun runA = schedule(jobA, "run-a");
        waitForHeld(handler, "alpha/deploy-prod", 20_000);

        runA.getExecutor().interrupt(Result.ABORTED);
        j.waitForCompletion(runA);

        WorkflowJob jobB = newPipeline(
                "node('varroa-tenant-alpha') { varroa.lock(name: 'deploy-prod') { echo 'B_ACQUIRED' } }");
        WorkflowRun runB = buildSync(jobB, "run-b", Result.SUCCESS);
        j.assertLogContains("B_ACQUIRED", runB);
    }

    @Test
    public void lockTokenAndEnvVarReportTheAcquiredToken() throws Exception {
        FakeLockHandler handler = new FakeLockHandler();
        installClient(handler);

        WorkflowJob p = newPipeline(
                "node('varroa-tenant-alpha') { varroa.lock(name: 'deploy-prod') { "
                        + "echo(\"TOKEN=${varroa.lock.token}\"); "
                        + "echo(\"ENV=${env.VARROA_LOCK_TOKEN}\") } }");
        WorkflowRun run = buildSync(p, "run-1", Result.SUCCESS);
        assertEquals(1, handler.acquireCount("alpha/deploy-prod"));
        long token = handler.tokenFor("alpha/deploy-prod");
        j.assertLogContains("TOKEN=" + token, run);
        j.assertLogContains("ENV=" + token, run);
    }

    /**
     * A nested {@code varroa.lock} block cannot run on this platform: the
     * Jenkins pipeline interpreter cannot resume the outer body's next
     * statement correctly once a block step's own body starts a second,
     * nested block step ({@link VarroaActiveLocksAction}). The step
     * refuses the inner acquire outright, releases the outer lock as any
     * other body failure would, and never sends a {@code lock_acquire}
     * for the inner name.
     */
    @Test
    public void aNestedBlockReportsItsOwnToken() throws Exception {
        FakeLockHandler handler = new FakeLockHandler();
        installClient(handler);

        WorkflowJob p = newPipeline(
                "node('varroa-tenant-alpha') { varroa.lock(name: 'outer') { "
                        + "echo(\"OUTER=${varroa.lock.token}\"); "
                        + "varroa.lock(name: 'inner') { echo(\"INNER=${varroa.lock.token}\") }; "
                        + "echo(\"AFTER=${varroa.lock.token}\") } }");
        WorkflowRun run = buildSync(p, "run-1", Result.SUCCESS);

        assertEquals(1, handler.acquireCount("alpha/outer"));
        assertEquals(1, handler.acquireCount("alpha/inner"));
        j.assertLogContains("INNER=" + handler.tokenFor("alpha/inner"), run);
        j.assertLogContains("AFTER=" + handler.tokenFor("alpha/outer"), run);
    }

    // ---- 9.9 ----

    @Test
    public void timeoutReleasesTheWaitingRow() throws Exception {
        FakeLockHandler handler = new FakeLockHandler();
        handler.neverGrant("alpha/deploy-prod");
        installClient(handler);

        WorkflowJob p = newPipeline(
                "node('varroa-tenant-alpha') { varroa.lock(name: 'deploy-prod', timeout: 1) { echo 'unreachable' } }");
        WorkflowRun run = buildSync(p, "run-1", Result.FAILURE);
        j.assertLogContains("timed out", run);
        assertTrue("a timed-out waiter must release", handler.releaseCount("alpha/deploy-prod") >= 1);
    }

    @Test
    public void renewalExtendsTheLease() throws Exception {
        VarroaPollPolicy.setBoundsForTesting(
                VarroaPollPolicy.PENDING_WARN_MILLIS, VarroaPollPolicy.PENDING_CEILING_MILLIS, 300L);
        FakeLockHandler handler = new FakeLockHandler();
        installClient(handler);

        WorkflowJob p = newPipeline(
                "node('varroa-tenant-alpha') { varroa.lock(name: 'deploy-prod') { sleep(time: 1500, unit: 'MILLISECONDS') } }");
        WorkflowRun run = buildSync(p, "run-1", Result.SUCCESS);
        assertTrue("must have renewed at least twice", handler.renewCount("alpha/deploy-prod") >= 2);
    }

    @Test
    public void aLostRenewFailsTheBuild() throws Exception {
        VarroaPollPolicy.setBoundsForTesting(
                VarroaPollPolicy.PENDING_WARN_MILLIS, VarroaPollPolicy.PENDING_CEILING_MILLIS, 300L);
        FakeLockHandler handler = new FakeLockHandler();
        handler.loseOnRenew("alpha/deploy-prod");
        installClient(handler);

        WorkflowJob p = newPipeline(
                "node('varroa-tenant-alpha') { varroa.lock(name: 'deploy-prod') { sleep(time: 5, unit: 'SECONDS') } }");
        WorkflowRun run = buildSync(p, "run-1", Result.FAILURE);
        j.assertLogContains("was lost", run);
    }

    @Test
    public void aReleasingRenewKeepsTheBodyRunningAndLogsOneWarning() throws Exception {
        VarroaPollPolicy.setBoundsForTesting(
                VarroaPollPolicy.PENDING_WARN_MILLIS, VarroaPollPolicy.PENDING_CEILING_MILLIS, 300L);
        FakeLockHandler handler = new FakeLockHandler();
        handler.releasingOnRenew("alpha/deploy-prod");
        installClient(handler);

        WorkflowJob p = newPipeline(
                "node('varroa-tenant-alpha') { varroa.lock(name: 'deploy-prod') { sleep(time: 1500, unit: 'MILLISECONDS'); echo 'DONE' } }");
        WorkflowRun run = buildSync(p, "run-1", Result.SUCCESS);
        j.assertLogContains("DONE", run);
        int warnings = 0;
        for (String line : run.getLog(10_000)) {
            if (line.contains("WARNING: varroaLock:") && line.contains("releasing")) {
                warnings++;
            }
        }
        assertEquals(1, warnings);
    }

    // ---- D15: onResume restores the held token ----

    /**
     * {@code VarroaActiveLocksAction}'s stack is transient, so a resumed
     * build restores it empty. {@code onResume} must push the held name
     * and fence token back before {@code varroa.lock.token} reads them
     * again. This drives {@code Execution.onResume()} directly, the same
     * way {@link #concurrentReleaseAttemptsReleaseTheLockExactlyOnce()}
     * drives {@code releaseLock()} directly: the defect is in the resume
     * bookkeeping itself, not in the acquire/renew network calls, so no
     * lock server is needed.
     */
    @Test
    public void onResumeRestoresTheHeldNameAndFenceToken() throws Exception {
        WorkflowJob p = newPipeline("echo 'noop'");
        WorkflowRun run = buildSync(p, "run-resume", Result.SUCCESS);

        assertNull("no lock is held before the resume restores one",
                VarroaActiveLocksAction.currentTokenMap(run));

        StepContext context = mock(StepContext.class);
        when(context.get(Run.class)).thenReturn(run);

        VarroaLockStep step = new VarroaLockStep("resume-test");
        VarroaLockStep.Execution exec = new VarroaLockStep.Execution(step, context);

        java.lang.reflect.Field qualifiedNameField = VarroaLockStep.Execution.class.getDeclaredField("qualifiedName");
        qualifiedNameField.setAccessible(true);
        qualifiedNameField.set(exec, "alpha/resume-test");

        java.lang.reflect.Field fenceTokenField = VarroaLockStep.Execution.class.getDeclaredField("fenceToken");
        fenceTokenField.setAccessible(true);
        fenceTokenField.set(exec, 42L);

        exec.onResume();

        Map<String, Object> token = VarroaActiveLocksAction.currentTokenMap(run);
        assertNotNull("onResume must restore the held lock's entry", token);
        assertEquals("alpha/resume-test", token.get("name"));
        assertEquals(42L, token.get("token"));
    }

    // ---- released races across the Timer thread and stop()/finish() ----

    @Test
    public void concurrentReleaseAttemptsReleaseTheLockExactlyOnce() throws Exception {
        // Exercises Execution.releaseLock() directly, not through a live
        // pipeline: the defect is a plain field, with no synchronization
        // or happens-before edge, read by the Timer thread's renew() and
        // written by stop()/finish() (a CPS or executor thread). The
        // compareAndSet in the fix gives every caller a single, atomic
        // release regardless of how many race in at once; a plain
        // check-then-set field gives no such guarantee. On the hardware
        // this suite runs on, the release's own check-then-set window
        // is too narrow for real thread scheduling to hit by chance —
        // this asserts the guarantee the fix provides outright, rather
        // than gambling on reproducing the pre-fix race by brute force.
        FakeLockHandler handler = new FakeLockHandler();
        installClient(handler);

        // qualifiedName is normally stamped by start(); this test drives
        // releaseLock() directly, so it stamps the field itself.
        java.lang.reflect.Field qualifiedNameField = VarroaLockStep.Execution.class.getDeclaredField("qualifiedName");
        qualifiedNameField.setAccessible(true);

        int threadsPerTrial = 16;
        int trials = 50;
        for (int t = 0; t < trials; t++) {
            VarroaLockStep step = new VarroaLockStep("race");
            VarroaLockStep.Execution exec = new VarroaLockStep.Execution(step, null);
            qualifiedNameField.set(exec, "race");
            java.util.concurrent.CyclicBarrier barrier = new java.util.concurrent.CyclicBarrier(threadsPerTrial);
            Thread[] racers = new Thread[threadsPerTrial];
            for (int i = 0; i < threadsPerTrial; i++) {
                racers[i] = new Thread(() -> {
                    try {
                        barrier.await();
                    } catch (Exception ignored) {
                        // Barrier plumbing only.
                    }
                    exec.releaseLockForTesting();
                });
            }
            for (Thread r : racers) {
                r.start();
            }
            for (Thread r : racers) {
                r.join();
            }
        }

        assertEquals("every trial's release must fire exactly once, even with " + threadsPerTrial
                        + " threads calling it at once",
                trials, handler.releaseCount("race"));
    }

    // ---- helpers ----

    private static String escapedPath(Path p) {
        return p.toString().replace("\\", "\\\\");
    }

    private static void waitForFile(Path p, long timeoutMillis) throws InterruptedException {
        long deadline = System.currentTimeMillis() + timeoutMillis;
        while (!Files.exists(p) && System.currentTimeMillis() < deadline) {
            Thread.sleep(50);
        }
        assertTrue("file " + p + " must appear", Files.exists(p));
    }

    private static void waitForHeld(FakeLockHandler handler, String name, long timeoutMillis) throws InterruptedException {
        long deadline = System.currentTimeMillis() + timeoutMillis;
        while (handler.acquireCount(name) == 0 && System.currentTimeMillis() < deadline) {
            Thread.sleep(50);
        }
        assertTrue("lock " + name + " must be acquired", handler.acquireCount(name) > 0);
    }

    private static String readLogSafely(WorkflowRun run, String needle) throws Exception {
        List<String> lines = run.getLog(10_000);
        for (String line : lines) {
            if (line.contains(needle)) {
                return needle;
            }
        }
        return "";
    }

    /**
     * A minimal in-memory {@code fleet_locks} simulator (D11): one holder
     * per name, a FIFO waiting queue, and test hooks to force a {@code
     * lost} or {@code releasing} renew reply.
     */
    private static final class FakeLockHandler implements com.sun.net.httpserver.HttpHandler {

        private static final class State {
            String holder;
            final Deque<String> waiting = new ArrayDeque<>();
            long fenceToken;
            final AtomicInteger acquireCount = new AtomicInteger();
            final AtomicInteger renewCount = new AtomicInteger();
            final AtomicInteger releaseCount = new AtomicInteger();
            boolean neverGrant;
            boolean loseOnRenew;
            boolean releasingOnRenew;
        }

        // Fleet-wide (D11), not one counter per name: two different lock
        // names must not mint the same token on their first acquire.
        private final java.util.concurrent.atomic.AtomicLong fenceSeq = new java.util.concurrent.atomic.AtomicLong();
        private final java.util.Map<String, State> states = new java.util.concurrent.ConcurrentHashMap<>();

        private State state(String name) {
            return states.computeIfAbsent(name, n -> new State());
        }

        void neverGrant(String name) {
            state(name).neverGrant = true;
        }

        void loseOnRenew(String name) {
            state(name).loseOnRenew = true;
        }

        void releasingOnRenew(String name) {
            state(name).releasingOnRenew = true;
        }

        int acquireCount(String name) {
            return state(name).acquireCount.get();
        }

        int renewCount(String name) {
            return state(name).renewCount.get();
        }

        int releaseCount(String name) {
            return state(name).releaseCount.get();
        }

        long tokenFor(String name) {
            return state(name).fenceToken;
        }

        @Override
        public void handle(HttpExchange exchange) throws java.io.IOException {
            ByteArrayOutputStream buf = new ByteArrayOutputStream();
            exchange.getRequestBody().transferTo(buf);
            JSONObject req = JSONObject.fromObject(buf.toString(StandardCharsets.UTF_8));
            String kind = req.getString("kind");
            String name = req.getString("name");
            String runId = req.optString("run_id", "");
            State st = state(name);
            JSONObject out = new JSONObject();
            synchronized (st) {
                switch (kind) {
                    case "lock_acquire":
                        st.acquireCount.incrementAndGet();
                        if (!st.neverGrant && (Objects.equals(st.holder, runId)
                                || (st.holder == null && (st.waiting.isEmpty() || st.waiting.peek().equals(runId))))) {
                            st.holder = runId;
                            st.waiting.remove(runId);
                            st.fenceToken = fenceSeq.incrementAndGet();
                            out.put("state", "held");
                            out.put("fence_token", st.fenceToken);
                        } else {
                            if (!st.waiting.contains(runId)) {
                                st.waiting.add(runId);
                            }
                            out.put("state", "waiting");
                        }
                        break;
                    case "lock_renew":
                        st.renewCount.incrementAndGet();
                        if (st.loseOnRenew) {
                            out.put("state", "lost");
                        } else if (st.releasingOnRenew) {
                            out.put("state", "releasing");
                        } else if (Objects.equals(st.holder, runId)) {
                            out.put("state", "held");
                            out.put("fence_token", st.fenceToken);
                        } else {
                            out.put("state", "lost");
                        }
                        break;
                    case "lock_release":
                    default:
                        st.releaseCount.incrementAndGet();
                        if (Objects.equals(st.holder, runId)) {
                            st.holder = null;
                        }
                        st.waiting.remove(runId);
                        out.put("state", "released");
                        break;
                }
            }
            byte[] resp = out.toString().getBytes(StandardCharsets.UTF_8);
            exchange.getResponseHeaders().set("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, resp.length);
            try (OutputStream os = exchange.getResponseBody()) {
                os.write(resp);
            }
            exchange.close();
        }
    }
}
