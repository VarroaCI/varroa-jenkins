package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertTrue;
import static org.mockito.Mockito.doReturn;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

import hudson.model.Run;

import java.util.ArrayList;
import java.util.List;

import org.jenkinsci.plugins.workflow.support.steps.input.InputAction;
import org.jenkinsci.plugins.workflow.support.steps.input.InputStep;
import org.jenkinsci.plugins.workflow.support.steps.input.InputStepExecution;
import org.junit.After;
import org.junit.Before;
import org.junit.Test;

/**
 * Tests for the heartbeat carrier of a run's pending input set: the set
 * reaches every tick from the cache alone, including an empty one, and the
 * event passes the size caps.
 */
public class VarroaRunInputsHeartbeatTest {

    @Before
    public void setUp() {
        VarroaRunRecorder.resetStateForTesting();
        BuildEventSink.get().resetForTesting();
        VarroaRunRecorder.setGateOverrideForTesting("1");
    }

    @After
    public void tearDown() {
        VarroaRunRecorder.setGateOverrideForTesting(null);
        VarroaRunRecorder.resetStateForTesting();
        BuildEventSink.get().resetForTesting();
    }

    private static InputStep step(String message) {
        InputStep step = mock(InputStep.class);
        when(step.getMessage()).thenReturn(message);
        when(step.getOk()).thenReturn("Ship it");
        when(step.getSubmitter()).thenReturn("");
        when(step.getSubmitterParameter()).thenReturn("");
        when(step.getParameters()).thenReturn(List.of());
        return step;
    }

    /**
     * Builds one execution mock. The step mock is built first and held in a
     * local, because a stubbing call inside another stubbing call leaves the
     * outer one unfinished.
     */
    private static InputStepExecution execution(String id) {
        InputStep step = step("Deploy?");
        InputStepExecution execution = mock(InputStepExecution.class);
        when(execution.getId()).thenReturn(id);
        when(execution.getInput()).thenReturn(step);
        when(execution.isSettled()).thenReturn(false);
        return execution;
    }

    /**
     * Registers one run, attaches one real input action to a mock run, and
     * fills the run's cache with one read. The action list goes in with
     * {@code doReturn}, because {@code Run.getAllActions()} answers a
     * wildcard list.
     */
    private static void runWithPendingInputs(int count) throws Exception {
        Run<?, ?> run = mock(Run.class);
        when(run.getNumber()).thenReturn(1);
        when(run.isBuilding()).thenReturn(true);
        InputAction action = new InputAction();
        action.onAttached(run);
        for (int i = 0; i < count; i++) {
            action.add(execution("in-" + i));
        }
        doReturn(List.of(action)).when(run).getAllActions();
        VarroaRunRecorder.registerRunForTesting("", 1L, "run-a", "intent-a");
        VarroaRunRecorder.recheckInputs(run);
    }

    private static List<BuildEvent> events(String phase) {
        List<BuildEvent> found = new ArrayList<>();
        for (BuildEvent event : BuildEventSink.get().drain(200).events) {
            if (phase.equals(event.getPhase())) {
                found.add(event);
            }
        }
        return found;
    }

    @Test
    public void aHeartbeatAfterAPendingReadCarriesTheSet() throws Exception {
        runWithPendingInputs(1);
        assertEquals(1, events(BuildEvent.PHASE_RUN_INPUT).size());

        VarroaRunRecorder.heartbeatTick();

        List<BuildEvent> heartbeats = events(BuildEvent.PHASE_HEARTBEAT);
        assertEquals(1, heartbeats.size());
        assertEquals(1, heartbeats.get(0).getInputs().size());
        assertEquals("in-0", heartbeats.get(0).getInputs().get(0).getId());
        assertEquals("Deploy?", heartbeats.get(0).getInputs().get(0).getMessage());
    }

    @Test
    public void aHeartbeatWithAnEmptyCacheCarriesAnEmptySet() throws Exception {
        VarroaRunRecorder.registerRunForTesting("", 1L, "run-a", "intent-a");

        VarroaRunRecorder.heartbeatTick();

        List<BuildEvent> heartbeats = events(BuildEvent.PHASE_HEARTBEAT);
        assertEquals(1, heartbeats.size());
        assertNotNull("the event carries a set and not an absent field", heartbeats.get(0).getInputs());
        assertTrue(heartbeats.get(0).getInputs().isEmpty());
    }

    @Test
    public void aHeartbeatPassesTheSizeCaps() throws Exception {
        runWithPendingInputs(25);
        assertEquals("the read caps the set", 20, events(BuildEvent.PHASE_RUN_INPUT).get(0).getInputs().size());

        VarroaRunRecorder.heartbeatTick();

        List<BuildEvent> heartbeats = events(BuildEvent.PHASE_HEARTBEAT);
        assertEquals(1, heartbeats.size());
        BuildEvent heartbeat = heartbeats.get(0);
        assertEquals(20, heartbeat.getInputs().size());
        BuildEvent capped = VarroaRunRecorder.applyCaps(heartbeat);
        assertEquals("the heartbeat already passes the caps", heartbeat.getInputs(), capped.getInputs());
        assertEquals(20, capped.getInputs().size());
        assertFalse(capped.getInputs().isEmpty());
    }

    @Test
    public void aHeartbeatCarriesTheSummaryBesideTheSet() throws Exception {
        runWithPendingInputs(1);
        events(BuildEvent.PHASE_RUN_INPUT);

        VarroaRunRecorder.recordSummary(runForSummary(), "the summary text");
        VarroaRunRecorder.heartbeatTick();

        List<BuildEvent> heartbeats = events(BuildEvent.PHASE_HEARTBEAT);
        assertEquals(1, heartbeats.size());
        assertEquals("the summary text", heartbeats.get(0).getSummary());
        assertEquals(1, heartbeats.get(0).getInputs().size());
    }

    /** The run a summary write resolves, with the same identity the cache uses. */
    private static Run<?, ?> runForSummary() {
        Run<?, ?> run = mock(Run.class);
        when(run.getNumber()).thenReturn(1);
        return run;
    }
}
