package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.contains;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

import hudson.Launcher;
import hudson.model.AbstractBuild;
import hudson.model.BuildListener;
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
import java.io.PrintWriter;
import java.io.StringReader;
import java.io.StringWriter;
import java.util.List;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;

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
        StaplerResponse2 rsp = mock(StaplerResponse2.class);
        when(rsp.getWriter()).thenReturn(new PrintWriter(writer));
        return rsp;
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
            StaplerResponse2 rsp = mock(StaplerResponse2.class);

            action.doTrigger(req, rsp);

            verify(rsp).sendError(eq(409), contains("not a parameterized job"));
        }
        assertEquals(0, Queue.getInstance().getItems().length);
    }

    @Test
    public void anUnknownJobAnswers404() throws Exception {
        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"runId\":\"run-missing\",\"intentKey\":\"manual:run-missing\""
                    + ",\"jobPath\":\"does-not-exist\"}");
            StaplerResponse2 rsp = mock(StaplerResponse2.class);

            action.doTrigger(req, rsp);

            verify(rsp).sendError(eq(404), contains("does-not-exist"));
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
            StaplerResponse2 rsp = mock(StaplerResponse2.class);

            action.doTrigger(req, rsp);

            verify(rsp).sendError(403, "Forbidden: requires ROLE:varroa:system-mite authority");
        }
        assertEquals(0, Queue.getInstance().getItems().length);
    }

    @Test
    public void theEndpointExposesOnlyTriggerAndCancel() {
        int doMethods = 0;
        for (java.lang.reflect.Method m : VarroaRunTriggerAction.class.getDeclaredMethods()) {
            if (m.getName().startsWith("do") && java.lang.reflect.Modifier.isPublic(m.getModifiers())) {
                doMethods++;
                assertTrue("unexpected route method " + m.getName(),
                        m.getName().equals("doTrigger") || m.getName().equals("doCancel"));
            }
        }
        assertEquals(2, doMethods);
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

            StaplerResponse2 rsp = mock(StaplerResponse2.class);
            action.doCancel(requestWithBody("{\"runId\":\"run-wrong\",\"jobPath\":\"" + p.getFullName()
                    + "\",\"buildNumber\":" + build.getNumber() + "}"), rsp);

            verify(rsp).sendError(eq(404), contains("run-wrong"));
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
            StaplerResponse2 rsp = mock(StaplerResponse2.class);

            action.doCancel(req, rsp);

            verify(rsp).sendError(403, "Forbidden: requires ROLE:varroa:system-mite authority");
        }
    }

    @Test
    public void anUnmatchedRunIdAnswers404() throws Exception {
        try (ACLContext ctx = asMite()) {
            VarroaRunTriggerAction action = new VarroaRunTriggerAction();
            StaplerRequest2 req = requestWithBody("{\"runId\":\"run-nowhere\",\"jobPath\":\"any\",\"buildNumber\":0}");
            StaplerResponse2 rsp = mock(StaplerResponse2.class);

            action.doCancel(req, rsp);

            verify(rsp).sendError(eq(404), contains("run-nowhere"));
        }
    }
}
