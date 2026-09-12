package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertTrue;

import java.util.ArrayList;
import java.util.List;
import java.util.Set;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests for the four {@code BuildEvent} template fields and the held
 * {@code STARTED} event (tasks 5.14, 5.14a, 5.14b, 5.14c; design D8a).
 */
public class VarroaRunRecorderTemplateEventsTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Before
    public void setUp() {
        VarroaRunRecorder.resetStateForTesting();
        BuildEventSink.get().resetForTesting();
        VarroaRunRecorder.setGateOverrideForTesting("1");
        VarroaRunRecorder.setTemplateDefinitionClassNamesForTesting(
                Set.of(FakeVarroaFlowDefinition.class.getName()));
    }

    @After
    public void tearDown() {
        VarroaRunRecorder.setGateOverrideForTesting(null);
        VarroaRunRecorder.setTemplateDefinitionClassNamesForTesting(null);
    }

    private static VarroaIntentAction grantedAction() {
        return new VarroaIntentAction(Uuid7.next(), "", null, System.currentTimeMillis());
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

    @Test
    public void aMarkerDefinedRunsStartedEventCarriesAllFourTemplateFields() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "template-run");
        job.setDefinition(new FakeVarroaFlowDefinition("echo 'hi'", false, false));
        VarroaIntentAction action = grantedAction();

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, action));

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(200);
        List<BuildEvent> started = byPhase(result.events, BuildEvent.PHASE_STARTED);
        assertEquals(1, started.size());
        assertEquals("go-service", started.get(0).getTemplateName());
        assertEquals("2.1.4", started.get(0).getTemplateVersion());
        assertEquals(".varroa/pipeline.yaml", started.get(0).getMarkerPath());
        assertEquals("abc123", started.get(0).getMarkerRevision());

        List<BuildEvent> completed = byPhase(result.events, BuildEvent.PHASE_COMPLETED);
        assertEquals(1, completed.size());
        assertEquals("go-service", completed.get(0).getTemplateName());
        assertEquals("2.1.4", completed.get(0).getTemplateVersion());
    }

    @Test
    public void aHeartbeatCarriesNoneOfTheFourTemplateFields() {
        VarroaRunRecorder.registerRunForTesting("some/job", 1L, "run-1", "intent-1");

        VarroaRunRecorder.heartbeatTick();

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        List<BuildEvent> heartbeats = byPhase(result.events, BuildEvent.PHASE_HEARTBEAT);
        assertEquals(1, heartbeats.size());
        assertEquals("", heartbeats.get(0).getTemplateName());
        assertEquals("", heartbeats.get(0).getTemplateVersion());
        assertEquals("", heartbeats.get(0).getMarkerPath());
        assertEquals("", heartbeats.get(0).getMarkerRevision());
    }

    @Test
    public void aJenkinsfileRunsTimingIsUnchanged() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "plain-pipeline");
        job.setDefinition(new CpsFlowDefinition("echo 'hi'", true));
        VarroaIntentAction action = grantedAction();

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, action));

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(200);
        List<BuildEvent> started = byPhase(result.events, BuildEvent.PHASE_STARTED);
        assertEquals("a plain Jenkinsfile run emits STARTED at onStarted time, same as before this change",
                1, started.size());
        assertEquals("", started.get(0).getTemplateName());
        assertEquals("", started.get(0).getTemplateVersion());
        assertEquals("", started.get(0).getMarkerPath());
        assertEquals("", started.get(0).getMarkerRevision());
    }

    @Test
    public void anInvalidMarkerStillEmitsItsStartedEventBeforeCompleted() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "rejected-marker");
        job.setDefinition(new FakeVarroaFlowDefinition("echo 'unreached'", true, false));
        VarroaIntentAction action = grantedAction();

        WorkflowRun run = job.scheduleBuild2(0, action).get();
        j.assertBuildStatus(hudson.model.Result.FAILURE, run);

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(200);
        List<BuildEvent> started = byPhase(result.events, BuildEvent.PHASE_STARTED);
        List<BuildEvent> completed = byPhase(result.events, BuildEvent.PHASE_COMPLETED);
        assertEquals("no run completes with an unsent STARTED event", 1, started.size());
        assertEquals("go-service", started.get(0).getTemplateName());
        assertEquals(1, completed.size());
        assertTrue("STARTED must precede COMPLETED", started.get(0).getSeq() < completed.get(0).getSeq());
    }

    @Test
    public void aRunThatCompletesWithNeitherActionStillEmitsItsStartedEvent() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "no-action-attached");
        job.setDefinition(new FakeVarroaFlowDefinition("echo 'hi'", false, true));
        VarroaIntentAction action = grantedAction();

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, action));

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(200);
        List<BuildEvent> started = byPhase(result.events, BuildEvent.PHASE_STARTED);
        assertEquals(1, started.size());
        assertEquals("", started.get(0).getTemplateName());
        assertEquals("", started.get(0).getMarkerPath());
        List<BuildEvent> completed = byPhase(result.events, BuildEvent.PHASE_COMPLETED);
        assertEquals(1, completed.size());
    }
}
