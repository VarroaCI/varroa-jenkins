package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.fail;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

import hudson.model.Item;
import hudson.model.StringParameterValue;
import hudson.security.ACL;
import hudson.security.ACLContext;

import java.io.BufferedReader;
import java.io.PrintWriter;
import java.io.StringReader;
import java.io.StringWriter;

import net.sf.json.JSONArray;
import net.sf.json.JSONObject;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.jenkinsci.plugins.workflow.support.steps.input.InputAction;
import org.jenkinsci.plugins.workflow.support.steps.input.InputStepExecution;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.jvnet.hudson.test.MockAuthorizationStrategy;
import org.kohsuke.stapler.StaplerRequest2;
import org.kohsuke.stapler.StaplerResponse2;
import org.springframework.security.authentication.UsernamePasswordAuthenticationToken;
import org.springframework.security.core.authority.SimpleGrantedAuthority;

/**
 * Tests for the two input routes of {@link VarroaRunTriggerAction} (D10, D11):
 * the proceed route and the abort route.
 *
 * <p>A mocked input execution cannot serve these tests. The route resolves its
 * input through the run's own input action, and a mocked run action carries no
 * input field, so every case drives a real paused pipeline under
 * {@link JenkinsRule}.
 */
public class VarroaRunInputRouteTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private static final String MITE_AUTHORITY = "ROLE:varroa:system-mite";
    private static final String PAYMENTS_GROUP = "varroa-payments";

    /** A paused step with a group-scoped submitter and one declared parameter. */
    private static final String ENV_INPUT_SCRIPT =
            "def v = input message: 'Deploy?', submitter: 'varroa-payments', "
                    + "parameters: [string(name: 'ENV', defaultValue: 'staging')]\n"
                    + "echo 'ENV=' + v\n";

    /** A paused step with a group-scoped submitter and no parameter. */
    private static final String PLAIN_INPUT_SCRIPT =
            "input message: 'Deploy?', submitter: 'varroa-payments'\n";

    /** A paused step that declares a file parameter beside a string parameter. */
    private static final String FILE_INPUT_SCRIPT =
            "def v = input message: 'Deploy?', submitter: 'varroa-payments', "
                    + "parameters: [file(name: 'F', description: 'a file'), "
                    + "string(name: 'ENV', defaultValue: 'staging')]\n"
                    + "echo 'ENV=' + v\n";

    /** A paused step that declares a submitter parameter beside a string parameter. */
    private static final String APPROVER_INPUT_SCRIPT =
            "def r = input message: 'Deploy?', submitter: 'varroa-payments', "
                    + "submitterParameter: 'APPROVER', "
                    + "parameters: [string(name: 'ENV', defaultValue: 'staging')]\n"
                    + "echo 'APPROVER=' + r['APPROVER']\n";

    private int jobCounter;
    private String runId;

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

    // ---- The proceed route ----

    @Test
    public void aPermittedActingUserProceedsTheInput() throws Exception {
        WorkflowRun run = start(PLAIN_INPUT_SCRIPT, true);
        String inputId = awaitPendingInput(run);

        StaplerResponse2 rsp = proceed(body(run, inputId, actingUser("ada", PAYMENTS_GROUP), null),
                new StringWriter());

        verify(rsp).setStatus(202);
        j.waitForCompletion(run);
        awaitNoPendingInput(run);
    }

    @Test
    public void aSubmitterMismatchLeavesTheInputPending() throws Exception {
        useSecureRealm();
        WorkflowRun run = start(PLAIN_INPUT_SCRIPT, true);
        String inputId = awaitPendingInput(run);

        StaplerResponse2 rsp = proceed(body(run, inputId, actingUser("ada", "varroa-other"), null),
                new StringWriter());

        verify(rsp).sendError(403, "Forbidden: the acting user may not answer the input");
        assertEquals(1, pendingCount(run));
    }

    @Test
    public void aSettledInputAnswers409() throws Exception {
        WorkflowRun run = start(PLAIN_INPUT_SCRIPT, true);
        String inputId = awaitPendingInput(run);
        InputAction action = run.getAction(InputAction.class);
        InputStepExecution execution = action.getExecution(inputId);

        StaplerResponse2 first = proceed(body(run, inputId, actingUser("ada", PAYMENTS_GROUP), null),
                new StringWriter());
        verify(first).setStatus(202);
        j.waitForCompletion(run);

        // The settle removes the execution from the run's input action. A
        // request that names an input the record still lists as pending
        // therefore arrives at an execution the action holds and the step
        // has settled: the state a second click on one card reaches.
        action.add(execution);

        StaplerResponse2 second = proceed(body(run, inputId, actingUser("ada", PAYMENTS_GROUP), null),
                new StringWriter());

        verify(second).sendError(409, "Conflict: the input is already settled");
    }

    @Test
    public void anUnknownInputIdAnswers404() throws Exception {
        WorkflowRun run = start(PLAIN_INPUT_SCRIPT, true);
        awaitPendingInput(run);

        StaplerResponse2 rsp = proceed(
                body(run, "no-such-input", actingUser("ada", PAYMENTS_GROUP), null), new StringWriter());

        verify(rsp).sendError(404, "Not Found: no input carries id no-such-input");
    }

    @Test
    public void aCallerWithoutTheMiteAuthorityAnswers403() throws Exception {
        WorkflowRun run = start(PLAIN_INPUT_SCRIPT, true);
        String inputId = awaitPendingInput(run);

        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "bob", "", java.util.List.of(new SimpleGrantedAuthority("ROLE:varroa:developer"))))) {
            StaplerRequest2 req = requestWithBody(body(run, inputId, actingUser("ada", PAYMENTS_GROUP), null));
            StaplerResponse2 rsp = mock(StaplerResponse2.class);

            new VarroaRunTriggerAction().doProceedInput(req, rsp);

            verify(rsp).sendError(403, "Forbidden: requires ROLE:varroa:system-mite authority");
        }
        assertEquals(1, pendingCount(run));
    }

    // ---- The parameter pairing ----

    @Test
    public void anOmittedParameterTakesTheDeclaredDefault() throws Exception {
        WorkflowRun run = start(ENV_INPUT_SCRIPT, true);
        String inputId = awaitPendingInput(run);

        StaplerResponse2 rsp = proceed(body(run, inputId, actingUser("ada", PAYMENTS_GROUP), null),
                new StringWriter());

        verify(rsp).setStatus(202);
        j.waitForCompletion(run);
        j.assertLogContains("ENV=staging", run);
    }

    @Test
    public void anUndeclaredParameterIsDroppedAndNamed() throws Exception {
        WorkflowRun run = start(ENV_INPUT_SCRIPT, true);
        String inputId = awaitPendingInput(run);
        StringWriter out = new StringWriter();

        StaplerResponse2 rsp = proceed(
                body(run, inputId, actingUser("ada", PAYMENTS_GROUP), "{\"ENV\":\"prod\",\"NOPE\":\"x\"}"), out);

        verify(rsp).setStatus(202);
        JSONArray dropped = JSONObject.fromObject(out.toString()).getJSONArray("droppedParameters");
        assertEquals(1, dropped.size());
        assertEquals("NOPE", dropped.getString(0));
        j.waitForCompletion(run);
        j.assertLogContains("ENV=prod", run);
    }

    @Test
    public void aFileParameterIsDroppedAndNamed() throws Exception {
        // The input step refuses a file parameter unless this property
        // allows it, so the case enables it for its own run only.
        String property = InputStepExecution.class.getName() + ".supportUnsafeParameters";
        String previous = System.getProperty(property);
        System.setProperty(property, "true");
        try {
            WorkflowRun run = start(FILE_INPUT_SCRIPT, true);
            String inputId = awaitPendingInput(run);
            StringWriter out = new StringWriter();

            StaplerResponse2 rsp = proceed(
                    body(run, inputId, actingUser("ada", PAYMENTS_GROUP), "{\"F\":\"upload\",\"ENV\":\"prod\"}"), out);

            verify(rsp).setStatus(202);
            JSONArray dropped = JSONObject.fromObject(out.toString()).getJSONArray("droppedParameters");
            assertEquals(1, dropped.size());
            assertEquals("F", dropped.getString(0));
            j.waitForCompletion(run);
            j.assertLogContains("ENV=prod", run);
        } finally {
            if (previous == null) {
                System.clearProperty(property);
            } else {
                System.setProperty(property, previous);
            }
        }
    }

    @Test
    public void theSubmitterParameterHoldsTheActingUser() throws Exception {
        WorkflowRun run = start(APPROVER_INPUT_SCRIPT, true);
        String inputId = awaitPendingInput(run);

        StaplerResponse2 rsp = proceed(
                body(run, inputId, actingUser("ada", PAYMENTS_GROUP), "{\"ENV\":\"prod\"}"), new StringWriter());

        verify(rsp).setStatus(202);
        j.waitForCompletion(run);
        j.assertLogContains("APPROVER=ada", run);
    }

    // ---- The abort route ----

    @Test
    public void anAbortAnswers202() throws Exception {
        WorkflowRun run = start(PLAIN_INPUT_SCRIPT, true);
        String inputId = awaitPendingInput(run);

        StaplerResponse2 rsp = abort(body(run, inputId, actingUser("ada", PAYMENTS_GROUP), null),
                new StringWriter());

        verify(rsp).setStatus(202);
        j.waitForCompletion(run);
        awaitNoPendingInput(run);
    }

    @Test
    public void aRefusedAbortLeavesTheInputPending() throws Exception {
        useSecureRealm();
        WorkflowRun run = start(PLAIN_INPUT_SCRIPT, true);
        String inputId = awaitPendingInput(run);

        StaplerResponse2 rsp = abort(body(run, inputId, actingUser("ada", "varroa-other"), null),
                new StringWriter());

        verify(rsp).sendError(403, "Forbidden: the acting user may not reject the input");
        assertEquals(1, pendingCount(run));
    }

    // ---- The harness ----

    /** Starts one run of a fresh pipeline and records its run identifier. */
    private WorkflowRun start(String script, boolean sandbox) throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "input-route-" + ++jobCounter);
        job.setDefinition(new CpsFlowDefinition(script, sandbox));
        runId = "run-input-route-" + jobCounter;
        return (WorkflowRun) job.scheduleBuild2(0,
                new VarroaIntentAction(runId, "", null, System.currentTimeMillis())).waitForStart();
    }

    /**
     * Grants the two identities this JVM uses the permissions a run needs, and
     * enables security, so a group-scoped submitter check decides the outcome.
     * The build-cancel permission stays ungranted, so the abort path refuses a
     * caller the step's submitter also refuses.
     */
    private void useSecureRealm() {
        MockAuthorizationStrategy strategy = new MockAuthorizationStrategy()
                .grant(Item.READ, Item.CREATE, Item.BUILD).everywhere().toEveryone();
        j.jenkins.setSecurityRealm(j.createDummySecurityRealm());
        j.jenkins.setAuthorizationStrategy(strategy);
    }

    /** Waits for the run's first pending input and answers its identifier. */
    private static String awaitPendingInput(WorkflowRun run) throws Exception {
        long deadline = System.currentTimeMillis() + 60000L;
        while (System.currentTimeMillis() < deadline) {
            InputAction action = run.getAction(InputAction.class);
            if (action != null) {
                for (InputStepExecution execution : action.getExecutions()) {
                    return execution.getId();
                }
            }
            Thread.sleep(50L);
            if (!run.isBuilding()) {
                break;
            }
        }
        fail("the run reached no pending input; result=" + run.getResult() + " log=" + run.getLog(60));
        return null;
    }

    /** Waits until the run's input action lists no pending execution. */
    private static void awaitNoPendingInput(WorkflowRun run) throws Exception {
        long deadline = System.currentTimeMillis() + 60000L;
        while (System.currentTimeMillis() < deadline) {
            if (pendingCount(run) == 0) {
                return;
            }
            Thread.sleep(50L);
        }
        fail("the run still lists a pending input");
    }

    private static int pendingCount(WorkflowRun run) throws Exception {
        InputAction action = run.getAction(InputAction.class);
        return action == null ? 0 : action.getExecutions().size();
    }

    /** Builds one command body, with the request's own parameters object. */
    private String body(WorkflowRun run, String inputId, String actingUser, String parameters) {
        return "{\"runId\":\"" + runId + "\",\"jobPath\":\"" + run.getParent().getFullName()
                + "\",\"buildNumber\":" + run.getNumber() + ",\"inputId\":\"" + inputId + "\""
                + (parameters != null ? ",\"parameters\":" + parameters : "")
                + ",\"actingUser\":" + actingUser + "}";
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

    private static StaplerResponse2 proceed(String json, StringWriter out) throws Exception {
        try (ACLContext ctx = asMite()) {
            StaplerResponse2 rsp = responseCapturing(out);
            new VarroaRunTriggerAction().doProceedInput(requestWithBody(json), rsp);
            return rsp;
        }
    }

    private static StaplerResponse2 abort(String json, StringWriter out) throws Exception {
        try (ACLContext ctx = asMite()) {
            StaplerResponse2 rsp = responseCapturing(out);
            new VarroaRunTriggerAction().doAbortInput(requestWithBody(json), rsp);
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
        StaplerResponse2 rsp = mock(StaplerResponse2.class);
        when(rsp.getWriter()).thenReturn(new PrintWriter(writer));
        return rsp;
    }

    private static ACLContext asMite() {
        return ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", java.util.List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))));
    }
}
