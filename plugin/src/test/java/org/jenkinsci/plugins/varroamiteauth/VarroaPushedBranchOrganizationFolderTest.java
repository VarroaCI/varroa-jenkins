package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.fail;

import com.cloudbees.hudson.plugins.folder.computed.ComputedFolder;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.Collections;
import java.util.Map;
import java.util.function.BooleanSupplier;
import jenkins.branch.OrganizationFolder;
import jenkins.scm.api.SCMEvent;
import jenkins.scm.api.SCMHead;
import jenkins.scm.api.SCMHeadEvent;
import jenkins.scm.api.SCMRevision;
import jenkins.scm.api.SCMSource;
import jenkins.scm.api.mixin.ChangeRequestCheckoutStrategy;
import jenkins.scm.impl.mock.MockChangeRequestSCMHead;
import jenkins.scm.impl.mock.MockChangeRequestSCMRevision;
import jenkins.scm.impl.mock.MockSCMController;
import jenkins.scm.impl.mock.MockSCMDiscoverBranches;
import jenkins.scm.impl.mock.MockSCMDiscoverChangeRequests;
import jenkins.scm.impl.mock.MockSCMHead;
import jenkins.scm.impl.mock.MockSCMHeadEvent;
import jenkins.scm.impl.mock.MockSCMNavigator;
import jenkins.scm.impl.mock.MockSCMRevision;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProjectFactory;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests the pushed-branch strategy on an organization folder. A push that
 * creates a repository builds its pushed branch once, and the other branches of
 * that repository build nothing. A change request event builds its change
 * request once. A scan builds nothing that it discovers.
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

    @Test
    public void scanThatDiscoversRepositoryBuildsNothing() throws Exception {
        OrganizationFolder folder = createFolder("org");
        folder.scheduleBuild2(0);
        j.waitUntilNoActivity();

        controller.createRepository("scanned");
        controller.addFile("scanned", "master", "add Jenkinsfile", "Jenkinsfile",
                "echo 'hi'".getBytes(StandardCharsets.UTF_8));
        controller.cloneBranch("scanned", "master", "feature");
        Integer number = controller.openChangeRequest("scanned", "feature");
        folder.scheduleBuild2(0);
        j.waitUntilNoActivity();

        assertEquals(0, buildCount(folder, "scanned", "master"));
        assertEquals(0, buildCount(folder, "scanned", "feature"));
        assertEquals(0, buildCount(folder, "scanned", "CR-" + number));
    }

    @Test
    public void changeRequestEventThatCreatesRepositoryBuildsChangeRequestOnce() throws Exception {
        OrganizationFolder folder = createFolder("org");
        folder.scheduleBuild2(0);
        j.waitUntilNoActivity();

        controller.createRepository("newrepo");
        controller.cloneBranch("newrepo", "master", "feature");
        controller.addFile("newrepo", "feature", "add Jenkinsfile", "Jenkinsfile",
                "echo 'hi'".getBytes(StandardCharsets.UTF_8));
        Integer number = controller.openChangeRequest("newrepo", "feature");
        String changeRequest = "CR-" + number;
        SCMHeadEvent.fireNow(new ChangeRequestOpenedEvent(controller, "newrepo", number));
        awaitTrue(() -> buildCount(folder, "newrepo", changeRequest) == 1, "one build of " + changeRequest);
        j.waitUntilNoActivity();

        assertEquals(1, buildCount(folder, "newrepo", changeRequest));
        assertEquals(0, buildCount(folder, "newrepo", "feature"));
    }

    private OrganizationFolder createFolder(String name) throws IOException {
        OrganizationFolder folder = j.jenkins.createProject(OrganizationFolder.class, name);
        folder.getNavigators().add(new MockSCMNavigator(controller, new MockSCMDiscoverBranches(),
                new MockSCMDiscoverChangeRequests(ChangeRequestCheckoutStrategy.HEAD)));
        folder.getProjectFactories().add(new WorkflowMultiBranchProjectFactory());
        folder.getBuildStrategies().add(new VarroaPushedBranchBuildStrategy());
        folder.save();
        return folder;
    }

    /**
     * A create event that names one change request at its current revision, as a
     * forge sends when a pull request opens.
     */
    private static final class ChangeRequestOpenedEvent extends MockSCMHeadEvent {

        private final MockChangeRequestSCMHead head;
        private final MockChangeRequestSCMRevision revision;

        ChangeRequestOpenedEvent(MockSCMController controller, String repository, Integer number) throws IOException {
            super(SCMEvent.Type.CREATED, controller, repository, "CR-" + number,
                    controller.getRevision(repository, "change-request/" + number));
            String target = controller.getTarget(repository, number);
            this.head = new MockChangeRequestSCMHead(number, target);
            this.revision = new MockChangeRequestSCMRevision(head,
                    new MockSCMRevision(new MockSCMHead(target), controller.getRevision(repository, target)),
                    controller.getRevision(repository, "change-request/" + number));
        }

        @Override
        public Map<SCMHead, SCMRevision> heads(SCMSource source) {
            if (super.heads(source).isEmpty()) {
                return Collections.emptyMap();
            }
            return Collections.singletonMap(head, revision);
        }
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
