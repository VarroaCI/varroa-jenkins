package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertTrue;
import static org.junit.Assert.fail;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

import hudson.model.Item;
import hudson.model.Queue;
import hudson.security.ACL;
import hudson.security.ACLContext;

import java.io.BufferedReader;
import java.io.StringReader;
import java.io.StringWriter;
import java.util.List;
import java.util.Set;

import jenkins.model.Jenkins;
import jenkins.model.ParameterizedJobMixIn;

import net.sf.json.JSONObject;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.After;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.jvnet.hudson.test.MockAuthorizationStrategy;
import hudson.model.queue.CauseOfBlockage;
import hudson.model.queue.QueueTaskDispatcher;
import org.jvnet.hudson.test.TestExtension;
import org.kohsuke.stapler.StaplerRequest2;
import org.kohsuke.stapler.StaplerResponse2;
import org.springframework.security.authentication.UsernamePasswordAuthenticationToken;
import org.springframework.security.core.authority.SimpleGrantedAuthority;


/**
 * Tests for the restart route of {@link VarroaRunTriggerAction}: a restart
 * from a stage skips the stages before it, the refusals name their reasons,
 * and a repeated run identifier schedules nothing.
 *
 * <p>The declarative plugin is a test-scope dependency, so the restart
 * tests run a real declarative source build and restart it.
 */
public class VarroaRunTriggerActionRestartTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private static final String MITE_AUTHORITY = "ROLE:varroa:system-mite";

    private static final String DECLARATIVE = "pipeline {\n"
            + "  agent any\n"
            + "  stages {\n"
            + "    stage('one') { steps { echo 'ONE-RUN' } }\n"
            + "    stage('two') { steps { echo 'TWO-RUN' } }\n"
            + "  }\n"
            + "}\n";

    private int jobCounter;

    @After
    public void tearDown() {
        Queue.getInstance().clear();
    }

    // ---- The permitted restart ----

    @Test
    public void aRestartFromStageTwoSkipsStageOne() throws Exception {
        WorkflowJob job = declarativeJob();
        WorkflowRun source = completedSource(job, "src-run-1");

        StringWriter out = new StringWriter();
        StaplerResponse2 rsp = restart(restartBody("run-restart-1", job.getFullName(), "src-run-1", 1,
                "two", "ada"), out);

        verify(rsp).setStatus(202);
        WorkflowRun run = awaitBuild(job, 2);
        j.assertBuildStatusSuccess(j.waitForCompletion(run));
        String log = JenkinsRule.getLog(run);
        assertTrue("the restart must run stage two, got: " + log, log.contains("TWO-RUN"));
        assertFalse("the restart must skip stage one, got: " + log, log.contains("ONE-RUN"));

        VarroaRestartCause cause = run.getCause(VarroaRestartCause.class);
        assertNotNull("the build must carry a Varroa restart cause, causes=" + run.getCauses(), cause);
        assertEquals("src-run-1", cause.getSourceRunId());
        assertEquals("two", cause.getStage());
        VarroaIntentAction intent = run.getAction(VarroaIntentAction.class);
        assertNotNull("the build must carry the stamped intent action", intent);
        assertEquals("run-restart-1", intent.getRunId());
        assertEquals("restart:src-run-1:run-restart-1", intent.getIntentKey());
        assertEquals("the source build must stay as it was", "src-run-1",
                source.getAction(VarroaIntentAction.class).getRunId());
    }

    @Test
    public void aRepeatedRunIdSchedulesNothing() throws Exception {
        WorkflowJob job = declarativeJob();
        completedSource(job, "src-run-2");

        StaplerResponse2 first = restart(restartBody("run-restart-2", job.getFullName(), "src-run-2", 1,
                "two", "ada"), new StringWriter());
        verify(first).setStatus(202);
        j.waitForCompletion(awaitBuild(job, 2));

        StaplerResponse2 repeat = restart(restartBody("run-restart-2", job.getFullName(), "src-run-2", 1,
                "two", "ada"), new StringWriter());
        verify(repeat).setStatus(202);
        j.waitUntilNoActivity();
        assertEquals("a repeat must start no third build", 2, job.getLastBuild().getNumber());
    }

    // ---- The refusals ----

    @Test
    public void aScriptedPipelineIsRefusedAsNotDeclarative() throws Exception {
        WorkflowJob job = scriptedJob();
        completedSource(job, "src-run-3");

        StaplerResponse2 rsp = restart(restartBody("run-restart-3", job.getFullName(), "src-run-3", 1,
                "one", "ada"), new StringWriter());

        RefusalAssert.assertRefused(rsp, 409, "Conflict: not-declarative");
        assertEquals("a refused restart must queue nothing", 0, Queue.getInstance().getItems().length);
    }

    @Test
    public void anUnlistedStageIsRefused() throws Exception {
        WorkflowJob job = declarativeJob();
        completedSource(job, "src-run-4");

        StaplerResponse2 rsp = restart(restartBody("run-restart-4", job.getFullName(), "src-run-4", 1,
                "publish", "ada"), new StringWriter());

        RefusalAssert.assertRefused(rsp, 409, "Conflict: stage publish is not restartable");
        assertEquals(1, job.getLastBuild().getNumber());
    }

    @Test
    public void aSourceNumberWithAnotherRunIdIsRefused() throws Exception {
        WorkflowJob job = declarativeJob();
        completedSource(job, "src-run-5");

        StaplerResponse2 rsp = restart(restartBody("run-restart-5", job.getFullName(), "other-run", 1,
                "two", "ada"), new StringWriter());

        RefusalAssert.assertRefused(rsp, 409, "Conflict: build 1 of " + job.getFullName()
                + " is no longer on this cell");
    }

    @Test
    public void aCallerWithoutTheMiteAuthorityIsForbidden() throws Exception {
        WorkflowJob job = declarativeJob();
        completedSource(job, "src-run-6");

        StaplerResponse2 rsp = restartWithoutAuthority(restartBody("run-restart-6", job.getFullName(),
                "src-run-6", 1, "two", "ada"));

        RefusalAssert.assertRefused(rsp, 403,
                "Forbidden: requires ROLE:varroa:system-mite authority");
    }

    @Test
    public void aGetIsNotAllowed() throws Exception {
        StaplerRequest2 req = mock(StaplerRequest2.class);
        when(req.getMethod()).thenReturn("GET");
        StaplerResponse2 rsp = RefusalAssert.capturing();
        new VarroaRunTriggerAction().doRestart(req, rsp);

        RefusalAssert.assertRefused(rsp, 405, "Method Not Allowed: only POST is served");
    }

    @Test
    public void anActingUserWithoutTheBuildPermissionIsForbidden() throws Exception {
        WorkflowJob job = declarativeJob();
        completedSource(job, "src-run-7");
        MockAuthorizationStrategy strategy = new MockAuthorizationStrategy()
                .grant(Jenkins.READ, Item.READ).everywhere().toEveryone()
                .grant(Item.CONFIGURE).onItems(job).to("ada");
        j.jenkins.setSecurityRealm(j.createDummySecurityRealm());
        j.jenkins.setAuthorizationStrategy(strategy);

        StaplerResponse2 rsp = restart(restartBody("run-restart-7", job.getFullName(), "src-run-7", 1,
                "two", "bob"), new StringWriter());

        RefusalAssert.assertRefused(rsp, 403, "Forbidden: the acting user may not build this job");
        assertEquals(1, job.getLastBuild().getNumber());
    }

    @Test
    public void aBuildThatHasNotEndedIsRefused() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "restart-busy-" + ++jobCounter);
        job.setDefinition(new CpsFlowDefinition(
                "pipeline {\n  agent any\n  stages {\n"
                + "    stage('one') { steps { sleep 600 } }\n"
                + "    stage('two') { steps { echo 'TWO' } }\n  }\n}\n", true));
        VarroaIntentAction intent = new VarroaIntentAction("src-busy", "", null, System.currentTimeMillis());
        intent.setOrigin(IntentKey.ORIGIN_VARROA_INTENT);
        intent.setIntentKey("manual:src-busy");
        ParameterizedJobMixIn.scheduleBuild2(job, 0, intent);
        WorkflowRun source = awaitBuild(job, 1);
        assertTrue("the source build must still be building", source.isBuilding());

        try {
            StaplerResponse2 rsp = restart(restartBody("run-restart-busy", job.getFullName(), "src-busy", 1,
                    "two", "ada"), new StringWriter());

            RefusalAssert.assertRefused(rsp, 409, "Conflict: the source build has not ended");
        } finally {
            source.doStop();
        }
    }

    @Test
    public void aMissingSourceBuildIsRefused() throws Exception {
        WorkflowJob job = declarativeJob();

        StaplerResponse2 rsp = restart(restartBody("run-restart-missing", job.getFullName(), "src-missing", 99,
                "two", "ada"), new StringWriter());

        RefusalAssert.assertRefused(rsp, 409, "Conflict: build 99 of " + job.getFullName()
                + " is no longer on this cell");
        assertEquals(0, Queue.getInstance().getItems().length);
    }

    @Test
    public void aTemplateDefinedJobIsRefused() throws Exception {
        VarroaTemplateDefinitions.setClassNamesForTesting(Set.of(FakeVarroaFlowDefinition.class.getName()));
        try {
            WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "restart-template-" + ++jobCounter);
            job.setDefinition(new FakeVarroaFlowDefinition("echo 'FROM-TEMPLATE'\n", false, false));

            StaplerResponse2 rsp = restart(restartBody("run-restart-template", job.getFullName(), "src-t", 1,
                    "two", "ada"), new StringWriter());

            RefusalAssert.assertRefused(rsp, 409, "Conflict: " + job.getFullName() + " is template defined");
        } finally {
            VarroaTemplateDefinitions.setClassNamesForTesting(null);
        }
    }

    @Test
    public void aRestartKeepsTheUnstashableStagesWithPreserveStashes() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "restart-stash-" + ++jobCounter);
        job.setDefinition(new CpsFlowDefinition(
                "pipeline {\n  agent any\n  options { preserveStashes(buildCount: 2) }\n  stages {\n"
                + "    stage('one') { steps { writeFile file: 'x.txt', text: 'hi'; stash name: 'files', includes: 'x.txt' } }\n"
                + "    stage('two') { steps { unstash 'files'; echo 'TWO-UNSTASH' } }\n  }\n}\n", true));
        completedSource(job, "src-stash");

        StaplerResponse2 rsp = restart(restartBody("run-restart-stash", job.getFullName(), "src-stash", 1,
                "two", "ada"), new StringWriter());

        verify(rsp).setStatus(202);
        WorkflowRun run = awaitBuild(job, 2);
        j.assertBuildStatusSuccess(j.waitForCompletion(run));
        assertTrue("the restarted stage must unstash, got: " + JenkinsRule.getLog(run),
                JenkinsRule.getLog(run).contains("TWO-UNSTASH"));
    }

    @Test
    public void aRepeatedRunIdWhileQueuedSchedulesNothing() throws Exception {
        WorkflowJob job = declarativeJob();
        completedSource(job, "src-held");

        // The dispatcher holds the restart in the queue, so the repeat must find
        // the queued item, not a build.
        HoldRestartDispatcher.holdRunId = "run-restart-held";
        try {
            verify(restart(restartBody("run-restart-held", job.getFullName(), "src-held", 1,
                    "two", "ada"), new StringWriter())).setStatus(202);
            assertEquals("the first restart must be queued", 1, queuedCopies("run-restart-held"));

            // The repeat names a source build that does not exist. Only the queued-item
            // check answers it with 202; without that check the source lookup refuses it.
            verify(restart(restartBody("run-restart-held", job.getFullName(), "src-held", 99,
                    "two", "ada"), new StringWriter())).setStatus(202);
            assertEquals("a repeat of a queued restart must add no second item", 1,
                    queuedCopies("run-restart-held"));
            assertEquals("no restart build may run while it is held", 1, job.getBuilds().size());
        } finally {
            HoldRestartDispatcher.holdRunId = null;
        }
        j.waitUntilNoActivity();
        assertEquals("releasing the hold runs the one restart", 2, job.getBuilds().size());
    }

    private static int queuedCopies(String runId) {
        int copies = 0;
        for (Queue.Item item : Queue.getInstance().getItems()) {
            for (VarroaIntentAction action : item.getActions(VarroaIntentAction.class)) {
                if (runId.equals(action.getRunId())) {
                    copies++;
                }
            }
        }
        return copies;
    }

    /** Holds the queued restart whose run identifier matches {@code holdRunId}. */
    @TestExtension
    public static class HoldRestartDispatcher extends QueueTaskDispatcher {
        static volatile String holdRunId;

        @Override
        public CauseOfBlockage canRun(Queue.Item item) {
            String held = holdRunId;
            if (held == null) {
                return null;
            }
            for (VarroaIntentAction action : item.getActions(VarroaIntentAction.class)) {
                if (held.equals(action.getRunId())) {
                    return new CauseOfBlockage() {
                        @Override
                        public String getShortDescription() {
                            return "held by the restart test";
                        }
                    };
                }
            }
            return null;
        }
    }

    // ---- The harness ----

    private WorkflowJob declarativeJob() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "restart-job-" + ++jobCounter);
        job.setDefinition(new CpsFlowDefinition(DECLARATIVE, true));
        return job;
    }

    private WorkflowJob scriptedJob() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "restart-job-" + ++jobCounter);
        job.setDefinition(new CpsFlowDefinition("echo 'SCRIPTED'\n", true));
        return job;
    }

    /**
     * Runs a source build that carries the given run identifier, and waits
     * for it to end. The stamped intent action stands in for the claim gate.
     */
    private WorkflowRun completedSource(WorkflowJob job, String runId) throws Exception {
        VarroaIntentAction intent = new VarroaIntentAction(runId, "", null, System.currentTimeMillis());
        intent.setOrigin(IntentKey.ORIGIN_VARROA_INTENT);
        intent.setIntentKey("manual:" + runId);
        intent.setActor("ada");
        Queue.Item item = ParameterizedJobMixIn.scheduleBuild2(job, 0, intent);
        assertNotNull("the source build must schedule", item);
        WorkflowRun run = awaitBuild(job, job.getLastBuild() == null ? 1 : job.getLastBuild().getNumber() + 1);
        j.waitForCompletion(run);
        return run;
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

    /** Builds one restart body with the acting user named by {@code actor}. */
    private static String restartBody(String runId, String jobPath, String sourceRunId, long number,
            String stage, String actor) {
        JSONObject o = new JSONObject();
        o.put("runId", runId);
        o.put("intentKey", "restart:" + sourceRunId + ":" + runId);
        o.put("jobPath", jobPath);
        o.put("sourceRunId", sourceRunId);
        o.put("sourceBuildNumber", number);
        o.put("stage", stage);
        o.put("actingUser", JSONObject.fromObject("{\"name\":\"" + actor + "\",\"groups\":[]}"));
        return o.toString();
    }

    private static StaplerResponse2 restart(String json, StringWriter out) throws Exception {
        try (ACLContext ctx = asMite()) {
            StaplerResponse2 rsp = RefusalAssert.capturing(out);
            new VarroaRunTriggerAction().doRestart(requestWithBody(json), rsp);
            return rsp;
        }
    }

    private static StaplerResponse2 restartWithoutAuthority(String json) throws Exception {
        StaplerResponse2 rsp = RefusalAssert.capturing();
        new VarroaRunTriggerAction().doRestart(requestWithBody(json), rsp);
        return rsp;
    }

    private static StaplerRequest2 requestWithBody(String json) throws Exception {
        StaplerRequest2 req = mock(StaplerRequest2.class);
        when(req.getMethod()).thenReturn("POST");
        when(req.getReader()).thenReturn(new BufferedReader(new StringReader(json)));
        return req;
    }

    private static ACLContext asMite() {
        return ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))));
    }
}
