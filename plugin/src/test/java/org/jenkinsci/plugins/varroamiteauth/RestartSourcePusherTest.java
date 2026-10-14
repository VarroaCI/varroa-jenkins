package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertTrue;
import static org.mockito.Mockito.mock;

import com.sun.net.httpserver.HttpServer;

import hudson.model.Run;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.Base64;
import java.util.Deque;
import java.util.List;
import java.util.concurrent.Executor;
import java.util.concurrent.atomic.AtomicInteger;

import net.sf.json.JSONObject;

import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;

public class RestartSourcePusherTest {

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private static final String JOB = "team-a/app";
    private static final long BUILD = 7L;
    private static final String PREFIX = "jobs/team-a/jobs/app/builds/7/";
    private static final String URL = "http://127.0.0.1:9";

    /** Records every post and answers from a script; accepted once the script is empty. */
    private static final class Recording implements RunStatePusher.RunStateSender {
        final List<JSONObject> records = new ArrayList<>();
        final Deque<Answer> answers = new ArrayDeque<>();

        @Override
        public Answer post(String url, JSONObject record) {
            records.add(record);
            Answer next = answers.poll();
            return next != null ? next : Answer.accepted();
        }
    }

    private Recording sender;

    @Before
    public void setUp() {
        RestartSourcePusher.resetForTesting();
        RunStatePusher.resetForTesting();
        PluginCounters.resetForTesting();
        RestartSourcePusher.setSleeperForTesting(millis -> { });
        sender = new Recording();
        RestartSourcePusher.setSenderForTesting(sender);
    }

    @After
    public void tearDown() {
        RestartSourcePusher.resetForTesting();
        RunStatePusher.resetForTesting();
        PluginCounters.resetForTesting();
    }

    private Path root() throws IOException {
        return tmp.getRoot().toPath();
    }

    private void write(String rel, byte[] content) throws IOException {
        Path p = root().resolve(rel);
        Files.createDirectories(p.getParent());
        Files.write(p, content);
    }

    private void write(String rel, String content) throws IOException {
        write(rel, content.getBytes(StandardCharsets.UTF_8));
    }

    private long counter(String name) {
        return PluginCounters.snapshot().get(name);
    }

    private static byte[] gunzip(String base64) throws IOException {
        byte[] gz = Base64.getDecoder().decode(base64);
        try (java.util.zip.GZIPInputStream in = new java.util.zip.GZIPInputStream(new java.io.ByteArrayInputStream(gz))) {
            ByteArrayOutputStream out = new ByteArrayOutputStream();
            in.transferTo(out);
            return out.toByteArray();
        }
    }

    @Test
    public void fileSetIncludesBuildXmlWorkflowCompletedAndLibs() throws IOException {
        write("build.xml", "b");
        write("workflow-completed/1/log.xml", "l");
        write("libs/a.jar", "a");
        write("program.dat", "not part of the copy");

        assertEquals(List.of("build.xml", "libs/a.jar", "workflow-completed/1/log.xml"),
                RestartSourcePusher.fileSet(root()));
    }

    @Test
    public void fileSetSkipsSymlinks() throws IOException {
        write("workflow-completed/real.xml", "r");
        Files.createSymbolicLink(root().resolve("workflow-completed/link.xml"), root().resolve("workflow-completed/real.xml"));

        assertEquals(List.of("workflow-completed/real.xml"), RestartSourcePusher.fileSet(root()));
    }

    @Test
    public void noWorkflowCompletedPushesNothing() throws IOException {
        write("build.xml", "b");

        assertTrue(RestartSourcePusher.fileSet(root()).isEmpty());
        RestartSourcePusher.copy(root(), JOB, BUILD, "run-1", URL);
        assertTrue(sender.records.isEmpty());
    }

    @Test
    public void overFileCapPushesNothing() throws IOException {
        for (int i = 0; i <= RestartSourcePusher.MAX_FILES; i++) {
            write("workflow-completed/f" + i + ".xml", "x");
        }

        RestartSourcePusher.copy(root(), JOB, BUILD, "run-1", URL);

        assertTrue(sender.records.isEmpty());
        assertEquals(1L, counter(PluginCounters.RESTART_SOURCE_DROPPED));
    }

    @Test
    public void overByteCapPushesNothing() throws IOException {
        write("workflow-completed/big.bin", new byte[(int) RestartSourcePusher.MAX_BYTES + 1]);

        RestartSourcePusher.copy(root(), JOB, BUILD, "run-1", URL);

        assertTrue(sender.records.isEmpty());
        assertEquals(1L, counter(PluginCounters.RESTART_SOURCE_DROPPED));
    }

    @Test
    public void recordsCarryRestartFlagAndCounts() throws IOException {
        write("build.xml", "abc");
        write("workflow-completed/x.xml", "de");

        RestartSourcePusher.copy(root(), JOB, BUILD, "run-1", URL);

        assertEquals(2, sender.records.size());
        JSONObject first = sender.records.get(0);
        assertEquals(PREFIX + "build.xml", first.getString("relpath"));
        assertTrue(first.getBoolean("restart_source"));
        assertEquals(2L, first.getLong("copy_files"));
        assertEquals(5L, first.getLong("copy_bytes"));
        assertEquals(1L, first.getLong("state_gen"));
        assertEquals(RunStatePusher.PUSHER_EPOCH, first.getLong("pusher_epoch"));
        assertEquals("abc", new String(gunzip(first.getString("gzip_base64")), StandardCharsets.UTF_8));
        assertEquals(1L, counter(PluginCounters.RESTART_SOURCE_KEPT));
    }

    @Test
    public void multiChunkFileUsesChunkBytes() throws IOException {
        int size = RunStatePusher.CHUNK_BYTES * 2 + 10;
        write("workflow-completed/big.bin", new byte[size]);

        RestartSourcePusher.copy(root(), JOB, BUILD, "run-1", URL);

        assertEquals(3, sender.records.size());
        for (int seq = 0; seq < 3; seq++) {
            JSONObject r = sender.records.get(seq);
            assertEquals(seq, r.getLong("seq"));
            assertEquals(3L, r.getLong("total"));
            assertEquals((long) size, r.getLong("object_size"));
        }
        assertEquals(10L, sender.records.get(2).getLong("raw_size"));
    }

    @Test
    public void budgetFullRetriesSameChunk() throws IOException {
        write("build.xml", "abc");
        write("workflow-completed/x.xml", "de");
        sender.answers.add(RunStatePusher.RunStateSender.Answer.refused("gateway-budget-full", 0));

        RestartSourcePusher.copy(root(), JOB, BUILD, "run-1", URL);

        // build.xml posts twice (the refusal, then the retry), then the second file posts once.
        assertEquals(3, sender.records.size());
        assertEquals(PREFIX + "build.xml", sender.records.get(0).getString("relpath"));
        assertEquals(0L, sender.records.get(0).getLong("seq"));
        assertEquals(PREFIX + "build.xml", sender.records.get(1).getString("relpath"));
        assertEquals(0L, sender.records.get(1).getLong("seq"));
        assertEquals(1L, counter(PluginCounters.RESTART_SOURCE_KEPT));
    }

    @Test
    public void zeroByteFilesAreLeftOutOfTheCopy() throws IOException {
        write("build.xml", "abc");
        write("workflow-completed/empty.xml", "");

        RestartSourcePusher.copy(root(), JOB, BUILD, "run-1", URL);

        assertEquals(1, sender.records.size());
        JSONObject only = sender.records.get(0);
        assertEquals(PREFIX + "build.xml", only.getString("relpath"));
        assertEquals(1L, only.getLong("copy_files"));
        assertEquals(3L, only.getLong("copy_bytes"));
    }

    @Test
    public void permanentRefusalStopsCopy() throws IOException {
        write("build.xml", "abc");
        write("workflow-completed/x.xml", "de");
        sender.answers.add(RunStatePusher.RunStateSender.Answer.refused("state-cap", 0));

        RestartSourcePusher.copy(root(), JOB, BUILD, "run-1", URL);

        assertEquals(1, sender.records.size());
        assertEquals(1L, counter(PluginCounters.RESTART_SOURCE_DROPPED));
        assertEquals(0L, counter(PluginCounters.RESTART_SOURCE_KEPT));
    }

    @Test
    public void incompleteRestartsFile() throws IOException {
        write("workflow-completed/big.bin", new byte[RunStatePusher.CHUNK_BYTES + 5]);
        sender.answers.add(RunStatePusher.RunStateSender.Answer.accepted());
        sender.answers.add(RunStatePusher.RunStateSender.Answer.refused("incomplete", 0));

        RestartSourcePusher.copy(root(), JOB, BUILD, "run-1", URL);

        List<Long> seqs = new ArrayList<>();
        for (JSONObject r : sender.records) {
            seqs.add(r.getLong("seq"));
        }
        assertEquals(List.of(0L, 1L, 0L, 1L), seqs);
    }

    @Test
    public void deadlineEndsCopy() throws IOException {
        write("build.xml", "abc");
        write("workflow-completed/x.xml", "de");
        AtomicInteger calls = new AtomicInteger();
        RestartSourcePusher.setClockForTesting(() -> calls.getAndIncrement() == 0
                ? 0L : RestartSourcePusher.COPY_DEADLINE_MILLIS);
        for (int i = 0; i < 5; i++) {
            sender.answers.add(RunStatePusher.RunStateSender.Answer.refused("gateway-budget-full", 0));
        }

        RestartSourcePusher.copy(root(), JOB, BUILD, "run-1", URL);

        assertEquals(1, sender.records.size());
        assertEquals(1L, counter(PluginCounters.RESTART_SOURCE_DROPPED));
    }

    @Test
    public void notOnCellPushesNothing() {
        RunStatePusher.setActivationUrlForTesting(URL);
        RestartSourcePusher.setCellOverrideForTesting(false);
        AtomicInteger scheduled = new AtomicInteger();
        RestartSourcePusher.setExecutorForTesting((Executor) command -> scheduled.incrementAndGet());

        RestartSourcePusher.scheduleFor(mock(Run.class), "run-1");

        assertEquals(0, scheduled.get());
    }

    @Test
    public void postRunStateAddsRestartHeaders() throws Exception {
        write("build.xml", "abc");
        write("workflow-completed/x.xml", "de");
        RestartSourcePusher.copy(root(), JOB, BUILD, "run-1", URL);
        JSONObject record = sender.records.get(0);
        byte[] gzip = Base64.getDecoder().decode(record.getString("gzip_base64"));

        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        List<String> seen = new ArrayList<>();
        server.createContext("/", exchange -> {
            seen.add(exchange.getRequestHeaders().getFirst("X-Varroa-Restart-Source"));
            seen.add(exchange.getRequestHeaders().getFirst("X-Varroa-Copy-Files"));
            seen.add(exchange.getRequestHeaders().getFirst("X-Varroa-Copy-Bytes"));
            exchange.getRequestBody().transferTo(new ByteArrayOutputStream());
            exchange.sendResponseHeaders(202, -1);
            try (OutputStream os = exchange.getResponseBody()) {
                os.flush();
            }
            exchange.close();
        });
        server.start();
        try {
            Path tokenFile = tmp.getRoot().toPath().resolve("token");
            Files.writeString(tokenFile, "test-token");
            LoopbackTokenFile tf = new LoopbackTokenFile(tokenFile, System::currentTimeMillis);
            MiteLoopbackClient client = new MiteLoopbackClient(
                    "http://127.0.0.1:" + server.getAddress().getPort(), tf,
                    Duration.ofSeconds(1), Duration.ofSeconds(5));
            MiteLoopbackClient.RunStateAnswer answer = client.postRunState(
                    "http://127.0.0.1:" + server.getAddress().getPort() + "/v1/run-state", record, gzip);
            assertEquals(202, answer.statusCode);
        } finally {
            server.stop(0);
        }
        assertEquals(List.of("1", "2", "5"), seen);
    }
}
