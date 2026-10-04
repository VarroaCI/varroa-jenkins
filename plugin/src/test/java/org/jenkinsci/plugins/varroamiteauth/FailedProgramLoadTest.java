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
}
