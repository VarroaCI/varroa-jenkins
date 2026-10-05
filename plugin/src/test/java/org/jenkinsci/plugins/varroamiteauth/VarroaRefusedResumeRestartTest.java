package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import java.io.IOException;
import java.lang.reflect.Field;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.attribute.FileTime;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.TimeUnit;

import net.sf.json.JSONObject;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.After;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.jvnet.hudson.test.JenkinsSessionRule;

/**
 * Proves that a pipeline build that Jenkins refuses to resume after a
 * restart gets exactly one {@code COMPLETED} event, with the log reason
 * {@code jenkins-restart} and the journal's last-seen time as its end.
 *
 * <p>workflow-job refuses to resume a build older than its maximum
 * resumption age. The load marks the build completed with no result, and
 * a timer thread calls {@code WorkflowRun.finish} later, which fires the
 * run listener. The boot reconcile can thus end the build too.
 *
 * <p>The age is {@code null} in a unit test JVM, so each test sets it to
 * zero between the two sessions. The first session clears the recorder's
 * static state, because the second session runs in the same JVM and must
 * start like a new process.
 */
public class VarroaRefusedResumeRestartTest {

    private static final String JOB = "refused-resume";

    @Rule
    public JenkinsSessionRule sessions = new JenkinsSessionRule();

    @After
    public void clearOverrides() throws Exception {
        setMaxResumptionAge(null);
        VarroaRunRecorder.setGateOverrideForTesting(null);
        VarroaBootReconcile.setSkipAtBootForTesting(false);
        VarroaRunRecorder.resetStateForTesting();
        BuildEventSink.get().resetForTesting();
    }

    @Test
    public void aRefusedResumeEndedByTheBootReconcileSendsOneCompleted() throws Throwable {
        String runId = Uuid7.next();
        long[] startedAt = new long[1];
        sessions.then(j -> startedAt[0] = startSleepingBuild(j, runId));
        long lastSeen = pinLastSeen(runId, startedAt[0]);
        setMaxResumptionAge(Duration.ZERO);
        sessions.then(j -> {
            WorkflowRun run = awaitFinish(j);
            assertEquals("ABORTED", String.valueOf(run.getResult()));
            assertTrue("the build counts as ended by the restart", FailedProgramLoad.endedByRestart(run));

            assertTrue("the run listener sends no second completion", completedInSink(runId).isEmpty());
            List<JSONObject> spooled = completedInSpool(j, runId);
            assertEquals("the boot reconcile spools the only completion", 1, spooled.size());
            assertRestartCompletion(spooled.get(0), runId, lastSeen);
            assertTrue("the spool write deletes the entry", new RunJournal().readAll().isEmpty());
        });
    }

    @Test
    public void aRefusedResumeEndedByTheListenerFirstReportsJenkinsRestart() throws Throwable {
        String runId = Uuid7.next();
        long[] startedAt = new long[1];
        sessions.then(j -> startedAt[0] = startSleepingBuild(j, runId));
        long lastSeen = pinLastSeen(runId, startedAt[0]);
        setMaxResumptionAge(Duration.ZERO);
        // The pipeline resume of the next boot loads the build, so the run
        // listener ends it before any boot reconcile runs.
        VarroaBootReconcile.setSkipAtBootForTesting(true);
        sessions.then(j -> {
            WorkflowRun run = awaitFinish(j);
            assertTrue(FailedProgramLoad.endedByRestart(run));

            List<BuildEvent> completed = completedInSink(runId);
            assertEquals("the run listener sends one completion", 1, completed.size());
            assertRestartCompletion(completed.get(0).toJson(), runId, lastSeen);

            VarroaBootReconcile.setSkipAtBootForTesting(false);
            assertEquals("a later boot reconcile sends nothing", 0,
                    VarroaBootReconcile.reconcile(new RunJournal(), new RunSpool(), System.currentTimeMillis()));
            assertTrue(completedInSpool(j, runId).isEmpty());
            assertEquals("the entry stays until the listener's event is on disk", 1,
                    new RunJournal().readAll().size());
        });
    }

    /**
     * Sets workflow-job's maximum resumption age. The field is package
     * private and holds {@code null} in a unit test JVM.
     */
    private static void setMaxResumptionAge(Duration age) throws Exception {
        Field field = WorkflowRun.class.getDeclaredField("MAX_RESUMPTION_AGE");
        field.setAccessible(true);
        field.set(null, age);
    }

    /**
     * Starts one build that is sleeping when the session ends, and returns
     * its start time. The session's clean stop suspends the build for a
     * resume.
     */
    private static long startSleepingBuild(JenkinsRule j, String runId) throws Exception {
        VarroaRunRecorder.setGateOverrideForTesting("1");
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, JOB);
        job.setDefinition(new CpsFlowDefinition("sleep 600\n", true));
        VarroaIntentAction action = new VarroaIntentAction(runId, "", null, System.currentTimeMillis());
        WorkflowRun run = job.scheduleBuild2(0, action).waitForStart();
        j.waitForMessage("Sleeping for", run);
        assertNotNull("the run listener writes the journal entry", new RunJournal().read(runId));
        VarroaRunRecorder.resetStateForTesting();
        BuildEventSink.get().resetForTesting();
        return run.getStartTimeInMillis();
    }

    /**
     * Sets the journal entry's last-seen time while no Jenkins runs, and
     * returns it.
     */
    private long pinLastSeen(String runId, long startedAt) throws IOException {
        long lastSeen = (startedAt + 5_000L) / 1_000L * 1_000L;
        Path entry = sessions.getHome().toPath().resolve("varroa").resolve("live").resolve(runId + ".json");
        Files.setLastModifiedTime(entry, FileTime.fromMillis(lastSeen));
        return lastSeen;
    }

    /**
     * Waits until {@code WorkflowRun.finish} has run for build 1. It sets
     * a duration and drops the build listener after the run listener
     * returns.
     */
    private static WorkflowRun awaitFinish(JenkinsRule j) throws Exception {
        WorkflowRun run = j.jenkins.getItemByFullName(JOB, WorkflowJob.class).getBuildByNumber(1);
        assertNotNull(run);
        long deadline = System.currentTimeMillis() + TimeUnit.SECONDS.toMillis(60);
        while ((run.isBuilding() || run.isLogUpdated() || run.getDuration() == 0)
                && System.currentTimeMillis() < deadline) {
            Thread.sleep(100);
        }
        assertFalse("the build ends", run.isBuilding());
        return run;
    }

    private static void assertRestartCompletion(JSONObject event, String runId, long lastSeen) {
        assertEquals(runId, event.getString("run_id"));
        assertEquals("ABORTED", event.getString("result"));
        assertEquals("the build ends at the journal's last-seen time", lastSeen, event.getLong("completed_at_millis"));
        JSONObject summary = event.getJSONObject("log_summary");
        assertTrue(summary.getBoolean("incomplete"));
        assertEquals(LogSummary.REASON_JENKINS_RESTART, summary.getString("incomplete_reason"));
    }

    private static List<BuildEvent> completedInSink(String runId) {
        List<BuildEvent> out = new ArrayList<>();
        for (BuildEvent e : BuildEventSink.get().drain(1000).events) {
            if (BuildEvent.PHASE_COMPLETED.equals(e.getPhase()) && runId.equals(e.getRunId())) {
                out.add(e);
            }
        }
        return out;
    }

    private static List<JSONObject> completedInSpool(JenkinsRule j, String runId) {
        Path spoolDir = j.jenkins.getRootDir().toPath().resolve("varroa").resolve("spool");
        List<JSONObject> out = new ArrayList<>();
        for (RunSpool.Entry e : new RunSpool(spoolDir, RunSpool.DEFAULT_CAP_BYTES).readOrdered(runId)) {
            if (!e.runState && BuildEvent.PHASE_COMPLETED.equals(e.phase)) {
                out.add(e.toJsonObject());
            }
        }
        return out;
    }
}
