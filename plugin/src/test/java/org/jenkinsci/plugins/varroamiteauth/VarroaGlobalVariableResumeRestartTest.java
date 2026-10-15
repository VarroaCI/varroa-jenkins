package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
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
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;

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
import org.jvnet.hudson.test.JenkinsSessionRule;

/**
 * Proves that a program which holds the {@code varroa} global variable,
 * or a run handle it returned, loads again on a second Jenkins boot over
 * the same Jenkins home. Each script sleeps 10 seconds, so the session
 * stop suspends the program in the sleep. The second session asserts that
 * the step after the sleep runs only after the resume line.
 */
public class VarroaGlobalVariableResumeRestartTest {

    private static final String RESUMED = "Resuming build at";

    @Rule
    public JenkinsSessionRule sessions = new JenkinsSessionRule();

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private HttpServer server;

    /** The {@code caller_job_path} of every status request, in arrival order. */
    private final List<String> statusCallers = new CopyOnWriteArrayList<>();

    @Before
    public void startRecorder() throws Exception {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/v1/cross-tenant/build",
                ex -> respond(ex, "{\"run_id\":\"\",\"state\":\"pending\"}"));
        server.createContext("/v1/cross-tenant/status", ex -> {
            statusCallers.add(readBody(ex).getString("caller_job_path"));
            respond(ex, "{\"run_id\":\"run-9\",\"state\":\"running\"}");
        });
        server.createContext("/v1/cross-tenant/lock", VarroaGlobalVariableResumeRestartTest::lock);
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
        server.stop(0);
        VarroaCrossTenantOps.setClientForTesting(null);
    }

    @Test
    public void aRunHandleSurvivesARestart() throws Throwable {
        startBuildAndRestart("def h = varroa.build(tenant: 'beta', job: 'beta/deploy', wait: false)\n"
                + "echo(\"in-body state=${h.state}\")\n"
                + "sleep(time: 10, unit: 'SECONDS')\n"
                + "def r = h.refresh()\n"
                + "echo(\"after-sleep state=${r.state} runId=${r.runId}\")\n");
        sessions.then(j -> {
            WorkflowRun run = resumedRun(j);
            j.waitForCompletion(run);
            j.assertBuildStatus(Result.SUCCESS, run);
            j.assertLogNotContains("ClassNotFoundException", run);
            j.assertLogContains("after-sleep state=running runId=run-9", run);
            assertEquals(List.of("alpha/deploy"), statusCallers);
            assertCrossedTheRestart(run);
        });
    }

    @Test
    public void aNestedVarroaLockResumes() throws Throwable {
        startBuildAndRestart("varroa.lock(name: 'outer') {\n"
                + "  varroa.lock(name: 'inner') {\n"
                + "    echo 'in-body'\n"
                + "    sleep(time: 10, unit: 'SECONDS')\n"
                + "    echo(\"after-sleep name=${varroa.lock.name} token=${varroa.lock.token}\")\n"
                + "  }\n"
                + "}\n");
        sessions.then(j -> {
            WorkflowRun run = resumedRun(j);
            j.waitForCompletion(run);
            j.assertBuildStatus(Result.SUCCESS, run);
            j.assertLogNotContains("ClassNotFoundException", run);
            j.assertLogContains("after-sleep name=inner token=5", run);
            assertCrossedTheRestart(run);
        });
    }

    private void startBuildAndRestart(String script) throws Throwable {
        sessions.then(j -> {
            Folder tenant = j.jenkins.createProject(Folder.class, "alpha");
            VarroaTenantResolverTest.hostTenants(j, "alpha");
            WorkflowJob p = tenant.createProject(WorkflowJob.class, "deploy");
            p.setDefinition(new CpsFlowDefinition(script, true));
            WorkflowRun run = (WorkflowRun) p.scheduleBuild2(0,
                    new VarroaIntentAction("run-restart", "", null, System.currentTimeMillis())).waitForStart();
            j.waitForMessage("Sleeping for", run);
            assertTrue("the sleep holds the program at the restart", run.isBuilding());
        });
    }

    private static WorkflowRun resumedRun(JenkinsRule j) {
        return j.jenkins.getItemByFullName("alpha/deploy", WorkflowJob.class).getBuildByNumber(1);
    }

    /**
     * Asserts that the build stopped in the sleep and continued after the
     * restart. Only a resumed build prints the resume line between the two
     * markers.
     */
    private static void assertCrossedTheRestart(WorkflowRun run) throws java.io.IOException {
        String log = JenkinsRule.getLog(run);
        int inBody = log.indexOf("in-body");
        int resumed = log.indexOf(RESUMED);
        int afterSleep = log.indexOf("after-sleep");
        assertTrue("the build resumes after the restart; log:\n" + log, resumed >= 0);
        assertTrue("the body starts before the restart; log:\n" + log, inBody >= 0 && inBody < resumed);
        assertTrue("the step after the sleep runs only after the resume; log:\n" + log, afterSleep > resumed);
    }

    /** Grants every acquire and confirms every renew under fence token 5. */
    private static void lock(HttpExchange exchange) throws java.io.IOException {
        JSONObject req = readBody(exchange);
        String kind = req.getString("kind");
        if ("lock_acquire".equals(kind) || "lock_renew".equals(kind)) {
            respond(exchange, "{\"state\":\"held\",\"fence_token\":5,\"tenant\":\"alpha\"}");
        } else {
            respond(exchange, "{\"state\":\"released\"}");
        }
    }

    private static JSONObject readBody(HttpExchange exchange) throws java.io.IOException {
        ByteArrayOutputStream buf = new ByteArrayOutputStream();
        exchange.getRequestBody().transferTo(buf);
        return JSONObject.fromObject(buf.toString(StandardCharsets.UTF_8));
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
}
