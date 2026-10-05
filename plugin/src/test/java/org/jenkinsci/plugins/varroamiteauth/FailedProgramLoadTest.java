package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import java.util.Arrays;
import java.util.List;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.After;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/** Tests for {@link FailedProgramLoad}'s claim set and its build match. */
public class FailedProgramLoadTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @After
    public void tearDown() {
        FailedProgramLoad.markBoot(System.currentTimeMillis());
        VarroaRunRecorder.setGateOverrideForTesting(null);
        BuildEventSink.get().resetForTesting();
    }

    @Test
    public void aClaimIsTakenOnceUnderAnyOfTheRunIds() {
        FailedProgramLoad.markBoot(System.currentTimeMillis());
        String entryId = Uuid7.next();
        String actionId = Uuid7.next();

        assertTrue(FailedProgramLoad.claim(Arrays.asList(entryId, actionId, actionId)));
        assertFalse("a claim under a shared id fails", FailedProgramLoad.claim(Arrays.asList(actionId, "")));
        assertFalse(FailedProgramLoad.claim(List.of(entryId)));
        assertTrue("an unrelated run claims on its own", FailedProgramLoad.claim(List.of(Uuid7.next())));
    }

    @Test
    public void aReleasedClaimCanBeTakenAgain() {
        FailedProgramLoad.markBoot(System.currentTimeMillis());
        List<String> ids = List.of(Uuid7.next());
        assertTrue(FailedProgramLoad.claim(ids));
        FailedProgramLoad.release(ids);
        assertTrue(FailedProgramLoad.claim(ids));
    }

    @Test
    public void aNewBootClearsTheClaims() {
        FailedProgramLoad.markBoot(System.currentTimeMillis());
        assertTrue(FailedProgramLoad.claim(List.of(Uuid7.next())));
        FailedProgramLoad.markBoot(System.currentTimeMillis());
        assertEquals(0, FailedProgramLoad.claimedCountForTesting());
    }

    @Test
    public void aScriptErrorWithTheLoadFailureMessageIsNotAFailedLoad() throws Exception {
        VarroaRunRecorder.setGateOverrideForTesting("1");
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "script-error");
        job.setDefinition(new CpsFlowDefinition("error '" + FailedProgramLoad.LOAD_FAILURE_MESSAGE + "'\n", true));
        VarroaIntentAction action = new VarroaIntentAction(Uuid7.next(), "", null, System.currentTimeMillis());
        WorkflowRun run = j.assertBuildStatus(hudson.model.Result.FAILURE, job.scheduleBuild2(0, action));

        assertFalse("a build of this boot never matches", FailedProgramLoad.matches(run));
        BuildEvent completed = null;
        for (BuildEvent e : BuildEventSink.get().drain(1000).events) {
            if (BuildEvent.PHASE_COMPLETED.equals(e.getPhase())) {
                completed = e;
            }
        }
        assertNotNull(completed);
        net.sf.json.JSONObject summary = completed.toJson().optJSONObject("log_summary");
        String reason = summary != null ? summary.optString("incomplete_reason") : "";
        assertNotEquals(LogSummary.REASON_JENKINS_RESTART, reason);
    }

    @Test
    public void aBuildOfThisBootIsNotARefusedResume() throws Exception {
        WorkflowRun run = startSleepingBuild("this-boot");
        try {
            assertFalse(FailedProgramLoad.refusedResume(run));
            assertFalse(FailedProgramLoad.endedByRestart(run));
        } finally {
            stop(run);
        }
    }

    @Test
    public void aResumedBuildIsNotARefusedResume() throws Exception {
        WorkflowRun run = startSleepingBuild("resumed");
        try {
            FailedProgramLoad.markBoot(System.currentTimeMillis() + 1);
            assertTrue("a build of an earlier boot with a live execution matches",
                    FailedProgramLoad.refusedResume(run));
            assertTrue(FailedProgramLoad.endedByRestart(run));

            FailedProgramLoad.markResumed(run);
            assertFalse("a resumed build does not match", FailedProgramLoad.refusedResume(run));
            assertFalse(FailedProgramLoad.endedByRestart(run));
        } finally {
            stop(run);
        }
    }

    @Test
    public void aNewBootClearsTheResumedMarks() throws Exception {
        WorkflowRun run = startSleepingBuild("new-boot");
        try {
            FailedProgramLoad.markResumed(run);
            assertEquals(1, FailedProgramLoad.resumedCountForTesting());
            FailedProgramLoad.markBoot(System.currentTimeMillis() + 1);
            assertEquals(0, FailedProgramLoad.resumedCountForTesting());
            assertTrue(FailedProgramLoad.refusedResume(run));
        } finally {
            stop(run);
        }
    }

    @Test
    public void aCompletedBuildIsNotARefusedResume() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "completed");
        job.setDefinition(new CpsFlowDefinition("echo 'done'\n", true));
        WorkflowRun run = j.buildAndAssertSuccess(job);
        FailedProgramLoad.markBoot(System.currentTimeMillis() + 1);
        assertFalse("a complete execution does not match", FailedProgramLoad.refusedResume(run));
    }

    private WorkflowRun startSleepingBuild(String name) throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, name);
        job.setDefinition(new CpsFlowDefinition("sleep 600\n", true));
        WorkflowRun run = job.scheduleBuild2(0).waitForStart();
        j.waitForMessage("Sleeping for", run);
        return run;
    }

    private void stop(WorkflowRun run) throws Exception {
        hudson.model.Executor executor = run.getExecutor();
        if (executor != null) {
            executor.interrupt();
        }
        j.waitForCompletion(run);
    }
}
