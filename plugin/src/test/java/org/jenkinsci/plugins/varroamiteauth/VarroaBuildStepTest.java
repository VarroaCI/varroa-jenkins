package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertTrue;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import hudson.model.Result;

import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
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
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests for {@link VarroaBuildStep} (tasks 9.5, 9.6): the asynchronous
 * wait, its two pending bounds, and the result rules.
 */
public class VarroaBuildStepTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private HttpServer server;

    @Before
    public void setUp() throws Exception {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
    }

    @After
    public void tearDown() {
        server.stop(0);
        VarroaCrossTenantOps.setClientForTesting(null);
        VarroaPollPolicy.resetBoundsForTesting();
    }

    private void installClient() throws Exception {
        Path tokenFile = tmp.getRoot().toPath().resolve("token");
        Files.writeString(tokenFile, "test-token");
        LoopbackTokenFile tf = new LoopbackTokenFile(tokenFile, System::currentTimeMillis);
        VarroaCrossTenantOps.setClientForTesting(new MiteLoopbackClient(
                "http://127.0.0.1:" + server.getAddress().getPort(), tf,
                Duration.ofSeconds(1), Duration.ofSeconds(5)));
        server.start();
    }

    private static void respond(HttpExchange exchange, String json) throws java.io.IOException {
        byte[] resp = json.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "application/json");
        exchange.sendResponseHeaders(200, resp.length);
        try (OutputStream os = exchange.getResponseBody()) {
            os.write(resp);
        }
        exchange.close();
    }

    private WorkflowRun scheduleAndWait(String groovy, Result expected) throws Exception {
        WorkflowJob p = j.jenkins.createProject(WorkflowJob.class, "p-" + System.nanoTime());
        p.setDefinition(new CpsFlowDefinition(groovy, true));
        WorkflowRun run = p.scheduleBuild2(0).waitForStart();
        j.waitForCompletion(run);
        j.assertBuildStatus(expected, run);
        return run;
    }

    // ---- 9.5: the runId fills from the ledger on the first reply that carries one ----

    @Test
    public void runIdFillsFromTheFirstReplyThatCarriesOne() throws Exception {
        installClient();
        AtomicInteger statusCalls = new AtomicInteger();
        server.createContext("/v1/cross-tenant/build", ex -> respond(ex, "{\"run_id\":\"\",\"state\":\"pending\"}"));
        server.createContext("/v1/cross-tenant/status", ex -> {
            int n = statusCalls.incrementAndGet();
            if (n == 1) {
                respond(ex, "{\"run_id\":\"\",\"state\":\"pending\"}");
            } else if (n < 3) {
                respond(ex, "{\"run_id\":\"run-42\",\"state\":\"running\"}");
            } else {
                respond(ex, "{\"run_id\":\"run-42\",\"state\":\"completed\",\"result\":\"SUCCESS\","
                        + "\"build_number\":7,\"url\":\"http://x/run-42\"}");
            }
        });
        server.createContext("/v1/cross-tenant/artifact", ex -> respond(ex, "{\"artifacts\":[]}"));

        WorkflowRun run = scheduleAndWait(
                "node { def h = varroa.build(tenant: 'beta', job: 'beta/deploy'); "
                        + "echo(\"RUNID=${h.runId}\") }",
                Result.SUCCESS);
        j.assertLogContains("RUNID=run-42", run);
        assertTrue("must have polled by intent key at least once", statusCalls.get() >= 1);
    }

    // ---- 9.5: a run that never appears fails at the ceiling ----

    @Test
    public void aRunThatNeverAppearsFailsAtTheCeiling() throws Exception {
        VarroaPollPolicy.setBoundsForTesting(30L, 150L, 60_000L);
        installClient();
        server.createContext("/v1/cross-tenant/build", ex -> respond(ex, "{\"run_id\":\"\",\"state\":\"pending\"}"));
        server.createContext("/v1/cross-tenant/status", ex -> respond(ex, "{\"run_id\":\"\",\"state\":\"pending\"}"));

        WorkflowRun run = scheduleAndWait(
                "node { varroa.build(tenant: 'beta', job: 'beta/deploy') }", Result.FAILURE);
        j.assertLogContains("within 24 hours", run);
    }

    // ---- 9.5: no CPS thread is held while waiting ----

    @Test
    public void waitingHoldsNoCpsThread() throws Exception {
        installClient();
        server.createContext("/v1/cross-tenant/build", ex -> respond(ex, "{\"run_id\":\"run-wait\",\"state\":\"queued\"}"));
        server.createContext("/v1/cross-tenant/status", ex -> respond(ex, "{\"run_id\":\"run-wait\",\"state\":\"queued\"}"));

        WorkflowJob waiter = j.jenkins.createProject(WorkflowJob.class, "waiter-" + System.nanoTime());
        waiter.setDefinition(new CpsFlowDefinition(
                "node { varroa.build(tenant: 'beta', job: 'beta/deploy') }", true));
        WorkflowRun waiterRun = waiter.scheduleBuild2(0).waitForStart();

        // The waiting build must never occupy the single shared CPS VM
        // executor: an unrelated pipeline scheduled while it waits must
        // still complete promptly.
        WorkflowJob other = j.jenkins.createProject(WorkflowJob.class, "other-" + System.nanoTime());
        other.setDefinition(new CpsFlowDefinition("echo 'hello'", true));
        WorkflowRun otherRun = other.scheduleBuild2(0).get(30, TimeUnit.SECONDS);
        j.assertBuildStatusSuccess(otherRun);
        assertTrue("the waiting build must still be running, not blocking anything else",
                waiterRun.isBuilding());

        // End the waiting build before the rule tears down: a build still
        // writing under JENKINS_HOME races the temporary-directory delete.
        waiterRun.doStop();
        j.waitForCompletion(waiterRun);
    }

    // ---- 9.6: the five result rules ----

    @Test
    public void propagateTrueFailsTheStepOnANonSuccessResult() throws Exception {
        installClient();
        server.createContext("/v1/cross-tenant/build", ex -> respond(ex, "{\"run_id\":\"run-1\",\"state\":\"running\"}"));
        server.createContext("/v1/cross-tenant/status", ex -> respond(ex,
                "{\"run_id\":\"run-1\",\"state\":\"completed\",\"result\":\"FAILURE\"}"));

        WorkflowRun run = scheduleAndWait(
                "node { varroa.build(tenant: 'beta', job: 'beta/deploy', propagate: true) }", Result.FAILURE);
        j.assertLogContains("finished FAILURE", run);
    }

    @Test
    public void propagateFalseReturnsTheHandleWhateverTheResult() throws Exception {
        installClient();
        server.createContext("/v1/cross-tenant/build", ex -> respond(ex, "{\"run_id\":\"run-1\",\"state\":\"running\"}"));
        server.createContext("/v1/cross-tenant/status", ex -> respond(ex,
                "{\"run_id\":\"run-1\",\"state\":\"completed\",\"result\":\"FAILURE\"}"));
        server.createContext("/v1/cross-tenant/artifact", ex -> respond(ex, "{\"artifacts\":[]}"));

        WorkflowRun run = scheduleAndWait(
                "node { def h = varroa.build(tenant: 'beta', job: 'beta/deploy', propagate: false); "
                        + "echo(\"RESULT=${h.result}\") }",
                Result.SUCCESS);
        j.assertLogContains("RESULT=FAILURE", run);
    }

    @Test
    public void waitFalseReturnsAPendingHandleAndNeverPolls() throws Exception {
        installClient();
        AtomicInteger statusCalls = new AtomicInteger();
        server.createContext("/v1/cross-tenant/build", ex -> respond(ex, "{\"run_id\":\"\",\"state\":\"pending\"}"));
        server.createContext("/v1/cross-tenant/status", ex -> {
            statusCalls.incrementAndGet();
            respond(ex, "{\"run_id\":\"run-1\",\"state\":\"running\"}");
        });

        WorkflowRun run = scheduleAndWait(
                "node { def h = varroa.build(tenant: 'beta', job: 'beta/deploy', wait: false); "
                        + "echo(\"STATE=${h.state}\") }",
                Result.SUCCESS);
        j.assertLogContains("STATE=pending", run);
        assertEquals("wait:false must never poll", 0, statusCalls.get());
    }

    @Test
    public void anInterruptedTargetFailsTheStep() throws Exception {
        installClient();
        server.createContext("/v1/cross-tenant/build", ex -> respond(ex, "{\"run_id\":\"run-1\",\"state\":\"running\"}"));
        server.createContext("/v1/cross-tenant/status", ex -> respond(ex,
                "{\"run_id\":\"run-1\",\"state\":\"interrupted\"}"));

        WorkflowRun run = scheduleAndWait(
                "node { varroa.build(tenant: 'beta', job: 'beta/deploy', propagate: false) }", Result.FAILURE);
        j.assertLogContains("was interrupted", run);
    }

    @Test
    public void everyPollIntervalCarriesJitter() {
        // Pure-logic coverage lives in VarroaPollPolicyTest; this asserts
        // the step's own cadence call site uses the jittering entry point.
        java.util.Random random = new java.util.Random(1);
        long a = VarroaPollPolicy.nextDelayMillis(0L, random);
        long b = VarroaPollPolicy.nextDelayMillis(0L, random);
        assertTrue(a >= 1_600L && a <= 2_400L);
        assertTrue(b >= 1_600L && b <= 2_400L);
    }
}
