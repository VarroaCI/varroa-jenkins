package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertTrue;
import static org.mockito.Mockito.doReturn;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

import hudson.model.Run;

import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.atomic.AtomicBoolean;

import org.jenkinsci.plugins.workflow.cps.nodes.StepAtomNode;
import org.jenkinsci.plugins.workflow.steps.StepDescriptor;
import org.jenkinsci.plugins.workflow.support.steps.input.InputAction;
import org.jenkinsci.plugins.workflow.support.steps.input.InputStep;
import org.jenkinsci.plugins.workflow.support.steps.input.InputStepExecution;
import org.junit.After;
import org.junit.Before;
import org.junit.Test;

/**
 * Tests for the deferred re-check {@link VarroaRunRecorder} schedules when a
 * node arrives: the attempt schedule, the single-task rule, and the two
 * guards that keep a task of a finished run silent.
 *
 * <p>One node buys three reads, at 250 ms, 1 s, and 4 s. A case that waits
 * for every read of a node therefore waits past the last delay.
 */
public class VarroaRunInputsDetectionTest {

    private static final String JOB_PATH = "";
    private static final int BUILD_NUMBER = 1;
    /** The sum of the three re-check delays, plus margin for a slow machine. */
    private static final long ATTEMPT_BUDGET_MILLIS = 9_000L;
    /** A wait that outlasts every read of one node. */
    private static final long AFTER_LAST_ATTEMPT_MILLIS = 7_000L;

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

    // ---- The run under test ----

    private static Run<?, ?> runningRun() {
        Run<?, ?> run = mock(Run.class);
        when(run.getNumber()).thenReturn(BUILD_NUMBER);
        when(run.isBuilding()).thenReturn(true);
        when(run.getAllActions()).thenReturn(List.of());
        return run;
    }

    private static InputStep step(String... messages) {
        InputStep step = mock(InputStep.class);
        if (messages.length == 1) {
            when(step.getMessage()).thenReturn(messages[0]);
        } else {
            when(step.getMessage()).thenReturn(messages[0], java.util.Arrays.copyOfRange(messages, 1, messages.length));
        }
        when(step.getOk()).thenReturn("Ship it");
        when(step.getSubmitter()).thenReturn("");
        when(step.getSubmitterParameter()).thenReturn("");
        when(step.getParameters()).thenReturn(List.of());
        return step;
    }

    private static InputStepExecution execution(String id, InputStep step) {
        InputStepExecution execution = mock(InputStepExecution.class);
        when(execution.getId()).thenReturn(id);
        when(execution.getInput()).thenReturn(step);
        when(execution.isSettled()).thenReturn(false);
        return execution;
    }

    /**
     * One execution whose settled state a test flips later. The answer reads
     * an {@link AtomicBoolean} on every call, because the timer thread
     * invokes it while the test thread runs: a second stubbing pass on a
     * mock under that concurrent call is not safe.
     */
    private static final class PendingExecution {
        private final AtomicBoolean settled = new AtomicBoolean();
        private final InputStepExecution execution;

        private PendingExecution(String id, InputStep step) {
            this.execution = mock(InputStepExecution.class);
            when(execution.getId()).thenReturn(id);
            when(execution.getInput()).thenReturn(step);
            when(execution.isSettled()).thenAnswer(invocation -> settled.get());
        }

        private void settle() {
            settled.set(true);
        }
    }

    /**
     * Attaches one real input action to {@code run} and adds every execution
     * to it. The action list goes in with {@code doReturn}, because {@code
     * Run.getAllActions()} answers a wildcard list.
     */
    private static InputAction actionWith(Run<?, ?> run, InputStepExecution... executions) throws Exception {
        InputAction action = new InputAction();
        action.onAttached(run);
        for (InputStepExecution execution : executions) {
            action.add(execution);
        }
        doReturn(List.of(action)).when(run).getAllActions();
        return action;
    }

    private static StepAtomNode node(String functionName) {
        StepDescriptor descriptor = mock(StepDescriptor.class);
        when(descriptor.getFunctionName()).thenReturn(functionName);
        StepAtomNode node = mock(StepAtomNode.class);
        when(node.getDescriptor()).thenReturn(descriptor);
        return node;
    }

    /** Delivers one node to the graph listener, the way the flow does. */
    private static void deliver(Run<?, ?> run, StepAtomNode node) {
        new VarroaRunRecorder.Graph(run).onNewHead(node);
    }

    private static List<BuildEvent> runInputEvents() {
        List<BuildEvent> found = new ArrayList<>();
        for (BuildEvent event : BuildEventSink.get().drain(200).events) {
            if (BuildEvent.PHASE_RUN_INPUT.equals(event.getPhase())) {
                found.add(event);
            }
        }
        return found;
    }

    /** Waits until {@code count} run.input events arrived, and answers them. */
    private static List<BuildEvent> awaitEvents(int count, long budgetMillis) throws Exception {
        List<BuildEvent> found = new ArrayList<>();
        long deadline = System.currentTimeMillis() + budgetMillis;
        while (System.currentTimeMillis() < deadline) {
            found.addAll(runInputEvents());
            if (found.size() >= count) {
                return found;
            }
            Thread.sleep(100L);
        }
        return found;
    }

    // ---- The attempt schedule ----

    @Test
    public void anInputNodeStartsThreeAttempts() throws Exception {
        Run<?, ?> run = runningRun();
        actionWith(run, execution("in-1", step("first", "second", "third")));
        VarroaRunRecorder.registerRunForTesting(JOB_PATH, BUILD_NUMBER, "run-a", "intent-a");

        deliver(run, node("input"));

        List<BuildEvent> events = awaitEvents(3, ATTEMPT_BUDGET_MILLIS);
        assertEquals("one node buys three reads", 3, events.size());
        assertEquals("first", events.get(0).getInputs().get(0).getMessage());
        assertEquals("third", events.get(2).getInputs().get(0).getMessage());
    }

    @Test
    public void aBurstOfNodesArmsOneTask() throws Exception {
        Run<?, ?> run = runningRun();
        actionWith(run, execution("in-1", step("first", "second", "third", "fourth", "fifth", "sixth")));
        VarroaRunRecorder.registerRunForTesting(JOB_PATH, BUILD_NUMBER, "run-a", "intent-a");

        deliver(run, node("input"));
        deliver(run, node("input"));
        deliver(run, node("input"));

        List<BuildEvent> events = awaitEvents(3, ATTEMPT_BUDGET_MILLIS);
        Thread.sleep(2_000L);
        events.addAll(runInputEvents());
        assertEquals("a burst of nodes arms one task", 3, events.size());
    }

    @Test
    public void aNonInputNodeWithAnEmptyCacheArmsNothing() throws Exception {
        Run<?, ?> run = runningRun();
        actionWith(run, execution("in-1", step("Deploy?")));
        VarroaRunRecorder.registerRunForTesting(JOB_PATH, BUILD_NUMBER, "run-a", "intent-a");

        deliver(run, node("sh"));

        Thread.sleep(1_500L);
        assertTrue("a non-input node with an empty cache reads nothing", runInputEvents().isEmpty());
    }

    // ---- The clearing path ----

    @Test
    public void aNodeWhileTheCacheIsNotEmptyArmsARecheck() throws Exception {
        Run<?, ?> run = runningRun();
        PendingExecution pending = new PendingExecution("in-1", step("Deploy?"));
        actionWith(run, pending.execution);
        VarroaRunRecorder.registerRunForTesting(JOB_PATH, BUILD_NUMBER, "run-a", "intent-a");

        deliver(run, node("input"));
        assertEquals(1, awaitEvents(1, ATTEMPT_BUDGET_MILLIS).size());
        // Every read of the first node runs out before the next node
        // arrives. The arriving node finds no armed task, so that node alone
        // arms the clearing read.
        Thread.sleep(AFTER_LAST_ATTEMPT_MILLIS);
        pending.settle();

        deliver(run, node("sh"));

        // The first await drained the reporting event, so the clearing
        // event is the only event left to arrive.
        List<BuildEvent> events = awaitEvents(1, 5_000L);
        assertEquals(1, events.size());
        assertTrue("the clearing event carries an empty set", events.get(0).getInputs().isEmpty());
    }

    @Test
    public void anEmptyReadClearsTheSetOnce() throws Exception {
        Run<?, ?> run = runningRun();
        PendingExecution pending = new PendingExecution("in-1", step("Deploy?"));
        actionWith(run, pending.execution);
        VarroaRunRecorder.registerRunForTesting(JOB_PATH, BUILD_NUMBER, "run-a", "intent-a");

        deliver(run, node("input"));
        awaitEvents(1, ATTEMPT_BUDGET_MILLIS);
        Thread.sleep(AFTER_LAST_ATTEMPT_MILLIS);
        pending.settle();
        deliver(run, node("sh"));
        assertEquals("the clearing read emits once", 1, awaitEvents(1, 5_000L).size());

        Thread.sleep(1_000L);

        assertEquals("the same empty set emits nothing again", 0, runInputEvents().size());
    }

    // ---- The two guards ----

    @Test
    public void aRecheckWhoseStateLeftTheMapEmitsNothing() throws Exception {
        Run<?, ?> run = runningRun();
        actionWith(run, execution("in-1", step("Deploy?")));
        VarroaRunRecorder.registerRunForTesting(JOB_PATH, BUILD_NUMBER, "run-a", "intent-a");

        deliver(run, node("input"));
        VarroaRunRecorder.removeRunForTesting(JOB_PATH, BUILD_NUMBER);

        Thread.sleep(1_500L);
        assertTrue("a task whose run left the map emits nothing", runInputEvents().isEmpty());
    }

    @Test
    public void aRecheckOnAStoppedRunEmitsNothing() throws Exception {
        Run<?, ?> run = runningRun();
        actionWith(run, execution("in-1", step("Deploy?")));
        VarroaRunRecorder.registerRunForTesting(JOB_PATH, BUILD_NUMBER, "run-a", "intent-a");
        when(run.isBuilding()).thenReturn(false);

        deliver(run, node("input"));

        Thread.sleep(1_500L);
        assertTrue("a task of a stopped run emits nothing", runInputEvents().isEmpty());
    }

    // ---- Two parallel inputs ----

    @Test
    public void twoParallelInputsBothReachTheSet() throws Exception {
        Run<?, ?> run = runningRun();
        InputAction action = actionWith(run, execution("in-1", step("Deploy?")));
        VarroaRunRecorder.registerRunForTesting(JOB_PATH, BUILD_NUMBER, "run-a", "intent-a");

        deliver(run, node("input"));
        // The second step attaches its execution after the first read ran.
        Thread.sleep(500L);
        action.add(execution("in-2", step("Second?")));

        List<BuildEvent> events = awaitEvents(2, ATTEMPT_BUDGET_MILLIS);
        boolean bothReported = false;
        for (BuildEvent event : events) {
            if (event.getInputs().size() == 2) {
                bothReported = true;
            }
        }
        assertTrue("a later attempt reports both inputs", bothReported);
    }
}
