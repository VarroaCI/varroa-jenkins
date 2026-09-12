package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import hudson.model.Cause;
import hudson.model.CauseAction;
import hudson.model.FreeStyleProject;
import hudson.model.ParameterDefinition;
import hudson.model.ParametersAction;
import hudson.model.ParametersDefinitionProperty;
import hudson.model.PasswordParameterDefinition;
import hudson.model.PasswordParameterValue;
import hudson.model.StringParameterDefinition;
import hudson.model.StringParameterValue;

import java.util.ArrayList;
import java.util.List;
import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests for {@link VarroaRunRecorder}'s {@code Runs}, {@code Flows}, and
 * {@code Graph} extension points against a real Jenkins instance (5.9,
 * 5.9a, 5.10b, 5.12).
 */
public class VarroaRunRecorderTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Before
    public void resetRecorderState() {
        VarroaRunRecorder.resetStateForTesting();
        BuildEventSink.get().resetForTesting();
        VarroaRunRecorder.setGateOverrideForTesting("1");
    }

    @After
    public void clearGateOverride() {
        VarroaRunRecorder.setGateOverrideForTesting(null);
    }

    private static VarroaIntentAction grantedAction(String deliveryId) {
        VarroaIntentAction action = new VarroaIntentAction(Uuid7.next(), deliveryId, null, System.currentTimeMillis());
        return action;
    }

    private static List<BuildEvent> byPhase(List<BuildEvent> events, String phase) {
        List<BuildEvent> matched = new ArrayList<>();
        for (BuildEvent e : events) {
            if (phase.equals(e.getPhase())) {
                matched.add(e);
            }
        }
        return matched;
    }

    // --- 5.9: one STARTED and one COMPLETED for a freestyle job ---

    @Test
    public void freestyleRunEmitsOneStartedAndOneCompletedEvent() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        VarroaIntentAction action = grantedAction("");

        j.assertBuildStatusSuccess(project.scheduleBuild2(0, action));

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        List<BuildEvent> started = byPhase(result.events, BuildEvent.PHASE_STARTED);
        List<BuildEvent> completed = byPhase(result.events, BuildEvent.PHASE_COMPLETED);

        assertEquals(1, started.size());
        assertEquals("", started.get(0).getResult());
        assertEquals(1, completed.size());
        assertEquals("SUCCESS", completed.get(0).getResult());
        assertEquals(started.get(0).getRunId(), completed.get(0).getRunId());
        assertEquals(started.get(0).getIntentKey(), completed.get(0).getIntentKey());
    }

    @Test
    public void aRunWithNoGrantedClaimEmitsNoEvent() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();

        j.buildAndAssertSuccess(project);

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        assertTrue(result.events.isEmpty());
    }

    @Test
    public void theGateOffEmitsNoEvent() throws Exception {
        VarroaRunRecorder.setGateOverrideForTesting("0");
        FreeStyleProject project = j.createFreeStyleProject();
        VarroaIntentAction action = grantedAction("");

        j.assertBuildStatusSuccess(project.scheduleBuild2(0, action));

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        assertTrue(result.events.isEmpty());
    }

    // --- 5.9a: D3 metadata, masking, and the started revision from the action ---

    @Test
    public void theStartedRevisionComesFromTheIntentActionAndSensitiveParametersAreMasked() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        project.addProperty(new ParametersDefinitionProperty(
                new StringParameterDefinition("PLAIN", "default"),
                (ParameterDefinition) new PasswordParameterDefinition("TOKEN", "unused", "")));

        VarroaIntentAction action = new VarroaIntentAction(Uuid7.next(), "github:delivery-1", null, System.currentTimeMillis());
        action.setRevision("abc123");

        ParametersAction params = new ParametersAction(
                new StringParameterValue("PLAIN", "hello"),
                new PasswordParameterValue("TOKEN", "s3cr3t"));
        CauseAction causeAction = new CauseAction(new Cause.UserIdCause());

        j.assertBuildStatusSuccess(project.scheduleBuild2(0, action, params, causeAction));

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        BuildEvent started = byPhase(result.events, BuildEvent.PHASE_STARTED).get(0);
        BuildEvent completed = byPhase(result.events, BuildEvent.PHASE_COMPLETED).get(0);

        assertEquals("abc123", started.getScmRevision());
        assertEquals("github:delivery-1", started.getDeliveryId());
        assertEquals(project.getFullName(), started.getJobPath());
        assertEquals(1L, started.getBuildNumber());
        assertTrue(started.getUrl().length() > 0);

        assertEquals("hello", completed.getParameters().get("PLAIN"));
        assertFalse("a sensitive value never leaves the cell",
                completed.getParameters().containsValue("s3cr3t"));
        assertTrue(completed.getMaskedParameterNames().contains("TOKEN"));
        assertTrue(completed.getCauseTypes().contains("UserIdCause"));
        assertTrue(completed.getQueueWaitMillis() >= 0);
        assertTrue(completed.getDurationMillis() >= 0);
    }

    // --- 5.10b: the completion rebuild carries every stage and branch ---

    @Test
    public void aPipelineWithAParallelBlockReportsEveryStageAndBranchWithTheRightParentId() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "pipe");
        job.setDefinition(new CpsFlowDefinition(
                "node {\n"
                        + "  stage('build') {\n"
                        + "    parallel(\n"
                        + "      unit: { echo 'unit' },\n"
                        + "      integration: { echo 'integration' }\n"
                        + "    )\n"
                        + "  }\n"
                        + "}\n",
                true));
        VarroaIntentAction action = grantedAction("");

        WorkflowRun run = j.assertBuildStatusSuccess(job.scheduleBuild2(0, action));

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(200);
        List<BuildEvent> completed = byPhase(result.events, BuildEvent.PHASE_COMPLETED);
        assertEquals(1, completed.size());
        List<BuildStage> stages = completed.get(0).getStages();

        BuildStage buildStage = findStageByName(stages, "build");
        assertNotNull("the 'build' stage is in the completion rebuild", buildStage);
        assertEquals("stage", buildStage.getNodeType());
        assertEquals("SUCCESS", buildStage.getStatus());

        BuildStage unit = findStageByBranch(stages, "unit");
        BuildStage integration = findStageByBranch(stages, "integration");
        assertNotNull(unit);
        assertNotNull(integration);
        assertEquals("parallel", unit.getNodeType());
        assertEquals("parallel", integration.getNodeType());
        assertEquals("the branch's parent_id names the recorded stage, not the structural parallel block",
                buildStage.getStageId(), unit.getParentId());
        assertEquals(buildStage.getStageId(), integration.getParentId());

        // At least one STAGE_STARTED event described the running topology
        // before the terminal STAGE/COMPLETED snapshot arrived.
        List<BuildEvent> stageStarted = byPhase(result.events, BuildEvent.PHASE_STAGE_STARTED);
        assertFalse(stageStarted.isEmpty());
        assertTrue(stageStarted.stream().anyMatch(e ->
                !e.getStages().isEmpty() && "IN_PROGRESS".equals(e.getStages().get(0).getStatus())));
    }

    // --- 5.10a: a failing stage's error is truncated to 2048 characters ---

    @Test
    public void aFailingStageReportsFailureAndATruncatedError() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "pipe-fail");
        StringBuilder longMessage = new StringBuilder();
        for (int i = 0; i < 4000; i++) {
            longMessage.append('x');
        }
        job.setDefinition(new CpsFlowDefinition(
                "node {\n"
                        + "  stage('build') {\n"
                        + "    error('" + longMessage + "')\n"
                        + "  }\n"
                        + "}\n",
                true));
        VarroaIntentAction action = grantedAction("");

        WorkflowRun run = job.scheduleBuild2(0, action).get();
        j.assertBuildStatus(hudson.model.Result.FAILURE, run);

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(200);
        List<BuildEvent> completed = byPhase(result.events, BuildEvent.PHASE_COMPLETED);
        assertEquals(1, completed.size());
        assertEquals("FAILURE", completed.get(0).getResult());

        BuildStage buildStage = findStageByName(completed.get(0).getStages(), "build");
        assertNotNull(buildStage);
        assertEquals("FAILURE", buildStage.getStatus());
        assertEquals(2048, buildStage.getError().length());
    }

    // --- 5.10b: an UNSTABLE stage ---

    @Test
    public void anUnstableStageReportsUnstable() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "pipe-unstable");
        job.setDefinition(new CpsFlowDefinition(
                "node {\n"
                        + "  stage('build') {\n"
                        + "    unstable('flaky')\n"
                        + "  }\n"
                        + "}\n",
                true));
        VarroaIntentAction action = grantedAction("");

        WorkflowRun run = job.scheduleBuild2(0, action).get();
        j.assertBuildStatus(hudson.model.Result.UNSTABLE, run);

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(200);
        List<BuildEvent> completed = byPhase(result.events, BuildEvent.PHASE_COMPLETED);
        assertEquals(1, completed.size());
        assertEquals("UNSTABLE", completed.get(0).getResult());

        BuildStage buildStage = findStageByName(completed.get(0).getStages(), "build");
        assertNotNull(buildStage);
        assertEquals("UNSTABLE", buildStage.getStatus());
    }

    // --- 5.9a: every D3 field, not a subset ---

    @Test
    public void everyD3FieldOfTheCompletedEventMatchesItsSource() throws Exception {
        FreeStyleProject upstream = j.createFreeStyleProject("d3-upstream");
        hudson.model.FreeStyleBuild upstreamRun = j.buildAndAssertSuccess(upstream);

        FreeStyleProject project = j.createFreeStyleProject("d3-downstream");
        project.setQuietPeriod(0);
        project.addProperty(new ParametersDefinitionProperty(
                new StringParameterDefinition("PLAIN", "default"),
                (ParameterDefinition) new PasswordParameterDefinition("TOKEN", "unused", "")));

        VarroaIntentAction action = new VarroaIntentAction(Uuid7.next(), "github:delivery-2", null, System.currentTimeMillis());
        action.setRevision("d3-revision");
        ParametersAction params = new ParametersAction(
                new StringParameterValue("PLAIN", "hello"),
                new PasswordParameterValue("TOKEN", "s3cr3t"));
        CauseAction causeAction = new CauseAction(new Cause.UpstreamCause(upstreamRun));

        long before = System.currentTimeMillis();
        j.assertBuildStatusSuccess(project.scheduleBuild2(0, action, params, causeAction));

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        BuildEvent started = byPhase(result.events, BuildEvent.PHASE_STARTED).get(0);
        BuildEvent completed = byPhase(result.events, BuildEvent.PHASE_COMPLETED).get(0);

        // Identity, carried from the action through ClaimRegistry.
        assertEquals(action.getRunId(), completed.getRunId());
        assertEquals(action.getIntentKey(), completed.getIntentKey());
        assertEquals("github:delivery-2", completed.getDeliveryId());
        assertEquals("d3-revision", started.getScmRevision());

        // Core run identity.
        assertEquals(project.getFullName(), completed.getJobPath());
        assertEquals(1L, completed.getBuildNumber());
        assertTrue(completed.getUrl().contains(project.getFullName()));
        assertEquals("SUCCESS", completed.getResult());
        assertTrue(completed.getCauseTypes().contains("UpstreamCause"));
        assertFalse(completed.getCauseDescription().isEmpty());
        assertEquals(upstream.getFullName(), completed.getUpstreamJobPath());
        assertEquals(1L, completed.getUpstreamBuildNumber());
        assertEquals("upstream:" + upstream.getFullName(), completed.getActor());

        // Timing: queued at or after the schedule call, started no
        // earlier than queued, and the queue wait derived from the two.
        assertTrue(completed.getQueuedAtMillis() >= before);
        assertTrue(completed.getStartedAtMillis() >= completed.getQueuedAtMillis());
        assertEquals(completed.getStartedAtMillis() - completed.getQueuedAtMillis(), completed.getQueueWaitMillis());
        assertTrue(completed.getDurationMillis() >= 0);
        assertTrue(completed.getEstimatedDurationMillis() >= 0);

        // Parameters and masking.
        assertEquals("hello", completed.getParameters().get("PLAIN"));
        assertFalse(completed.getParameters().containsValue("s3cr3t"));
        assertEquals(List.of("TOKEN"), completed.getMaskedParameterNames());

        // No SCM is configured: the change-set-derived fields stay empty,
        // not merely absent from the assertion set.
        assertTrue(completed.getChanges().isEmpty());
        assertEquals(0, completed.getChangeCount());
        assertEquals("", completed.getScmUrl());
        assertEquals("", completed.getScmHead());

        // The junit plugin is absent: every test counter reads zero.
        assertEquals(0, completed.getTestTotal());
        assertEquals(0, completed.getTestPassed());
        assertEquals(0, completed.getTestFailed());
        assertEquals(0, completed.getTestSkipped());
    }

    private static BuildStage findStageByName(List<BuildStage> stages, String name) {
        for (BuildStage s : stages) {
            if (name.equals(s.getName())) {
                return s;
            }
        }
        return null;
    }

    private static BuildStage findStageByBranch(List<BuildStage> stages, String branchName) {
        for (BuildStage s : stages) {
            if (branchName.equals(s.getParallelBranchName())) {
                return s;
            }
        }
        return null;
    }
}
