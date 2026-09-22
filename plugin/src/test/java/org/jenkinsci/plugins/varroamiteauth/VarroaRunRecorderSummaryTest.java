package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertTrue;

import hudson.model.Executor;

import java.util.ArrayList;
import java.util.List;

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
 * Tests for the two carrier phases that report a run's summary: the
 * heartbeat, the completion, and the rehydration of a build that resumes
 * in another process.
 */
public class VarroaRunRecorderSummaryTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private int jobCounter;

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

    /**
     * Starts {@code script} with a granted claim and returns the running
     * build without waiting for it.
     */
    private WorkflowRun start(String script) throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "summary-" + ++jobCounter);
        job.setDefinition(new CpsFlowDefinition(script, true));
        return (WorkflowRun) job.scheduleBuild2(0,
                new VarroaIntentAction(Uuid7.next(), "", null, System.currentTimeMillis())).waitForStart();
    }

    /** Stops a run this test left building, and waits for its end. */
    private void stop(WorkflowRun run) throws Exception {
        Executor executor = run.getExecutor();
        if (executor != null) {
            executor.interrupt();
        } else {
            run.doStop();
        }
        j.waitForCompletion(run);
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
    public void aHeartbeatCarriesTheSummaryOnceThePipelineSetsIt() throws Exception {
        WorkflowRun run = start("varroa.summary('deployed canary')\necho 'SUM_SET'\nsleep 120\n");
        try {
            j.waitForMessage("SUM_SET", run);
            BuildEventSink.get().drain(100);

            VarroaRunRecorder.heartbeatTick();

            List<BuildEvent> beats = byPhase(BuildEventSink.get().drain(100).events, BuildEvent.PHASE_HEARTBEAT);
            assertEquals(1, beats.size());
            assertEquals("deployed canary", beats.get(0).getSummary());
        } finally {
            stop(run);
        }
    }

    @Test
    public void aHeartbeatBeforeTheSummaryCarriesNoSummaryKey() throws Exception {
        WorkflowRun run = start("echo 'BEFORE_SUM'\nsleep 120\n");
        try {
            j.waitForMessage("BEFORE_SUM", run);
            BuildEventSink.get().drain(100);

            VarroaRunRecorder.heartbeatTick();

            List<BuildEvent> beats = byPhase(BuildEventSink.get().drain(100).events, BuildEvent.PHASE_HEARTBEAT);
            assertEquals(1, beats.size());
            assertEquals("", beats.get(0).getSummary());
            assertFalse("a heartbeat with no summary writes no key", beats.get(0).toJson().containsKey("summary"));
        } finally {
            stop(run);
        }
    }

    @Test
    public void aCompletionCarriesThePersistedSummaryWithNoHeartbeat() throws Exception {
        WorkflowRun run = start("varroa.summary('finished late')\necho 'SUM_SET'\n");

        j.assertBuildStatusSuccess(j.waitForCompletion(run));

        List<BuildEvent> events = BuildEventSink.get().drain(100).events;
        assertTrue("the run emits no heartbeat of its own", byPhase(events, BuildEvent.PHASE_HEARTBEAT).isEmpty());
        List<BuildEvent> completed = byPhase(events, BuildEvent.PHASE_COMPLETED);
        assertEquals(1, completed.size());
        assertEquals("finished late", completed.get(0).getSummary());
    }

    @Test
    public void aResumedBuildRehydratesTheSummaryBeforeItsFirstHeartbeat() throws Exception {
        WorkflowRun run = start("varroa.summary('resume summary')\necho 'SUM_SET'\nsleep 120\n");
        try {
            j.waitForMessage("SUM_SET", run);
            assertNotNull("the persisted action holds the text", run.getAction(VarroaSummaryAction.class));

            // The process that set the summary is gone, so its per-run
            // state is gone too. The run's own action still holds the
            // text, and the new process reads it from there.
            VarroaRunRecorder.resetStateForTesting();
            BuildEventSink.get().resetForTesting();

            long before = System.currentTimeMillis();
            FlowExecutionListener.fireResumed(run.getExecution());

            List<BuildEvent> beats = byPhase(BuildEventSink.get().drain(100).events, BuildEvent.PHASE_HEARTBEAT);
            assertEquals("the resume sends its first heartbeat at once", 1, beats.size());
            assertEquals("resume summary", beats.get(0).getSummary());
            assertEquals(run.getAction(VarroaIntentAction.class).getRunId(), beats.get(0).getRunId());
            assertTrue("the resumed counter starts above the wall clock",
                    beats.get(0).getSeq() >= before);
        } finally {
            stop(run);
        }
    }

    @Test
    public void aCompletionWithNoLiveStateCarriesTheSummaryAndOrdersLast() throws Exception {
        WorkflowRun run = start("varroa.summary('late summary')\necho 'SUM_SET'\nsleep 120\n");
        try {
            j.waitForMessage("SUM_SET", run);
            assertNotNull("the persisted action holds the text", run.getAction(VarroaSummaryAction.class));

            // The process that watched this build is gone, so its per-run
            // state is gone too, and no resume rebuilds one. The run's own
            // action still holds the text, and the completion reads it there.
            long before = System.currentTimeMillis();
            VarroaRunRecorder.resetStateForTesting();
            BuildEventSink.get().resetForTesting();

            stop(run);

            List<BuildEvent> completed =
                    byPhase(BuildEventSink.get().drain(100).events, BuildEvent.PHASE_COMPLETED);
            assertEquals(1, completed.size());
            assertTrue("a completion with no live state orders after the wall clock it read",
                    completed.get(0).getSeq() >= before);
            assertEquals("late summary", completed.get(0).getSummary());
        } finally {
            if (run.isBuilding()) {
                stop(run);
            }
        }
    }

    @Test
    public void aRunThatNeverSetsASummaryEmitsNoSummaryField() throws Exception {
        WorkflowRun run = start("echo 'NO_SUM'\n");

        j.assertBuildStatusSuccess(j.waitForCompletion(run));

        List<BuildEvent> events = BuildEventSink.get().drain(100).events;
        assertFalse("the run emits at least one event", events.isEmpty());
        for (BuildEvent e : events) {
            assertEquals("", e.getSummary());
            assertFalse(e.getPhase() + " carries no summary key", e.toJson().containsKey("summary"));
        }
    }
}
