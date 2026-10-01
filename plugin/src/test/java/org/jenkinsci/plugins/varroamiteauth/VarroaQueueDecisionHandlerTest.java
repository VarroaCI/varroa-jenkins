package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import com.cloudbees.hudson.plugins.folder.Folder;
import com.sun.net.httpserver.HttpServer;

import hudson.ExtensionList;
import hudson.model.Action;
import hudson.model.FreeStyleProject;
import hudson.model.Queue;
import hudson.model.queue.ScheduleResult;
import hudson.security.ACL;
import hudson.security.ACLContext;

import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicReference;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.jvnet.hudson.test.JenkinsRule;
import org.springframework.security.authentication.UsernamePasswordAuthenticationToken;
import org.springframework.security.core.authority.SimpleGrantedAuthority;

/**
 * Tests for {@link VarroaQueueDecisionHandler}: the gate-off path, the
 * granted-action path, the refused-run-id path, and that the handler
 * itself makes no network call — only a claimer thread does (D4).
 */
public class VarroaQueueDecisionHandlerTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    @Rule
    public HomeForwardHarness homes = new HomeForwardHarness();

    @Before
    public void resetRegistries() {
        DuplicateRegistry.get().resetForTesting();
        PluginCounters.resetForTesting();
    }

    @After
    public void cleanup() {
        DuplicateRegistry.get().resetForTesting();
    }

    @Test
    public void gateOffAdmitsWithNoActionStamped() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        VarroaQueueDecisionHandler handler = new VarroaQueueDecisionHandler();
        List<Action> actions = new ArrayList<>();

        assertTrue(handler.shouldSchedule(project, actions, null));
        assertTrue("no action should be stamped while the gate is off", actions.isEmpty());
    }

    @Test
    public void anAlreadyGrantedActionAdmitsAtOnce() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        VarroaQueueDecisionHandler handler = new VarroaQueueDecisionHandler();
        VarroaIntentAction granted = new VarroaIntentAction("run-1", "", null, 0L);
        granted.setGranted(true);
        List<Action> actions = new ArrayList<>(List.of(granted));

        assertTrue(handler.shouldSchedule(project, actions, "1"));
        // No second action added.
        assertEquals(1, actions.size());
    }

    @Test
    public void aKnownDuplicateRunIdIsRefused() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        DuplicateRegistry.get().record("run-dup");
        VarroaQueueDecisionHandler handler = new VarroaQueueDecisionHandler();
        VarroaIntentAction existing = new VarroaIntentAction("run-dup", "", null, 0L);
        List<Action> actions = new ArrayList<>(List.of(existing));

        assertFalse(handler.shouldSchedule(project, actions, "1"));
    }

    @Test
    public void aFreshItemIsStampedWithAnUngrantedAction() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        VarroaQueueDecisionHandler handler = new VarroaQueueDecisionHandler();
        List<Action> actions = new ArrayList<>();

        assertTrue(handler.shouldSchedule(project, actions, "1"));
        assertEquals(1, actions.size());
        VarroaIntentAction stamped = (VarroaIntentAction) actions.get(0);
        assertFalse(stamped.getRunId().isEmpty());
        assertFalse(stamped.isGranted());
        assertEquals("", stamped.getDeliveryId());
    }

    // ---- 9.3: the claim actor derivation ----

    @Test
    public void aStampedActorIsNeverOverwritten() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        VarroaQueueDecisionHandler handler = new VarroaQueueDecisionHandler();
        VarroaIntentAction stamped = new VarroaIntentAction("run-1", "", null, 0L);
        stamped.setActor("varroa-upstream/payments");
        List<Action> actions = new ArrayList<>(List.of(stamped));

        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "alice", "", List.of(new SimpleGrantedAuthority("authenticated"))))) {
            assertTrue(handler.shouldSchedule(project, actions, "1"));
        }

        assertEquals("varroa-upstream/payments", stamped.getActor());
    }

    @Test
    public void aSystemTriggerDerivesAnEmptyActor() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        VarroaQueueDecisionHandler handler = new VarroaQueueDecisionHandler();
        List<Action> actions = new ArrayList<>();

        try (ACLContext ctx = ACL.as2(ACL.SYSTEM2)) {
            assertTrue(handler.shouldSchedule(project, actions, "1"));
        }

        assertEquals(1, actions.size());
        VarroaIntentAction stamped = (VarroaIntentAction) actions.get(0);
        assertEquals("", stamped.getActor());
    }

    @Test
    public void aHumanTriggerDerivesTheAuthenticatedName() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        VarroaQueueDecisionHandler handler = new VarroaQueueDecisionHandler();
        List<Action> actions = new ArrayList<>();

        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "alice", "", List.of(new SimpleGrantedAuthority("authenticated"))))) {
            assertTrue(handler.shouldSchedule(project, actions, "1"));
        }

        assertEquals(1, actions.size());
        VarroaIntentAction stamped = (VarroaIntentAction) actions.get(0);
        assertEquals("alice", stamped.getActor());
    }

    @Test
    public void anAnonymousTriggerDerivesAnEmptyActor() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        VarroaQueueDecisionHandler handler = new VarroaQueueDecisionHandler();
        List<Action> actions = new ArrayList<>();

        try (ACLContext ctx = ACL.as2(jenkins.model.Jenkins.ANONYMOUS2)) {
            assertTrue(handler.shouldSchedule(project, actions, "1"));
        }

        assertEquals(1, actions.size());
        VarroaIntentAction stamped = (VarroaIntentAction) actions.get(0);
        assertEquals("", stamped.getActor());
    }

    @Test
    public void aNonJobTaskAdmitsAtOnce() throws Exception {
        VarroaQueueDecisionHandler handler = new VarroaQueueDecisionHandler();
        Queue.Task notAJob = mockNonJobTask();
        List<Action> actions = new ArrayList<>();

        assertTrue(handler.shouldSchedule(notAJob, actions, "1"));
        assertTrue(actions.isEmpty());
    }

    private static Queue.Task mockNonJobTask() {
        return org.mockito.Mockito.mock(Queue.Task.class);
    }

    // ---- The claim reaches the mite only from a claimer thread ----

    @Test
    public void theHandlerMakesNoCallAndOnlyAClaimerThreadReachesTheMite() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();

        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        AtomicInteger hits = new AtomicInteger();
        server.createContext("/v1/claim", exchange -> {
            // A start mark follows the grant on the same route; only claims count.
            String body = new String(exchange.getRequestBody().readAllBytes(), StandardCharsets.UTF_8);
            if (!body.contains("\"start_mark\":true")) {
                hits.incrementAndGet();
            }
            byte[] resp = "{\"decision\":\"granted\",\"run_id\":\"run-1\",\"build_number\":1}"
                    .getBytes(StandardCharsets.UTF_8);
            exchange.getResponseHeaders().set("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, resp.length);
            try (OutputStream os = exchange.getResponseBody()) {
                os.write(resp);
            }
            exchange.close();
        });
        server.start();
        try {
            // Step 1: the handler runs on this (the test/handler) thread
            // and must reach the network zero times.
            VarroaQueueDecisionHandler handler = new VarroaQueueDecisionHandler();
            List<Action> actions = new ArrayList<>();
            assertTrue(handler.shouldSchedule(project, actions, "1"));
            assertEquals("the handler must perform no I/O", 0, hits.get());
            String handlerThreadName = Thread.currentThread().getName();

            // Step 2: schedule the item for real, carrying the stamped
            // action, and let a VarroaClaimQueue claimer thread claim it.
            ScheduleResult result = Queue.getInstance().schedule2(project, 300, actions.toArray(new Action[0]));
            Queue.Item item = result.getItem();

            Path tokenFile = tmp.getRoot().toPath().resolve("token");
            Files.writeString(tokenFile, "test-token");
            LoopbackTokenFile tf = new LoopbackTokenFile(tokenFile, System::currentTimeMillis);
            MiteLoopbackClient client = new MiteLoopbackClient(
                    "http://127.0.0.1:" + server.getAddress().getPort(), tf, Duration.ofSeconds(5));
            // The clock runs on whichever thread calls VarroaClaimQueue's
            // internals, so it reliably names the claimer thread — unlike
            // the stub server's own handler thread, which never carries
            // the caller's identity.
            AtomicReference<String> claimerThreadName = new AtomicReference<>();
            VarroaClaimQueue.Clock clock = () -> {
                claimerThreadName.set(Thread.currentThread().getName());
                return System.currentTimeMillis();
            };
            VarroaClaimQueue claimQueue = new VarroaClaimQueue(client, clock, (task, d) -> task.run());

            claimQueue.offer(item, project);

            long deadline = System.currentTimeMillis() + 10_000L;
            while (System.currentTimeMillis() < deadline && hits.get() == 0) {
                Thread.sleep(20L);
            }
            assertEquals("the claim must reach the mite exactly once", 1, hits.get());
            assertNotNull("the claimer thread must have run", claimerThreadName.get());
            assertNotEquals("the claim must not run on the handler's thread",
                    handlerThreadName, claimerThreadName.get());
            assertTrue("the claim must run on a claim-queue daemon thread",
                    claimerThreadName.get().startsWith("varroa-claim-queue-"));
        } finally {
            server.stop(0);
        }
    }

    // ---- The waiting-step refusal ----

    /**
     * Registers a second handler that forces the gate on and reads the
     * seeded cache. The test process cannot set an environment variable,
     * and the gate value is the one input this path must not read from the
     * environment under test.
     *
     * <p>The registered handler stays in place. It admits every item with
     * the gate off, and the queue refuses on the first {@code false}, so
     * the two together answer exactly as one gate-on handler answers.
     */
    private void installGateOnHandler() {
        ExtensionList<Queue.QueueDecisionHandler> all = Queue.QueueDecisionHandler.all();
        all.add(0, new GateOnHandler(homes.cache()));
    }

    private static final class GateOnHandler extends VarroaQueueDecisionHandler {
        GateOnHandler(VarroaHomeCache cache) {
            super(cache);
        }

        @Override
        public boolean shouldSchedule(Queue.Task task, List<Action> actions) {
            return shouldSchedule(task, actions, "1");
        }
    }

    /** One job inside a tenant folder, so its full path opens with the tenant. */
    private <T extends hudson.model.TopLevelItem> T tenantItem(Folder folder, Class<T> type, String name)
            throws Exception {
        return folder.createProject(type, name);
    }

    @Test
    public void aWaitingBuildStepForANonHomeJobFailsAtOnce() throws Exception {
        Folder folder = j.jenkins.createProject(Folder.class, "payments");
        tenantItem(folder, FreeStyleProject.class, "deploy");
        WorkflowJob caller = tenantItem(folder, WorkflowJob.class, "caller");
        caller.setDefinition(new CpsFlowDefinition("build job: 'deploy', wait: true", true));
        homes.seedHomes("payments", 0, "active", java.util.Map.of("payments/deploy", 1));
        installGateOnHandler();

        WorkflowRun run = j.assertBuildStatus(hudson.model.Result.FAILURE,
                caller.scheduleBuild2(0));

        assertEquals("the refusal must be counted once", Long.valueOf(1L),
                PluginCounters.snapshot().get(PluginCounters.FORWARD_REFUSED));
        assertNotNull(run);
    }

    @Test
    public void aRefusedWaitingStepWritesTheNoticeOnTheCallerConsole() throws Exception {
        Folder folder = j.jenkins.createProject(Folder.class, "payments");
        tenantItem(folder, FreeStyleProject.class, "deploy");
        WorkflowJob caller = tenantItem(folder, WorkflowJob.class, "caller");
        caller.setDefinition(new CpsFlowDefinition(
                "try { build job: 'deploy', wait: true } catch (e) { echo 'refused' }\nsleep 3", true));
        homes.seedHomes("payments", 0, "active", java.util.Map.of("payments/deploy", 1));
        installGateOnHandler();

        WorkflowRun run = j.buildAndAssertSuccess(caller);

        j.assertLogContains("job payments/deploy runs on another slot of this tenant.", run);
        j.assertLogContains("Use varroa.build(job: 'deploy', wait: true) instead.", run);
    }

    @Test
    public void aStepThatWaitsForStartIsRefusedTheSameWay() throws Exception {
        Folder folder = j.jenkins.createProject(Folder.class, "payments");
        tenantItem(folder, FreeStyleProject.class, "deploy");
        WorkflowJob caller = tenantItem(folder, WorkflowJob.class, "caller");
        caller.setDefinition(new CpsFlowDefinition("build job: 'deploy', waitForStart: true", true));
        homes.seedHomes("payments", 0, "active", java.util.Map.of("payments/deploy", 1));
        installGateOnHandler();

        j.assertBuildStatus(hudson.model.Result.FAILURE, caller.scheduleBuild2(0));
    }

    @Test
    public void aStepThatDoesNotWaitIsAdmitted() throws Exception {
        Folder folder = j.jenkins.createProject(Folder.class, "payments");
        FreeStyleProject target = tenantItem(folder, FreeStyleProject.class, "deploy");
        WorkflowJob caller = tenantItem(folder, WorkflowJob.class, "caller");
        caller.setDefinition(new CpsFlowDefinition("build job: 'deploy', wait: false", true));
        homes.seedHomes("payments", 0, "active", java.util.Map.of("payments/deploy", 1));
        installGateOnHandler();

        j.buildAndAssertSuccess(caller);

        assertEquals("a step that does not wait must be admitted, not refused", Long.valueOf(0L),
                PluginCounters.snapshot().get(PluginCounters.FORWARD_REFUSED));
        assertNotNull(target);
    }

    // ---- 8.5: the control case, a job this cell homes ----

    @Test
    public void aWaitingStepForAHomeJobIsUntouched() throws Exception {
        Folder folder = j.jenkins.createProject(Folder.class, "payments");
        tenantItem(folder, FreeStyleProject.class, "build");
        WorkflowJob caller = tenantItem(folder, WorkflowJob.class, "caller");
        caller.setDefinition(new CpsFlowDefinition("build job: 'build', wait: true", true));
        homes.seedHomes("payments", 0, "active", java.util.Map.of("payments/build", 0));
        installGateOnHandler();

        j.buildAndAssertSuccess(caller);

        assertEquals("a home job must not be refused", Long.valueOf(0L),
                PluginCounters.snapshot().get(PluginCounters.FORWARD_REFUSED));
    }

    // ---- 8.8b: the two notice failure paths ----

    @Test
    public void aRejectedHandOffStillLogsTheLineAndStillRefuses() throws Exception {
        Folder folder = j.jenkins.createProject(Folder.class, "payments");
        FreeStyleProject target = tenantItem(folder, FreeStyleProject.class, "deploy");
        homes.seedHomes("payments", 0, "active", java.util.Map.of("payments/deploy", 1));

        List<String> warnings = captureWarnings();
        CountDownLatch release = new CountDownLatch(1);
        fillNoticeExecutor(release);
        try {
            VarroaQueueDecisionHandler handler = new VarroaQueueDecisionHandler(homes.cache());
            List<Action> actions = new ArrayList<>();
            actions.add(new BuildTriggerAction());

            assertFalse("a rejected hand-off must still refuse",
                    handler.shouldSchedule(target, actions, "1"));
            assertTrue("the rejected notice must reach the system log: " + warnings,
                    warnings.stream().anyMatch(line -> line.contains("Use varroa.build(job: 'deploy'")));
        } finally {
            release.countDown();
        }
    }

    @Test
    public void aThrownHandOffStillRefuses() throws Exception {
        Folder folder = j.jenkins.createProject(Folder.class, "payments");
        FreeStyleProject target = tenantItem(folder, FreeStyleProject.class, "deploy");
        homes.seedHomes("payments", 0, "active", java.util.Map.of("payments/deploy", 1));

        hudson.model.CauseAction throwing = org.mockito.Mockito.mock(hudson.model.CauseAction.class);
        org.mockito.Mockito.when(throwing.getCauses()).thenThrow(new RuntimeException("boom"));

        VarroaQueueDecisionHandler handler = new VarroaQueueDecisionHandler(homes.cache());
        List<Action> actions = new ArrayList<>();
        actions.add(new BuildTriggerAction());
        actions.add(throwing);

        assertFalse("a thrown hand-off must still refuse",
                handler.shouldSchedule(target, actions, "1"));
    }

    @Test
    public void theNoticeNamesEachCallInItsOwnPathForm() {
        String notice = VarroaQueueDecisionHandler.noticeFor("payments/api/deploy");

        assertTrue("build job: takes the job's own path: " + notice,
                notice.contains("`build job: 'payments/api/deploy', wait: true`"));
        assertTrue("varroa.build takes the path below the tenant folder: " + notice,
                notice.contains("Use varroa.build(job: 'api/deploy', wait: true) instead."));
    }

    /** A stand-in action whose class simple name is the one the handler matches. */
    private static final class BuildTriggerAction implements Action {
        @Override
        public String getIconFileName() {
            return null;
        }

        @Override
        public String getDisplayName() {
            return null;
        }

        @Override
        public String getUrlName() {
            return null;
        }
    }

    /** Fills the notice executor's one thread and its whole queue. */
    private static void fillNoticeExecutor(CountDownLatch release) throws Exception {
        CountDownLatch started = new CountDownLatch(1);
        VarroaQueueDecisionHandler.NOTICE_EXECUTOR.execute(() -> {
            started.countDown();
            try {
                release.await();
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            }
        });
        assertTrue("the blocking notice task must start", started.await(10, java.util.concurrent.TimeUnit.SECONDS));
        for (int i = 0; i < 32; i++) {
            VarroaQueueDecisionHandler.NOTICE_EXECUTOR.execute(() -> { });
        }
    }

    /** Collects every WARNING the handler logs while the test runs. */
    private List<String> captureWarnings() {
        List<String> lines = new java.util.concurrent.CopyOnWriteArrayList<>();
        java.util.logging.Logger logger =
                java.util.logging.Logger.getLogger(VarroaQueueDecisionHandler.class.getName());
        java.util.logging.Handler handler = new java.util.logging.Handler() {
            @Override
            public void publish(java.util.logging.LogRecord record) {
                if (record.getLevel().intValue() >= java.util.logging.Level.WARNING.intValue()) {
                    lines.add(record.getMessage());
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
        noticeLogHandlers.add(handler);
        return lines;
    }

    private final List<java.util.logging.Handler> noticeLogHandlers = new ArrayList<>();

    @After
    public void removeLogHandlers() {
        java.util.logging.Logger logger =
                java.util.logging.Logger.getLogger(VarroaQueueDecisionHandler.class.getName());
        for (java.util.logging.Handler handler : noticeLogHandlers) {
            logger.removeHandler(handler);
        }
        noticeLogHandlers.clear();
    }
}
