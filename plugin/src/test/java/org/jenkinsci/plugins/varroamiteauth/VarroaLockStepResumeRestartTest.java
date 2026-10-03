package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import com.cloudbees.hudson.plugins.folder.Folder;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import hudson.model.Result;

import java.io.ByteArrayOutputStream;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.jvnet.hudson.test.JenkinsSessionRule;

import net.sf.json.JSONObject;

/**
 * Proves the resume gate on a real second Jenkins boot over the same
 * Jenkins home. The body sleeps across the restart, and the fake recorder
 * holds the first renew reply after the boot. The statement after the
 * sleep must not run until the recorder answers, and must never run when
 * the answer is {@code lost}.
 */
public class VarroaLockStepResumeRestartTest {

    // The script calls the step itself, not varroa.lock: a program that
    // holds the varroa global variable fails to load after a restart.
    private static final String SCRIPT = "varroaLock(name: 'deploy-prod') {\n"
            + "  echo 'in-body'\n"
            + "  sleep(time: 2, unit: 'SECONDS')\n"
            + "  echo 'after-sleep'\n"
            + "}";

    @Rule
    public JenkinsSessionRule sessions = new JenkinsSessionRule();

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private HttpServer server;
    private final RecorderStub recorder = new RecorderStub();

    @Before
    public void startRecorder() throws Exception {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/v1/cross-tenant/lock", recorder);
        server.start();
        Path tokenFile = tmp.getRoot().toPath().resolve("token");
        Files.writeString(tokenFile, "test-token");
        VarroaCrossTenantOps.setClientForTesting(new MiteLoopbackClient(
                "http://127.0.0.1:" + server.getAddress().getPort(),
                new LoopbackTokenFile(tokenFile, System::currentTimeMillis),
                Duration.ofSeconds(1), Duration.ofSeconds(30)));
    }

    @After
    public void stopRecorder() {
        recorder.renewGate.countDown();
        server.stop(0);
        VarroaCrossTenantOps.setClientForTesting(null);
    }

    @Test
    public void aResumedBodyWaitsForTheRenewToConfirmTheHold() throws Throwable {
        startBuildAndRestart();
        sessions.then(j -> {
            WorkflowRun run = awaitGate(j);
            assertFalse("the body must not advance before the renew answers",
                    JenkinsLogs.contains(run, "after-sleep"));

            recorder.renewState = "held";
            recorder.renewGate.countDown();
            j.waitForCompletion(run);
            j.assertBuildStatus(Result.SUCCESS, run);
            j.assertLogContains("the recorder confirmed the hold on alpha/deploy-prod", run);
            j.assertLogContains("after-sleep", run);
        });
    }

    @Test
    public void aResumedBodyNeverRunsAfterALostRenew() throws Throwable {
        startBuildAndRestart();
        sessions.then(j -> {
            WorkflowRun run = awaitGate(j);

            recorder.renewState = "lost";
            recorder.renewGate.countDown();
            j.waitForCompletion(run);
            j.assertBuildStatus(Result.FAILURE, run);
            j.assertLogContains("the lease for alpha/deploy-prod was lost", run);
            j.assertLogNotContains("after-sleep", run);
        });
    }

    private void startBuildAndRestart() throws Throwable {
        sessions.then(j -> {
            Folder tenant = j.jenkins.createProject(Folder.class, "alpha");
            VarroaTenantResolverTest.hostTenants(j, "alpha");
            WorkflowJob p = tenant.createProject(WorkflowJob.class, "deploy");
            p.setDefinition(new CpsFlowDefinition(SCRIPT, true));
            WorkflowRun run = (WorkflowRun) p.scheduleBuild2(0,
                    new VarroaIntentAction("run-restart", "", null, System.currentTimeMillis())).waitForStart();
            j.waitForMessage("in-body", run);
            assertEquals("no renew before the restart", 0, recorder.renews.get());
        });
    }

    /**
     * Waits until the resumed step sends its first renew, then gives the
     * sleep, which ended during the restart, time to complete if the
     * program were running.
     */
    private WorkflowRun awaitGate(org.jvnet.hudson.test.JenkinsRule j) throws Exception {
        WorkflowJob p = j.jenkins.getItemByFullName("alpha/deploy", WorkflowJob.class);
        WorkflowRun run = p.getBuildByNumber(1);
        long deadline = System.currentTimeMillis() + 30_000L;
        while (recorder.renews.get() == 0 && System.currentTimeMillis() < deadline) {
            Thread.sleep(50);
        }
        assertTrue("the resumed step must renew; result " + run.getResult() + ", log:\n"
                + org.apache.commons.io.IOUtils.toString(run.getLogReader()), recorder.renews.get() > 0);
        Thread.sleep(3_000L);
        assertTrue(run.isBuilding());
        j.assertLogContains("confirming the hold on alpha/deploy-prod after a resume", run);
        return run;
    }

    /** Reads a run's log text, so a test can assert a line is absent before the run ends. */
    private static final class JenkinsLogs {
        static boolean contains(WorkflowRun run, String text) throws Exception {
            return org.apache.commons.io.IOUtils.toString(run.getLogReader()).contains(text);
        }
    }

    /**
     * A one-lock recorder: every acquire is granted under token 5, every
     * renew waits on {@code renewGate} and then answers {@code renewState}
     * with the same token, and every release answers {@code released}.
     */
    private static final class RecorderStub implements com.sun.net.httpserver.HttpHandler {

        final AtomicInteger renews = new AtomicInteger();
        final CountDownLatch renewGate = new CountDownLatch(1);
        volatile String renewState = "held";

        @Override
        public void handle(HttpExchange exchange) throws java.io.IOException {
            ByteArrayOutputStream buf = new ByteArrayOutputStream();
            exchange.getRequestBody().transferTo(buf);
            JSONObject req = JSONObject.fromObject(buf.toString(StandardCharsets.UTF_8));
            JSONObject out = new JSONObject();
            switch (req.getString("kind")) {
                case "lock_acquire":
                    out.put("state", "held");
                    out.put("fence_token", 5L);
                    out.put("tenant", "alpha");
                    break;
                case "lock_renew":
                    renews.incrementAndGet();
                    try {
                        renewGate.await(60, TimeUnit.SECONDS);
                    } catch (InterruptedException e) {
                        Thread.currentThread().interrupt();
                    }
                    out.put("state", renewState);
                    out.put("fence_token", 5L);
                    break;
                default:
                    out.put("state", "released");
                    break;
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
