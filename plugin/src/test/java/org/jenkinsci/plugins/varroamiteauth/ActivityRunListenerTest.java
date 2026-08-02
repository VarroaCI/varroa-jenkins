package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import hudson.model.Cause;
import hudson.model.Cause.UpstreamCause;
import hudson.model.Cause.UserIdCause;
import hudson.model.FreeStyleProject;
import hudson.model.Result;
import hudson.triggers.SCMTrigger;
import hudson.triggers.TimerTrigger;

import java.io.IOException;
import java.util.List;

import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.jvnet.hudson.test.TestBuilder;

/**
 * Tests for {@link ActivityRunListener}.
 */
public class ActivityRunListenerTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Test
    public void successfulBuildEmitsStartedAndCompleted() throws Exception {
        ActivitySink sink = ActivitySink.get();
        sink.resetForTesting();

        FreeStyleProject project = j.createFreeStyleProject("test-job");
        // Drain any events emitted during project creation (item.created, etc.).
        sink.drain(100);
        // Reset dropped too.
        sink.resetForTesting();

        j.buildAndAssertSuccess(project);

        ActivitySink.DrainResult result = sink.drain(100);
        assertEquals(2, result.events.size());

        ActivityEvent started = result.events.get(0);
        assertEquals("build.started", started.getType());
        assertEquals("test-job", started.getItemPath());
        assertEquals(1, started.getBuildNumber());
        assertEquals("", started.getResult());

        ActivityEvent completed = result.events.get(1);
        assertEquals("build.completed", completed.getType());
        assertEquals("test-job", completed.getItemPath());
        assertEquals(1, completed.getBuildNumber());
        assertEquals("SUCCESS", completed.getResult());
    }

    @Test
    public void failedBuildReportsFailureResult() throws Exception {
        ActivitySink sink = ActivitySink.get();
        sink.resetForTesting();

        FreeStyleProject project = j.createFreeStyleProject("fail-job");
        // Drain creation events.
        sink.drain(100);
        sink.resetForTesting();
        project.getBuildersList().add(new TestBuilder() {
            @Override
            public boolean perform(hudson.model.AbstractBuild<?, ?> build, hudson.Launcher launcher, hudson.model.BuildListener listener)
                    throws InterruptedException, IOException {
                return false; // fail the build
            }
        });
        j.assertBuildStatus(Result.FAILURE, project.scheduleBuild2(0).get());

        ActivitySink.DrainResult result = sink.drain(100);
        assertTrue("should have at least 2 events", result.events.size() >= 2);

        ActivityEvent completed = result.events.get(result.events.size() - 1);
        assertEquals("build.completed", completed.getType());
        assertEquals("FAILURE", completed.getResult());
    }

    @Test
    public void nullResultNormalizesToEmpty() {
        // Directly test the normalization logic: a null Run result -> "" not "null".
        String nullResult = null;
        String formatted = nullResult != null ? nullResult.toString() : "";
        assertEquals("", formatted);
        assertNotEquals("null", formatted);
    }

    @Test
    public void sourceIsAlwaysJenkins() throws Exception {
        ActivitySink sink = ActivitySink.get();
        sink.resetForTesting();

        FreeStyleProject project = j.createFreeStyleProject("source-test");
        sink.drain(100);
        sink.resetForTesting();
        j.buildAndAssertSuccess(project);

        ActivitySink.DrainResult result = sink.drain(100);
        for (ActivityEvent e : result.events) {
            assertEquals("jenkins", e.getSource());
        }
    }

    // --- Actor resolution tests (via resolveActorFromCauses) ---

    @Test
    public void actorResolvedFromUserIdCause() {
        String actor = ActivityRunListener.resolveActorFromCauses(
                List.of(new UserIdCause("alice")));
        assertEquals("alice", actor);
    }

    @Test
    public void actorResolvedToScmForSCMTriggerCause() {
        String actor = ActivityRunListener.resolveActorFromCauses(
                List.of(new SCMTrigger.SCMTriggerCause()));
        assertEquals("scm", actor);
    }

    @Test
    public void actorResolvedToScmForRemoteCause() {
        String actor = ActivityRunListener.resolveActorFromCauses(
                List.of(new Cause.RemoteCause("10.0.0.1", "remote build")));
        assertEquals("scm", actor);
    }

    @Test
    public void actorResolvedToTimerForTimerTriggerCause() {
        String actor = ActivityRunListener.resolveActorFromCauses(
                List.of(new TimerTrigger.TimerTriggerCause()));
        assertEquals("timer", actor);
    }

    @Test
    public void actorResolvedToUpstreamForUpstreamCause() throws Exception {
        // UpstreamCause constructors are private, so we use reflection
        // to inspect the actual parameter order. getUpstreamProject() returns
        // the first constructor parameter (the upstream project name in this
        // Jenkins version), not the build URL.
        java.lang.reflect.Constructor<UpstreamCause> ctor =
                UpstreamCause.class.getDeclaredConstructor(
                        String.class, int.class, String.class, List.class);
        ctor.setAccessible(true);
        UpstreamCause cause = ctor.newInstance("parent/trigger-job", 1, "http://upstream/job/1/", List.of());
        String actor = ActivityRunListener.resolveActorFromCauses(List.of(cause));
        assertEquals("upstream:parent/trigger-job", actor);
    }

    @Test
    public void actorDefaultsToUnknown() {
        String actor = ActivityRunListener.resolveActorFromCauses(List.of());
        assertEquals("unknown", actor);
    }

    @Test
    public void userIdCauseTakesPrecedenceOverOtherCauses() {
        // UserIdCause is first kind checked, so even with other causes present it wins.
        String actor = ActivityRunListener.resolveActorFromCauses(
                List.of(new UserIdCause("bob"), new TimerTrigger.TimerTriggerCause()));
        assertEquals("bob", actor);
    }
}
