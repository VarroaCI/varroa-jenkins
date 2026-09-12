package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import com.sun.net.httpserver.HttpServer;

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
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicReference;

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

    @Before
    public void resetRegistries() {
        DuplicateRegistry.get().resetForTesting();
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
            hits.incrementAndGet();
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
            VarroaClaimQueue claimQueue = new VarroaClaimQueue(client, clock, millis -> { });

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
}
