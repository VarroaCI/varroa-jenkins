package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.*;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import hudson.model.Action;
import hudson.model.FreeStyleProject;
import hudson.model.Queue;
import hudson.model.queue.CauseOfBlockage;
import hudson.model.queue.ScheduleResult;

import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;

import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.jvnet.hudson.test.JenkinsRule;
import org.mockito.ArgumentCaptor;

/**
 * Tests for {@link VarroaClaimDispatcher}: the block, one offer per run
 * id, a retried offer after a refusal, the release on grant, the gate-off
 * path, the missing-action stamp, and the started map's lifecycle (D4,
 * ruling p, ruling q).
 */
public class VarroaClaimDispatcherTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private static final int QUIET_PERIOD = 300;
    private static final String GATE_ON = "1";

    private HttpServer server;
    private volatile java.util.function.Function<String, String> claimResponder;

    @Before
    public void startClaimServer() throws Exception {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/v1/claim", this::handleClaim);
        server.start();
    }

    @After
    public void stopClaimServer() {
        if (server != null) {
            server.stop(0);
        }
    }

    private void handleClaim(HttpExchange exchange) throws java.io.IOException {
        exchange.getRequestBody().readAllBytes();
        String reply = claimResponder.apply("");
        byte[] resp = reply.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "application/json");
        exchange.sendResponseHeaders(200, resp.length);
        try (OutputStream os = exchange.getResponseBody()) {
            os.write(resp);
        }
        exchange.close();
    }

    private VarroaClaimQueue realClaimQueue() throws Exception {
        String base = "http://127.0.0.1:" + server.getAddress().getPort();
        Path tokenFile = tmp.getRoot().toPath().resolve("token");
        Files.writeString(tokenFile, "test-token");
        LoopbackTokenFile tf = new LoopbackTokenFile(tokenFile, System::currentTimeMillis);
        MiteLoopbackClient client = new MiteLoopbackClient(base, tf, Duration.ofSeconds(5));
        return new VarroaClaimQueue(client, System::currentTimeMillis, millis -> { });
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

    private Queue.Item scheduleItem(FreeStyleProject project, String runId) {
        VarroaIntentAction action = new VarroaIntentAction(runId, "", null, 0L);
        ScheduleResult result = Queue.getInstance().schedule2(project, QUIET_PERIOD, action);
        assertTrue(result.isAccepted());
        return result.getItem();
    }

    /** Schedules an item carrying no {@link VarroaIntentAction} at all. */
    private Queue.Item scheduleItemWithNoAction(FreeStyleProject project) {
        ScheduleResult result = Queue.getInstance().schedule2(project, QUIET_PERIOD, new Action[0]);
        assertTrue(result.isAccepted());
        return result.getItem();
    }

    @Test
    public void blocksAnItemWhoseActionIsNotGranted() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        Queue.Item item = scheduleItem(project, "run-1");
        VarroaClaimQueue claimQueue = mock(VarroaClaimQueue.class);
        when(claimQueue.offer(any(), any(), any())).thenReturn(true);
        VarroaClaimDispatcher dispatcher = new VarroaClaimDispatcher(claimQueue);

        CauseOfBlockage cause = dispatcher.canRun(item, GATE_ON);
        assertNotNull(cause);
        assertEquals("awaiting Varroa claim", cause.getShortDescription());
    }

    @Test
    public void offersExactlyOncePerRunIdAcrossPasses() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        Queue.Item item = scheduleItem(project, "run-1");
        VarroaClaimQueue claimQueue = mock(VarroaClaimQueue.class);
        when(claimQueue.offer(any(), any(), any())).thenReturn(true);
        VarroaClaimDispatcher dispatcher = new VarroaClaimDispatcher(claimQueue);

        dispatcher.canRun(item, GATE_ON);
        dispatcher.canRun(item, GATE_ON);
        dispatcher.canRun(item, GATE_ON);

        verify(claimQueue, times(1)).offer(eq(item), eq(project), any());
    }

    @Test
    public void aRefusedOfferIsRetriedOnTheNextPass() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        Queue.Item item = scheduleItem(project, "run-1");
        VarroaClaimQueue claimQueue = mock(VarroaClaimQueue.class);
        when(claimQueue.offer(any(), any(), any())).thenReturn(false, true);
        VarroaClaimDispatcher dispatcher = new VarroaClaimDispatcher(claimQueue);

        dispatcher.canRun(item, GATE_ON);
        dispatcher.canRun(item, GATE_ON);

        verify(claimQueue, times(2)).offer(eq(item), eq(project), any());
    }

    @Test
    public void releasesTheItemOnceTheActionIsGranted() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        Queue.Item item = scheduleItem(project, "run-1");
        VarroaClaimQueue claimQueue = mock(VarroaClaimQueue.class);
        when(claimQueue.offer(any(), any(), any())).thenReturn(true);
        VarroaClaimDispatcher dispatcher = new VarroaClaimDispatcher(claimQueue);

        assertNotNull(dispatcher.canRun(item, GATE_ON));

        item.getAction(VarroaIntentAction.class).setGranted(true);
        assertNull(dispatcher.canRun(item, GATE_ON));
    }

    @Test
    public void gateOffNeverBlocksAndNeverOffers() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        Queue.Item item = scheduleItem(project, "run-1");
        VarroaClaimQueue claimQueue = mock(VarroaClaimQueue.class);
        VarroaClaimDispatcher dispatcher = new VarroaClaimDispatcher(claimQueue);

        assertNull(dispatcher.canRun(item, null));
        assertNull(dispatcher.canRun(item, "0"));
        verifyNoInteractions(claimQueue);
    }

    // ---- Missing action (ruling p) ----

    @Test
    public void anItemWithNoActionGetsOneAndIsBlockedAndOfferedOnce() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        Queue.Item item = scheduleItemWithNoAction(project);
        assertNull("fixture must start with no action", item.getAction(VarroaIntentAction.class));
        VarroaClaimQueue claimQueue = mock(VarroaClaimQueue.class);
        when(claimQueue.offer(any(), any(), any())).thenReturn(true);
        VarroaClaimDispatcher dispatcher = new VarroaClaimDispatcher(claimQueue);

        CauseOfBlockage cause = dispatcher.canRun(item, GATE_ON);
        CauseOfBlockage second = dispatcher.canRun(item, GATE_ON);

        assertNotNull(cause);
        assertEquals("awaiting Varroa claim", cause.getShortDescription());
        assertNotNull(second);
        VarroaIntentAction stamped = item.getAction(VarroaIntentAction.class);
        assertNotNull("the dispatcher must stamp a missing action before offering", stamped);
        assertFalse("the stamped action needs a real run id", stamped.getRunId().isEmpty());
        assertFalse(stamped.isGranted());
        verify(claimQueue, times(1)).offer(eq(item), eq(project), any());
    }

    // ---- Started map lifecycle (ruling q) ----

    @Test
    public void startedEntryIsReleasedWhenTheOfferCallbackRuns() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        Queue.Item item = scheduleItem(project, "run-1");
        VarroaClaimQueue claimQueue = mock(VarroaClaimQueue.class);
        ArgumentCaptor<Runnable> onResolved = ArgumentCaptor.forClass(Runnable.class);
        when(claimQueue.offer(any(), any(), onResolved.capture())).thenReturn(true);
        VarroaClaimDispatcher dispatcher = new VarroaClaimDispatcher(claimQueue);

        dispatcher.canRun(item, GATE_ON);
        assertEquals(1, dispatcher.startedCountForTesting());

        // The claim resolved — granted, cancelled, given up, or an
        // unexpected error — so VarroaClaimQueue runs the callback the
        // dispatcher handed it.
        onResolved.getValue().run();
        assertEquals(0, dispatcher.startedCountForTesting());

        // The released entry lets the next pass offer again.
        dispatcher.canRun(item, GATE_ON);
        verify(claimQueue, times(2)).offer(eq(item), eq(project), any());
    }

    @Test
    public void startedEntryExpiresAfterOneHourWithNoResolution() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        Queue.Item item = scheduleItem(project, "run-1");
        VarroaClaimQueue claimQueue = mock(VarroaClaimQueue.class);
        when(claimQueue.offer(any(), any(), any())).thenReturn(true);
        long[] clockMillis = {0L};
        VarroaClaimDispatcher dispatcher = new VarroaClaimDispatcher(claimQueue, () -> clockMillis[0]);

        dispatcher.canRun(item, GATE_ON);
        assertEquals(1, dispatcher.startedCountForTesting());

        // Short of the one-hour lifetime: the entry, and the offer count,
        // both stay put — no callback ever ran.
        clockMillis[0] = VarroaClaimDispatcher.STARTED_TTL_MILLIS - 1;
        dispatcher.canRun(item, GATE_ON);
        assertEquals(1, dispatcher.startedCountForTesting());
        verify(claimQueue, times(1)).offer(eq(item), eq(project), any());

        // Past the lifetime: the sweep drops the entry and the next pass
        // offers again.
        clockMillis[0] = VarroaClaimDispatcher.STARTED_TTL_MILLIS + 1;
        dispatcher.canRun(item, GATE_ON);
        assertEquals(1, dispatcher.startedCountForTesting());
        verify(claimQueue, times(2)).offer(eq(item), eq(project), any());
    }

    // ---- End-to-end cleanup, real VarroaClaimQueue (ruling q) ----

    @Test
    public void startedEntryClearsAfterARealGrant() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        Queue.Item item = scheduleItem(project, "run-1");
        claimResponder = body -> "{\"decision\":\"granted\",\"run_id\":\"run-1\",\"build_number\":1}";
        VarroaClaimDispatcher dispatcher = new VarroaClaimDispatcher(realClaimQueue());

        dispatcher.canRun(item, GATE_ON);
        assertEquals(1, dispatcher.startedCountForTesting());

        awaitTrue(() -> item.getAction(VarroaIntentAction.class).isGranted(), "the action to be granted");
        awaitTrue(() -> dispatcher.startedCountForTesting() == 0, "the started entry to clear after a grant");
    }

    @Test
    public void startedEntryClearsAfterARealCancel() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        Queue.Item item = scheduleItem(project, "run-1");
        claimResponder = body -> "{\"decision\":\"duplicate\",\"run_id\":\"run-1\"}";
        VarroaClaimDispatcher dispatcher = new VarroaClaimDispatcher(realClaimQueue());

        dispatcher.canRun(item, GATE_ON);
        assertEquals(1, dispatcher.startedCountForTesting());

        awaitTrue(() -> item.getFuture().isCancelled(), "the item to be cancelled");
        awaitTrue(() -> dispatcher.startedCountForTesting() == 0, "the started entry to clear after a cancel");
    }
}
