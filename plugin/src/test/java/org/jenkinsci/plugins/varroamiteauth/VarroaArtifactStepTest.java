package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;

import com.sun.net.httpserver.HttpServer;

import hudson.model.Result;
import hudson.slaves.DumbSlave;

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
 * Tests for {@link VarroaArtifactStep} (task 9.10): the signed URL crosses
 * the agent's workspace channel and the download happens there, on a real
 * agent, not the controller.
 */
public class VarroaArtifactStepTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private HttpServer loopback;
    private HttpServer blobServer;

    @Before
    public void setUp() throws Exception {
        loopback = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        blobServer = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
    }

    @After
    public void tearDown() {
        loopback.stop(0);
        blobServer.stop(0);
        VarroaCrossTenantOps.setClientForTesting(null);
    }

    @Test
    public void theSignedUrlCrossesTheChannelAndTheAgentDownloadsTheBytes() throws Exception {
        byte[] artifactBytes = "varroa-cross-tenant-artifact-bytes".getBytes(StandardCharsets.UTF_8);
        blobServer.createContext("/blob/app.jar", exchange -> {
            exchange.sendResponseHeaders(200, artifactBytes.length);
            try (OutputStream os = exchange.getResponseBody()) {
                os.write(artifactBytes);
            }
            exchange.close();
        });
        blobServer.start();
        String artifactUrl = "http://127.0.0.1:" + blobServer.getAddress().getPort() + "/blob/app.jar";

        loopback.createContext("/v1/cross-tenant/artifact", exchange -> {
            String body = "{\"url\":\"" + artifactUrl + "\",\"expires_at\":\"2030-01-01T00:00:00Z\"}";
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

        DumbSlave agent = j.createOnlineSlave();
        WorkflowJob p = j.jenkins.createProject(WorkflowJob.class, "p-" + System.nanoTime());
        p.setDefinition(new CpsFlowDefinition(
                "node('" + agent.getNodeName() + "') { "
                        + "varroaArtifact(runId: 'run-1', path: 'app.jar', target: 'downloaded.jar'); "
                        + "def text = readFile(file: 'downloaded.jar'); "
                        + "echo(\"LEN=${text.length()}\") }",
                true));
        WorkflowRun run = j.buildAndAssertStatus(Result.SUCCESS, p);

        String downloaded = agent.getWorkspaceRoot().child(p.getName()).child("downloaded.jar").readToString();
        assertEquals(new String(artifactBytes, StandardCharsets.UTF_8), downloaded);
        j.assertLogContains("LEN=" + artifactBytes.length, run);
    }

    private void installLoopbackAnswering(String body) throws Exception {
        loopback.createContext("/v1/cross-tenant/artifact", exchange -> {
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

    private WorkflowRun runCopy() throws Exception {
        WorkflowJob p = j.jenkins.createProject(WorkflowJob.class, "p-" + System.nanoTime());
        p.setDefinition(new CpsFlowDefinition(
                "node { varroaArtifact(runId: 'run-1', path: 'does/not/exist') }", true));
        return j.buildAndAssertStatus(Result.FAILURE, p);
    }

    @Test
    public void aNotFoundReplyShowsTheRecorderCodeAndText() throws Exception {
        installLoopbackAnswering("{\"code\":\"NOT_FOUND\",\"error\":\"no artifact at path \\\"does/not/exist\\\"\"}");
        WorkflowRun run = runCopy();
        j.assertLogContains("varroaArtifact: NOT_FOUND: no artifact at path \"does/not/exist\"", run);
        j.assertLogNotContains("did not answer", run);
    }

    @Test
    public void aCrossTenantVetoFailsWithTheVetoMessage() throws Exception {
        installLoopbackAnswering(
                "{\"reason_code\":\"CROSS_TENANT\",\"message\":\"Tenant beta does not grant job beta/x to alpha.\"}");
        WorkflowRun run = runCopy();
        j.assertLogContains("varroaArtifact: CROSS_TENANT: Tenant beta does not grant job beta/x to alpha.", run);
        j.assertLogNotContains("no signed URL", run);
    }
}
