package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import com.google.common.util.concurrent.ListenableFuture;
import com.google.common.util.concurrent.SettableFuture;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.attribute.FileTime;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.TimeUnit;

import jenkins.util.Timer;
import net.sf.json.JSONObject;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.flow.FlowExecutionOwner;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.jenkinsci.plugins.workflow.pickles.Pickle;
import org.jenkinsci.plugins.workflow.pickles.PickleFactory;
import org.junit.After;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.jvnet.hudson.test.JenkinsSessionRule;
import org.jvnet.hudson.test.TestExtension;

/**
 * Proves that a pipeline build whose program does not load after a
 * Jenkins restart gets exactly one {@code COMPLETED} event, with the log
 * reason {@code jenkins-restart} and the journal's last-seen time as its
 * end. Jenkins ends such a build twice: the boot reconcile ends it, and
 * {@code WorkflowRun.finish} fires the run listener about one second
 * after the failed load.
 *
 * <p>Each test runs one build in a first session and leaves its journal
 * entry. The first session clears the recorder's static state, because
 * the second session runs in the same JVM and must start like a new
 * process.
 */
public class VarroaFailedLoadRestartTest {

    private static final String JOB = "failed-load";

    @Rule
    public JenkinsSessionRule sessions = new JenkinsSessionRule();

    @After
    public void clearOverrides() {
        VarroaRunRecorder.setGateOverrideForTesting(null);
        VarroaBootReconcile.setSkipAtBootForTesting(false);
        VarroaRunRecorder.resetStateForTesting();
        BuildEventSink.get().resetForTesting();
    }

    @Test
    public void aFailedLoadEndedByTheBootReconcileSendsOneCompleted() throws Throwable {
        String runId = Uuid7.next();
        long[] startedAt = new long[1];
        sessions.then(j -> startedAt[0] = startSleepingBuild(j, runId, "sleep 600\n", true));
        long lastSeen = pinLastSeen(runId, startedAt[0]);
        blockResume();
        sessions.then(j -> {
            WorkflowRun run = awaitFinish(j);
            assertTrue("the end node carries the workflow-cps load failure", FailedProgramLoad.matches(run));

            assertTrue("the run listener sends no second completion", completedInSink(runId).isEmpty());
            List<JSONObject> spooled = completedInSpool(j, runId);
            assertEquals("the boot reconcile spools the only completion", 1, spooled.size());
            assertRestartCompletion(spooled.get(0), runId, lastSeen);
            assertTrue("the spool write deletes the entry", new RunJournal().readAll().isEmpty());
        });
    }

    @Test
    public void aFailedLoadEndedByTheListenerFirstReportsJenkinsRestart() throws Throwable {
        String runId = Uuid7.next();
        long[] startedAt = new long[1];
        sessions.then(j -> startedAt[0] = startSleepingBuild(j, runId, "sleep 600\n", true));
        long lastSeen = pinLastSeen(runId, startedAt[0]);
        blockResume();
        // The pipeline resume of the next boot loads the build, so the run
        // listener ends it before any boot reconcile runs.
        VarroaBootReconcile.setSkipAtBootForTesting(true);
        sessions.then(j -> {
            WorkflowRun run = awaitFinish(j);
            assertTrue(FailedProgramLoad.matches(run));

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

    @Test
    public void anAsyncFailedLoadReportsJenkinsRestartFromTheListener() throws Throwable {
        String runId = Uuid7.next();
        sessions.then(j -> startSleepingBuild(j, runId,
                "def marker = new org.jenkinsci.plugins.varroamiteauth.VarroaFailedLoadRestartTest.Marker()\n"
                        + "sleep 600\n"
                        + "echo \"${marker}\"\n", false));
        sessions.then(j -> {
            WorkflowRun run = awaitFinish(j);
            assertTrue(FailedProgramLoad.matches(run));

            assertTrue("the boot reconcile keeps the entry of a building build", completedInSpool(j, runId).isEmpty());
            List<BuildEvent> completed = completedInSink(runId);
            assertEquals("the run listener sends one completion", 1, completed.size());
            JSONObject event = completed.get(0).toJson();
            assertEquals(runId, event.getString("run_id"));
            assertEquals("FAILURE", event.getString("result"));
            JSONObject summary = event.getJSONObject("log_summary");
            assertTrue(summary.getBoolean("incomplete"));
            assertEquals(LogSummary.REASON_JENKINS_RESTART, summary.getString("incomplete_reason"));
            assertTrue("the build ends at or after its start",
                    event.getLong("completed_at_millis") >= run.getStartTimeInMillis());
        });
    }

    @Test
    public void aResumedPipelineKeepsItsOwnLogSummary() throws Throwable {
        String runId = Uuid7.next();
        sessions.then(j -> startSleepingBuild(j, runId, "sleep 5\necho 'after-sleep'\n", true));
        sessions.then(j -> {
            WorkflowRun run = j.jenkins.getItemByFullName(JOB, WorkflowJob.class).getBuildByNumber(1);
            j.assertBuildStatusSuccess(j.waitForCompletion(run));
            assertFalse(FailedProgramLoad.matches(run));

            List<BuildEvent> completed = completedInSink(runId);
            assertEquals(1, completed.size());
            JSONObject event = completed.get(0).toJson();
            JSONObject summary = event.optJSONObject("log_summary");
            assertTrue("a resumed build reports no restart reason",
                    summary == null || !LogSummary.REASON_JENKINS_RESTART.equals(summary.optString("incomplete_reason")));
        });
    }

    /**
     * Starts one build that is sleeping when the session ends, and returns
     * its start time. The session's clean stop suspends the build for a
     * resume.
     */
    private static long startSleepingBuild(JenkinsRule j, String runId, String script, boolean sandbox)
            throws Exception {
        VarroaRunRecorder.setGateOverrideForTesting("1");
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, JOB);
        job.setDefinition(new CpsFlowDefinition(script, sandbox));
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
     * Blocks the resume of the job while no Jenkins runs. A clean stop of
     * a resume-blocked build ends it, so the block is set only after the
     * stop. The next load then fails the program load.
     */
    private void blockResume() throws IOException {
        Path config = sessions.getHome().toPath().resolve("jobs").resolve(JOB).resolve("config.xml");
        String xml = Files.readString(config, StandardCharsets.UTF_8);
        String property = "<org.jenkinsci.plugins.workflow.job.properties.DisableResumeJobProperty/>";
        String blocked = xml.contains("<properties/>")
                ? xml.replace("<properties/>", "<properties>" + property + "</properties>")
                : xml.replace("<properties>", "<properties>" + property);
        assertNotEquals("the job config holds a properties element", xml, blocked);
        Files.writeString(config, blocked, StandardCharsets.UTF_8);
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
        assertEquals("FAILURE", event.getString("result"));
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

    /** A program-state value whose pickle fails to restore, after a delay. */
    public static final class Marker {
        @Override
        public String toString() {
            return "marker";
        }
    }

    private static final class FailingPickle extends Pickle {
        private static final long serialVersionUID = 1L;

        @Override
        public ListenableFuture<?> rehydrate(FlowExecutionOwner owner) {
            SettableFuture<Object> future = SettableFuture.create();
            Timer.get().schedule(() -> future.setException(new IOException("the marker is gone")),
                    2, TimeUnit.SECONDS);
            return future;
        }
    }

    @TestExtension("anAsyncFailedLoadReportsJenkinsRestartFromTheListener")
    public static final class FailingPickleFactory extends PickleFactory {
        @Override
        public Pickle writeReplace(Object object) {
            return object instanceof Marker ? new FailingPickle() : null;
        }
    }
}
