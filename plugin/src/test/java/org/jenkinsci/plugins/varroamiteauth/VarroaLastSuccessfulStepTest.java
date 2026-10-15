package org.jenkinsci.plugins.varroamiteauth;

import com.sun.net.httpserver.HttpServer;

import hudson.model.Result;

import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;

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
 * Tests for {@link VarroaLastSuccessfulStep}: a veto or a failure fails
 * the step with the recorder's code and text, and never answers null.
 */
public class VarroaLastSuccessfulStepTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private HttpServer loopback;

    @Before
    public void setUp() throws Exception {
        loopback = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
    }

    @After
    public void tearDown() {
        loopback.stop(0);
        VarroaCrossTenantOps.setClientForTesting(null);
    }

    private void installLoopbackAnswering(String body) throws Exception {
        loopback.createContext("/v1/cross-tenant/status", exchange -> {
            byte[] resp = body.getBytes(StandardCharsets.UTF_8);
            exchange.getResponseHeaders().set("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, resp.length);
            try (OutputStream os = exchange.getResponseBody()) {
                os.write(resp);
            }
            exchange.close();
        });
        loopback.start();
        Path tokenFile = tmp.getRoot().toPath().resolve("token");
        Files.writeString(tokenFile, "test-token");
        LoopbackTokenFile tf = new LoopbackTokenFile(tokenFile, System::currentTimeMillis);
        VarroaCrossTenantOps.setClientForTesting(new MiteLoopbackClient(
                "http://127.0.0.1:" + loopback.getAddress().getPort(), tf,
                Duration.ofSeconds(1), Duration.ofSeconds(5)));
    }

    private WorkflowRun runLastSuccessful(Result expected) throws Exception {
        WorkflowJob p = j.jenkins.createProject(WorkflowJob.class, "p-" + System.nanoTime());
        p.setDefinition(new CpsFlowDefinition(
                "def h = varroa.lastSuccessful(tenant: 'beta', job: 'release'); echo(\"HANDLE=${h}\")", true));
        return j.buildAndAssertStatus(expected, p);
    }

    @Test
    public void aCrossTenantVetoFailsTheStepWithTheVetoMessage() throws Exception {
        installLoopbackAnswering(
                "{\"reason_code\":\"CROSS_TENANT\",\"message\":\"Tenant beta exports no job matching release.\"}");
        WorkflowRun run = runLastSuccessful(Result.FAILURE);
        j.assertLogContains("varroaLastSuccessful: CROSS_TENANT: Tenant beta exports no job matching release.", run);
        j.assertLogNotContains("HANDLE=", run);
    }

    @Test
    public void aFailureShowsTheRecorderCodeAndText() throws Exception {
        installLoopbackAnswering("{\"code\":\"INTERNAL\",\"error\":\"read runs: boom\"}");
        WorkflowRun run = runLastSuccessful(Result.FAILURE);
        j.assertLogContains("varroaLastSuccessful: INTERNAL: read runs: boom", run);
    }

    @Test
    public void noSuccessfulRunStillAnswersNull() throws Exception {
        installLoopbackAnswering("{\"run_id\":\"\"}");
        WorkflowRun run = runLastSuccessful(Result.SUCCESS);
        j.assertLogContains("HANDLE=null", run);
    }
}
