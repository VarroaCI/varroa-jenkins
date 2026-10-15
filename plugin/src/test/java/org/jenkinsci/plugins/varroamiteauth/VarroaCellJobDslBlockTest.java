package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;

import hudson.model.Result;
import hudson.plugins.git.GitSCM;

import java.io.File;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.cps.CpsScmFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.jenkinsci.plugins.workflow.steps.Step;
import org.jenkinsci.plugins.workflow.steps.StepContext;
import org.jenkinsci.plugins.workflow.steps.StepExecution;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

public class VarroaCellJobDslBlockTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private static final String SEED_SCRIPT = "job('seeded')";

    @Before
    public void cell() {
        VarroaCellJobDslBlock.setCellForTest(true);
    }

    @After
    public void resetCell() {
        VarroaCellJobDslBlock.resetCellForTest();
    }

    @Test
    public void anInlineJobDslStepFailsOnACell() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "inline-dsl");
        job.setDefinition(new CpsFlowDefinition("jobDsl scriptText: \"" + SEED_SCRIPT + "\"", true));

        assertBlocked(job);
    }

    @Test
    public void aJobDslStepInAnScmJenkinsfileFailsOnACell() throws Exception {
        File repo = new File(j.jenkins.getRootDir(), "scm-dsl-repo");
        repo.mkdirs();
        git(repo, "init", "-b", "master");
        git(repo, "config", "user.email", "varroa@example.com");
        git(repo, "config", "user.name", "Varroa Test");
        Files.write(new File(repo, "Jenkinsfile").toPath(),
                ("node { jobDsl scriptText: \"" + SEED_SCRIPT + "\" }\n").getBytes(StandardCharsets.UTF_8));
        git(repo, "add", "Jenkinsfile");
        git(repo, "commit", "-m", "the seed step");

        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "scm-dsl");
        job.setDefinition(new CpsScmFlowDefinition(new GitSCM(repo.toURI().toString()), "Jenkinsfile"));

        assertBlocked(job);
    }

    @Test
    public void theMetaStepFormFailsOnACell() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "meta-dsl");
        job.setDefinition(new CpsFlowDefinition(
                "step([$class: 'ExecuteDslScripts', scriptText: \"" + SEED_SCRIPT + "\"])", true));

        assertBlocked(job);
    }

    @Test
    public void anotherCoreStepRunsOnACell() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "core-step");
        job.setDefinition(new CpsFlowDefinition(
                "node { writeFile file: 'a.txt', text: 'x'; archiveArtifacts 'a.txt' }", true));

        WorkflowRun run = job.scheduleBuild2(0).get();
        j.assertBuildStatusSuccess(run);
    }

    @Test
    public void theStepIsNotBlockedOffACell() throws Exception {
        VarroaCellJobDslBlock.setCellForTest(false);
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "off-cell-dsl");
        job.setDefinition(new CpsFlowDefinition(
                "node { jobDsl scriptText: \"" + SEED_SCRIPT + "\" }", true));

        WorkflowRun run = job.scheduleBuild2(0).get();
        assertFalse(JenkinsRule.getLog(run).contains(VarroaCellJobDslBlock.MESSAGE));
        assertNotNull(j.jenkins.getItemByFullName("seeded"));
    }

    @Test
    public void anotherMetaStepRunsOnACell() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "meta-archiver");
        job.setDefinition(new CpsFlowDefinition(
                "node { writeFile file: 'a.txt', text: 'x'\n"
                        + "       step([$class: 'ArtifactArchiver', artifacts: 'a.txt']) }", true));

        WorkflowRun run = job.scheduleBuild2(0).get();
        j.assertBuildStatusSuccess(run);
    }

    @Test
    public void aStepThatIsNotCoreStepIsNotJobDsl() {
        Step fake = new Step() {
            @Override
            public StepExecution start(StepContext context) {
                return null;
            }
        };
        assertFalse(VarroaCellJobDslBlock.isJobDsl(fake));
    }

    /** Runs the job on a cell. It must fail, log the block message, and create no job. */
    private void assertBlocked(WorkflowJob job) throws Exception {
        WorkflowRun run = job.scheduleBuild2(0).get();
        j.assertBuildStatus(Result.FAILURE, run);
        assertTrue(JenkinsRule.getLog(run).contains("The jobDsl step is not allowed on a cell"));
        assertNull(j.jenkins.getItemByFullName("seeded"));
    }

    private static void git(File dir, String... args) throws IOException, InterruptedException {
        List<String> cmd = new ArrayList<>();
        cmd.add("git");
        cmd.addAll(Arrays.asList(args));
        Process p = new ProcessBuilder(cmd).directory(dir).redirectErrorStream(true).start();
        String out = new String(p.getInputStream().readAllBytes(), StandardCharsets.UTF_8);
        if (p.waitFor() != 0) {
            throw new IOException("git " + Arrays.toString(args) + " failed: " + out);
        }
    }
}
