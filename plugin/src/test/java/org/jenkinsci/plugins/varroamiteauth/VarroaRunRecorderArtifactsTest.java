package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertTrue;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

import java.util.List;
import java.util.concurrent.TimeUnit;

/**
 * {@code JenkinsRule} test for task 6.11: a run that archives two artifacts
 * reports both {@link BuildArtifact} entries, their keys, and the attached
 * {@link LogSummary} on the {@code COMPLETED} {@link BuildEvent}.
 */
public class VarroaRunRecorderArtifactsTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Before
    public void setUp() {
        VarroaRunRecorder.setGateOverrideForTesting("1");
        VarroaLogStorageFactory.setActivationUrlForTesting("http://127.0.0.1:9095/v1/log");
        BuildEventSink.get().resetForTesting();
    }

    @After
    public void tearDown() {
        VarroaRunRecorder.setGateOverrideForTesting(null);
        VarroaLogStorageFactory.setActivationUrlForTesting(null);
        ArtifactCollector.setPrefixSupplierForTesting(null);
    }

    @Test
    public void aCompletedRunRecordsBothArtifactsAndTheLogSummary() throws Exception {
        VarroaLogStorageFactory factory = VarroaLogStorageFactory.get();
        factory.setCollaboratorsForTesting(
                new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(),
                        (url, chunk, token, timeout) -> new LogChunkSender.Attempt(202, -1, false),
                        ms -> { }, java.util.concurrent.Executors.newCachedThreadPool()),
                new LogReadCache());

        WorkflowJob p = j.jenkins.createProject(WorkflowJob.class, "p-artifacts-" + System.nanoTime());
        p.setDefinition(new CpsFlowDefinition(
                "node {\n"
                        + "  writeFile file: 'out1.txt', text: 'first artifact'\n"
                        + "  writeFile file: 'out2.txt', text: 'second artifact contents'\n"
                        + "  archiveArtifacts artifacts: '*.txt'\n"
                        + "}", true));
        String runId = Uuid7.next();
        WorkflowRun run = (WorkflowRun) p.scheduleBuild2(0,
                new VarroaIntentAction(runId, "", null, System.currentTimeMillis())).get(60, TimeUnit.SECONDS);
        j.assertBuildStatusSuccess(run);

        BuildEvent completed = findCompleted(runId);
        assertNotNull("a COMPLETED event must be published for the run", completed);

        List<BuildArtifact> artifacts = completed.getArtifacts();
        assertEquals(2, artifacts.size());
        for (BuildArtifact a : artifacts) {
            assertTrue(a.getPath().equals("out1.txt") || a.getPath().equals("out2.txt"));
            // artifact-manager-s3 is not installed in this test environment,
            // so the collector falls back to local mode: an empty blob key.
            assertEquals(BuildArtifact.STORAGE_LOCAL, a.getStorage());
            assertEquals("", a.getBlobKey());
            assertTrue("a written artifact's size must be known", a.getSizeBytes() >= 0);
        }

        LogSummary summary = completed.getLogSummary();
        assertNotNull("the COMPLETED event must carry a log summary", summary);
        assertTrue("finish() must count at least the final chunk", summary.getChunkCount() >= 1);
        // The plugin never sets finalized true; only the gateway does (D2).
        assertTrue(!summary.isFinalized());
    }

    @Test
    public void aConfiguredS3PrefixProducesTheD6KeyLayout() throws Exception {
        VarroaLogStorageFactory factory = VarroaLogStorageFactory.get();
        factory.setCollaboratorsForTesting(
                new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(),
                        (url, chunk, token, timeout) -> new LogChunkSender.Attempt(202, -1, false),
                        ms -> { }, java.util.concurrent.Executors.newCachedThreadPool()),
                new LogReadCache());
        String prefix = "artifacts/core/varroa/cell-a/";
        ArtifactCollector.setPrefixSupplierForTesting(() -> prefix);

        WorkflowJob p = j.jenkins.createProject(WorkflowJob.class, "p-s3key-" + System.nanoTime());
        p.setDefinition(new CpsFlowDefinition(
                "node {\n"
                        + "  writeFile file: 'out.txt', text: 'artifact body'\n"
                        + "  archiveArtifacts artifacts: '*.txt'\n"
                        + "}", true));
        String runId = Uuid7.next();
        WorkflowRun run = (WorkflowRun) p.scheduleBuild2(0,
                new VarroaIntentAction(runId, "", null, System.currentTimeMillis())).get(60, TimeUnit.SECONDS);
        j.assertBuildStatusSuccess(run);

        BuildEvent completed = findCompleted(runId);
        assertNotNull("a COMPLETED event must be published for the run", completed);

        List<BuildArtifact> artifacts = completed.getArtifacts();
        assertEquals(1, artifacts.size());
        BuildArtifact artifact = artifacts.get(0);
        String expectedKey = prefix + run.getParent().getFullName() + "/" + run.getNumber() + "/artifacts/out.txt";
        assertEquals(BuildArtifact.STORAGE_S3, artifact.getStorage());
        assertEquals(expectedKey, artifact.getBlobKey());
    }

    private BuildEvent findCompleted(String runId) throws InterruptedException {
        long deadline = System.currentTimeMillis() + 10_000;
        while (System.currentTimeMillis() < deadline) {
            BuildEventSink.DrainResult result = BuildEventSink.get().drain(256);
            for (BuildEvent e : result.events) {
                if (runId.equals(e.getRunId()) && e.isCompletedPhase()) {
                    return e;
                }
            }
            Thread.sleep(50);
        }
        return null;
    }
}
