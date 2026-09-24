package org.jenkinsci.plugins.varroamiteauth;

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
import java.util.concurrent.atomic.AtomicInteger;

import net.sf.json.JSONObject;

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
 * Tests for the {@code varroa} global variable and {@code VarroaRunHandle}
 * (task 9.4): every handle property, the five state values, {@code
 * lastSuccessful} returning null with no successful run, and {@code
 * varroa.lock.token} being null outside a block.
 */
public class VarroaGlobalVariableTest {

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
    }

    private void installClient() throws Exception {
        server.start();
        Path tokenFile = tmp.getRoot().toPath().resolve("token");
        Files.writeString(tokenFile, "test-token");
        LoopbackTokenFile tf = new LoopbackTokenFile(tokenFile, System::currentTimeMillis);
        VarroaCrossTenantOps.setClientForTesting(new MiteLoopbackClient(
                "http://127.0.0.1:" + server.getAddress().getPort(), tf,
                Duration.ofSeconds(1), Duration.ofSeconds(5)));
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

    private static String requestKind(HttpExchange exchange) throws java.io.IOException {
        ByteArrayOutputStream buf = new ByteArrayOutputStream();
        exchange.getRequestBody().transferTo(buf);
        return JSONObject.fromObject(buf.toString(StandardCharsets.UTF_8)).getString("kind");
    }

    @Test
    public void everyHandlePropertyAndTheFiveStatesAndLastSuccessfulNullAndLockTokenNull() throws Exception {
        installClient();
        AtomicInteger statusCalls = new AtomicInteger();
        server.createContext("/v1/cross-tenant/build", ex -> respond(ex, "{\"run_id\":\"\",\"state\":\"pending\"}"));
        server.createContext("/v1/cross-tenant/status", ex -> {
            String kind = requestKind(ex);
            if ("last_successful".equals(kind)) {
                // No successful run yet: the reply carries an empty run id.
                respond(ex, "{\"run_id\":\"\"}");
                return;
            }
            int n = statusCalls.incrementAndGet();
            if (n == 1) {
                respond(ex, "{\"run_id\":\"\",\"state\":\"pending\"}");
            } else if (n == 2) {
                respond(ex, "{\"run_id\":\"run-42\",\"state\":\"queued\"}");
            } else if (n == 3) {
                respond(ex, "{\"run_id\":\"run-42\",\"state\":\"running\"}");
            } else {
                respond(ex, "{\"run_id\":\"run-42\",\"state\":\"completed\",\"result\":\"SUCCESS\","
                        + "\"build_number\":7,\"url\":\"http://varroa.test/run-42\","
                        + "\"started_at\":\"2026-01-01T00:00:00Z\",\"ended_at\":\"2026-01-01T00:05:00Z\"}");
            }
        });
        server.createContext("/v1/cross-tenant/artifact", ex -> respond(ex, "{\"artifacts\":[{\"path\":\"out.jar\"}]}"));
        server.createContext("/v1/cross-tenant/lock", ex -> respond(ex, "{}"));

        WorkflowJob p = j.jenkins.createProject(WorkflowJob.class, "p-" + System.nanoTime());
        p.setDefinition(new CpsFlowDefinition(
                "node {\n"
                        + "  def h = varroa.build(tenant: 'beta', job: 'beta/deploy')\n"
                        + "  echo(\"runId=${h.runId}\")\n"
                        + "  echo(\"tenant=${h.tenant}\")\n"
                        + "  echo(\"job=${h.job}\")\n"
                        + "  echo(\"buildNumber=${h.buildNumber}\")\n"
                        + "  echo(\"state=${h.state}\")\n"
                        + "  echo(\"result=${h.result}\")\n"
                        + "  echo(\"startedAt=${h.startedAt}\")\n"
                        + "  echo(\"endedAt=${h.endedAt}\")\n"
                        + "  echo(\"url=${h.url}\")\n"
                        + "  echo(\"artifacts=${h.artifacts.size()}\")\n"
                        + "  echo(\"intentKey=${h.intentKey}\")\n"
                        + "  echo(\"lockOutside=${varroa.lock.token}\")\n"
                        + "  echo(\"lockNameOutside=${varroa.lock.name} lockTenantOutside=${varroa.lock.tenant}\")\n"
                        + "  def ls = varroa.lastSuccessful(tenant: 'beta', job: 'beta/never-built')\n"
                        + "  echo(\"lastSuccessful=${ls}\")\n"
                        + "}",
                true));

        WorkflowRun run = j.buildAndAssertStatus(Result.SUCCESS, p);

        j.assertLogContains("runId=run-42", run);
        j.assertLogContains("tenant=beta", run);
        j.assertLogContains("job=beta/deploy", run);
        j.assertLogContains("buildNumber=7", run);
        j.assertLogContains("state=completed", run);
        j.assertLogContains("result=SUCCESS", run);
        j.assertLogContains("startedAt=2026-01-01T00:00:00Z", run);
        j.assertLogContains("endedAt=2026-01-01T00:05:00Z", run);
        j.assertLogContains("url=http://varroa.test/run-42", run);
        j.assertLogContains("artifacts=1", run);
        j.assertLogContains("intentKey=upstream::s1", run);
        j.assertLogContains("lockOutside=null", run);
        j.assertLogContains("lockNameOutside=null lockTenantOutside=null", run);
        j.assertLogContains("lastSuccessful=null", run);
    }
}
