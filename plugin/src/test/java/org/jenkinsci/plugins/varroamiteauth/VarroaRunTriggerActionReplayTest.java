package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;
import static org.junit.Assert.fail;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.contains;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

import hudson.model.FreeStyleProject;
import hudson.model.Item;
import hudson.model.ParametersDefinitionProperty;
import hudson.model.Queue;
import hudson.model.StringParameterDefinition;
import hudson.model.StringParameterValue;
import hudson.model.queue.CauseOfBlockage;
import hudson.model.queue.QueueTaskDispatcher;
import hudson.plugins.git.GitSCM;
import hudson.security.ACL;
import hudson.security.ACLContext;

import java.io.BufferedReader;
import java.io.File;
import java.io.StringReader;
import java.io.StringWriter;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.util.ArrayList;
import java.util.List;
import java.util.Set;

import jenkins.model.Jenkins;

import net.sf.json.JSONObject;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.cps.CpsFlowExecution;
import org.jenkinsci.plugins.workflow.cps.CpsScmFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.After;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.jvnet.hudson.test.MockAuthorizationStrategy;
import org.jvnet.hudson.test.MockFolder;
import org.jvnet.hudson.test.TestExtension;
import org.kohsuke.stapler.StaplerRequest2;
import org.kohsuke.stapler.StaplerResponse2;
import org.kohsuke.stapler.WebMethod;
import org.springframework.security.authentication.UsernamePasswordAuthenticationToken;
import org.springframework.security.core.authority.SimpleGrantedAuthority;

/**
 * Tests for the replay route of {@link VarroaRunTriggerAction} (D8, D9, D10):
 * the prelude's response codes, the acting user's own replay permission, the
 * template refusal, the redelivery guard, and the execution of the edited
 * script.
 */
public class VarroaRunTriggerActionReplayTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private static final String MITE_AUTHORITY = "ROLE:varroa:system-mite";

    private static final String NO_FIELD_MESSAGE =
            "Bad Request: the body names no run, no source run, no job, no script, or no acting user";

    private int jobCounter;

    @After
    public void tearDown() {
        HoldReplayDispatcher.holdRunId = null;
        VarroaTemplateDefinitions.setClassNamesForTesting(null);
        Queue.getInstance().clear();
    }

    // ---- The permitted replay ----

    @Test
    public void aPermittedActingUserReplaysTheEditedScript() throws Exception {
        WorkflowJob job = pipeline("echo 'FROM-JOB'\n");
        job.addProperty(new ParametersDefinitionProperty(new StringParameterDefinition("ENV", "dev")));
        useSecurity(job);

        StringWriter out = new StringWriter();
        StaplerResponse2 rsp = replay(body("run-replay-1", job.getFullName(), "src-run-1",
                "echo \"FROM-REPLAY ENV=${params.ENV}\"\n", actingUser("ada", "varroa-payments"),
                "{\"ENV\":\"prod\",\"EXTRA\":\"x\"}"), out);

        verify(rsp).setStatus(202);
        JSONObject answer = JSONObject.fromObject(out.toString());
        assertEquals("run-replay-1", answer.getString("runId"));
        assertEquals(List.of("EXTRA"), new ArrayList<Object>(answer.getJSONArray("droppedParameters")));

        WorkflowRun run = awaitBuild(job, 1);
        j.assertBuildStatusSuccess(j.waitForCompletion(run));
        String log = JenkinsRule.getLog(run);
        assertTrue("the build must run the edited script, got: " + log, log.contains("FROM-REPLAY ENV=prod"));
        assertFalse("the build must not run the job's own script, got: " + log, log.contains("FROM-JOB"));
        assertTrue("the replayed execution must be sandboxed",
                ((CpsFlowExecution) run.getExecution()).isSandbox());

        VarroaUserCause userCause = run.getCause(VarroaUserCause.class);
        assertNotNull("the build must carry a Varroa user cause, causes=" + run.getCauses(), userCause);
        assertEquals("ada", userCause.getUserId());
        VarroaReplayCause replayCause = run.getCause(VarroaReplayCause.class);
        assertNotNull("the build must carry a Varroa replay cause, causes=" + run.getCauses(), replayCause);
        assertEquals("src-run-1", replayCause.getSourceRunId());
        assertTrue("the log must name the source run, got: " + log,
                log.contains("Replayed from run src-run-1 through Varroa"));

        VarroaIntentAction intent = run.getAction(VarroaIntentAction.class);
        assertNotNull("the build must carry the stamped intent action", intent);
        assertEquals("run-replay-1", intent.getRunId());
        assertEquals("replay:src-run-1:1", intent.getIntentKey());
        assertEquals("ada", intent.getActor());
    }

    @Test
    public void anActingUserWithoutTheReplayPermissionQueuesNothing() throws Exception {
        j.jenkins.setNumExecutors(0);
        WorkflowJob job = pipeline("echo 'FROM-JOB'\n");
        useSecurity(job);

        StaplerResponse2 rsp = replay(body("run-replay-2", job.getFullName(), "src-run-1",
                "echo 'FROM-REPLAY'\n", actingUser("bob"), null), new StringWriter());

        RefusalAssert.assertRefused(rsp, 403, "Forbidden: the acting user may not replay this job");
        assertEquals(0, Queue.getInstance().getItems().length);
        assertNull(job.getLastBuild());
    }

    @Test
    public void aCallerWithoutTheMiteAuthorityQueuesNothing() throws Exception {
        j.jenkins.setNumExecutors(0);
        WorkflowJob job = pipeline("echo 'FROM-JOB'\n");
        String json = body("run-replay-3", job.getFullName(), "src-run-1", "echo 'x'\n", actingUser("ada"), null);

        StaplerResponse2 rsp = responseCapturing(new StringWriter());
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken("someone", "",
                List.of(new SimpleGrantedAuthority("authenticated"))))) {
            new VarroaRunTriggerAction().doReplay(requestWithBody(json), rsp);
        }

        RefusalAssert.assertRefused(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
        assertEquals(0, Queue.getInstance().getItems().length);
    }

    // ---- The prelude ----

    @Test
    public void anUnknownJobPathAnswers404() throws Exception {
        StaplerResponse2 rsp = replay(body("run-replay-4", "no-such-job", "src-run-1", "echo 'x'\n",
                actingUser("ada"), null), new StringWriter());

        RefusalAssert.assertRefused(rsp, 404, "Not Found: no item named no-such-job");
    }

    @Test
    public void aFolderAnswers409() throws Exception {
        MockFolder folder = j.createFolder("replay-folder");

        StaplerResponse2 rsp = replay(body("run-replay-5", folder.getFullName(), "src-run-1", "echo 'x'\n",
                actingUser("ada"), null), new StringWriter());

        RefusalAssert.assertRefusedContaining(rsp, 409, "not a parameterized job");
        assertEquals(0, Queue.getInstance().getItems().length);
    }

    @Test
    public void aFreestyleJobAnswers409() throws Exception {
        j.jenkins.setNumExecutors(0);
        FreeStyleProject project = j.createFreeStyleProject("replay-freestyle");

        StaplerResponse2 rsp = replay(body("run-replay-6", project.getFullName(), "src-run-1", "echo 'x'\n",
                actingUser("ada"), null), new StringWriter());

        RefusalAssert.assertRefused(rsp, 409, "Conflict: replay-freestyle is not a pipeline job");
        assertEquals(0, Queue.getInstance().getItems().length);
    }

    @Test
    public void aBodyNamingNoScriptAnswers400() throws Exception {
        j.jenkins.setNumExecutors(0);
        WorkflowJob job = pipeline("echo 'FROM-JOB'\n");

        StaplerResponse2 rsp = replay(body("run-replay-7", job.getFullName(), "src-run-1", "",
                actingUser("ada"), null), new StringWriter());

        RefusalAssert.assertRefused(rsp, 400, NO_FIELD_MESSAGE);
        assertEquals(0, Queue.getInstance().getItems().length);
    }

    @Test
    public void aBodyNamingNoSourceRunAnswers400() throws Exception {
        j.jenkins.setNumExecutors(0);
        WorkflowJob job = pipeline("echo 'FROM-JOB'\n");

        StaplerResponse2 rsp = replay(body("run-replay-12", job.getFullName(), "", "echo 'x'\n",
                actingUser("ada"), null), new StringWriter());

        RefusalAssert.assertRefused(rsp, 400, NO_FIELD_MESSAGE);
        assertEquals(0, Queue.getInstance().getItems().length);
    }

    @Test
    public void aBodyNamingNoActingUserAnswers400() throws Exception {
        j.jenkins.setNumExecutors(0);
        WorkflowJob job = pipeline("echo 'FROM-JOB'\n");

        StaplerResponse2 rsp = replay(body("run-replay-8", job.getFullName(), "src-run-1", "echo 'x'\n",
                null, null), new StringWriter());

        RefusalAssert.assertRefused(rsp, 400, NO_FIELD_MESSAGE);
        assertEquals(0, Queue.getInstance().getItems().length);
    }

    @Test
    public void aMethodOtherThanPostAnswers405() throws Exception {
        StaplerRequest2 req = mock(StaplerRequest2.class);
        when(req.getMethod()).thenReturn("GET");
        StaplerResponse2 rsp = responseCapturing(new StringWriter());
        try (ACLContext ctx = asMite()) {
            new VarroaRunTriggerAction().doReplay(req, rsp);
        }

        RefusalAssert.assertRefused(rsp, 405, "Method Not Allowed: only POST is served");
    }

    @Test
    public void theOneWordRouteCarriesNoWebMethod() throws Exception {
        assertNull("replay holds one word, so Stapler derives its token from the method name",
                VarroaRunTriggerAction.class
                        .getMethod("doReplay", StaplerRequest2.class, StaplerResponse2.class)
                        .getAnnotation(WebMethod.class));
    }

    // ---- The template refusal and the redelivery guard ----

    @Test
    public void aTemplateDefinedJobAnswers409() throws Exception {
        j.jenkins.setNumExecutors(0);
        VarroaTemplateDefinitions.setClassNamesForTesting(Set.of(FakeVarroaFlowDefinition.class.getName()));
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "replay-template");
        job.setDefinition(new FakeVarroaFlowDefinition("echo 'FROM-TEMPLATE'\n", false, false));

        StaplerResponse2 rsp = replay(body("run-replay-9", job.getFullName(), "src-run-1", "echo 'x'\n",
                actingUser("ada"), null), new StringWriter());

        RefusalAssert.assertRefused(rsp, 409, "Conflict: replay-template is template defined");
        assertEquals(0, Queue.getInstance().getItems().length);
    }

    @Test
    public void aRedeliveredReplayQueuesOneBuild() throws Exception {
        // A pipeline job is a flyweight task, so zero executors do not hold it; the dispatcher does.
        HoldReplayDispatcher.holdRunId = "run-replay-10";
        WorkflowJob job = pipeline("echo 'FROM-JOB'\n");
        String json = body("run-replay-10", job.getFullName(), "src-run-1", "echo 'FROM-REPLAY'\n",
                actingUser("ada"), null);

        StaplerResponse2 first = replay(json, new StringWriter());
        StaplerResponse2 second = replay(json, new StringWriter());

        verify(first).setStatus(202);
        verify(second).setStatus(202);
        int matching = 0;
        for (Queue.Item item : Queue.getInstance().getItems()) {
            VarroaIntentAction intent = item.getAction(VarroaIntentAction.class);
            if (intent != null && "run-replay-10".equals(intent.getRunId())) {
                matching++;
            }
        }
        assertEquals("one queue item must exist for the run identifier", 1, matching);
    }

    @Test
    public void aReplayRedeliveredAfterTheBuildStartedSchedulesNothing() throws Exception {
        WorkflowJob job = pipeline("sleep 60\n");
        useSecurity(job);
        String json = body("run-replay-13", job.getFullName(), "src-run-1", "sleep 60\n",
                actingUser("ada"), "{\"EXTRA\":\"x\"}");

        verify(replay(json, new StringWriter())).setStatus(202);
        WorkflowRun run = awaitBuild(job, 1);
        long deadline = System.currentTimeMillis() + 30000L;
        while (!run.isBuilding() && System.currentTimeMillis() < deadline) {
            Thread.sleep(50L);
        }
        assertTrue("the first replay must be building", run.isBuilding());

        StringWriter out = new StringWriter();
        StaplerResponse2 rsp = replay(json, out);

        verify(rsp).setStatus(202);
        JSONObject answer = JSONObject.fromObject(out.toString());
        assertEquals("run-replay-13", answer.getString("runId"));
        assertEquals(List.of("EXTRA"), new ArrayList<Object>(answer.getJSONArray("droppedParameters")));
        assertEquals("a repeat must queue nothing", 0, Queue.getInstance().getItems().length);

        run.doStop();
        j.waitForCompletion(run);
        j.waitUntilNoActivity();
        assertEquals("a repeat must start no second build", 1, job.getLastBuild().getNumber());
    }

    /** Holds the queued replay whose run identifier matches {@code holdRunId}. */
    @TestExtension
    public static class HoldReplayDispatcher extends QueueTaskDispatcher {
        static volatile String holdRunId;

        @Override
        public CauseOfBlockage canRun(Queue.Item item) {
            String held = holdRunId;
            if (held == null) {
                return null;
            }
            VarroaIntentAction intent = item.getAction(VarroaIntentAction.class);
            if (intent != null && held.equals(intent.getRunId())) {
                return new CauseOfBlockage() {
                    @Override
                    public String getShortDescription() {
                        return "held by the replay test";
                    }
                };
            }
            return null;
        }
    }

    // ---- The SCM definition ----

    @Test
    public void anScmDefinedJobRunsTheEditedScriptWithNoCheckout() throws Exception {
        File repo = new File(j.jenkins.getRootDir(), "replay-repo");
        repo.mkdirs();
        git(repo, "init", "-b", "master");
        git(repo, "config", "user.email", "varroa@example.com");
        git(repo, "config", "user.name", "Varroa Test");
        Files.write(new File(repo, "Jenkinsfile").toPath(),
                "echo 'FROM-SCM'\n".getBytes(StandardCharsets.UTF_8));
        git(repo, "add", "Jenkinsfile");
        git(repo, "commit", "-m", "the first commit");

        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "replay-scm");
        job.setDefinition(new CpsScmFlowDefinition(new GitSCM(repo.toURI().toString()), "Jenkinsfile"));

        StaplerResponse2 rsp = replay(body("run-replay-11", job.getFullName(), "src-run-1",
                "echo 'FROM-REPLAY'\n", actingUser("ada"), null), new StringWriter());

        verify(rsp).setStatus(202);
        WorkflowRun run = awaitBuild(job, 1);
        j.assertBuildStatusSuccess(j.waitForCompletion(run));
        String log = JenkinsRule.getLog(run);
        assertTrue("the build must run the edited script, got: " + log, log.contains("FROM-REPLAY"));
        assertFalse("the build must not run the stored Jenkinsfile, got: " + log, log.contains("FROM-SCM"));
        assertFalse("the build must not check out the script path, got: " + log,
                log.contains("Obtained Jenkinsfile from"));
        assertFalse("the build must not run git, got: " + log, log.contains("git init")
                || log.contains("Cloning") || log.contains("Checking out Revision"));
    }

    // ---- The harness ----

    private WorkflowJob pipeline(String script) throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "replay-job-" + ++jobCounter);
        job.setDefinition(new CpsFlowDefinition(script, true));
        return job;
    }

    /**
     * Enables security. Every identity reads every item, and only {@code
     * ada} holds {@link Item#CONFIGURE} on the job, which implies the
     * replay permission.
     */
    private void useSecurity(WorkflowJob job) {
        MockAuthorizationStrategy strategy = new MockAuthorizationStrategy()
                .grant(Jenkins.READ, Item.READ).everywhere().toEveryone()
                .grant(Item.CONFIGURE).onItems(job).to("ada");
        j.jenkins.setSecurityRealm(j.createDummySecurityRealm());
        j.jenkins.setAuthorizationStrategy(strategy);
    }

    private static WorkflowRun awaitBuild(WorkflowJob job, int number) throws Exception {
        long deadline = System.currentTimeMillis() + 60000L;
        while (System.currentTimeMillis() < deadline) {
            WorkflowRun run = job.getBuildByNumber(number);
            if (run != null) {
                return run;
            }
            Thread.sleep(50L);
        }
        fail("build " + number + " of " + job.getFullName() + " never started; queue="
                + List.of(Queue.getInstance().getItems()));
        return null;
    }

    /** Builds one replay body. A null acting user or parameters object omits the key. */
    private static String body(String runId, String jobPath, String sourceRunId, String script,
            String actingUser, String parameters) {
        JSONObject o = new JSONObject();
        o.put("runId", runId);
        o.put("intentKey", "replay:" + sourceRunId + ":1");
        o.put("jobPath", jobPath);
        o.put("sourceRunId", sourceRunId);
        o.put("script", script);
        String json = o.toString();
        StringBuilder sb = new StringBuilder(json.substring(0, json.length() - 1));
        if (actingUser != null) {
            sb.append(",\"actingUser\":").append(actingUser);
        }
        if (parameters != null) {
            sb.append(",\"parameters\":").append(parameters);
        }
        return sb.append('}').toString();
    }

    private static String actingUser(String name, String... groups) {
        StringBuilder sb = new StringBuilder("{\"name\":\"").append(name)
                .append("\",\"email\":\"").append(name).append("@example.com\",\"groups\":[");
        for (int i = 0; i < groups.length; i++) {
            if (i > 0) {
                sb.append(',');
            }
            sb.append('"').append(groups[i]).append('"');
        }
        return sb.append("]}").toString();
    }

    private static StaplerResponse2 replay(String json, StringWriter out) throws Exception {
        try (ACLContext ctx = asMite()) {
            StaplerResponse2 rsp = responseCapturing(out);
            new VarroaRunTriggerAction().doReplay(requestWithBody(json), rsp);
            return rsp;
        }
    }

    private static StaplerRequest2 requestWithBody(String json) throws Exception {
        StaplerRequest2 req = mock(StaplerRequest2.class);
        when(req.getMethod()).thenReturn("POST");
        when(req.getReader()).thenReturn(new BufferedReader(new StringReader(json)));
        when(req.bindJSON(eq(StringParameterValue.class), any(JSONObject.class))).thenAnswer(invocation -> {
            JSONObject jo = invocation.getArgument(1);
            return new StringParameterValue(jo.getString("name"), jo.getString("value"));
        });
        return req;
    }

    private static StaplerResponse2 responseCapturing(StringWriter writer) throws Exception {
        return RefusalAssert.capturing(writer);
    }

    private static ACLContext asMite() {
        return ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))));
    }

    private static String git(File directory, String... args) throws Exception {
        List<String> command = new ArrayList<>();
        command.add("git");
        command.addAll(List.of(args));
        ProcessBuilder builder = new ProcessBuilder(command).directory(directory).redirectErrorStream(true);
        Process process = builder.start();
        String output = new String(process.getInputStream().readAllBytes(), StandardCharsets.UTF_8);
        assertEquals("git " + String.join(" ", args) + " failed: " + output, 0, process.waitFor());
        return output;
    }
}
