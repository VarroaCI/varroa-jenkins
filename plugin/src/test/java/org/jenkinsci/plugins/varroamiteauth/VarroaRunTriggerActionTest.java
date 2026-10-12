package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotEquals;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.contains;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

import hudson.EnvVars;
import hudson.Launcher;
import hudson.model.AbstractBuild;
import hudson.model.Action;
import hudson.model.BuildListener;
import hudson.model.Cause;
import hudson.model.CauseAction;
import hudson.model.FreeStyleBuild;
import hudson.model.FreeStyleProject;
import hudson.model.ParametersAction;
import hudson.model.ParametersDefinitionProperty;
import hudson.model.Queue;
import hudson.model.StringParameterDefinition;
import hudson.model.StringParameterValue;
import hudson.security.ACL;
import hudson.security.ACLContext;

import java.io.BufferedReader;
import java.io.StringReader;
import java.io.StringWriter;
import java.net.URL;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;

import org.htmlunit.HttpMethod;
import org.htmlunit.WebRequest;
import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.jvnet.hudson.test.MockFolder;
import org.jvnet.hudson.test.TestBuilder;
import org.kohsuke.stapler.StaplerRequest2;
import org.kohsuke.stapler.StaplerResponse2;
import org.springframework.security.authentication.UsernamePasswordAuthenticationToken;
import org.springframework.security.core.authority.SimpleGrantedAuthority;

import net.sf.json.JSONObject;

/**
 * Tests for {@link VarroaRunTriggerAction} (2.5, 2.6): the trigger route
 * stamps the caller's identity before scheduling and handles the job's
 * declared parameters correctly, and the cancel route resolves its target
 * by run identifier.
 */
public class VarroaRunTriggerActionTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private static final String MITE_AUTHORITY = "ROLE:varroa:system-mite";

    /** Builds a mocked trigger/cancel request, stubbing the JSON parameter binding used by {@code createValue}. */
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
        return RefusalAssert.capturing(writer);
    }

    private static ACLContext asMite() {
        return ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))));
    }

    // ---- 2.5 trigger route ----

    @Test
    public void triggerStampsTheQueueItemWithTheCallersIdentity() throws Exception {
        j.jenkins.setNumExecutors(0);
        FreeStyleProject p = j.createFreeStyleProject("trigger-stamp");

        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"runId\":\"run-stamp-1\",\"intentKey\":\"manual:run-stamp-1\""
                    + ",\"jobPath\":\"" + p.getFullName() + "\"}");
            StaplerResponse2 rsp = responseCapturing(new StringWriter());

            action.doTrigger(req, rsp);

            verify(rsp).setStatus(202);
        }

        Queue.Item[] items = Queue.getInstance().getItems();
        assertEquals(1, items.length);
        VarroaIntentAction stamped = items[0].getAction(VarroaIntentAction.class);
        assertNotNull("queued item must carry a VarroaIntentAction", stamped);
        assertEquals("run-stamp-1", stamped.getRunId());
        assertEquals("manual:run-stamp-1", stamped.getIntentKey());
        assertEquals(IntentKey.ORIGIN_VARROA_INTENT, stamped.getOrigin());
        assertEquals("", stamped.getActor());
    }

    @Test
    public void triggerStampsTheSuppliedActorUnchanged() throws Exception {
        j.jenkins.setNumExecutors(0);
        FreeStyleProject p = j.createFreeStyleProject("trigger-actor");

        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"runId\":\"run-actor-1\",\"intentKey\":\"manual:run-actor-1\""
                    + ",\"jobPath\":\"" + p.getFullName() + "\",\"actor\":\"varroa-upstream/payments\"}");
            StaplerResponse2 rsp = responseCapturing(new StringWriter());

            action.doTrigger(req, rsp);

            verify(rsp).setStatus(202);
        }

        Queue.Item[] items = Queue.getInstance().getItems();
        assertEquals(1, items.length);
        VarroaIntentAction stamped = items[0].getAction(VarroaIntentAction.class);
        assertNotNull("queued item must carry a VarroaIntentAction", stamped);
        assertEquals("varroa-upstream/payments", stamped.getActor());
    }

    @Test
    public void triggerSchedulesAVarroaCauseNamingTheActor() throws Exception {
        j.jenkins.setNumExecutors(0);
        FreeStyleProject p = j.createFreeStyleProject("trigger-cause");

        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"runId\":\"run-cause-1\",\"intentKey\":\"manual:run-cause-1\""
                    + ",\"jobPath\":\"" + p.getFullName() + "\",\"actor\":\"nate\"}");
            StaplerResponse2 rsp = responseCapturing(new StringWriter());

            action.doTrigger(req, rsp);

            verify(rsp).setStatus(202);
        }

        Queue.Item[] items = Queue.getInstance().getItems();
        assertEquals(1, items.length);
        List<VarroaUserCause> causes = varroaCausesOf(items[0]);
        assertEquals("the item carries one Varroa cause", 1, causes.size());
        assertEquals("nate", causes.get(0).getUserId());
        assertEquals("Started by nate through Varroa", causes.get(0).getShortDescription());
    }

    @Test
    public void anEmptyActorAttachesNoVarroaCause() throws Exception {
        j.jenkins.setNumExecutors(0);
        FreeStyleProject p = j.createFreeStyleProject("trigger-no-cause");

        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"runId\":\"run-no-cause\",\"intentKey\":\"manual:run-no-cause\""
                    + ",\"jobPath\":\"" + p.getFullName() + "\"}");
            StaplerResponse2 rsp = responseCapturing(new StringWriter());

            action.doTrigger(req, rsp);

            verify(rsp).setStatus(202);
        }

        Queue.Item[] items = Queue.getInstance().getItems();
        assertEquals(1, items.length);
        assertTrue("an empty actor attaches no Varroa cause", varroaCausesOf(items[0]).isEmpty());
    }

    @Test
    public void anActorWithWhitespaceAndANewlineNormalizesToTheTrimmedName() throws Exception {
        j.jenkins.setNumExecutors(0);
        FreeStyleProject p = j.createFreeStyleProject("trigger-actor-trimmed");

        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"runId\":\"run-actor-trimmed\","
                    + "\"intentKey\":\"manual:run-actor-trimmed\""
                    + ",\"jobPath\":\"" + p.getFullName() + "\",\"actor\":\"  alice\\n\"}");
            StaplerResponse2 rsp = responseCapturing(new StringWriter());

            action.doTrigger(req, rsp);

            verify(rsp).setStatus(202);
        }

        Queue.Item[] items = Queue.getInstance().getItems();
        assertEquals(1, items.length);
        VarroaIntentAction stamped = items[0].getAction(VarroaIntentAction.class);
        assertNotNull("queued item must carry a VarroaIntentAction", stamped);
        assertEquals("alice", stamped.getActor());
        List<VarroaUserCause> causes = varroaCausesOf(items[0]);
        assertEquals("the item carries one Varroa cause", 1, causes.size());
        assertEquals("alice", causes.get(0).getUserId());
    }

    @Test
    public void anAnonymousActorAttachesNoVarroaCause() throws Exception {
        j.jenkins.setNumExecutors(0);
        FreeStyleProject p = j.createFreeStyleProject("trigger-anonymous");

        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"runId\":\"run-anonymous\","
                    + "\"intentKey\":\"manual:run-anonymous\""
                    + ",\"jobPath\":\"" + p.getFullName() + "\",\"actor\":\"anonymous\"}");
            StaplerResponse2 rsp = responseCapturing(new StringWriter());

            action.doTrigger(req, rsp);

            verify(rsp).setStatus(202);
        }

        Queue.Item[] items = Queue.getInstance().getItems();
        assertEquals(1, items.length);
        VarroaIntentAction stamped = items[0].getAction(VarroaIntentAction.class);
        assertNotNull("queued item must carry a VarroaIntentAction", stamped);
        assertEquals("", stamped.getActor());
        assertTrue("an anonymous actor attaches no Varroa cause", varroaCausesOf(items[0]).isEmpty());
    }

    @Test
    public void aTriggeredBuildNamesTheActorInItsCausesAndItsFirstLogLines() throws Exception {
        j.jenkins.setNumExecutors(1);
        FreeStyleProject p = j.createFreeStyleProject("trigger-log");
        CountDownLatch started = new CountDownLatch(1);
        p.getBuildersList().add(new TestBuilder() {
            @Override
            public boolean perform(AbstractBuild<?, ?> build, Launcher launcher, BuildListener listener)
                    throws InterruptedException {
                started.countDown();
                return true;
            }
        });

        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"runId\":\"run-log-1\",\"intentKey\":\"manual:run-log-1\""
                    + ",\"jobPath\":\"" + p.getFullName() + "\",\"actor\":\"nate\"}");
            StaplerResponse2 rsp = responseCapturing(new StringWriter());

            action.doTrigger(req, rsp);

            verify(rsp).setStatus(202);
            assertTrue("the triggered build must start within 10s", started.await(10, TimeUnit.SECONDS));
        }

        FreeStyleBuild build = p.getLastBuild();
        assertNotNull("the triggered build must run", build);
        j.assertBuildStatusSuccess(j.waitForCompletion(build));

        List<Cause> causes = build.getCauses();
        boolean namesTheActor = false;
        for (Cause cause : causes) {
            if (cause instanceof VarroaUserCause) {
                namesTheActor = true;
                assertEquals("nate", ((VarroaUserCause) cause).getUserId());
            }
        }
        assertTrue("the run must carry a VarroaUserCause naming the actor", namesTheActor);
        assertEquals("nate", VarroaUserCause.actorFromCauses(causes));

        // Jenkins prints every cause's short description at the top of the log.
        String head = String.join("\n", build.getLog(20));
        assertTrue("the first log lines must name the actor, got: " + head,
                head.contains("Started by nate through Varroa"));
        assertFalse("no cause may render as anonymous, got: " + head, head.contains("anonymous"));
    }

    /**
     * Answers the {@link VarroaUserCause} causes a queue item holds. Jenkins
     * core may attach a cause of its own, so this reads every cause action the
     * item carries and keeps the Varroa ones.
     */
    private static List<VarroaUserCause> varroaCausesOf(Queue.Item item) {
        List<VarroaUserCause> found = new ArrayList<>();
        for (Action action : item.getActions()) {
            if (action instanceof CauseAction) {
                for (Cause cause : ((CauseAction) action).getCauses()) {
                    if (cause instanceof VarroaUserCause) {
                        found.add((VarroaUserCause) cause);
                    }
                }
            }
        }
        return found;
    }

    @Test
    public void undeclaredParameterIsDroppedAndNamed() throws Exception {
        j.jenkins.setNumExecutors(0);
        FreeStyleProject p = j.createFreeStyleProject("trigger-undeclared");
        p.addProperty(new ParametersDefinitionProperty(new StringParameterDefinition("FOO", "default-foo")));

        StringWriter writer = new StringWriter();
        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"runId\":\"run-undeclared\",\"intentKey\":\"manual:run-undeclared\""
                    + ",\"jobPath\":\"" + p.getFullName() + "\",\"parameters\":{\"FOO\":\"bar\",\"UNKNOWN\":\"x\"}}");
            StaplerResponse2 rsp = responseCapturing(writer);

            action.doTrigger(req, rsp);

            verify(rsp).setStatus(202);
        }

        JSONObject body = JSONObject.fromObject(writer.toString());
        assertEquals(1, body.getJSONArray("droppedParameters").size());
        assertEquals("UNKNOWN", body.getJSONArray("droppedParameters").getString(0));

        Queue.Item[] items = Queue.getInstance().getItems();
        ParametersAction pa = items[0].getAction(ParametersAction.class);
        assertNotNull(pa);
        assertEquals(1, pa.getParameters().size());
        assertEquals("bar", ((StringParameterValue) pa.getParameter("FOO")).getValue());
        assertNull("an undeclared parameter must never reach the build", pa.getParameter("UNKNOWN"));
    }

    @Test
    public void omittedParameterTakesTheDeclaredDefault() throws Exception {
        j.jenkins.setNumExecutors(0);
        FreeStyleProject p = j.createFreeStyleProject("trigger-default");
        p.addProperty(new ParametersDefinitionProperty(new StringParameterDefinition("BRANCH", "main")));

        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"runId\":\"run-default\",\"intentKey\":\"manual:run-default\""
                    + ",\"jobPath\":\"" + p.getFullName() + "\"}");
            StaplerResponse2 rsp = responseCapturing(new StringWriter());

            action.doTrigger(req, rsp);

            verify(rsp).setStatus(202);
        }

        Queue.Item[] items = Queue.getInstance().getItems();
        ParametersAction pa = items[0].getAction(ParametersAction.class);
        assertNotNull(pa);
        StringParameterValue value = (StringParameterValue) pa.getParameter("BRANCH");
        assertNotNull(value);
        assertEquals("main", value.getValue());
    }

    @Test
    public void aFolderTargetAnswers409() throws Exception {
        MockFolder folder = j.createFolder("trigger-folder");

        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"runId\":\"run-folder\",\"intentKey\":\"manual:run-folder\""
                    + ",\"jobPath\":\"" + folder.getFullName() + "\"}");
            StaplerResponse2 rsp = RefusalAssert.capturing();

            action.doTrigger(req, rsp);

            RefusalAssert.assertRefusedContaining(rsp, 409, "not a parameterized job");
        }
        assertEquals(0, Queue.getInstance().getItems().length);
    }

    @Test
    public void anUnknownJobAnswers404() throws Exception {
        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"runId\":\"run-missing\",\"intentKey\":\"manual:run-missing\""
                    + ",\"jobPath\":\"does-not-exist\"}");
            StaplerResponse2 rsp = RefusalAssert.capturing();

            action.doTrigger(req, rsp);

            RefusalAssert.assertRefusedContaining(rsp, 404, "does-not-exist");
        }
    }

    @Test
    public void unauthorizedTriggerCallerGets403() throws Exception {
        j.createFreeStyleProject("trigger-unauth");
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "bob", "", List.of(new SimpleGrantedAuthority("ROLE:varroa:developer"))))) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"runId\":\"run-x\",\"intentKey\":\"manual:run-x\""
                    + ",\"jobPath\":\"trigger-unauth\"}");
            StaplerResponse2 rsp = RefusalAssert.capturing();

            action.doTrigger(req, rsp);

            RefusalAssert.assertRefused(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
        }
        assertEquals(0, Queue.getInstance().getItems().length);
    }

    @Test
    public void theEndpointExposesOnlyItsNineRoutes() {
        int doMethods = 0;
        for (java.lang.reflect.Method m : VarroaRunTriggerAction.class.getDeclaredMethods()) {
            if (m.getName().startsWith("do") && java.lang.reflect.Modifier.isPublic(m.getModifiers())) {
                doMethods++;
                assertTrue("unexpected route method " + m.getName(),
                        m.getName().equals("doTrigger") || m.getName().equals("doCancel")
                                || m.getName().equals("doAdoptNode") || m.getName().equals("doLoadBuild")
                                || m.getName().equals("doProceedInput") || m.getName().equals("doAbortInput")
                                || m.getName().equals("doReplay") || m.getName().equals("doRestart")
                                || m.getName().equals("doScan"));
            }
        }
        assertEquals(9, doMethods);
    }

    // ---- D13 adopt-node route ----

    @Test
    public void adoptNodeUnauthorizedCallerGets403() throws Exception {
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "bob", "", List.of(new SimpleGrantedAuthority("ROLE:varroa:developer"))))) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"name\":\"agent-1\"}");
            StaplerResponse2 rsp = RefusalAssert.capturing();

            action.doAdoptNode(req, rsp);

            RefusalAssert.assertRefused(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
        }
    }

    @Test
    public void adoptNodeWithNoConfigurationFileGets404() throws Exception {
        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"name\":\"missing-agent\"}");
            StaplerResponse2 rsp = RefusalAssert.capturing();

            action.doAdoptNode(req, rsp);

            RefusalAssert.assertRefusedContaining(rsp, 404, "missing-agent");
        }
    }

    @Test
    public void adoptNodeAlreadyHeldGets409() throws Exception {
        j.createSlave("held-agent", "", new EnvVars());

        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"name\":\"held-agent\"}");
            StaplerResponse2 rsp = RefusalAssert.capturing();

            action.doAdoptNode(req, rsp);

            RefusalAssert.assertRefusedContaining(rsp, 409, "held-agent");
        }
    }

    @Test
    public void adoptNodeWithATraversingNameGets400() throws Exception {
        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"name\":\"../secrets\"}");
            StaplerResponse2 rsp = RefusalAssert.capturing();

            action.doAdoptNode(req, rsp);

            RefusalAssert.assertRefusedContaining(rsp, 400, "no usable node");
        }
    }

    // ---- Live finding L3: the load-build route ----

    @Test
    public void loadBuildUnauthorizedCallerGets403() throws Exception {
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "bob", "", List.of(new SimpleGrantedAuthority("ROLE:varroa:developer"))))) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"jobPath\":\"pipe\",\"buildNumber\":1}");
            StaplerResponse2 rsp = RefusalAssert.capturing();

            action.doLoadBuild(req, rsp);

            RefusalAssert.assertRefused(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
        }
    }

    @Test
    public void loadBuildWithNoJobAndBuildGets400() throws Exception {
        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"jobPath\":\"\",\"buildNumber\":0}");
            StaplerResponse2 rsp = RefusalAssert.capturing();

            action.doLoadBuild(req, rsp);

            RefusalAssert.assertRefusedContaining(rsp, 400, "no job and build");
        }
    }

    @Test
    public void loadBuildOfAnUnknownJobGets404() throws Exception {
        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"jobPath\":\"no-such-job\",\"buildNumber\":1}");
            StaplerResponse2 rsp = RefusalAssert.capturing();

            action.doLoadBuild(req, rsp);

            RefusalAssert.assertRefusedContaining(rsp, 404, "no-such-job");
        }
    }

    @Test
    public void loadBuildOfAnUnknownBuildGets404() throws Exception {
        FreeStyleProject p = j.createFreeStyleProject("load-missing-build");

        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody(
                    "{\"jobPath\":\"" + p.getFullName() + "\",\"buildNumber\":42}");
            StaplerResponse2 rsp = RefusalAssert.capturing();

            action.doLoadBuild(req, rsp);

            RefusalAssert.assertRefusedContaining(rsp, 404, "42");
        }
    }

    @Test
    public void loadBuildOfAFinishedBuildAnswersNotBuilding() throws Exception {
        FreeStyleProject p = j.createFreeStyleProject("load-finished");
        j.buildAndAssertSuccess(p);

        StringWriter body = new StringWriter();
        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody(
                    "{\"jobPath\":\"" + p.getFullName() + "\",\"buildNumber\":1}");
            StaplerResponse2 rsp = responseCapturing(body);

            action.doLoadBuild(req, rsp);

            verify(rsp).setStatus(200);
        }
        JSONObject answer = JSONObject.fromObject(body.toString());
        assertFalse("a finished build does not run", answer.getBoolean("building"));
        assertFalse("a finished build holds no place on the list", answer.getBoolean("registered"));
    }

    @Test
    public void loadBuildOfARunningPipelineAnswersBuildingAndRegistered() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "load-running");
        job.setDefinition(new CpsFlowDefinition("sleep 120\n", true));
        WorkflowRun run = job.scheduleBuild2(0).waitForStart();
        j.waitForMessage("Sleeping", run);

        StringWriter body = new StringWriter();
        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody(
                    "{\"jobPath\":\"" + job.getFullName() + "\",\"buildNumber\":"
                            + run.getNumber() + "}");
            StaplerResponse2 rsp = responseCapturing(body);

            action.doLoadBuild(req, rsp);

            verify(rsp).setStatus(200);
        }
        JSONObject answer = JSONObject.fromObject(body.toString());
        assertTrue("a running build runs", answer.getBoolean("building"));
        assertTrue("a running pipeline holds a place on the flow execution list",
                answer.getBoolean("registered"));

        run.doStop();
        j.waitForCompletion(run);
    }

    @Test
    public void probeOfARunningPipelineAnswersBuilding() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "probe-running");
        job.setDefinition(new CpsFlowDefinition("sleep 120\n", true));
        WorkflowRun run = job.scheduleBuild2(0).waitForStart();
        j.waitForMessage("Sleeping", run);

        StringWriter body = new StringWriter();
        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody(
                    "{\"jobPath\":\"" + job.getFullName() + "\",\"buildNumber\":"
                            + run.getNumber() + ",\"probe\":true}");
            StaplerResponse2 rsp = responseCapturing(body);

            action.doLoadBuild(req, rsp);

            verify(rsp).setStatus(200);
        }
        JSONObject running = JSONObject.fromObject(body.toString());
        assertTrue("the probe sees a running pipeline", running.getBoolean("building"));
        assertFalse("a running build has not finished", running.getBoolean("finished"));

        run.doStop();
        j.waitForCompletion(run);
    }

    @Test
    public void probeOfAFinishedBuildAnswersNotBuilding() throws Exception {
        FreeStyleProject p = j.createFreeStyleProject("probe-finished");
        j.buildAndAssertSuccess(p);

        StringWriter body = new StringWriter();
        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody(
                    "{\"jobPath\":\"" + p.getFullName() + "\",\"buildNumber\":1,\"probe\":true}");
            StaplerResponse2 rsp = responseCapturing(body);

            action.doLoadBuild(req, rsp);

            verify(rsp).setStatus(200);
        }
        JSONObject answer = JSONObject.fromObject(body.toString());
        assertFalse("a finished build does not run", answer.getBoolean("building"));
        // The build is loaded, because it just ran here. A resume of it
        // is a repeat of work this process already did.
        assertTrue("a loaded build that ended reads as finished", answer.getBoolean("finished"));
    }

    @Test
    public void probeOfAnAbsentBuildAnswersNotBuildingRatherThanMissing() throws Exception {
        FreeStyleProject p = j.createFreeStyleProject("probe-absent");

        StringWriter body = new StringWriter();
        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody(
                    "{\"jobPath\":\"" + p.getFullName() + "\",\"buildNumber\":42,\"probe\":true}");
            StaplerResponse2 rsp = responseCapturing(body);

            action.doLoadBuild(req, rsp);

            verify(rsp).setStatus(200);
        }
        JSONObject absent = JSONObject.fromObject(body.toString());
        assertFalse("a build that is not on the list does not run", absent.getBoolean("building"));
        assertFalse("a build this process never held has not finished here",
                absent.getBoolean("finished"));
    }

    // ---- Live finding L4: the served URL of each route ----

    /**
     * Posts to one {@code /varroa-run} route over HTTP and answers the
     * status. The caller holds no mite authority, so a mapped route
     * answers {@code 403} and an unmapped one answers {@code 404}.
     */
    private int postToRoute(String route) throws Exception {
        // The crumb filter answers 403 before Stapler routes, which
        // would hide an unmapped route behind the same status.
        j.jenkins.setCrumbIssuer(null);
        JenkinsRule.WebClient wc = j.createWebClient();
        wc.setThrowExceptionOnFailingStatusCode(false);
        WebRequest request = new WebRequest(new URL(j.getURL(), "varroa-run/" + route), HttpMethod.POST);
        request.setAdditionalHeader("Content-Type", "application/json");
        request.setRequestBody("{}");
        return wc.getPage(request).getWebResponse().getStatusCode();
    }

    @Test
    public void everyRouteIsServedAtTheNameItsCallersUse() throws Exception {
        // Stapler derives a URL token from the method name and writes no
        // dash, so a route of more than one word needs @WebMethod. The
        // mite posts these names and nothing else.
        for (String route : List.of("trigger", "cancel", "adopt-node", "load-build", "replay")) {
            assertNotEquals("route " + route + " is not served at that name",
                    404, postToRoute(route));
        }
    }

    // ---- 2.6 cancel route ----

    @Test
    public void queuedItemIsCancelledByRunIdSecondStaysQueued() throws Exception {
        j.jenkins.setNumExecutors(0);
        FreeStyleProject p = j.createFreeStyleProject("cancel-queued");

        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            action.doTrigger(requestWithBody("{\"runId\":\"run-cancel-a\",\"intentKey\":\"manual:run-cancel-a\""
                    + ",\"jobPath\":\"" + p.getFullName() + "\"}"), responseCapturing(new StringWriter()));
            action.doTrigger(requestWithBody("{\"runId\":\"run-cancel-b\",\"intentKey\":\"manual:run-cancel-b\""
                    + ",\"jobPath\":\"" + p.getFullName() + "\"}"), responseCapturing(new StringWriter()));
            assertEquals(2, Queue.getInstance().getItems().length);

            StaplerResponse2 rsp = mock(StaplerResponse2.class);
            action.doCancel(requestWithBody("{\"runId\":\"run-cancel-a\",\"jobPath\":\"" + p.getFullName()
                    + "\",\"buildNumber\":0}"), rsp);

            verify(rsp).setStatus(202);
        }

        Queue.Item[] remaining = Queue.getInstance().getItems();
        assertEquals(1, remaining.length);
        VarroaIntentAction remainingAction = remaining[0].getAction(VarroaIntentAction.class);
        assertNotNull(remainingAction);
        assertEquals("run-cancel-b", remainingAction.getRunId());
    }

    @Test
    public void buildNumberMismatchStopsNothingAndAnswers404() throws Exception {
        j.jenkins.setNumExecutors(1);
        FreeStyleProject p = j.createFreeStyleProject("cancel-mismatch");
        CountDownLatch started = new CountDownLatch(1);
        CountDownLatch release = new CountDownLatch(1);
        p.getBuildersList().add(new TestBuilder() {
            @Override
            public boolean perform(AbstractBuild<?, ?> build, Launcher launcher, BuildListener listener)
                    throws InterruptedException {
                started.countDown();
                release.await(10, TimeUnit.SECONDS);
                return true;
            }
        });

        FreeStyleBuild build;
        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            action.doTrigger(requestWithBody("{\"runId\":\"run-real\",\"intentKey\":\"manual:run-real\""
                    + ",\"jobPath\":\"" + p.getFullName() + "\"}"), responseCapturing(new StringWriter()));

            assertTrue("build must start within 10s", started.await(10, TimeUnit.SECONDS));
            build = p.getLastBuild();
            assertNotNull(build);
            assertTrue(build.isBuilding());

            StaplerResponse2 rsp = RefusalAssert.capturing();
            action.doCancel(requestWithBody("{\"runId\":\"run-wrong\",\"jobPath\":\"" + p.getFullName()
                    + "\",\"buildNumber\":" + build.getNumber() + "}"), rsp);

            RefusalAssert.assertRefusedContaining(rsp, 404, "run-wrong");
            assertTrue("a mismatched cancel must not stop the build", build.isBuilding());
        } finally {
            release.countDown();
        }
        j.waitForCompletion(build);
    }

    @Test
    public void unauthorizedCancelCallerGets403() throws Exception {
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "bob", "", List.of(new SimpleGrantedAuthority("ROLE:varroa:developer"))))) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"runId\":\"run-y\",\"jobPath\":\"any\",\"buildNumber\":0}");
            StaplerResponse2 rsp = RefusalAssert.capturing();

            action.doCancel(req, rsp);

            RefusalAssert.assertRefused(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
        }
    }

    @Test
    public void anUnmatchedRunIdAnswers404() throws Exception {
        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"runId\":\"run-nowhere\",\"jobPath\":\"any\",\"buildNumber\":0}");
            StaplerResponse2 rsp = RefusalAssert.capturing();

            action.doCancel(req, rsp);

            RefusalAssert.assertRefusedContaining(rsp, 404, "run-nowhere");
        }
    }

    // ---- The redelivery guard ----

    private static StaplerResponse2 trigger(String runId, String jobPath, StringWriter out) throws Exception {
        try (ACLContext ctx = asMite()) {
            StaplerResponse2 rsp = responseCapturing(out);
            new VarroaRunTriggerAction().doTrigger(requestWithBody("{\"runId\":\"" + runId
                    + "\",\"intentKey\":\"manual:" + runId + "\",\"jobPath\":\"" + jobPath
                    + "\",\"parameters\":{\"EXTRA\":\"x\"}}"), rsp);
            return rsp;
        }
    }

    @Test
    public void aTriggerRepeatedAfterTheBuildFinishedSchedulesNothing() throws Exception {
        j.jenkins.setNumExecutors(1);
        FreeStyleProject p = j.createFreeStyleProject("repeat-finished");
        String runId = Uuid7.next();

        verify(trigger(runId, p.getFullName(), new StringWriter())).setStatus(202);
        j.waitUntilNoActivity();
        FreeStyleBuild first = p.getLastBuild();
        assertNotNull("the first trigger must build", first);
        assertEquals(runId, first.getAction(VarroaIntentAction.class).getRunId());

        StringWriter out = new StringWriter();
        StaplerResponse2 rsp = trigger(runId, p.getFullName(), out);

        verify(rsp).setStatus(202);
        JSONObject answer = JSONObject.fromObject(out.toString());
        assertEquals(runId, answer.getString("runId"));
        assertEquals(List.of("EXTRA"), new ArrayList<Object>(answer.getJSONArray("droppedParameters")));
        assertEquals("a repeat must queue nothing", 0, Queue.getInstance().getItems().length);
        j.waitUntilNoActivity();
        assertEquals("a repeat must start no second build", 1, p.getLastBuild().getNumber());
        assertEquals(2, p.getNextBuildNumber());
    }

    @Test
    public void aTriggerRepeatedWhileTheBuildRunsSchedulesNothing() throws Exception {
        j.jenkins.setNumExecutors(2);
        FreeStyleProject p = j.createFreeStyleProject("repeat-running");
        p.setConcurrentBuild(true);
        CountDownLatch started = new CountDownLatch(1);
        CountDownLatch release = new CountDownLatch(1);
        p.getBuildersList().add(new TestBuilder() {
            @Override
            public boolean perform(AbstractBuild<?, ?> build, Launcher launcher, BuildListener listener)
                    throws InterruptedException {
                started.countDown();
                release.await(30, TimeUnit.SECONDS);
                return true;
            }
        });
        String runId = Uuid7.next();

        FreeStyleBuild build;
        try {
            verify(trigger(runId, p.getFullName(), new StringWriter())).setStatus(202);
            assertTrue("the build must start within 10s", started.await(10, TimeUnit.SECONDS));
            build = p.getLastBuild();
            assertTrue(build.isBuilding());

            StaplerResponse2 rsp = trigger(runId, p.getFullName(), new StringWriter());

            verify(rsp).setStatus(202);
            assertEquals("a repeat must queue nothing", 0, Queue.getInstance().getItems().length);
        } finally {
            release.countDown();
        }
        j.assertBuildStatusSuccess(j.waitForCompletion(build));
        j.waitUntilNoActivity();
        assertEquals("a repeat must start no second build", 1, p.getLastBuild().getNumber());
    }

    @Test
    public void aBuildOlderThanTheTimeBoundIsNotAMatch() throws Exception {
        j.jenkins.setNumExecutors(1);
        FreeStyleProject p = j.createFreeStyleProject("repeat-bound");
        // A run id minted ten minutes after the build: the walk stops at the
        // build, because the build is older than the id's time less the margin.
        String future = Uuid7.next(System.currentTimeMillis() + 10L * 60 * 1000, new java.security.SecureRandom());
        String current = Uuid7.next();

        verify(trigger(future, p.getFullName(), new StringWriter())).setStatus(202);
        j.waitUntilNoActivity();
        assertNotNull(p.getLastBuild());
        assertNull("a build older than the bound must not match",
                VarroaRunTriggerAction.findBuildForRunId(p, future));

        verify(trigger(current, p.getFullName(), new StringWriter())).setStatus(202);
        j.waitUntilNoActivity();
        assertEquals(2, p.getLastBuild().getNumber());
        assertEquals("a build inside the bound must match", p.getLastBuild(),
                VarroaRunTriggerAction.findBuildForRunId(p, current));
        assertNull("another run id must not match",
                VarroaRunTriggerAction.findBuildForRunId(p, Uuid7.next()));
    }

    @Test
    public void aRunIdReadsBackItsOwnTime() {
        long millis = 1_758_000_000_000L;
        assertEquals(millis, Uuid7.timestampMillis(Uuid7.next(millis, new java.security.SecureRandom())));
        assertEquals(-1L, Uuid7.timestampMillis("run-real"));
        assertEquals(-1L, Uuid7.timestampMillis("00000000-0000-4000-8000-000000000000"));
    }
}
