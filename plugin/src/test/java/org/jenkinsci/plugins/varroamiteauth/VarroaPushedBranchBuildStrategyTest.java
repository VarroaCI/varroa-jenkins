package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.fail;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.function.BooleanSupplier;
import jenkins.branch.BranchSource;
import jenkins.scm.api.SCMEvent;
import jenkins.scm.api.SCMHeadEvent;
import jenkins.scm.impl.mock.MockSCMController;
import jenkins.scm.impl.mock.MockSCMDiscoverBranches;
import jenkins.scm.impl.mock.MockSCMHeadEvent;
import jenkins.scm.impl.mock.MockSCMSource;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests the pushed-branch strategy on a multibranch project. The project has
 * only this strategy, so a branch builds only when the strategy answers true.
 */
public class VarroaPushedBranchBuildStrategyTest {

    private static final long AWAIT_MILLIS = 30_000;
    private static final long POLL_MILLIS = 200;

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private MockSCMController controller;

    @Before
    public void setUp() throws IOException {
        PushedHeadRegistry.get().resetForTesting();
        controller = MockSCMController.create();
        controller.createRepository("repo");
        controller.addFile("repo", "master", "add Jenkinsfile", "Jenkinsfile", "echo 'hi'".getBytes(StandardCharsets.UTF_8));
    }

    private WorkflowMultiBranchProject createProject(String name) throws IOException {
        WorkflowMultiBranchProject project = j.jenkins.createProject(WorkflowMultiBranchProject.class, name);
        BranchSource branchSource = new BranchSource(
                new MockSCMSource(controller, "repo", new MockSCMDiscoverBranches()));
        branchSource.setBuildStrategies(new ArrayList<>(java.util.List.of(new VarroaPushedBranchBuildStrategy())));
        project.getSourcesList().add(branchSource);
        project.save();
        return project;
    }

    private int buildsOf(WorkflowMultiBranchProject project, String branch) {
        WorkflowJob job = (WorkflowJob) project.getItem(branch);
        assertNotNull("branch job " + branch + " exists", job);
        return job.getBuilds().size();
    }

    private static void awaitTrue(BooleanSupplier condition, String what) throws InterruptedException {
        long deadline = System.currentTimeMillis() + AWAIT_MILLIS;
        while (!condition.getAsBoolean()) {
            if (System.currentTimeMillis() > deadline) {
                fail("timed out waiting for " + what);
            }
            Thread.sleep(POLL_MILLIS);
        }
    }

    private int buildCount(WorkflowMultiBranchProject project, String branch) {
        WorkflowJob job = (WorkflowJob) project.getItem(branch);
        return job == null ? -1 : job.getBuilds().size();
    }

    /** Fires a create event and waits until the branch job holds {@code builds} builds. */
    private void fireCreatedAwaitBuilds(WorkflowMultiBranchProject project, String head, int builds) throws Exception {
        String hash = controller.getRevision("repo", head);
        SCMHeadEvent.fireNow(new MockSCMHeadEvent(SCMEvent.Type.CREATED, controller, "repo", head, hash));
        awaitTrue(() -> buildCount(project, head) == builds, builds + " build(s) of " + head);
        j.waitUntilNoActivity();
    }

    /**
     * Fires a repeated create event and waits until the registry records it. The dispatcher
     * is asynchronous, and a repeated event starts no computation, so the registry size is the
     * observable signal. The listener records the event before branch-api handles it.
     */
    private void fireCreatedRepeated(String head) throws Exception {
        int recorded = PushedHeadRegistry.get().sizeForTesting();
        String hash = controller.getRevision("repo", head);
        SCMHeadEvent.fireNow(new MockSCMHeadEvent(SCMEvent.Type.CREATED, controller, "repo", head, hash));
        awaitTrue(() -> PushedHeadRegistry.get().sizeForTesting() > recorded, "the repeated event to be recorded");
        j.waitUntilNoActivity();
    }

    @Test
    public void scanCreatedBranchDoesNotBuild() throws Exception {
        WorkflowMultiBranchProject project = createProject("scanned");
        project.scheduleBuild2(0);
        j.waitUntilNoActivity();

        assertEquals(0, buildsOf(project, "master"));
    }

    @Test
    public void pushThatCreatesBranchBuildsOnce() throws Exception {
        WorkflowMultiBranchProject project = createProject("pushed");
        project.scheduleBuild2(0);
        j.waitUntilNoActivity();

        controller.cloneBranch("repo", "master", "feature");
        fireCreatedAwaitBuilds(project, "feature", 1);
        assertEquals(1, buildsOf(project, "feature"));

        fireCreatedRepeated("feature");
        assertEquals(1, buildsOf(project, "feature"));
    }

    @Test
    public void pushBeforeScanBuildsWhenScanCreatesBranch() throws Exception {
        controller.cloneBranch("repo", "master", "feature");
        String hash = controller.getRevision("repo", "feature");
        SCMHeadEvent.fireNow(new MockSCMHeadEvent(SCMEvent.Type.CREATED, controller, "repo", "feature", hash));
        j.waitUntilNoActivity();

        WorkflowMultiBranchProject project = createProject("late");
        project.scheduleBuild2(0);
        j.waitUntilNoActivity();

        assertEquals(1, buildsOf(project, "feature"));
        assertEquals(0, buildsOf(project, "master"));
    }
}
