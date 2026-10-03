package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import hudson.model.FreeStyleBuild;
import hudson.model.FreeStyleProject;
import hudson.model.Result;

import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.attribute.FileTime;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.TimeUnit;

import net.sf.json.JSONObject;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests for {@link VarroaBootReconcile}'s rules against a real Jenkins
 * instance. Each test plants journal entries directly and calls the
 * reconcile, because a test cannot stop the Jenkins process mid-build.
 */
public class VarroaBootReconcileTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private RunJournal journal;
    private RunSpool spool;

    @Before
    public void setUp() throws Exception {
        VarroaRunRecorder.resetStateForTesting();
        BuildEventSink.get().resetForTesting();
        PluginCounters.resetForTesting();
        VarroaRunRecorder.setGateOverrideForTesting("1");
        journal = new RunJournal(tmp.newFolder("live").toPath());
        spool = new RunSpool(tmp.newFolder("spool").toPath(), RunSpool.DEFAULT_CAP_BYTES);
    }

    @After
    public void tearDown() {
        VarroaRunRecorder.setGateOverrideForTesting(null);
    }

    private static VarroaIntentAction grantedAction() {
        return new VarroaIntentAction(Uuid7.next(), "", null, System.currentTimeMillis());
    }

    private Path entryFile(String runId) {
        return tmp.getRoot().toPath().resolve("live").resolve(runId + ".json");
    }

    private void setLastSeen(String runId, long millis) throws Exception {
        Files.setLastModifiedTime(entryFile(runId), FileTime.fromMillis(millis));
    }

    private List<JSONObject> completedEvents(String runId) {
        List<JSONObject> out = new ArrayList<>();
        for (RunSpool.Entry e : spool.readOrdered(runId)) {
            if (BuildEvent.PHASE_COMPLETED.equals(e.phase)) {
                out.add(e.toJsonObject());
            }
        }
        return out;
    }

    @Test
    public void loadedCompletedBuildEmitsLoadedResult() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("done");
        VarroaIntentAction action = grantedAction();
        FreeStyleBuild build = j.assertBuildStatusSuccess(project.scheduleBuild2(0, action));
        String runId = action.getRunId();
        journal.write(new RunJournal.Entry(runId, "intent-done", "done", build.getNumber(), "",
                build.getStartTimeInMillis(), false));

        assertEquals(1, VarroaBootReconcile.reconcile(journal, spool, System.currentTimeMillis()));

        List<JSONObject> events = completedEvents(runId);
        assertEquals(1, events.size());
        JSONObject event = events.get(0);
        assertEquals("SUCCESS", event.getString("result"));
        assertEquals(runId, event.getString("run_id"));
        assertEquals("intent-done", event.getString("intent_key"));
        assertEquals("done", event.getString("job_path"));
        assertEquals(build.getNumber(), event.getLong("build_number"));
        assertEquals("a build with its own duration keeps it", build.getDuration(), event.getLong("duration_millis"));
        assertEquals("a cap drop died with the process, so the log never reads as complete",
                LogSummary.REASON_JENKINS_RESTART, event.getJSONObject("log_summary").getString("incomplete_reason"));
        assertTrue(journal.readAll().isEmpty());
        assertEquals(Long.valueOf(1L), PluginCounters.snapshot().get(PluginCounters.RUNS_RECONCILED_AT_BOOT));
    }

    @Test
    public void missingFreestyleBuildEmitsAbortedFromJournal() throws Exception {
        String runId = Uuid7.next();
        long startedAt = System.currentTimeMillis() - 60_000L;
        journal.write(new RunJournal.Entry(runId, "intent-gone", "gone", 3, "delivery-1", startedAt, false));
        setLastSeen(runId, startedAt + 5_000L);

        assertEquals(1, VarroaBootReconcile.reconcile(journal, spool, System.currentTimeMillis()));

        List<JSONObject> events = completedEvents(runId);
        assertEquals(1, events.size());
        JSONObject event = events.get(0);
        assertEquals(Result.ABORTED.toString(), event.getString("result"));
        assertEquals("gone", event.getString("job_path"));
        assertEquals(3, event.getLong("build_number"));
        assertEquals("delivery-1", event.getString("delivery_id"));
        assertEquals(startedAt, event.getLong("started_at_millis"));
        assertEquals("the run ends at its last-seen time", startedAt + 5_000L, event.getLong("completed_at_millis"));
        assertEquals(5_000L, event.getLong("duration_millis"));
        assertTrue(journal.readAll().isEmpty());
    }

    @Test
    public void eventCarriesJenkinsRestartLogReason() throws Exception {
        String runId = Uuid7.next();
        journal.write(new RunJournal.Entry(runId, "intent", "gone", 1, "", System.currentTimeMillis(), false));

        VarroaBootReconcile.reconcile(journal, spool, System.currentTimeMillis());

        JSONObject summary = completedEvents(runId).get(0).getJSONObject("log_summary");
        assertTrue(summary.getBoolean("incomplete"));
        assertEquals(LogSummary.REASON_JENKINS_RESTART, summary.getString("incomplete_reason"));
    }

    @Test
    public void missingPipelineBuildDropsEntryWithoutEvent() throws Exception {
        String runId = Uuid7.next();
        journal.write(new RunJournal.Entry(runId, "intent", "moved-pipeline", 4, "", System.currentTimeMillis(), true));

        assertEquals(0, VarroaBootReconcile.reconcile(journal, spool, System.currentTimeMillis()));

        assertTrue(completedEvents(runId).isEmpty());
        assertTrue(journal.readAll().isEmpty());
    }

    @Test
    public void buildingPipelineKeepsEntry() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "running");
        job.setDefinition(new CpsFlowDefinition("sleep 120\n", true));
        VarroaIntentAction action = grantedAction();
        WorkflowRun run = job.scheduleBuild2(0, action).waitForStart();
        j.waitForMessage("Sleeping", run);
        String runId = action.getRunId();
        journal.write(new RunJournal.Entry(runId, "intent", "running", run.getNumber(), "",
                run.getStartTimeInMillis(), true));

        assertEquals(0, VarroaBootReconcile.reconcile(journal, spool, System.currentTimeMillis()));

        assertTrue(completedEvents(runId).isEmpty());
        assertEquals("a building pipeline keeps its entry", 1, journal.readAll().size());
        run.doStop();
        j.waitForCompletion(run);
    }

    @Test
    public void spooledCompletedSkipsReemit() throws Exception {
        String runId = Uuid7.next();
        journal.write(new RunJournal.Entry(runId, "intent", "gone", 2, "", System.currentTimeMillis(), false));
        spool.append(runId, BuildEvent.builder()
                .phase(BuildEvent.PHASE_COMPLETED)
                .seq(7L)
                .runId(runId)
                .jobPath("gone")
                .buildNumber(2)
                .result("ABORTED")
                .build());

        assertEquals(0, VarroaBootReconcile.reconcile(journal, spool, System.currentTimeMillis()));

        assertEquals("the spooled event stays the only one", 1, completedEvents(runId).size());
        assertTrue(journal.readAll().isEmpty());
    }

    @Test
    public void reconcileTwiceEmitsOnce() throws Exception {
        String runId = Uuid7.next();
        journal.write(new RunJournal.Entry(runId, "intent", "gone", 5, "", System.currentTimeMillis(), false));

        assertEquals(1, VarroaBootReconcile.reconcile(journal, spool, System.currentTimeMillis()));
        // A stop between the spool write and the entry delete leaves both.
        journal.write(new RunJournal.Entry(runId, "intent", "gone", 5, "", System.currentTimeMillis(), false));
        assertEquals(0, VarroaBootReconcile.reconcile(journal, spool, System.currentTimeMillis()));

        assertEquals(1, completedEvents(runId).size());
        assertTrue(journal.readAll().isEmpty());
    }

    @Test
    public void failedSpoolWriteKeepsEntry() throws Exception {
        // A regular file where the spool directory should be makes every write fail.
        Path blocked = tmp.newFile("blocked-spool").toPath();
        RunSpool unwritable = new RunSpool(blocked, RunSpool.DEFAULT_CAP_BYTES);
        String runId = Uuid7.next();
        journal.write(new RunJournal.Entry(runId, "intent", "gone", 8, "", System.currentTimeMillis(), false));

        assertEquals(0, VarroaBootReconcile.reconcile(journal, unwritable, System.currentTimeMillis()));

        assertEquals("the entry stays for the next boot", 1, journal.readAll().size());
        assertEquals(Long.valueOf(0L), PluginCounters.snapshot().get(PluginCounters.RUNS_RECONCILED_AT_BOOT));
    }

    @Test
    public void expiredEntryIsDeletedWithoutEvent() throws Exception {
        String runId = Uuid7.next();
        long eightDaysAgo = System.currentTimeMillis() - TimeUnit.DAYS.toMillis(8);
        journal.write(new RunJournal.Entry(runId, "intent", "gone", 6, "", eightDaysAgo, false));
        setLastSeen(runId, eightDaysAgo);

        assertEquals(0, VarroaBootReconcile.reconcile(journal, spool, System.currentTimeMillis()));

        assertTrue(completedEvents(runId).isEmpty());
        assertTrue(journal.readAll().isEmpty());
    }

    @Test
    public void buildWithAnotherRunIdCountsAsMissing() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("reused");
        FreeStyleBuild build = j.assertBuildStatusSuccess(project.scheduleBuild2(0, grantedAction()));
        String runId = Uuid7.next();
        long startedAt = System.currentTimeMillis() - 10_000L;
        journal.write(new RunJournal.Entry(runId, "intent", "reused", build.getNumber(), "", startedAt, false));

        assertEquals(1, VarroaBootReconcile.reconcile(journal, spool, System.currentTimeMillis()));

        JSONObject event = completedEvents(runId).get(0);
        assertEquals("the later build's result is never reported", "ABORTED", event.getString("result"));
        assertEquals(startedAt, event.getLong("started_at_millis"));
    }
}
