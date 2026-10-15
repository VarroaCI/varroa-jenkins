package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

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
import java.util.concurrent.atomic.AtomicReference;

import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests for {@link VarroaClaimQueue#applyBuildNumber} — contract G3's five
 * steps, held under the job's monitor (D4).
 */
public class BuildNumberApplyTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private VarroaClaimQueue newQueue() throws Exception {
        return new VarroaClaimQueue(new MiteLoopbackClient(), System::currentTimeMillis, (task, d) -> task.run());
    }

    @Test
    public void appliesTheGrantedNumberOnAFreshJob() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        VarroaClaimQueue queue = newQueue();

        assertTrue(queue.applyBuildNumber(project, 5));
        assertEquals(5, project.getNextBuildNumber());
    }

    @Test
    public void refusesOnThePreReadWhenTheJobsNumberIsAlreadyHigher() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        j.buildAndAssertSuccess(project); // build #1 completes; next build number becomes 2
        VarroaClaimQueue queue = newQueue();

        assertFalse(queue.applyBuildNumber(project, 1));
        // The number is untouched: the pre-read refusal never calls update.
        assertEquals(2, project.getNextBuildNumber());
    }

    @Test
    public void refusesOnThePostReadWhenTheUpdateIsANoOp() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        j.buildAndAssertSuccess(project);
        j.buildAndAssertSuccess(project);
        j.buildAndAssertSuccess(project); // last build number is 3

        // Simulate a stale nextBuildNumber field (below the last build's
        // number) with the no-file-write setter, so the pre-read passes
        // but Job.updateNextBuildNumber is a silent no-op.
        project.fastUpdateNextBuildNumber(1);

        VarroaClaimQueue queue = newQueue();
        assertFalse(queue.applyBuildNumber(project, 2));
        assertEquals(1, project.getNextBuildNumber());
    }

    @Test
    public void aFailedSaveCancelsTheItemAndNamesNoConflict() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        VarroaIntentAction action = new VarroaIntentAction("run-io", "", null, 0L);
        Queue.Item item = Queue.getInstance().schedule2(project, 300, action).getItem();
        VarroaClaimQueue queue = new VarroaClaimQueue(new MiteLoopbackClient(), System::currentTimeMillis, (task, d) -> task.run()) {
            @Override
            ApplyResult applyNumber(hudson.model.Job<?, ?> job, long grantedNumber, String runId) {
                return ApplyResult.PERSIST_FAILED;
            }
        };

        assertFalse(queue.applyGrant(item, project, action, "manual:run-io", 5));
        assertTrue("a failed save cancels the item", item.getFuture().isCancelled());
        assertEquals("a failed save names no conflict", 0, action.getNumberConflict());
        assertFalse(action.isGranted());
    }

    @Test
    public void grantedFlagIsSetOnlyAfterTheNumberMatches() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        j.buildAndAssertSuccess(project); // next build number is now 2

        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        AtomicReference<String> requestBody = new AtomicReference<>();
        server.createContext("/v1/claim", (HttpExchange exchange) -> {
            requestBody.set(new String(exchange.getRequestBody().readAllBytes(), StandardCharsets.UTF_8));
            // Grant a number the job has already passed: forces NUMBER_CONFLICT.
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
            Path tokenFile = tmp.getRoot().toPath().resolve("token");
            Files.writeString(tokenFile, "test-token");
            LoopbackTokenFile tf = new LoopbackTokenFile(tokenFile, System::currentTimeMillis);
            MiteLoopbackClient client = new MiteLoopbackClient(
                    "http://127.0.0.1:" + server.getAddress().getPort(), tf, Duration.ofSeconds(5));
            VarroaClaimQueue queue = new VarroaClaimQueue(client, System::currentTimeMillis, (task, d) -> task.run());

            VarroaIntentAction action = new VarroaIntentAction("run-1", "", null, 0L);
            ScheduleResult result = Queue.getInstance().schedule2(project, 300, action);
            Queue.Item item = result.getItem();
            queue.offer(item, project);

            long deadline = System.currentTimeMillis() + 10_000L;
            while (System.currentTimeMillis() < deadline && action.getNumberConflict() == 0) {
                Thread.sleep(20L);
            }
            assertFalse("NUMBER_CONFLICT must leave the action ungranted", action.isGranted());
            assertEquals("the item records the number it could not apply", 1, action.getNumberConflict());
            assertFalse("the apply is no longer pending", action.isApplyPending());
            assertFalse("NUMBER_CONFLICT keeps the item for a new claim", item.getFuture().isCancelled());
        } finally {
            server.stop(0);
        }
    }
}
