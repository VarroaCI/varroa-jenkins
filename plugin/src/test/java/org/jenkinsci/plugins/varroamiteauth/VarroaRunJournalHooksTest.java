package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import hudson.Launcher;
import hudson.model.AbstractBuild;
import hudson.model.BuildListener;
import hudson.model.FreeStyleBuild;
import hudson.model.FreeStyleProject;
import hudson.model.Result;

import java.io.File;
import java.util.List;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.jvnet.hudson.test.JenkinsRule;
import org.jvnet.hudson.test.TestBuilder;

/**
 * Tests for the journal hooks in {@link VarroaRunRecorder}: the entry a
 * start writes, the entry a spooled completion deletes, the build record a
 * freestyle start saves, and the heartbeat touch.
 */
public class VarroaRunJournalHooksTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private final CountDownLatch started = new CountDownLatch(1);
    private final CountDownLatch release = new CountDownLatch(1);

    @Before
    public void setUp() {
        VarroaRunRecorder.resetStateForTesting();
        BuildEventSink.get().resetForTesting();
        VarroaRunRecorder.setGateOverrideForTesting("1");
    }

    @After
    public void tearDown() {
        release.countDown();
        VarroaRunRecorder.setGateOverrideForTesting(null);
    }

    private FreeStyleProject blockingProject(String name) throws Exception {
        FreeStyleProject project = j.createFreeStyleProject(name);
        project.getBuildersList().add(new TestBuilder() {
            @Override
            public boolean perform(AbstractBuild<?, ?> build, Launcher launcher, BuildListener listener)
                    throws InterruptedException {
                started.countDown();
                return release.await(60, TimeUnit.SECONDS);
            }
        });
        return project;
    }

    private static VarroaIntentAction grantedAction() {
        return new VarroaIntentAction(Uuid7.next(), "delivery-7", null, System.currentTimeMillis());
    }

    @Test
    public void onStartedWritesJournalEntry() throws Exception {
        FreeStyleProject project = blockingProject("journaled");
        VarroaIntentAction action = grantedAction();
        FreeStyleBuild build = project.scheduleBuild2(0, action).waitForStart();
        assertTrue(started.await(30, TimeUnit.SECONDS));

        List<RunJournal.Entry> entries = new RunJournal().readAll();
        assertEquals(1, entries.size());
        RunJournal.Entry entry = entries.get(0);
        assertEquals(action.getRunId(), entry.runId);
        assertEquals("journaled", entry.jobPath);
        assertEquals(build.getNumber(), entry.buildNumber);
        assertEquals("delivery-7", entry.deliveryId);
        assertEquals(build.getStartTimeInMillis(), entry.startedAtMillis);
        assertFalse(entry.pipeline);

        release.countDown();
        j.waitForCompletion(build);
    }

    @Test
    public void entryStaysUntilTheCompletionIsSpooled() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("cleared");
        VarroaIntentAction action = grantedAction();
        j.assertBuildStatusSuccess(project.scheduleBuild2(0, action));

        assertEquals("a completion still in the sink keeps the entry", 1, new RunJournal().readAll().size());

        BuildEvent completed = null;
        for (BuildEvent e : BuildEventSink.get().drain(100).events) {
            if (BuildEvent.PHASE_COMPLETED.equals(e.getPhase())) {
                completed = e;
            }
        }
        assertNotNull(completed);
        RunSpool spool = new RunSpool(tmp.newFolder("spool").toPath(), RunSpool.DEFAULT_CAP_BYTES, new RunJournal());
        assertTrue(spool.append(completed.getRunId(), completed));

        assertTrue("the spooled completion deletes the entry", new RunJournal().readAll().isEmpty());
    }

    @Test
    public void pipelineEntryIsMarked() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "piped");
        job.setDefinition(new CpsFlowDefinition("sleep 60\n", true));
        WorkflowRun run = job.scheduleBuild2(0, grantedAction()).waitForStart();
        j.waitForMessage("Sleeping", run);

        List<RunJournal.Entry> entries = new RunJournal().readAll();
        assertEquals(1, entries.size());
        assertTrue(entries.get(0).pipeline);

        run.doStop();
        j.waitForCompletion(run);
    }

    @Test
    public void onDeletedRemovesJournalEntry() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("deleted");
        FreeStyleBuild build = j.assertBuildStatusSuccess(project.scheduleBuild2(0, grantedAction()));
        assertEquals(1, new RunJournal().readAll().size());

        build.delete();

        assertTrue(new RunJournal().readAll().isEmpty());
    }

    @Test
    public void freestyleBuildXmlExistsWhileRunning() throws Exception {
        FreeStyleProject project = blockingProject("saved");
        FreeStyleBuild build = project.scheduleBuild2(0, grantedAction()).waitForStart();
        assertTrue(started.await(30, TimeUnit.SECONDS));

        File record = new File(build.getRootDir(), "build.xml");
        assertTrue("the start saves the build record", record.isFile());
        // A stopped process loads the saved record as a completed build.
        FreeStyleBuild loaded = new FreeStyleBuild(project, build.getRootDir());
        assertFalse(loaded.isBuilding());
        assertEquals(Result.ABORTED, loaded.getResult());

        release.countDown();
        j.waitForCompletion(build);
    }

    @Test
    public void noJournalWhenGateOff() throws Exception {
        VarroaRunRecorder.setGateOverrideForTesting("0");
        FreeStyleProject project = blockingProject("ungated");
        FreeStyleBuild build = project.scheduleBuild2(0, grantedAction()).waitForStart();
        assertTrue(started.await(30, TimeUnit.SECONDS));

        assertTrue(new RunJournal().readAll().isEmpty());

        release.countDown();
        j.waitForCompletion(build);
    }

    @Test
    public void heartbeatTickTouchesJournal() throws Exception {
        FreeStyleProject project = blockingProject("touched");
        VarroaIntentAction action = grantedAction();
        FreeStyleBuild build = project.scheduleBuild2(0, action).waitForStart();
        assertTrue(started.await(30, TimeUnit.SECONDS));
        File entry = new File(j.jenkins.getRootDir(), "varroa/live/" + action.getRunId() + ".json");
        assertTrue(entry.setLastModified(5_000L));

        VarroaRunRecorder.heartbeatTick();

        assertTrue(entry.lastModified() > 5_000L);
        release.countDown();
        j.waitForCompletion(build);
    }
}
