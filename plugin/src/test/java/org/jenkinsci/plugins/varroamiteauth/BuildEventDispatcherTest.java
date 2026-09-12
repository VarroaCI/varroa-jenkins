package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Path;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.atomic.AtomicInteger;

import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;

/**
 * Tests for {@link BuildEventDispatcher} against a stub loopback server
 * (com.sun.net.httpserver ships with the JDK).
 */
public class BuildEventDispatcherTest {

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private HttpServer server;
    private final List<String> requestBodies = new CopyOnWriteArrayList<>();
    private final List<Long> sleptMillis = new ArrayList<>();
    private volatile java.util.function.Function<String, String> responder;

    @Before
    public void startServer() throws Exception {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/v1/events", this::handle);
        server.start();
    }

    @After
    public void stopServer() {
        if (server != null) {
            server.stop(0);
        }
    }

    private void handle(HttpExchange exchange) throws java.io.IOException {
        byte[] req = exchange.getRequestBody().readAllBytes();
        String reqBody = new String(req, StandardCharsets.UTF_8);
        requestBodies.add(reqBody);

        String reply = responder != null ? responder.apply(reqBody) : "[]";
        int status = reply == null ? 503 : 202;
        byte[] resp = (reply != null ? reply : "").getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "application/json");
        exchange.sendResponseHeaders(status, resp.length == 0 ? -1 : resp.length);
        if (resp.length > 0) {
            try (OutputStream os = exchange.getResponseBody()) {
                os.write(resp);
            }
        }
        exchange.close();
    }

    private MiteLoopbackClient newClient() {
        String base = "http://127.0.0.1:" + server.getAddress().getPort();
        Path tokenFile = tmp.getRoot().toPath().resolve("token");
        try {
            java.nio.file.Files.writeString(tokenFile, "test-token");
        } catch (java.io.IOException e) {
            throw new RuntimeException(e);
        }
        LoopbackTokenFile tf = new LoopbackTokenFile(tokenFile, System::currentTimeMillis);
        return new MiteLoopbackClient(base, tf, Duration.ofSeconds(5));
    }

    private static BuildEvent event(String runId, long seq, String phase) {
        return BuildEvent.builder().runId(runId).seq(seq).phase(phase).build();
    }

    private static String ackReply(String runId, long seq, String status) {
        return "[{\"run_id\":\"" + runId + "\",\"event_seq\":" + seq
                + ",\"status\":\"" + status + "\",\"reason\":\"\"}]";
    }

    // ---- Split ----

    @Test
    public void splitKeepsEachGroupUnderThePostCap() {
        List<BuildEvent> events = new ArrayList<>();
        String filler = "x".repeat(300_000);
        for (int i = 0; i < 3; i++) {
            events.add(BuildEvent.builder().runId("run-1").seq(i)
                    .phase(BuildEvent.PHASE_STAGE).causeDescription(filler).build());
        }
        List<List<PostableEvent>> groups = BuildEventDispatcher.split(events);
        assertTrue("expected more than one group, got " + groups.size(), groups.size() > 1);
        for (List<PostableEvent> group : groups) {
            int bytes = 2;
            for (PostableEvent e : group) {
                bytes += e.toJson().toString().getBytes(StandardCharsets.UTF_8).length + 1;
            }
            assertTrue("group of " + group.size() + " exceeded the cap: " + bytes,
                    bytes <= BuildEventDispatcher.POST_CAP_BYTES);
        }
    }

    @Test
    public void singleSmallEventStaysInOneGroup() {
        List<BuildEvent> events = List.of(event("run-1", 0, BuildEvent.PHASE_STARTED));
        List<List<PostableEvent>> groups = BuildEventDispatcher.split(events);
        assertEquals(1, groups.size());
        assertEquals(1, groups.get(0).size());
    }

    @Test
    public void postCapLeavesHeadroomUnderTheFramedLoopbackBodyLimit() {
        // split()'s per-group cap (POST_CAP_BYTES) already accounts for
        // the "[" "]" array framing and one comma per item, so a group
        // built under that cap never approaches the loopback's hard 1
        // MiB body limit — even a lone event whose own body already
        // exceeds POST_CAP_BYTES stays isolated in its own group and
        // framed well under the limit. A future change to either
        // constant that closes this headroom must fail this test.
        assertTrue("POST_CAP_BYTES must stay well under the framed loopback limit",
                BuildEventDispatcher.POST_CAP_BYTES + 2 <= VarroaRunRecorder.LOOPBACK_BODY_LIMIT_BYTES);
    }

    // ---- Endless retry ----

    @Test
    public void retriesForeverWithAGrowingBackoff() {
        RunSpool spool = new RunSpool(tmp.getRoot().toPath(), RunSpool.DEFAULT_CAP_BYTES);
        BuildEventSink sink = new BuildEventSink(16);
        AtomicInteger calls = new AtomicInteger();
        responder = body -> {
            int n = calls.incrementAndGet();
            // Fail the first 7 attempts (past every step of the ladder,
            // including the repeated final value), then succeed.
            return n <= 7 ? null : ackReply("run-1", 0, "STORED");
        };

        sink.offer(event("run-1", 0, BuildEvent.PHASE_STARTED));

        BuildEventDispatcher dispatcher = new BuildEventDispatcher(
                sink, spool, newClient(), (millis) -> sleptMillis.add(millis));
        dispatcher.runOnce();

        assertEquals(8, calls.get());
        assertEquals(List.of(1000L, 2000L, 5000L, 10_000L, 20_000L, 30_000L, 30_000L), sleptMillis);
        // Once acked, the spool file is gone.
        assertTrue(spool.readOrdered("run-1").isEmpty());
    }

    // ---- Spool replay order ----

    @Test
    public void retriesTheOlderUnackedEventBeforeAnyNewerOne() {
        RunSpool spool = new RunSpool(tmp.getRoot().toPath(), RunSpool.DEFAULT_CAP_BYTES);
        BuildEventSink sink = new BuildEventSink(16);
        AtomicInteger calls = new AtomicInteger();
        responder = body -> {
            int n = calls.incrementAndGet();
            if (n == 1) {
                // Only the older event (seq 0) is acked; seq 1 must retry.
                return ackReply("run-1", 0, "STORED");
            }
            return "[{\"run_id\":\"run-1\",\"event_seq\":1,\"status\":\"STORED\",\"reason\":\"\"},"
                    + "{\"run_id\":\"run-1\",\"event_seq\":0,\"status\":\"STORED\",\"reason\":\"\"}]";
        };

        sink.offer(event("run-1", 0, BuildEvent.PHASE_STARTED));
        sink.offer(event("run-1", 1, BuildEvent.PHASE_COMPLETED));

        BuildEventDispatcher dispatcher = new BuildEventDispatcher(
                sink, spool, newClient(), (millis) -> sleptMillis.add(millis));
        dispatcher.runOnce();

        assertEquals(2, calls.get());
        // The second request carries only the still-pending seq 1 — the
        // dispatcher never re-sends the already-acked older event, and it
        // never lets a newer event skip ahead of an unacked older one.
        assertFalse(requestBodies.get(1).contains("\"seq\":0"));
        assertTrue(requestBodies.get(1).contains("\"seq\":1"));
        // The COMPLETED ack for the run's last event deletes the directory.
        assertTrue(spool.readOrdered("run-1").isEmpty());
    }

    // ---- COMPLETED ack must not wipe an unacked sibling ----

    @Test
    public void aCompletedAckNeverDeletesAStillPendingSiblingsSpoolFile() {
        RunSpool spool = new RunSpool(tmp.getRoot().toPath(), RunSpool.DEFAULT_CAP_BYTES);
        BuildEventSink sink = new BuildEventSink(16);
        AtomicInteger calls = new AtomicInteger();
        responder = body -> {
            int n = calls.incrementAndGet();
            if (n == 1) {
                // Only the COMPLETED event (seq 1) is acked this round;
                // the still-pending seq 0 must retry.
                return ackReply("run-1", 1, "STORED");
            }
            return ackReply("run-1", 0, "STORED");
        };

        sink.offer(event("run-1", 0, BuildEvent.PHASE_STAGE));
        sink.offer(event("run-1", 1, BuildEvent.PHASE_COMPLETED));

        List<Boolean> seq0SurvivedFirstRound = new ArrayList<>();
        BuildEventDispatcher dispatcher = new BuildEventDispatcher(
                sink, spool, newClient(), (millis) -> {
                    sleptMillis.add(millis);
                    if (sleptMillis.size() == 1) {
                        // Between round 1 and round 2: the COMPLETED
                        // ack for seq 1 alone must not have wiped seq
                        // 0's still-unacknowledged spool file.
                        seq0SurvivedFirstRound.add(
                                spool.readOrdered("run-1").stream().anyMatch(e -> e.seq == 0));
                    }
                });
        dispatcher.runOnce();

        assertEquals(List.of(true), seq0SurvivedFirstRound);
        // Once seq 0 is also acked, both files — and the now-empty
        // directory — are gone.
        assertTrue(spool.readOrdered("run-1").isEmpty());
    }

    // ---- Heartbeat drop ----

    @Test
    public void heartbeatRidesTheFirstAttemptButIsDroppedFromARetry() {
        RunSpool spool = new RunSpool(tmp.getRoot().toPath(), RunSpool.DEFAULT_CAP_BYTES);
        BuildEventSink sink = new BuildEventSink(16);
        AtomicInteger calls = new AtomicInteger();
        responder = body -> {
            int n = calls.incrementAndGet();
            if (n == 1) {
                return null; // fail the first attempt entirely
            }
            return ackReply("run-1", 0, "STORED");
        };

        sink.offer(event("run-1", 0, BuildEvent.PHASE_STARTED));
        sink.offer(event("run-1", 1, BuildEvent.PHASE_HEARTBEAT));

        BuildEventDispatcher dispatcher = new BuildEventDispatcher(
                sink, spool, newClient(), (millis) -> sleptMillis.add(millis));
        dispatcher.runOnce();

        assertEquals(2, calls.get());
        // The heartbeat rides the first (failed) attempt...
        assertTrue(requestBodies.get(0).contains("HEARTBEAT"));
        // ...but never appears again on the retry.
        assertFalse(requestBodies.get(1).contains("HEARTBEAT"));
        // The heartbeat was never spooled in the first place.
        assertTrue(spool.readOrdered("run-1").stream().noneMatch(e -> e.seq == 1));
    }

    // ---- Spool replay (defect 1, ruling r) ----

    @Test
    public void replaysPrepopulatedSpoolFilesInOrderAndDeletesThemOnAck() {
        RunSpool spool = new RunSpool(tmp.getRoot().toPath(), RunSpool.DEFAULT_CAP_BYTES);
        // Prepopulate the spool directly, as a Jenkins restart would find
        // it, with no event ever offered to the (empty) sink.
        spool.append("run-1", event("run-1", 0, BuildEvent.PHASE_STARTED));
        spool.append("run-1", event("run-1", 1, BuildEvent.PHASE_STAGE));
        spool.append("run-1", event("run-1", 2, BuildEvent.PHASE_COMPLETED));
        BuildEventSink sink = new BuildEventSink(16);
        responder = body -> "[{\"run_id\":\"run-1\",\"event_seq\":0,\"status\":\"STORED\",\"reason\":\"\"},"
                + "{\"run_id\":\"run-1\",\"event_seq\":1,\"status\":\"STORED\",\"reason\":\"\"},"
                + "{\"run_id\":\"run-1\",\"event_seq\":2,\"status\":\"STORED\",\"reason\":\"\"}]";

        BuildEventDispatcher dispatcher = new BuildEventDispatcher(
                sink, spool, newClient(), (millis) -> sleptMillis.add(millis));
        dispatcher.noteSpooled("run-1");
        dispatcher.runOnce();

        assertEquals(1, requestBodies.size());
        String body = requestBodies.get(0);
        // The single post carries every file, in spool order.
        int seq0 = body.indexOf("\"seq\":0");
        int seq1 = body.indexOf("\"seq\":1");
        int seq2 = body.indexOf("\"seq\":2");
        assertTrue(seq0 >= 0 && seq1 > seq0 && seq2 > seq1);
        // The COMPLETED ack deletes the run's directory.
        assertTrue(spool.readOrdered("run-1").isEmpty());
    }

    @Test
    public void aTruncatedSpoolFileIsSkippedNotFatal() throws Exception {
        RunSpool spool = new RunSpool(tmp.getRoot().toPath(), RunSpool.DEFAULT_CAP_BYTES);
        spool.append("run-1", event("run-1", 0, BuildEvent.PHASE_STARTED));
        // A file that survived a mite restart mid-write: a valid spool
        // name, an incomplete body.
        Path truncated = tmp.getRoot().toPath().resolve("run-1")
                .resolve("00000000000000000001-STAGE.json");
        java.nio.file.Files.write(truncated, "{\"seq\":1,\"phase\":\"STAGE\"".getBytes(StandardCharsets.UTF_8));
        BuildEventSink sink = new BuildEventSink(16);
        responder = body -> "[{\"run_id\":\"run-1\",\"event_seq\":0,\"status\":\"STORED\",\"reason\":\"\"}]";

        BuildEventDispatcher dispatcher = new BuildEventDispatcher(
                sink, spool, newClient(), (millis) -> sleptMillis.add(millis));
        dispatcher.noteSpooled("run-1");
        dispatcher.runOnce();

        // The dispatcher's daemon-thread loop survives the corrupt file:
        // it posts the one readable entry and moves on instead of dying
        // on a JSON parse exception.
        assertEquals(1, requestBodies.size());
        assertTrue(requestBodies.get(0).contains("\"seq\":0"));
    }

    @Test
    public void aSinkFullEventAnnouncedAfterReplayIsAlsoDispatched() {
        RunSpool spool = new RunSpool(tmp.getRoot().toPath(), RunSpool.DEFAULT_CAP_BYTES);
        spool.append("run-1", event("run-1", 0, BuildEvent.PHASE_STARTED));
        BuildEventSink sink = new BuildEventSink(16);
        responder = body -> "[{\"run_id\":\"run-1\",\"event_seq\":0,\"status\":\"STORED\",\"reason\":\"\"}]";

        BuildEventDispatcher dispatcher = new BuildEventDispatcher(
                sink, spool, newClient(), (millis) -> sleptMillis.add(millis));
        dispatcher.noteSpooled("run-1");
        dispatcher.runOnce();
        assertEquals(1, requestBodies.size());

        // The listener spools a later event itself, because the sink is
        // full, and announces it — exactly what VarroaRunRecorder.emit()
        // does on that path.
        spool.append("run-1", event("run-1", 1, BuildEvent.PHASE_COMPLETED));
        responder = body -> "[{\"run_id\":\"run-1\",\"event_seq\":1,\"status\":\"STORED\",\"reason\":\"\"}]";
        dispatcher.noteSpooled("run-1");
        dispatcher.runOnce();

        assertEquals(2, requestBodies.size());
        assertTrue(requestBodies.get(1).contains("\"seq\":1"));
        assertTrue(spool.readOrdered("run-1").isEmpty());
    }

    @Test
    public void anOlderSinkQueuedEventPostsBeforeANewerSpooledAndAnnouncedSibling() {
        RunSpool spool = new RunSpool(tmp.getRoot().toPath(), RunSpool.DEFAULT_CAP_BYTES);
        BuildEventSink sink = new BuildEventSink(16);
        // seq 0 is already queued in the sink, exactly as an earlier
        // accepted offer() left it — the dispatcher has not drained it
        // yet.
        sink.offer(event("run-1", 0, BuildEvent.PHASE_STARTED));

        // seq 1 then finds the sink full: VarroaRunRecorder.emit() moves
        // every already-queued event of this run out of the sink and
        // into the spool first, in order, before spooling seq 1 itself
        // and announcing the run.
        for (BuildEvent queued : sink.drainRun("run-1")) {
            spool.append("run-1", queued);
        }
        spool.append("run-1", event("run-1", 1, BuildEvent.PHASE_COMPLETED));
        responder = body -> "[{\"run_id\":\"run-1\",\"event_seq\":0,\"status\":\"STORED\",\"reason\":\"\"},"
                + "{\"run_id\":\"run-1\",\"event_seq\":1,\"status\":\"STORED\",\"reason\":\"\"}]";

        BuildEventDispatcher dispatcher = new BuildEventDispatcher(
                sink, spool, newClient(), (millis) -> sleptMillis.add(millis));
        dispatcher.noteSpooled("run-1");
        dispatcher.runOnce();

        // Both events travel in the one announced-backlog post, and seq
        // 0 (older, unacknowledged) precedes seq 1 (newer) in the body —
        // D11: no newer event of a run posts before an older one.
        assertEquals(1, requestBodies.size());
        String body = requestBodies.get(0);
        int seq0 = body.indexOf("\"seq\":0");
        int seq1 = body.indexOf("\"seq\":1");
        assertTrue("expected both seq 0 and seq 1 in the post", seq0 >= 0 && seq1 >= 0);
        assertTrue("expected the older seq 0 before the newer seq 1", seq0 < seq1);
        assertTrue(spool.readOrdered("run-1").isEmpty());
    }
}
