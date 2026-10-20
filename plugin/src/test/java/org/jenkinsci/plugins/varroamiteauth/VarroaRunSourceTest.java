package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import hudson.EnvVars;
import hudson.model.Action;
import hudson.model.FreeStyleProject;
import hudson.model.InvisibleAction;
import hudson.model.Result;
import hudson.model.Run;
import hudson.model.TaskListener;
import hudson.plugins.git.GitSCM;
import hudson.slaves.DumbSlave;

import java.io.File;
import java.lang.reflect.Constructor;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.util.ArrayList;
import java.util.Collection;
import java.util.List;

import jenkins.branch.Branch;
import jenkins.scm.api.SCMHead;
import jenkins.scm.api.SCMRevision;
import jenkins.scm.api.SCMRevisionAction;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.cps.CpsScmFlowDefinition;
import org.jenkinsci.plugins.workflow.multibranch.BranchJobProperty;
import org.jenkinsci.plugins.workflow.flow.FlowDefinition;
import org.jenkinsci.plugins.workflow.flow.FlowExecution;
import org.jenkinsci.plugins.workflow.flow.FlowExecutionOwner;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.jvnet.hudson.test.TestExtension;

import com.cloudbees.hudson.plugins.folder.AbstractFolder;
import com.cloudbees.hudson.plugins.folder.AbstractFolderProperty;
import com.cloudbees.hudson.plugins.folder.AbstractFolderPropertyDescriptor;
import com.cloudbees.hudson.plugins.folder.Folder;

import jenkins.model.GlobalConfiguration;

/**
 * Tests for the two {@code run.source} emissions (E1 and E2), the source
 * facts they carry, and their interaction with the claim's revision and
 * the changelog fallback.
 */
public class VarroaRunSourceTest {

    /** A folder property that answers one configured library, read back reflectively. */
    public static final class FolderLibraries extends AbstractFolderProperty<AbstractFolder<?>> {

        private final List<LibraryConfigurationStub> libraries = new ArrayList<>();

        public FolderLibraries(String name, String remote) {
            libraries.add(new LibraryConfigurationStub(name,
                    new SCMSourceRetrieverStub(new GitSCMSource(remote))));
        }

        /** The configured libraries, read back reflectively. */
        public List<LibraryConfigurationStub> getLibraries() {
            return libraries;
        }

        /** Registers the descriptor the folder property needs before Folder.addProperty. */
        @TestExtension
        public static final class DescriptorImpl extends AbstractFolderPropertyDescriptor {
        }
    }

    /** Answers the global libraries the resolver reads back reflectively. */
    @TestExtension
    public static final class GlobalLibraries extends GlobalConfiguration {

        private final List<LibraryConfigurationStub> libraries = new ArrayList<>();

        public GlobalLibraries() {
            libraries.add(new LibraryConfigurationStub("shared-lib",
                    new SCMSourceRetrieverStub(new GitSCMSource("https://github.com/example/shared-lib.git"))));
        }

        /** The configured libraries, read back reflectively. */
        public List<LibraryConfigurationStub> getLibraries() {
            return libraries;
        }
    }

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private String repoUrl;
    private String commitId;
    private String secondCommitId;

    @Before
    public void setUp() {
        VarroaRunRecorder.resetStateForTesting();
        BuildEventSink.get().resetForTesting();
        VarroaRunRecorder.setGateOverrideForTesting("1");
    }

    @After
    public void tearDown() {
        VarroaRunRecorder.setGateOverrideForTesting(null);
    }

    private static VarroaIntentAction grantedAction() {
        return new VarroaIntentAction(Uuid7.next(), "", null, System.currentTimeMillis());
    }

    private static List<BuildEvent> byPhase(List<BuildEvent> events, String phase) {
        List<BuildEvent> matched = new ArrayList<>();
        for (BuildEvent e : events) {
            if (phase.equals(e.getPhase())) {
                matched.add(e);
            }
        }
        return matched;
    }

    private static List<BuildEvent> drainEvents() {
        return BuildEventSink.get().drain(400).events;
    }

    // --- E1: the flow's script compiled and the execution started ---

    @Test
    public void e1FiresAfterTheFlowLoadsCarryingTheScriptPathAndOneLibrary() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "library-run");
        job.setDefinition(new SourceFactsFlowDefinition("echo 'hi'", false, "shared-lib", true));

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction()));

        List<BuildEvent> sources = byPhase(drainEvents(), BuildEvent.PHASE_RUN_SOURCE);
        assertEquals("a run with no resolved revision emits E1 alone", 1, sources.size());
        assertEquals("ci/Jenkinsfile", sources.get(0).getDefinitionPath());
        assertEquals(1, sources.get(0).getLibraries().size());
        assertEquals("shared-lib", sources.get(0).getLibraries().get(0).getName());
        assertEquals("1.2.0", sources.get(0).getLibraries().get(0).getVersion());
        assertEquals("the pinned library record declares no revision method",
                "", sources.get(0).getLibraries().get(0).getRevision());
        assertEquals("the global configuration names the repository",
                "https://github.com/example/shared-lib.git",
                sources.get(0).getLibraries().get(0).getRepositoryUrl());
        assertEquals("the github host classifies as github",
                "github", sources.get(0).getLibraries().get(0).getRepositoryForge());
        assertEquals("E1 writes no revision", "", sources.get(0).getScmRevision());
        assertEquals("E1 carries no SCM URL", "", sources.get(0).getScmUrl());
        assertEquals("E1 carries no SCM head", "", sources.get(0).getScmHead());
        assertTrue("E1 carries no pod templates", sources.get(0).getPodTemplates().isEmpty());
    }

    @Test
    public void anUnconfiguredLibraryReportsNoRepository() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "unconfigured-run");
        job.setDefinition(new SourceFactsFlowDefinition("echo 'hi'", false, "other-lib", true));

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction()));

        BuildLibrary library = byPhase(drainEvents(), BuildEvent.PHASE_RUN_SOURCE).get(0).getLibraries().get(0);
        assertEquals("other-lib", library.getName());
        assertEquals("", library.getRepositoryUrl());
        assertEquals("", library.getRepositoryForge());
    }

    @Test
    public void aFolderLibraryShadowsAGlobalOfTheSameNameForAnUntrustedRecord() throws Exception {
        Folder folder = j.jenkins.createProject(Folder.class, "team");
        folder.addProperty(new FolderLibraries("shared-lib", "https://github.com/folder/shared-lib"));
        WorkflowJob job = folder.createProject(WorkflowJob.class, "library-run");
        job.setDefinition(new SourceFactsFlowDefinition("echo 'hi'", false, "shared-lib", false));

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction()));

        BuildLibrary library = byPhase(drainEvents(), BuildEvent.PHASE_RUN_SOURCE).get(0).getLibraries().get(0);
        assertEquals("https://github.com/folder/shared-lib", library.getRepositoryUrl());
        assertEquals("github", library.getRepositoryForge());
    }

    // --- E2: the first node at which the run answers a revision ---

    @Test
    public void aCheckoutRunEmitsASecondSourceEventWithTheScmFacts() throws Exception {
        createRepository();
        // The run allocates this agent, so the second emission carries
        // its pod template.
        DumbSlave agent = j.createSlave("probe-agent", "probe-agent", new EnvVars());
        j.waitOnline(agent);
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "checkout-run");
        job.setDefinition(new CpsFlowDefinition("node('probe-agent') {\n"
                + "  stage('checkout') { git url: '" + repoUrl + "', branch: 'master' }\n"
                + "  stage('build') { echo 'built' }\n"
                + "}", true));

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction()));

        List<BuildEvent> events = drainEvents();
        List<BuildEvent> sources = byPhase(events, BuildEvent.PHASE_RUN_SOURCE);
        assertEquals(2, sources.size());
        BuildEvent e1 = sources.get(0);
        BuildEvent e2 = sources.get(1);
        assertFalse("the second emission carries the allocated pod template",
                e2.getPodTemplates().isEmpty());
        assertEquals("probe-agent", e2.getPodTemplates().get(0).getName());
        assertTrue("E1 holds the lower sequence", e1.getSeq() < e2.getSeq());

        assertTrue("the branch names the checked-out branch", e2.getBranch().endsWith("master"));
        assertTrue("the SCM head names the same branch", e2.getScmHead().endsWith("master"));
        assertEquals(commitId, e2.getScmRevision());
        assertTrue("the SCM URL names the sample repository", e2.getScmUrl().startsWith("file:"));
        assertEquals("a file: remote names no forge", "", e2.getScmForge());

        assertTrue("a node after the second emission still emits its stage event",
                sawStage(events, "build"));
    }

    @Test
    public void aMultibranchChildResolvesItsBranchAndHashFromTheRevisionAction() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "child-run");
        job.setDefinition(new SourceFactsFlowDefinition("echo 'hi'", true));

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction()));

        List<BuildEvent> sources = byPhase(drainEvents(), BuildEvent.PHASE_RUN_SOURCE);
        assertEquals(2, sources.size());
        for (BuildEvent source : sources) {
            assertTrue("no allocation, so no source event carries a pod template",
                    source.getPodTemplates().isEmpty());
        }
        BuildEvent e2 = sources.get(1);
        assertEquals("main", e2.getBranch());
        assertEquals("main", e2.getScmHead());
        assertEquals("abc123", e2.getScmRevision());
        // This case drives a revision action on a plain pipeline job, so
        // E1 has no multibranch name to carry.
        assertEquals("E1 carries no SCM head even with a revision action",
                "", sources.get(0).getScmHead());
        assertEquals("E1's branch is empty: the job is a plain pipeline, not a multibranch child",
                "", sources.get(0).getBranch());
    }

    /**
     * A pull-request merge build checks out the local merge commit. The
     * record must carry the pull-request head, which the forge knows, and
     * never the merge commit.
     */
    @Test
    public void aPullRequestMergeRunReportsTheHeadHashNotTheMergeCommit() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "pr-merge-run");
        VarroaRunRecorderChangeRequestTest.PullRequestHead head = new VarroaRunRecorderChangeRequestTest.PullRequestHead(
                "PR-12", "12", new SCMHead("main"), "fix/cache");
        SCMRevision revision = new VarroaRunRecorderChangeRequestTest.GitHubStyleRevision(
                head, new FakeRevision(new SCMHead("main"), "base111"), "head222", "merge333");
        job.setDefinition(new SourceFactsFlowDefinition("echo 'hi'", revision));

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction()));

        List<BuildEvent> sources = byPhase(drainEvents(), BuildEvent.PHASE_RUN_SOURCE);
        BuildEvent e2 = sources.get(sources.size() - 1);
        assertEquals("head222", e2.getScmRevision());
        assertEquals("PR-12", e2.getScmHead());
    }

    // --- E3: the first allocation ---

    @Test
    public void anScmDefinedRunReportsItsPodTemplateAtTheFirstAllocation() throws Exception {
        // The agent carries a label of its own, and the script requests
        // that label. The workspace action holds the assigned labels minus
        // the node's own name label, so a label equal to the node name
        // reads empty.
        DumbSlave agent = j.createSlave("probe-agent", "probe-pool", new EnvVars());
        j.waitOnline(agent);
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "scm-defined-run");
        job.setDefinition(new SourceFactsFlowDefinition("node('probe-pool') { echo 'hi' }", true));

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction()));

        List<BuildEvent> sources = byPhase(drainEvents(), BuildEvent.PHASE_RUN_SOURCE);
        assertEquals(3, sources.size());
        BuildEvent e1 = sources.get(0);
        BuildEvent e2 = sources.get(1);
        BuildEvent e3 = sources.get(2);
        assertTrue("E1 holds the lowest sequence", e1.getSeq() < e2.getSeq());
        assertTrue("E2 holds the middle sequence", e2.getSeq() < e3.getSeq());
        assertTrue("the second emission carries no pod template yet", e2.getPodTemplates().isEmpty());
        assertEquals(1, e3.getPodTemplates().size());
        assertEquals("probe-agent", e3.getPodTemplates().get(0).getName());
        assertEquals("the label names the allocated node's labels", "probe-pool",
                e3.getPodTemplates().get(0).getLabel());
        assertEquals("the third emission carries no change list", 0, e3.getChanges().size());
        assertEquals("E3 re-reads the revision", e2.getScmRevision(), e3.getScmRevision());
    }

    @Test
    public void everyAllocatedTemplateReachesTheRecordEvenAfterItsAgentLeaves() throws Exception {
        DumbSlave first = j.createSlave("agent-a", "pool-a", new EnvVars());
        DumbSlave second = j.createSlave("agent-b", "pool-b", new EnvVars());
        j.waitOnline(first);
        j.waitOnline(second);
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "two-agent-run");
        job.setDefinition(new SourceFactsFlowDefinition(
                "sleep 1\nnode('pool-a') { echo 'a' }\n"
                        + "sleep 5\n"
                        + "node('pool-b') { echo 'b' }\n", true));

        WorkflowRun run = job.scheduleBuild2(0, grantedAction()).waitForStart();
        j.waitForMessage("Sleeping for 5 sec", run);
        // The first agent leaves before the run completes.
        j.jenkins.removeNode(first);
        j.assertBuildStatusSuccess(j.waitForCompletion(run));

        List<BuildEvent> sources = byPhase(drainEvents(), BuildEvent.PHASE_RUN_SOURCE);
        assertEquals("E1, E2, and one emission for each new template", 4, sources.size());
        BuildEvent last = sources.get(sources.size() - 1);
        for (BuildEvent source : sources) {
            assertTrue("the last emission holds the highest seq", source.getSeq() <= last.getSeq());
        }
        assertEquals(2, last.getPodTemplates().size());
        assertEquals("agent-a", last.getPodTemplates().get(0).getName());
        assertEquals("pool-a", last.getPodTemplates().get(0).getLabel());
        assertEquals("agent-b", last.getPodTemplates().get(1).getName());
        assertEquals("pool-b", last.getPodTemplates().get(1).getLabel());
        assertEquals("an allocation emission carries no change list", 0, last.getChanges().size());
    }

    @Test
    public void aSecondAllocationOfTheSameTemplateEmitsNothing() throws Exception {
        DumbSlave agent = j.createSlave("agent-a", "pool-a", new EnvVars());
        j.waitOnline(agent);
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "same-agent-run");
        job.setDefinition(new SourceFactsFlowDefinition(
                "sleep 1\nnode('pool-a') { echo 'a' }\nnode('pool-a') { echo 'again' }\n", true));

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction()));

        List<BuildEvent> sources = byPhase(drainEvents(), BuildEvent.PHASE_RUN_SOURCE);
        assertEquals("the second allocation adds no template", 3, sources.size());
        assertEquals(1, sources.get(2).getPodTemplates().size());
    }

    @Test
    public void aKubernetesAgentReportsItsTemplateNameAndImage() throws Exception {
        KubernetesSlave pod = new KubernetesSlave("golang-x1y2z", new FakePodTemplate());
        BuildPodTemplate live = VarroaRunRecorder.podTemplateFor("golang-x1y2z", "golang", pod);
        assertEquals("a kubernetes agent answers its template name", "golang", live.getName());
        assertEquals("golang", live.getLabel());
        assertEquals("the image comes from the template's first container", "golang:1.23", live.getImage());

        BuildPodTemplate gone = VarroaRunRecorder.podTemplateFor("golang-x1y2z", "golang", null);
        assertEquals("a gone agent answers its node name", "golang-x1y2z", gone.getName());
        assertEquals("a gone agent answers no image", "", gone.getImage());
    }

    /** A stand-in whose simple name matches the kubernetes plugin's agent class. */
    public static class KubernetesSlave extends hudson.model.Slave {
        private final transient FakePodTemplate template;

        KubernetesSlave(String name, FakePodTemplate template) throws Exception {
            super(name, "/tmp", new hudson.slaves.JNLPLauncher());
            this.template = template;
        }

        public FakePodTemplate getTemplateOrNull() {
            return template;
        }
    }

    /** A stand-in pod template with one container. */
    public static class FakePodTemplate {
        public String getName() {
            return "golang";
        }

        public String getImage() {
            return "";
        }

        public List<FakeContainer> getContainers() {
            return List.of(new FakeContainer());
        }
    }

    /** A stand-in container template. */
    public static class FakeContainer {
        public String getImage() {
            return "golang:1.23";
        }
    }

    // --- A run that answers no revision at all ---

    @Test
    public void aRunWithNoScmFactsStillReportsItsPodTemplate() throws Exception {
        DumbSlave agent = j.createSlave("inline-agent", "inline-pool", new EnvVars());
        j.waitOnline(agent);
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "no-scm-agent-run");
        job.setDefinition(new CpsFlowDefinition("node('inline-pool') { echo 'hi' }", true));

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction()));

        List<BuildEvent> events = drainEvents();
        List<BuildEvent> sources = byPhase(events, BuildEvent.PHASE_RUN_SOURCE);
        assertEquals("E1, then the completion-time emission; no revision means no E2", 2, sources.size());
        BuildEvent last = sources.get(1);
        assertEquals(1, last.getPodTemplates().size());
        assertEquals("inline-agent", last.getPodTemplates().get(0).getName());
        assertEquals("inline-pool", last.getPodTemplates().get(0).getLabel());
        assertTrue("no revision reaches the record", last.getScmRevision().isEmpty());
        BuildEvent completed = byPhase(events, BuildEvent.PHASE_COMPLETED).get(0);
        assertTrue("the emission orders before the completion", last.getSeq() < completed.getSeq());
    }

    @Test
    public void aRunWithNoScmFactsEmitsOneSourceEventAndThrowsNothing() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "no-scm-run");
        job.setDefinition(new CpsFlowDefinition("node { echo 'hi' }", true));

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction()));

        List<BuildEvent> events = drainEvents();
        assertEquals("no revision, so E1 alone", 1, byPhase(events, BuildEvent.PHASE_RUN_SOURCE).size());
        assertEquals(1, byPhase(events, BuildEvent.PHASE_COMPLETED).size());
    }

    @Test
    public void aFreestyleRunEmitsNoSourceEvent() throws Exception {
        FreeStyleProject project = j.createFreeStyleProject("freestyle-run");

        j.assertBuildStatusSuccess(project.scheduleBuild2(0, grantedAction()));

        assertEquals(0, byPhase(drainEvents(), BuildEvent.PHASE_RUN_SOURCE).size());
    }

    // --- The run's own checkout, not a library checkout ---

    @Test
    public void anInlineRunSkipsALibraryCheckoutForItsScmFacts() throws Exception {
        String[] library = newRepository("library-repo");
        String[] app = newRepository("app-repo");
        Folder folder = j.jenkins.createProject(Folder.class, "inline-team");
        folder.addProperty(new FolderLibraries("shared-lib", library[0]));
        WorkflowJob job = folder.createProject(WorkflowJob.class, "inline-run");
        job.setDefinition(new SourceFactsFlowDefinition("node {\n"
                + "  git url: '" + library[0] + "', branch: 'master'\n"
                + "  git url: '" + app[0] + "', branch: 'master'\n"
                + "  echo 'built'\n"
                + "}", false, "shared-lib", false, null));

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction()));

        List<BuildEvent> sources = byPhase(drainEvents(), BuildEvent.PHASE_RUN_SOURCE);
        assertEquals(2, sources.size());
        BuildEvent e2 = sources.get(1);
        assertEquals("the inline run reports its own checkout, not the library's",
                app[0], e2.getScmUrl());
        assertEquals(app[1], e2.getScmRevision());
    }

    @Test
    public void anScmDefinedRunReportsItsOwnRepositoryAfterAnotherCheckout() throws Exception {
        String[] library = newRepository("library-repo");
        String[] app = newRepository("app-repo");
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "scm-run");
        job.setDefinition(new SourceFactsFlowDefinition("node {\n"
                + "  git url: '" + library[0] + "', branch: 'master'\n"
                + "  git url: '" + app[0] + "', branch: 'master'\n"
                + "  echo 'built'\n"
                + "}", false, "shared-lib", false, new GitSCM(app[0])));

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction()));

        List<BuildEvent> sources = byPhase(drainEvents(), BuildEvent.PHASE_RUN_SOURCE);
        BuildEvent e2 = sources.get(sources.size() - 1);
        assertEquals(app[0], e2.getScmUrl());
        assertEquals(app[1], e2.getScmRevision());
    }

    @Test
    public void anScmDefinedRunReportsItsRepositoryWhenOnlyALibraryWasCheckedOut() throws Exception {
        String[] library = newRepository("library-repo");
        String[] app = newRepository("app-repo");
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "library-only-run");
        job.setDefinition(new SourceFactsFlowDefinition("node {\n"
                + "  git url: '" + library[0] + "', branch: 'master'\n"
                + "  echo 'built'\n"
                + "}", false, "shared-lib", false, new GitSCM(app[0])));

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction()));

        BuildEvent completed = byPhase(drainEvents(), BuildEvent.PHASE_COMPLETED).get(0);
        assertEquals("the job SCM names the run's repository even with no checkout of it",
                app[0], completed.getScmUrl());
    }

    @Test
    public void aCompletedRunCarriesItsJobRemoteWithoutCredentials() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "credentialed-remote-run");
        job.setDefinition(new CpsScmFlowDefinition(
                new GitSCM("https://user:token@127.0.0.1:1/example/app.git"), "Jenkinsfile"));

        j.assertBuildStatus(Result.FAILURE, job.scheduleBuild2(0, grantedAction()));

        BuildEvent completed = byPhase(drainEvents(), BuildEvent.PHASE_COMPLETED).get(0);
        assertEquals("https://127.0.0.1:1/example/app.git", completed.getScmUrl());
        assertFalse("the SCM URL must not carry the token", completed.getScmUrl().contains("token"));
    }

    @Test
    public void aMultibranchChildReportsItsBranchScmRemote() throws Exception {
        String[] app = newRepository("branch-repo");
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "branch-run");
        job.addProperty(branchProperty(new Branch("src", new SCMHead("main"), new GitSCM(app[0]), List.of())));
        job.setDefinition(new SourceFactsFlowDefinition("echo 'hi'", true));

        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction()));

        List<BuildEvent> sources = byPhase(drainEvents(), BuildEvent.PHASE_RUN_SOURCE);
        assertEquals(2, sources.size());
        assertEquals(app[0], sources.get(1).getScmUrl());
        assertEquals("a branch under no folder names no forge", "", sources.get(1).getScmForge());
    }

    // --- The two existing revision writers survive both emissions ---

    @Test
    public void startedFallsBackToTheClaimRevisionAndCompletedKeepsTheChangelogFallback() throws Exception {
        createRepository();
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "claim-revision-run");
        job.setDefinition(new CpsFlowDefinition(
                "node { git url: '" + repoUrl + "', branch: 'master' }", true));
        VarroaIntentAction action = grantedAction();
        action.setRevision("claim-revision");
        j.assertBuildStatusSuccess(job.scheduleBuild2(0, action));
        drainEvents();

        // A first build records no changelog, because the sample repository
        // holds no earlier build to compare against. The second commit gives
        // the second build a changelog, and its fallback a value.
        commitAgain();
        VarroaIntentAction secondAction = grantedAction();
        secondAction.setRevision("claim-revision");
        j.assertBuildStatusSuccess(job.scheduleBuild2(0, secondAction));

        List<BuildEvent> events = drainEvents();
        List<BuildEvent> started = byPhase(events, BuildEvent.PHASE_STARTED);
        List<BuildEvent> completed = byPhase(events, BuildEvent.PHASE_COMPLETED);
        assertEquals(1, started.size());
        assertEquals("the claim's revision survives the source emissions",
                "claim-revision", started.get(0).getScmRevision());
        assertEquals(1, completed.size());
        assertEquals("the changelog fallback survives the source emissions",
                secondCommitId, completed.get(0).getScmRevision());
    }

    @Test
    public void aPullRequestMergeRunCompletesWithTheHeadHashNotTheNewestChangelogCommit() throws Exception {
        createRepository();
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "pr-merge-completed");
        VarroaRunRecorderChangeRequestTest.PullRequestHead head = new VarroaRunRecorderChangeRequestTest.PullRequestHead(
                "PR-12", "12", new SCMHead("main"), "fix/cache");
        SCMRevision revision = new VarroaRunRecorderChangeRequestTest.GitHubStyleRevision(
                head, new FakeRevision(new SCMHead("main"), "base111"), "head222", "merge333");
        job.setDefinition(new SourceFactsFlowDefinition(
                "node { git url: '" + repoUrl + "', branch: 'master' }", revision));
        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction()));
        drainEvents();

        // The second commit gives the second build a changelog whose newest
        // entry is not the head hash. The completion must still send the head.
        commitAgain();
        j.assertBuildStatusSuccess(job.scheduleBuild2(0, grantedAction()));

        List<BuildEvent> completed = byPhase(drainEvents(), BuildEvent.PHASE_COMPLETED);
        assertEquals(1, completed.size());
        assertEquals("head222", completed.get(0).getScmRevision());
        assertNotEquals(secondCommitId, completed.get(0).getScmRevision());
    }

    @Test
    public void startedCarriesTheForgeRevisionOverTheClaimRevision() throws Exception {
        createRepository();
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "forge-revision-run");
        job.setDefinition(new CpsFlowDefinition(
                "node { git url: '" + repoUrl + "', branch: 'master' }", true));
        VarroaIntentAction action = grantedAction();
        action.setRevision("sha256:abc");
        action.setForgeRevision("head222");
        j.assertBuildStatusSuccess(job.scheduleBuild2(0, action));

        List<BuildEvent> started = byPhase(drainEvents(), BuildEvent.PHASE_STARTED);
        assertEquals(1, started.size());
        assertEquals("the forge revision replaces the claim revision",
                "head222", started.get(0).getScmRevision());
        assertEquals("the intent key keeps the claim revision", action.getIntentKey(),
                started.get(0).getIntentKey());
    }

    /** The branch job property has a package-private constructor, so the test builds it reflectively. */
    private static BranchJobProperty branchProperty(Branch branch) throws Exception {
        Constructor<BranchJobProperty> constructor = BranchJobProperty.class.getDeclaredConstructor(Branch.class);
        constructor.setAccessible(true);
        return constructor.newInstance(branch);
    }

    private static boolean sawStage(List<BuildEvent> events, String name) {
        for (BuildEvent event : byPhase(events, BuildEvent.PHASE_STAGE)) {
            for (BuildStage stage : event.getStages()) {
                if (name.equals(stage.getName())) {
                    return true;
                }
            }
        }
        return false;
    }

    /**
     * Builds the sample repository beside the test home. The pipeline receives
     * the repository as a {@code file:} URL, which encodes the space in the
     * home path that the git step refuses.
     */
    private void createRepository() throws Exception {
        String[] repository = newRepository("sample-repo");
        repoUrl = repository[0];
        commitId = repository[1];
    }

    /** Builds a repository under the test home. Answers its {@code file:} URL and its head commit. */
    private String[] newRepository(String dirName) throws Exception {
        File repo = new File(j.jenkins.getRootDir(), dirName);
        repo.mkdirs();
        git(repo, "init", "-b", "master");
        git(repo, "config", "user.email", "varroa@example.com");
        git(repo, "config", "user.name", "Varroa Test");
        commitFile(repo, "hello\n", "the first commit");
        return new String[] {repo.toURI().toString(), git(repo, "rev-parse", "HEAD").trim()};
    }

    private void commitAgain() throws Exception {
        File repo = new File(j.jenkins.getRootDir(), "sample-repo");
        commitFile(repo, "hello again\n", "the second commit");
        secondCommitId = git(repo, "rev-parse", "HEAD").trim();
    }

    private static void commitFile(File repo, String content, String message) throws Exception {
        Files.write(new File(repo, "README.md").toPath(), content.getBytes(StandardCharsets.UTF_8));
        git(repo, "add", "README.md");
        git(repo, "commit", "-m", message);
    }

    private static String git(File directory, String... args) throws Exception {
        List<String> command = new ArrayList<>();
        command.add("git");
        command.addAll(List.of(args));
        ProcessBuilder builder = new ProcessBuilder(command).directory(directory).redirectErrorStream(true);
        Process process = builder.start();
        String output = new String(process.getInputStream().readAllBytes(), StandardCharsets.UTF_8);
        assertEquals("git " + String.join(" ", args) + " failed: " + output, 0, process.waitFor());
        return output;
    }
}

/**
 * A flow definition double that declares a script path and attaches the
 * actions a real run carries: one libraries action, and, when asked, one
 * SCM revision action.
 */
final class SourceFactsFlowDefinition extends FlowDefinition {

    private final String script;
    private final boolean attachRevisionAction;
    private final String libraryName;
    private final Boolean libraryTrusted;
    private final Object scm;
    private final SCMRevision revision;

    SourceFactsFlowDefinition(String script, SCMRevision revision) {
        this(script, true, "shared-lib", null, null, revision);
    }

    SourceFactsFlowDefinition(String script, boolean attachRevisionAction) {
        this(script, attachRevisionAction, "shared-lib", null, null);
    }

    SourceFactsFlowDefinition(String script, boolean attachRevisionAction, String libraryName, Boolean libraryTrusted) {
        this(script, attachRevisionAction, libraryName, libraryTrusted, null);
    }

    /** A definition whose {@code scm} is the job's SCM. A null {@code scm} is an inline script. */
    SourceFactsFlowDefinition(String script, boolean attachRevisionAction, String libraryName,
            Boolean libraryTrusted, Object scm) {
        this(script, attachRevisionAction, libraryName, libraryTrusted, scm, null);
    }

    SourceFactsFlowDefinition(String script, boolean attachRevisionAction, String libraryName,
            Boolean libraryTrusted, Object scm, SCMRevision revision) {
        this.script = script;
        this.attachRevisionAction = attachRevisionAction;
        this.libraryName = libraryName;
        this.libraryTrusted = libraryTrusted;
        this.scm = scm;
        this.revision = revision;
    }

    /** The Jenkinsfile path a consumer repository's CpsScmFlowDefinition answers. */
    public String getScriptPath() {
        return "ci/Jenkinsfile";
    }

    /** The job SCM a CpsScmFlowDefinition answers. Null for an inline script. */
    public Object getScm() {
        return scm;
    }

    @Override
    public FlowExecution create(FlowExecutionOwner owner, TaskListener listener, List<? extends Action> actions)
            throws Exception {
        Run<?, ?> run = (Run<?, ?>) owner.getExecutable();
        run.addAction(new LibrariesAction(libraryName, libraryTrusted));
        if (attachRevisionAction) {
            SCMRevision attached = revision != null ? revision : new FakeRevision(new SCMHead("main"), "abc123");
            run.addAction(new SCMRevisionAction(attached));
        }
        return new CpsFlowDefinition(script, true).create(owner, listener, actions);
    }
}

/**
 * A stand-in for the global-library action. The resolver matches this
 * class by its simple name, so the name must stay {@code LibrariesAction}.
 */
final class LibrariesAction extends InvisibleAction {

    private final List<LibraryRecord> libraries = new ArrayList<>();

    LibrariesAction(String name, Boolean trusted) {
        libraries.add(new LibraryRecord(name, "1.2.0", trusted));
    }

    /** The records the resolver reads back reflectively. */
    public Collection<LibraryRecord> getLibraries() {
        return libraries;
    }
}

/** A stand-in for a library configuration. */
final class LibraryConfigurationStub {

    private final String name;
    private final SCMSourceRetrieverStub retriever;

    LibraryConfigurationStub(String name, SCMSourceRetrieverStub retriever) {
        this.name = name;
        this.retriever = retriever;
    }

    public String getName() {
        return name;
    }

    public SCMSourceRetrieverStub getRetriever() {
        return retriever;
    }
}

/** A stand-in for a library retriever, which answers its source. */
final class SCMSourceRetrieverStub {

    private final GitSCMSource scm;

    SCMSourceRetrieverStub(GitSCMSource scm) {
        this.scm = scm;
    }

    public GitSCMSource getScm() {
        return scm;
    }
}

/**
 * A stand-in for an SCM source. Its simple name is the one the resolver
 * matches for the source class. The tests use the Git source name.
 */
final class GitSCMSource {

    private final String remote;

    GitSCMSource(String remote) {
        this.remote = remote;
    }

    public String getRemote() {
        return remote;
    }
}

/**
 * A stand-in for a global-library record. It declares no {@code getRevision},
 * which matches the pinned record's own shape.
 */
final class LibraryRecord {

    private final String name;
    private final String version;
    private final Boolean trusted;

    LibraryRecord(String name, String version, Boolean trusted) {
        this.name = name;
        this.version = version;
        this.trusted = trusted;
    }

    public String getName() {
        return name;
    }

    public String getVersion() {
        return version;
    }

    public Boolean isTrusted() {
        return trusted;
    }
}

/** A stand-in for the git plugin's revision implementation. */
final class FakeRevision extends SCMRevision {

    private final String hash;

    FakeRevision(SCMHead head, String hash) {
        super(head);
        this.hash = hash;
    }

    /** The hash accessor the git plugin's own implementation declares. */
    public String getHash() {
        return hash;
    }

    @Override
    public boolean isDeterministic() {
        return true;
    }

    @Override
    public boolean equals(Object o) {
        return o instanceof FakeRevision && hash.equals(((FakeRevision) o).hash);
    }

    @Override
    public int hashCode() {
        return hash.hashCode();
    }

    @Override
    public String toString() {
        return hash;
    }
}
