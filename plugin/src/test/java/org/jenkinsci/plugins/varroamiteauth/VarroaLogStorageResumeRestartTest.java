package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertTrue;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import java.io.IOException;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Comparator;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.atomic.AtomicLong;
import java.util.stream.Stream;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsSessionRule;

/**
 * Proves on a real second Jenkins boot over the same Jenkins home that a
 * resumed build continues its log sequence. A fake mite loopback records
 * every accepted chunk by its sequence. Before the fix, the second JVM
 * started again at sequence 0, so one sequence carried two different
 * chunks, and the gateway kept only the first.
 */
public class VarroaLogStorageResumeRestartTest {

    private static final String RUN_ID = "run-log-resume";

    private static final String SCRIPT = "echo 'before-restart'\n"
            + "sleep(time: 5, unit: 'SECONDS')\n"
            + "echo 'after-sleep'\n";

    @Rule
    public JenkinsSessionRule sessions = new JenkinsSessionRule();

    private HttpServer server;
    private final MiteStub mite = new MiteStub();

    @Before
    public void startMite() throws IOException {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/v1/log", mite);
        server.start();
        VarroaLogStorageFactory.setActivationUrlForTesting(
                "http://127.0.0.1:" + server.getAddress().getPort() + "/v1/log");
    }

    @After
    public void stopMite() {
        server.stop(0);
        VarroaLogStorageFactory.setActivationUrlForTesting(null);
    }

    @Test
    public void aResumedBuildContinuesItsLogSequence() throws Throwable {
        sessions.then(j -> {
            WorkflowJob p = j.jenkins.createProject(WorkflowJob.class, "resume-log");
            p.setDefinition(new CpsFlowDefinition(SCRIPT, true));
            WorkflowRun run = (WorkflowRun) p.scheduleBuild2(0,
                    new VarroaIntentAction(RUN_ID, "", null, System.currentTimeMillis())).waitForStart();
            j.waitForMessage("Sleeping for", run);
            awaitChunkContaining("Sleeping for");
        });
        long highestBeforeRestart = mite.highestSeq.get();

        sessions.then(j -> {
            WorkflowRun run = j.jenkins.getItemByFullName("resume-log", WorkflowJob.class).getBuildByNumber(1);
            j.waitForCompletion(run);
            j.assertBuildStatusSuccess(run);
            VarroaLogStorageFactory.get().finish(RUN_ID);
            awaitFinalChunk();
        });

        assertTrue("no sequence may carry two different chunks: " + mite.conflicts, mite.conflicts.isEmpty());
        Long resumingSeq = seqOfChunkContaining("Resuming build");
        assertNotNull("the resume line must reach the mite; chunks: " + mite.bySeq, resumingSeq);
        assertTrue("the resume line must continue above seq " + highestBeforeRestart + ", got " + resumingSeq,
                resumingSeq > highestBeforeRestart);
        assertNotNull(seqOfChunkContaining("after-sleep"));
        long finalSeq = mite.finalSeq.get();
        for (long seq = 0; seq <= finalSeq; seq++) {
            assertTrue("the sequence must have no hole at " + seq, mite.bySeq.containsKey(seq));
        }
        assertEquals(finalSeq + 1, mite.bySeq.size());
    }

    /**
     * Models a move to another cell: between the two boots the read cache
     * is gone, and the build directory holds the value the mite writes.
     * The resume line must take that value, not 0.
     */
    @Test
    public void aMovedBuildContinuesFromTheHintFile() throws Throwable {
        sessions.then(j -> {
            WorkflowJob p = j.jenkins.createProject(WorkflowJob.class, "resume-log");
            p.setDefinition(new CpsFlowDefinition(SCRIPT, true));
            WorkflowRun run = (WorkflowRun) p.scheduleBuild2(0,
                    new VarroaIntentAction(RUN_ID, "", null, System.currentTimeMillis())).waitForStart();
            j.waitForMessage("Sleeping for", run);
            awaitChunkContaining("Sleeping for");
        });
        long hint = mite.highestSeq.get() + 10;
        Path home = sessions.getHome().toPath();
        deleteTree(home.resolve("varroa-log-cache"));
        Files.writeString(home.resolve("jobs/resume-log/builds/1").resolve(VarroaLogStorageFactory.LOG_NEXT_SEQ_FILE),
                Long.toString(hint), StandardCharsets.US_ASCII);

        sessions.then(j -> {
            WorkflowRun run = j.jenkins.getItemByFullName("resume-log", WorkflowJob.class).getBuildByNumber(1);
            j.waitForCompletion(run);
            j.assertBuildStatusSuccess(run);
            VarroaLogStorageFactory.get().finish(RUN_ID);
            awaitFinalChunk();
        });

        assertTrue("no sequence may carry two different chunks: " + mite.conflicts, mite.conflicts.isEmpty());
        Long resumingSeq = seqOfChunkContaining("Resuming build");
        assertNotNull("the resume line must reach the mite; chunks: " + mite.bySeq, resumingSeq);
        assertEquals("the resume line must take the hint", hint, resumingSeq.longValue());
        assertNotNull(seqOfChunkContaining("after-sleep"));
        for (long seq = hint; seq <= mite.finalSeq.get(); seq++) {
            assertTrue("the moved sequence must have no hole at " + seq, mite.bySeq.containsKey(seq));
        }
    }

    private static void deleteTree(Path root) throws IOException {
        if (!Files.exists(root)) {
            return;
        }
        try (Stream<Path> paths = Files.walk(root)) {
            for (Path p : paths.sorted(Comparator.reverseOrder()).toList()) {
                Files.delete(p);
            }
        }
    }

    private Long seqOfChunkContaining(String text) {
        for (Map.Entry<Long, String> e : mite.bySeq.entrySet()) {
            if (e.getValue().contains(text)) {
                return e.getKey();
            }
        }
        return null;
    }

    private void awaitChunkContaining(String text) throws InterruptedException {
        long deadline = System.currentTimeMillis() + 30_000L;
        while (seqOfChunkContaining(text) == null && System.currentTimeMillis() < deadline) {
            Thread.sleep(50);
        }
        assertNotNull("the mite must receive '" + text + "'; chunks: " + mite.bySeq, seqOfChunkContaining(text));
    }

    private void awaitFinalChunk() throws InterruptedException {
        long deadline = System.currentTimeMillis() + 30_000L;
        while (mite.finalSeq.get() < 0 && System.currentTimeMillis() < deadline) {
            Thread.sleep(50);
        }
        assertTrue("the mite must receive the final chunk", mite.finalSeq.get() >= 0);
    }

    /** Accepts every chunk of {@link #RUN_ID} and records its text by sequence. */
    private static final class MiteStub implements com.sun.net.httpserver.HttpHandler {

        final Map<Long, String> bySeq = new ConcurrentHashMap<>();
        final List<String> conflicts = new CopyOnWriteArrayList<>();
        final AtomicLong highestSeq = new AtomicLong(-1L);
        final AtomicLong finalSeq = new AtomicLong(-1L);

        @Override
        public void handle(HttpExchange exchange) throws IOException {
            byte[] body = exchange.getRequestBody().readAllBytes();
            if (RUN_ID.equals(exchange.getRequestHeaders().getFirst("X-Varroa-Run-Id"))) {
                long seq = Long.parseLong(exchange.getRequestHeaders().getFirst("X-Varroa-Seq"));
                String text = new String(body, StandardCharsets.UTF_8);
                String previous = bySeq.putIfAbsent(seq, text);
                if (previous != null && !previous.equals(text)) {
                    conflicts.add("seq " + seq + ": '" + previous + "' and '" + text + "'");
                }
                highestSeq.accumulateAndGet(seq, Math::max);
                if (Boolean.parseBoolean(exchange.getRequestHeaders().getFirst("X-Varroa-Final"))) {
                    finalSeq.set(seq);
                }
            }
            exchange.sendResponseHeaders(202, -1);
            exchange.close();
        }
    }
}
