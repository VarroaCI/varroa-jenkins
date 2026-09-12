package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import hudson.model.Run;

import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * {@code JenkinsRule} tests for {@link VarroaLogStorage} (task 6.4): a
 * two-stage pipeline's node ids and stage ids on the captured chunks, and
 * (task 6.8) the masking guarantee.
 */
public class VarroaLogStorageTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private final List<LogChunk> captured = new CopyOnWriteArrayList<>();

    @Before
    public void setUp() {
        VarroaLogStorageFactory.setActivationUrlForTesting("http://127.0.0.1:9095/v1/log");
    }

    @After
    public void tearDown() {
        VarroaLogStorageFactory.setActivationUrlForTesting(null);
    }

    private VarroaLogStorageFactory installRecordingFactory() {
        VarroaLogStorageFactory factory = VarroaLogStorageFactory.get();
        LogChunkSender.Transport recording = (url, chunk, token, timeout) -> {
            captured.add(chunk);
            return new LogChunkSender.Attempt(202, -1, false);
        };
        LogChunkSender sender = new LogChunkSender("http://127.0.0.1:9095/v1/log", new LoopbackTokenFile(),
                recording, ms -> { }, Executors.newCachedThreadPool());
        factory.setCollaboratorsForTesting(sender, new LogReadCache());
        return factory;
    }

    private WorkflowRun runPipeline(String groovy) throws Exception {
        WorkflowJob p = j.jenkins.createProject(WorkflowJob.class, "p-" + System.nanoTime());
        p.setDefinition(new CpsFlowDefinition(groovy, true));
        String runId = Uuid7.next();
        WorkflowRun run = (WorkflowRun) p.scheduleBuild2(0,
                new VarroaIntentAction(runId, "", null, System.currentTimeMillis())).get(60, TimeUnit.SECONDS);
        j.assertBuildStatusSuccess(run);
        VarroaLogStorageFactory.get().finish(runId);
        waitForCaptured(1);
        return run;
    }

    private void waitForCaptured(int atLeast) throws InterruptedException {
        long deadline = System.currentTimeMillis() + 10_000;
        while (captured.size() < atLeast && System.currentTimeMillis() < deadline) {
            Thread.sleep(50);
        }
    }

    @Test
    public void aTwoStagePipelineTagsNodeAndStageIds() throws Exception {
        installRecordingFactory();
        runPipeline("node {\n"
                + "  stage('one') { echo 'hello-from-one' }\n"
                + "  stage('two') { echo 'hello-from-two' }\n"
                + "}");
        waitForCaptured(2);

        boolean sawStageOneText = false;
        boolean sawStageTwoText = false;
        java.util.Set<String> stageIds = new java.util.HashSet<>();
        for (LogChunk c : captured) {
            String text = new String(c.data, StandardCharsets.UTF_8);
            if (text.contains("hello-from-one")) {
                sawStageOneText = true;
                assertFalse("a step chunk must carry a node id", c.nodeId.isEmpty());
                assertFalse("a step chunk must carry a stage id", c.stageId.isEmpty());
                stageIds.add(c.stageId);
            }
            if (text.contains("hello-from-two")) {
                sawStageTwoText = true;
                assertFalse(c.nodeId.isEmpty());
                assertFalse(c.stageId.isEmpty());
                stageIds.add(c.stageId);
            }
        }
        assertTrue("stage one output must be captured", sawStageOneText);
        assertTrue("stage two output must be captured", sawStageTwoText);
        assertTrue("the two stages must carry distinct stage ids", stageIds.size() >= 2);
    }

    @Test
    public void aBoundCredentialNeverReachesACapturedChunk() throws Exception {
        installRecordingFactory();
        String secretId = "varroa-log-test-secret-" + System.nanoTime();
        String secretValue = "s3cr3t-value-do-not-leak";
        com.cloudbees.plugins.credentials.CredentialsProvider.lookupStores(j.jenkins).iterator().next()
                .addCredentials(com.cloudbees.plugins.credentials.domains.Domain.global(),
                        new org.jenkinsci.plugins.plaincredentials.impl.StringCredentialsImpl(
                                com.cloudbees.plugins.credentials.CredentialsScope.GLOBAL, secretId, "",
                                hudson.util.Secret.fromString(secretValue)));

        runPipeline("node {\n"
                + "  withCredentials([string(credentialsId: '" + secretId + "', variable: 'SECRET')]) {\n"
                + "    echo \"the secret is ${SECRET}\"\n"
                + "  }\n"
                + "}");

        for (LogChunk c : captured) {
            String text = new String(c.data, StandardCharsets.UTF_8);
            assertFalse("no captured chunk may contain the bound secret value: " + text,
                    text.contains(secretValue));
        }
    }
}
