package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.fail;

import com.cloudbees.hudson.plugins.folder.computed.ComputedFolder;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.function.BooleanSupplier;
import jenkins.branch.OrganizationFolder;
import jenkins.scm.api.SCMEvent;
import jenkins.scm.api.SCMHeadEvent;
import jenkins.scm.impl.mock.MockSCMController;
import jenkins.scm.impl.mock.MockSCMDiscoverBranches;
import jenkins.scm.impl.mock.MockSCMHeadEvent;
import jenkins.scm.impl.mock.MockSCMNavigator;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProjectFactory;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests the pushed-branch strategy on an organization folder. A push that
 * creates a repository builds its pushed branch once, and the other branches of
 * that repository build nothing.
 */
public class VarroaPushedBranchOrganizationFolderTest {

    private static final long AWAIT_MILLIS = 30_000;
    private static final long POLL_MILLIS = 200;

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private MockSCMController controller;

    @Before
    public void setUp() throws IOException {
        PushedHeadRegistry.get().resetForTesting();
        controller = MockSCMController.create();
    }

    @Test
    public void pushThatCreatesRepositoryBuildsPushedBranchOnce() throws Exception {
        OrganizationFolder folder = j.jenkins.createProject(OrganizationFolder.class, "org");
        folder.getNavigators().add(new MockSCMNavigator(controller, new MockSCMDiscoverBranches()));
        folder.getProjectFactories().add(new WorkflowMultiBranchProjectFactory());
        folder.getBuildStrategies().add(new VarroaPushedBranchBuildStrategy());
        folder.save();
        folder.scheduleBuild2(0);
        j.waitUntilNoActivity();

        controller.createRepository("newrepo");
        controller.addFile("newrepo", "master", "add Jenkinsfile", "Jenkinsfile",
                "echo 'hi'".getBytes(StandardCharsets.UTF_8));
        controller.cloneBranch("newrepo", "master", "dev");
        String hash = controller.getRevision("newrepo", "master");
        SCMHeadEvent.fireNow(new MockSCMHeadEvent(SCMEvent.Type.CREATED, controller, "newrepo", "master", hash));
        // The event dispatcher runs off the test thread. The pushed branch builds first. The
        // other branch of the repository is observed in the same indexing, so once it exists
        // its build decision has been taken.
        awaitTrue(() -> buildCount(folder, "newrepo", "master") == 1, "one build of master");
        awaitTrue(() -> buildCount(folder, "newrepo", "dev") >= 0, "the dev branch job");
        j.waitUntilNoActivity();

        assertEquals(1, buildCount(folder, "newrepo", "master"));
        assertEquals(0, buildCount(folder, "newrepo", "dev"));
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

    /** Returns the build count of a branch job, or -1 while the repository or the job is missing. */
    private static int buildCount(OrganizationFolder folder, String repoName, String branch) {
        ComputedFolder<?> repo = (ComputedFolder<?>) folder.getItem(repoName);
        if (repo == null) {
            return -1;
        }
        WorkflowJob job = (WorkflowJob) repo.getItem(branch);
        return job == null ? -1 : job.getBuilds().size();
    }
}
