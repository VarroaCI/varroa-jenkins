package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import hudson.model.Cause;
import hudson.model.CauseAction;
import hudson.model.Executor;
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
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.function.BooleanSupplier;

import net.sf.json.JSONObject;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.flow.FlowExecutionListener;
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

    /** The gate value the run-state pusher accepts. */
    private static final String LOOPBACK_URL = "http://127.0.0.1:9095";

    private static void waitUntil(BooleanSupplier condition, long timeoutMillis) throws InterruptedException {
        long deadline = System.currentTimeMillis() + timeoutMillis;
        while (!condition.getAsBoolean() && System.currentTimeMillis() < deadline) {
            Thread.sleep(50);
        }
        assertTrue("condition not met within " + timeoutMillis + "ms", condition.getAsBoolean());
    }

    /** A {@link RunStatePusher.RunStateSender} that records each path it is given. */
    private static final class RecordingSender implements RunStatePusher.RunStateSender {

        private final List<String> paths = new CopyOnWriteArrayList<>();

        boolean holds(String suffix) {
            for (String path : paths) {
                if (path.endsWith(suffix)) {
                    return true;
                }
            }
            return false;
        }

        @Override
        public RunStatePusher.RunStateSender.Answer post(String url, JSONObject record) {
            paths.add(record.getString("relpath"));
            return RunStatePusher.RunStateSender.Answer.accepted();
        }
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

    // --- Live finding L5: a build another process began ---

    /**
     * Proves a build this process resumes reports itself again. The
     * pipeline mesh moves a running build to another Jenkins process,
     * where {@code RunListener.onStarted} never fires, so the recorder
     * has no entry for the run and the recorder sees nothing of it.
     *
     * <p>The resume is simulated the way Jenkins performs it: the per-run
     * state is cleared, as a fresh process holds none, and the flow
     * execution's own resumed event is fired.
     */
    @Test
    public void aResumedBuildHeartbeatsAndCompletesUnderItsOwnRunId() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "resumed");
        job.setDefinition(new CpsFlowDefinition("stage('one') { echo 'one' }\nsleep 5\n", true));
        VarroaIntentAction action = grantedAction("");
        WorkflowRun run = job.scheduleBuild2(0, action).waitForStart();
        j.waitForMessage("Sleeping", run);
        String runId = action.getRunId();

        // The process that began the build is gone. Its per-run state
        // went with it, and its heartbeat with that.
        VarroaRunRecorder.resetStateForTesting();
        BuildEventSink.get().resetForTesting();
        assertEquals(0, VarroaRunRecorder.stateSizeForTesting());

        FlowExecutionListener.fireResumed(run.getExecution());

        assertEquals("the resumed build owns one state entry", 1, VarroaRunRecorder.stateSizeForTesting());
        assertFalse("a build this process already holds is not attached again",
                VarroaRunRecorder.attachResumedRun(run));
        BuildEventSink.DrainResult afterResume = BuildEventSink.get().drain(100);
        List<BuildEvent> beats = byPhase(afterResume.events, BuildEvent.PHASE_HEARTBEAT);
        assertEquals("the resume heartbeats at once, not one interval later", 1, beats.size());
        assertEquals(runId, beats.get(0).getRunId());
        assertTrue("a resume starts no second run", byPhase(afterResume.events, BuildEvent.PHASE_STARTED).isEmpty());

        j.assertBuildStatusSuccess(j.waitForCompletion(run));

        BuildEventSink.DrainResult atEnd = BuildEventSink.get().drain(100);
        List<BuildEvent> completed = byPhase(atEnd.events, BuildEvent.PHASE_COMPLETED);
        assertEquals(1, completed.size());
        assertEquals("the completion carries the run the recorder resumed", runId, completed.get(0).getRunId());
    }

    /**
     * Proves a build this process resumed replicates its own state. The
     * node listener arms a push window for each flow node, and a build
     * that resumes inside one long step fires none. Without a schedule
     * at the attach, the new process pushes nothing for the whole step,
     * and a second loss resumes from the dead process's copy.
     */
    @Test
    public void aResumedBuildReplicatesItsOwnStateWithNoFlowNode() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "resumed-push");
        job.setDefinition(new CpsFlowDefinition("sleep 120\n", true));
        VarroaIntentAction action = grantedAction("");
        WorkflowRun run = job.scheduleBuild2(0, action).waitForStart();
        j.waitForMessage("Sleeping", run);
        String runId = action.getRunId();

        // The process that began the build is gone, and this one holds
        // neither its state nor a pusher for it.
        VarroaRunRecorder.resetStateForTesting();
        RunStatePusher.resetForTesting();
        RunStatePusher.setActivationUrlForTesting(LOOPBACK_URL);
        RecordingSender sender = new RecordingSender();
        RunStatePusher pusher = new RunStatePusher(runId, run.getParent().getFullName(), run.getNumber(),
                run.getRootDir().toPath(), LOOPBACK_URL, sender, new RunSpool());
        RunStatePusher.putForTesting(runId, pusher);
        try {
            FlowExecutionListener.fireResumed(run.getExecution());

            waitUntil(() -> sender.holds(RunStatePusher.BUILD_XML), 20_000);
            assertTrue("the resumed process replicates the flow program too",
                    sender.holds(RunStatePusher.PROGRAM_DAT));
        } finally {
            RunStatePusher.remove(runId);
            RunStatePusher.setActivationUrlForTesting(null);
            Executor executor = run.getExecutor();
            if (executor != null) {
                executor.interrupt();
            } else {
                run.doStop();
            }
            j.waitForCompletion(run);
        }
    }

    /**
     * Proves a build that ended is never attached. Its completion went
     * out from whichever process ran it, and an entry here would
     * heartbeat a finished run for the life of this process.
     */
    @Test
    public void aFinishedBuildIsNotAttached() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "attach-finished");
        job.setDefinition(new CpsFlowDefinition("echo 'once'\n", true));
        VarroaIntentAction action = grantedAction("");
        WorkflowRun run = j.assertBuildStatusSuccess(job.scheduleBuild2(0, action));

        assertFalse("a build that ended is not attached",
                VarroaRunRecorder.attachResumedRun(run));
        assertEquals("a finished build owns no state entry", 0, VarroaRunRecorder.stateSizeForTesting());
    }

    // --- Queue wait: measured from the queue entry, quiet period included ---

    @Test
    public void theQueuedTimeIsWhenTheItemEnteredTheQueue() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("queued-at-entry");
        long before = System.currentTimeMillis();
        j.assertBuildStatusSuccess(project.scheduleBuild2(3, grantedAction("")));

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        BuildEvent started = byPhase(result.events, BuildEvent.PHASE_STARTED).get(0);

        assertTrue("queued_at is not before the request", started.getQueuedAtMillis() >= before);
        assertTrue("the 3 s quiet period is queue wait, was " + started.getQueueWaitMillis(),
                started.getQueueWaitMillis() >= 2500);
        assertEquals(started.getStartedAtMillis() - started.getQueuedAtMillis(), started.getQueueWaitMillis());
    }

    @Test
    public void theAdmissionTimeIsTheFallbackWhenNoQueueItemResolves() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("queued-at-fallback");
        hudson.model.FreeStyleBuild run = new hudson.model.FreeStyleBuild(project);
        long now = System.currentTimeMillis();
        long admittedAt = now - 10_000;
        run.addAction(new VarroaIntentAction(Uuid7.next(), "", null, admittedAt));

        assertEquals("an unresolved queue id falls back to the admission time",
                admittedAt, VarroaRunRecorder.queuedAtMillis(run, now));
    }

    @Test
    public void theRunCreationTimeIsTheLastFallback() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("queued-at-last");
        hudson.model.FreeStyleBuild run = new hudson.model.FreeStyleBuild(project);
        long now = System.currentTimeMillis();

        assertEquals("with no queue item and no intent, the creation time answers",
                run.getTimeInMillis(), VarroaRunRecorder.queuedAtMillis(run, now));
        assertEquals("the queued time never follows the start",
                Math.min(run.getTimeInMillis(), now - 5_000),
                VarroaRunRecorder.queuedAtMillis(run, now - 5_000));
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

    // --- The replay source on STARTED ---

    @Test
    public void aReplayedBuildNamesItsSourceOnStartedAlone() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "replayed");
        job.setDefinition(new CpsFlowDefinition("stage('one') { echo 'one' }\n", true));

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction(""),
                new CauseAction(new VarroaReplayCause("src-run"))));

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        List<BuildEvent> started = byPhase(result.events, BuildEvent.PHASE_STARTED);
        assertEquals(1, started.size());
        assertEquals("src-run", started.get(0).getReplayOf());
        assertEquals("src-run", started.get(0).toJson().getString("replay_of"));

        List<String> otherPhases = new ArrayList<>();
        for (BuildEvent e : result.events) {
            if (BuildEvent.PHASE_STARTED.equals(e.getPhase())) {
                continue;
            }
            otherPhases.add(e.getPhase());
            assertEquals("phase " + e.getPhase() + " must carry no source", "", e.getReplayOf());
            assertFalse("phase " + e.getPhase() + " must write no replay_of key",
                    e.toJson().containsKey("replay_of"));
        }
        assertTrue("the build must emit a COMPLETED event, phases=" + otherPhases,
                otherPhases.contains(BuildEvent.PHASE_COMPLETED));
    }

    @Test
    public void aBuildWithNoReplayCauseCarriesAnEmptySource() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();

        j.assertBuildStatusSuccess(project.scheduleBuild2(0, grantedAction(""),
                new CauseAction(new VarroaUserCause("nate"))));

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        BuildEvent started = byPhase(result.events, BuildEvent.PHASE_STARTED).get(0);
        assertEquals("", started.getReplayOf());
        assertFalse(started.toJson().containsKey("replay_of"));
    }

    // --- The restart source on STARTED, and the restart answer on COMPLETED ---

    @Test
    public void aRestartedBuildNamesItsSourceAndStageOnStartedAlone() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "restarted");
        job.setDefinition(new CpsFlowDefinition("stage('one') { echo 'one' }\n", true));

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction(""),
                new CauseAction(new VarroaRestartCause("src-run", "two"))));

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        List<BuildEvent> started = byPhase(result.events, BuildEvent.PHASE_STARTED);
        assertEquals(1, started.size());
        assertEquals("src-run", started.get(0).getRestartOf());
        assertEquals("two", started.get(0).getRestartStage());
        assertEquals("src-run", started.get(0).toJson().getString("restart_of"));
        assertEquals("two", started.get(0).toJson().getString("restart_stage"));

        for (BuildEvent e : result.events) {
            if (BuildEvent.PHASE_STARTED.equals(e.getPhase())) {
                continue;
            }
            assertFalse("phase " + e.getPhase() + " must write no restart_of key",
                    e.toJson().containsKey("restart_of"));
            assertFalse("phase " + e.getPhase() + " must write no restart_stage key",
                    e.toJson().containsKey("restart_stage"));
        }
    }

    @Test
    public void aDeclarativeRunListsItsRestartableStagesOnCompleted() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "declared-restart");
        job.setDefinition(new CpsFlowDefinition("pipeline {\n  agent any\n  stages {\n"
                + "    stage('one') { steps { echo 'one' } }\n"
                + "    stage('two') { steps { echo 'two' } }\n  }\n}\n", true));

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction("")));

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        BuildEvent completed = byPhase(result.events, BuildEvent.PHASE_COMPLETED).get(0);
        assertEquals(List.of("one", "two"), completed.getRestartStages());
        assertEquals("", completed.getRestartRefusal());
        assertEquals(List.of("one", "two"), completed.toJson().getJSONArray("restart_stages"));
        assertFalse(completed.toJson().containsKey("restart_refusal"));
    }

    @Test
    public void aScriptedRunCompletesWithANotDeclarativeRefusal() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "scripted-restart");
        job.setDefinition(new CpsFlowDefinition("echo 'scripted'\n", true));

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction("")));

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        BuildEvent completed = byPhase(result.events, BuildEvent.PHASE_COMPLETED).get(0);
        assertEquals("not-declarative", completed.getRestartRefusal());
        assertEquals("not-declarative", completed.toJson().getString("restart_refusal"));
        assertFalse(completed.toJson().containsKey("restart_stages"));
    }

    // --- The granted number on STARTED ---

    @Test
    public void startedCarriesTheGrantTheRunApplied() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();
        VarroaIntentAction action = grantedAction("");
        action.setGrantedNumber(1);

        j.assertBuildStatusSuccess(project.scheduleBuild2(0, action));

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        BuildEvent started = byPhase(result.events, BuildEvent.PHASE_STARTED).get(0);
        assertEquals(1L, started.getGrantedBuildNumber());
        assertEquals(1L, started.toJson().getLong("granted_build_number"));
        for (BuildEvent e : byPhase(result.events, BuildEvent.PHASE_COMPLETED)) {
            assertFalse(e.toJson().containsKey("granted_build_number"));
        }
    }

    @Test
    public void aRunWithNoGrantWritesNoGrantedNumber() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject();

        j.assertBuildStatusSuccess(project.scheduleBuild2(0, grantedAction("")));

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        BuildEvent started = byPhase(result.events, BuildEvent.PHASE_STARTED).get(0);
        assertFalse(started.toJson().containsKey("granted_build_number"));
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

        // The run reports no test result: every test counter reads zero.
        assertEquals(0, completed.getTestTotal());
        assertEquals(0, completed.getTestPassed());
        assertEquals(0, completed.getTestFailed());
        assertEquals(0, completed.getTestSkipped());
    }

    // --- The test case caps ---

    private static BuildTestCase testCase(String name, int textBytes) {
        String text = "x".repeat(textBytes);
        return BuildTestCase.builder()
                .suite(text)
                .className(text)
                .name(name)
                .status("FAILED")
                .errorDetails(text)
                .build();
    }

    private static int serializedBytes(BuildEvent event) {
        return event.toJson().toString().getBytes(java.nio.charset.StandardCharsets.UTF_8).length;
    }

    @Test
    public void theHardCapKeepsTheFirst500TestCases() {
        List<BuildTestCase> cases = new ArrayList<>();
        for (int i = 0; i < 900; i++) {
            cases.add(testCase("case-" + i, 1));
        }
        BuildEvent event = BuildEvent.builder().phase(BuildEvent.PHASE_COMPLETED).testCases(cases).build();

        BuildEvent capped = VarroaRunRecorder.applyCaps(event);

        assertEquals(500, capped.getTestCases().size());
        assertEquals("the cap keeps the head", "case-0", capped.getTestCases().get(0).getName());
        assertEquals("case-499", capped.getTestCases().get(499).getName());
    }

    @Test
    public void theLadderDropsTheLastTestCaseBeforeAnyStage() {
        List<BuildTestCase> cases = new ArrayList<>();
        for (int i = 0; i < 500; i++) {
            cases.add(testCase("case-" + i, 512));
        }
        List<BuildStage> stages = new ArrayList<>();
        for (int i = 0; i < 10; i++) {
            stages.add(BuildStage.builder().stageId("s" + i).name("stage-" + i).status("SUCCESS").build());
        }
        BuildTestReport report = BuildTestReport.builder().blobKey("k").sizeBytes(10).caseCount(900).build();
        BuildEvent event = BuildEvent.builder()
                .phase(BuildEvent.PHASE_COMPLETED)
                .runId("run-1")
                .stages(stages)
                .testCases(cases)
                .testReport(report)
                .build();
        assertTrue("fixture must start oversize", serializedBytes(event) > 512 * 1024);

        BuildEvent shrunk = VarroaRunRecorder.applyCaps(event);

        assertTrue("the shrunk event fits the size limit", serializedBytes(shrunk) <= 512 * 1024);
        assertEquals("no stage is dropped while dropping cases fits", 10, shrunk.getStages().size());
        int kept = shrunk.getTestCases().size();
        assertTrue("some cases were dropped, kept=" + kept, kept < 500 && kept > 0);
        assertEquals("the rung keeps the head", "case-0", shrunk.getTestCases().get(0).getName());
        assertEquals("the rung drops from the end", "case-" + (kept - 1), shrunk.getTestCases().get(kept - 1).getName());
        assertNotNull("no rung drops the report pointer", shrunk.getTestReport());
        assertEquals(900, shrunk.getTestReport().getCaseCount());
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
