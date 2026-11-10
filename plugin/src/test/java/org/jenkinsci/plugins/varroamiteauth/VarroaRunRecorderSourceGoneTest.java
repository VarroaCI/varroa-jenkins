package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import jenkins.branch.Branch;
import jenkins.branch.BranchSource;
import jenkins.scm.impl.mock.MockSCMController;
import jenkins.scm.impl.mock.MockSCMDiscoverBranches;
import jenkins.scm.impl.mock.MockSCMSource;
import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * A completed run of a branch job that a scan marked dead carries source_gone.
 * A run of a live branch job, and a run of a plain pipeline job, do not.
 */
public class VarroaRunRecorderSourceGoneTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private MockSCMController controller;

    @Before
    public void setUp() throws Exception {
        VarroaRunRecorder.resetStateForTesting();
        BuildEventSink.get().resetForTesting();
        VarroaRunRecorder.setGateOverrideForTesting("1");
        PushedHeadRegistry.get().resetForTesting();
        controller = MockSCMController.create();
        controller.createRepository("repo");
    }

    @After
    public void tearDown() {
        VarroaRunRecorder.setGateOverrideForTesting(null);
        VarroaRunRecorder.resetStateForTesting();
        BuildEventSink.get().resetForTesting();
    }

    /** Creates a multibranch project whose branch jobs build only on request. */
    private WorkflowMultiBranchProject createProject(String name, String jenkinsfile) throws Exception {
        controller.addFile("repo", "master", "add Jenkinsfile", "Jenkinsfile",
                jenkinsfile.getBytes(StandardCharsets.UTF_8));
        WorkflowMultiBranchProject project = j.jenkins.createProject(WorkflowMultiBranchProject.class, name);
        BranchSource branchSource = new BranchSource(
                new MockSCMSource(controller, "repo", new MockSCMDiscoverBranches()));
        branchSource.setBuildStrategies(new ArrayList<>(List.of(new VarroaPushedBranchBuildStrategy())));
        project.getSourcesList().add(branchSource);
        project.save();
        project.scheduleBuild2(0).getFuture().get();
        j.waitUntilNoActivity();
        return project;
    }

    /** Returns the COMPLETED events that the sink holds. */
    private static List<BuildEvent> completedEvents() {
        List<BuildEvent> completed = new ArrayList<>();
        for (BuildEvent e : BuildEventSink.get().drain(100).events) {
            if (BuildEvent.PHASE_COMPLETED.equals(e.getPhase())) {
                completed.add(e);
            }
        }
        return completed;
    }

    @Test
    public void completedEventOfADeadBranchJobCarriesSourceGone() throws Exception {
        WorkflowMultiBranchProject p = createProject("dead-branch", "input message: 'go'\n");
        WorkflowJob job = (WorkflowJob) p.getItem("master");
        WorkflowRun run = (WorkflowRun) job.scheduleBuild2(0,
                new VarroaIntentAction(Uuid7.next(), "", null, System.currentTimeMillis())).waitForStart();
        VarroaRunInputsTest.awaitPendingInput(run);

        p.getProjectFactory().setBranch(job, new Branch.Dead(p.getProjectFactory().getBranch(job)));
        run.doStop();
        j.waitForCompletion(run);

        List<BuildEvent> completed = completedEvents();
        assertEquals("one COMPLETED event", 1, completed.size());
        assertTrue("the dead branch job sets source_gone", completed.get(0).isSourceGone());
    }

    @Test
    public void completedEventOfALiveBranchJobCarriesNoSourceGone() throws Exception {
        WorkflowMultiBranchProject p = createProject("live-branch", "echo 'hi'\n");
        WorkflowJob job = (WorkflowJob) p.getItem("master");
        WorkflowRun run = (WorkflowRun) job.scheduleBuild2(0,
                new VarroaIntentAction(Uuid7.next(), "", null, System.currentTimeMillis())).waitForStart();
        j.waitForCompletion(run);

        List<BuildEvent> completed = completedEvents();
        assertEquals("one COMPLETED event", 1, completed.size());
        assertFalse("a live branch job sets no source_gone", completed.get(0).isSourceGone());
    }

    @Test
    public void sourceGoneIsFalseForAPlainPipelineJob() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "plain-pipeline");
        job.setDefinition(new CpsFlowDefinition("echo 'hi'\n", true));
        WorkflowRun run = job.scheduleBuild2(0).waitForStart();
        j.waitForCompletion(run);
        assertFalse(VarroaRunRecorder.sourceGone(run));
    }
}
