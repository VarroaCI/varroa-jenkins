package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;
import static org.junit.Assert.fail;
import static org.mockito.Mockito.doReturn;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

import hudson.model.Failure;
import hudson.model.ParametersDefinitionProperty;
import hudson.model.Run;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.jenkinsci.plugins.workflow.support.steps.input.InputAction;
import org.jenkinsci.plugins.workflow.support.steps.input.InputStep;
import org.jenkinsci.plugins.workflow.support.steps.input.InputStepExecution;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests for {@link VarroaRunInputs}: the fail-closed reads, the calls that
 * let a refusal through, and the caps.
 */
public class VarroaRunInputsTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private static InputStep step(String message) {
        InputStep step = mock(InputStep.class);
        when(step.getMessage()).thenReturn(message);
        when(step.getOk()).thenReturn("Ship it");
        when(step.getSubmitter()).thenReturn("varroa-payments");
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

    private static BuildInputParameter parameter(String name, String defaultValue, int choices) {
        List<String> values = new ArrayList<>();
        for (int i = 0; i < choices; i++) {
            values.add(name + "-" + i);
        }
        return BuildInputParameter.builder()
                .name(name)
                .type("ChoiceParameterDefinition")
                .defaultValue(defaultValue)
                .choices(values)
                .build();
    }

    private static BuildInput input(String id, String message, List<BuildInputParameter> parameters) {
        return BuildInput.builder()
                .id(id)
                .message(message)
                .ok("Ship it")
                .submitter("varroa-payments")
                .parameters(parameters)
                .build();
    }

    private static String big(int chars) {
        StringBuilder sb = new StringBuilder(chars);
        for (int i = 0; i < chars; i++) {
            sb.append('x');
        }
        return sb.toString();
    }

    // ---- Fail-closed reads ----

    @Test
    public void noInputActionAnswersAnEmptySet() {
        Run<?, ?> run = mock(Run.class);
        when(run.getAllActions()).thenReturn(List.of());

        assertTrue(VarroaRunInputs.readPendingInputs(run).isEmpty());
        assertNull(VarroaRunInputs.inputActionOf(run));
    }

    @Test
    public void anUnreadableSettledStateLeavesTheExecutionOut() throws Exception {
        Run<?, ?> run = mock(Run.class);
        InputStepExecution execution = execution("in-1", step("Deploy?"));
        when(execution.isSettled()).thenThrow(new IllegalStateException("no answer"));
        actionWith(run, execution);

        assertTrue(VarroaRunInputs.readPendingInputs(run).isEmpty());
    }

    @Test
    public void anExecutionWithNoIdentifierIsLeftOut() throws Exception {
        Run<?, ?> run = mock(Run.class);
        actionWith(run, execution("", step("Deploy?")));

        assertTrue(VarroaRunInputs.readPendingInputs(run).isEmpty());
    }

    @Test
    public void anUnreadableReadCountsAsSettled() {
        assertTrue("a missing method must read as settled", VarroaRunInputs.isSettled(new Object()));
        assertTrue("a throwing method must read as settled", VarroaRunInputs.isSettled(new ThrowingSettled()));
        assertFalse("a readable false must answer false", VarroaRunInputs.isSettled(new Settled(false)));
        assertTrue("a readable true must answer true", VarroaRunInputs.isSettled(new Settled(true)));
    }

    @Test
    public void theSubmissionCheckFailureReachesTheCaller() {
        try {
            VarroaRunInputs.preSubmissionCheck(new RefusingCheck());
            fail("the refusal must reach the caller");
        } catch (Failure e) {
            assertEquals("You need to be varroa-payments to submit this.", e.getMessage());
        }
    }

    @Test
    public void aMissingSettleMethodNeverPassesAsASuccess() {
        try {
            VarroaRunInputs.abort(new Object());
            fail("a missing method must not pass as a success");
        } catch (RuntimeException e) {
            assertEquals(IllegalStateException.class, e.getClass());
        }
    }

    // ---- The caps ----

    @Test
    public void twentyFiveInputsAnswerTheOldestTwenty() {
        List<BuildInput> inputs = new ArrayList<>();
        for (int i = 0; i < 25; i++) {
            inputs.add(input("in-" + i, "q" + i, List.of()));
        }

        List<BuildInput> capped = VarroaRunInputs.capInputs(inputs);

        assertEquals(20, capped.size());
        for (int i = 0; i < 20; i++) {
            assertEquals("in-" + i, capped.get(i).getId());
        }
    }

    @Test
    public void aLongMessageIsCutAt512Bytes() {
        List<BuildInput> capped = VarroaRunInputs.capInputs(List.of(input("in-1", big(600), List.of())));

        String message = capped.get(0).getMessage();
        assertEquals(512, message.getBytes(StandardCharsets.UTF_8).length);
        assertEquals(big(512), message);
    }

    @Test
    public void thirtyChoicesAnswerTwenty() {
        BuildInputParameter definition = parameter("ENV", "ENV-0", 30);

        List<BuildInput> capped = VarroaRunInputs.capInputs(
                List.of(input("in-1", "Deploy?", List.of(definition))));

        assertEquals(20, capped.get(0).getParameters().get(0).getChoices().size());
        assertEquals("ENV-19", capped.get(0).getParameters().get(0).getChoices().get(19));
    }

    @Test
    public void twentyOneParametersAnswerTheFirstTwenty() {
        List<BuildInputParameter> definitions = new ArrayList<>();
        for (int i = 0; i < 21; i++) {
            definitions.add(parameter("P" + i, "", 1));
        }

        List<BuildInput> capped = VarroaRunInputs.capInputs(List.of(input("in-1", "Deploy?", definitions)));

        assertEquals(20, capped.get(0).getParameters().size());
        assertEquals("P19", capped.get(0).getParameters().get(19).getName());
    }

    @Test
    public void aReadWithinTheCapsKeepsEveryValue() throws Exception {
        Run<?, ?> run = mock(Run.class);
        actionWith(run, execution("in-1", step("Deploy?")));

        List<BuildInput> found = VarroaRunInputs.readPendingInputs(run);

        assertEquals(1, found.size());
        assertEquals("in-1", found.get(0).getId());
        assertEquals("Deploy?", found.get(0).getMessage());
        assertEquals("Ship it", found.get(0).getOk());
        assertEquals("varroa-payments", found.get(0).getSubmitter());
        assertEquals("", found.get(0).getSubmitterParameter());
        assertTrue(found.get(0).getParameters().isEmpty());
    }

    // ---- One real paused pipeline ----

    @Test
    public void aPasswordParameterReportsNoDefault() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "input-password");
        job.setDefinition(new CpsFlowDefinition(
                "input message: 'Deploy?', parameters: [password(name: 'TOKEN', defaultValue: 's3cret')]",
                false));
        WorkflowRun run = (WorkflowRun) job.scheduleBuild2(0).waitForStart();
        try {
            awaitPendingInput(run);

            List<BuildInput> found = VarroaRunInputs.readPendingInputs(run);

            assertEquals(1, found.size());
            BuildInputParameter definition = found.get(0).getParameters().get(0);
            assertEquals("TOKEN", definition.getName());
            assertEquals("PasswordParameterDefinition", definition.getType());
            assertEquals("", definition.getDefaultValue());
        } finally {
            run.doStop();
        }
    }

    @Test
    public void aRealInputStepReportsItsOwnDeclaredParameter() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "input-read");
        job.setDefinition(new CpsFlowDefinition(
                "input message: 'Deploy?', ok: 'Ship it', submitter: 'varroa-payments',"
                        + " parameters: [string(name: 'ENV', defaultValue: 'staging')]",
                false));
        WorkflowRun run = (WorkflowRun) job.scheduleBuild2(0).waitForStart();
        try {
            awaitPendingInput(run);

            List<BuildInput> found = VarroaRunInputs.readPendingInputs(run);

            assertEquals(1, found.size());
            assertFalse(found.get(0).getId().isEmpty());
            assertEquals("Deploy?", found.get(0).getMessage());
            assertEquals("Ship it", found.get(0).getOk());
            assertEquals("varroa-payments", found.get(0).getSubmitter());
            assertEquals("", found.get(0).getSubmitterParameter());
            assertEquals(1, found.get(0).getParameters().size());
            BuildInputParameter definition = found.get(0).getParameters().get(0);
            assertEquals("ENV", definition.getName());
            assertEquals("StringParameterDefinition", definition.getType());
            assertEquals("staging", definition.getDefaultValue());
            assertTrue(definition.getChoices().isEmpty());
            // The step's own definitions differ from the job's: this job
            // declares no parameter at all.
            assertNull(job.getProperty(ParametersDefinitionProperty.class));
        } finally {
            run.doStop();
            j.waitUntilNoActivity();
        }
    }

    @Test
    public void theActionResolvesOneExecutionByIdentifier() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "input-resolve");
        job.setDefinition(new CpsFlowDefinition("input message: 'Deploy?'", false));
        WorkflowRun run = (WorkflowRun) job.scheduleBuild2(0).waitForStart();
        try {
            awaitPendingInput(run);

            Object action = VarroaRunInputs.inputActionOf(run);
            assertNotNull(action);
            String id = VarroaRunInputs.readPendingInputs(run).get(0).getId();

            Object execution = VarroaRunInputs.executionFor(action, id);

            assertNotNull(execution);
            assertFalse(VarroaRunInputs.isSettled(execution));
            assertNull(VarroaRunInputs.executionFor(action, "no-such-input"));
        } finally {
            run.doStop();
            j.waitUntilNoActivity();
        }
    }

    /** Waits until the run holds one pending input, and fails after a minute. */
    static void awaitPendingInput(WorkflowRun run) throws Exception {
        long deadline = System.currentTimeMillis() + 60_000L;
        while (System.currentTimeMillis() < deadline) {
            InputAction action = run.getAction(InputAction.class);
            if (action != null && !action.getExecutions().isEmpty()) {
                return;
            }
            Thread.sleep(200L);
        }
        throw new AssertionError("no pending input appeared on " + run);
    }

    /** An object whose settled read throws. */
    public static final class ThrowingSettled {
        public boolean isSettled() {
            throw new IllegalStateException("no answer");
        }
    }

    /** An object whose settled read answers a fixed value. */
    public static final class Settled {
        private final boolean value;

        Settled(boolean value) {
            this.value = value;
        }

        public boolean isSettled() {
            return value;
        }
    }

    /** An object whose submission check refuses, the way the input step's own check does. */
    public static final class RefusingCheck {
        public void preSubmissionCheck() {
            throw new Failure("You need to be varroa-payments to submit this.");
        }
    }
}
