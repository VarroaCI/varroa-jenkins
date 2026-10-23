package org.jenkinsci.plugins.varroamiteauth.template;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertTrue;

import com.cloudbees.hudson.plugins.folder.Folder;
import com.cloudbees.plugins.credentials.CredentialsScope;
import com.cloudbees.plugins.credentials.SystemCredentialsProvider;
import com.cloudbees.plugins.credentials.impl.UsernamePasswordCredentialsImpl;

import hudson.model.Result;

import io.jenkins.plugins.casc.misc.ConfiguredWithCode;
import io.jenkins.plugins.casc.misc.JenkinsConfiguredWithCodeRule;

import java.io.File;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;

import jenkins.branch.BranchSource;

import org.jenkinsci.plugins.varroamiteauth.ActivityEvent;
import org.jenkinsci.plugins.varroamiteauth.ActivitySink;
import org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;

/**
 * Tests for {@link VarroaMarkerFlowDefinition} (tasks 5.12-5.12e): the
 * JenkinsRule harness is one real {@link WorkflowMultiBranchProject} over
 * a mock {@link TestScmSource}, a store seeded from JCasC (task 5.3's
 * fixture format), and a "commit a marker on a branch" helper
 * ({@link TestScmSource#commit}).
 */
public class VarroaMarkerFlowDefinitionTest {

    static {
        // Fixture-only: the credential-scope test's git repository is a
        // local directory, which GitSCM refuses by default as a
        // security guard against an untrusted-input remote URL. A real
        // fleet always uses a remote git host, so this guard stays on
        // in production; the test fixture opts a local path back in.
        System.setProperty("hudson.plugins.git.GitSCM.ALLOW_LOCAL_CHECKOUT", "true");
    }

    @Rule
    public JenkinsConfiguredWithCodeRule j = new JenkinsConfiguredWithCodeRule();

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private static final String VALID_MARKER =
            "apiVersion: varroa.dev/v1\n"
            + "kind: Pipeline\n"
            + "template: go-service\n"
            + "templateVersion: \"1.0.0\"\n"
            + "values:\n"
            + "  goVersion: \"1.24\"\n";

    // ---- Harness helpers ----

    private WorkflowJob indexBranch(TestScmSource source) throws Exception {
        return indexBranch(j.jenkins, "mp-" + System.nanoTime(), source);
    }

    private WorkflowJob indexBranch(Folder parent, String name,
            TestScmSource source) throws Exception {
        WorkflowMultiBranchProject mp = parent.createProject(WorkflowMultiBranchProject.class, name);
        mp.setProjectFactory(new VarroaTemplateBranchProjectFactory());
        mp.getSourcesList().add(new BranchSource(source));
        mp.scheduleBuild2(0).getFuture().get();
        WorkflowJob job = mp.getItem("main");
        assertNotNull("branch indexing must create the \"main\" job", job);
        return job;
    }

    private WorkflowJob indexBranch(jenkins.model.Jenkins parent, String name, TestScmSource source) throws Exception {
        WorkflowMultiBranchProject mp = parent.createProject(WorkflowMultiBranchProject.class, name);
        mp.setProjectFactory(new VarroaTemplateBranchProjectFactory());
        mp.getSourcesList().add(new BranchSource(source));
        mp.scheduleBuild2(0).getFuture().get();
        WorkflowJob job = mp.getItem("main");
        assertNotNull("branch indexing must create the \"main\" job", job);
        return job;
    }

    private static void addTemplate(VarroaPipelineTemplateStore.TemplateEntry entry) {
        VarroaPipelineTemplateStore store = VarroaPipelineTemplateStore.get();
        List<VarroaPipelineTemplateStore.TemplateEntry> merged = new ArrayList<>(store.getTemplates());
        merged.add(entry);
        store.setTemplates(merged);
    }

    private String initGitRepo(String jenkinsfileContent) throws Exception {
        File dir = tmp.newFolder("git-template-repo-" + System.nanoTime());
        run(dir, "git", "init", "-q", "-b", "main");
        run(dir, "git", "config", "user.email", "test@example.com");
        run(dir, "git", "config", "user.name", "Test");
        Files.writeString(dir.toPath().resolve("Jenkinsfile"), jenkinsfileContent, StandardCharsets.UTF_8);
        run(dir, "git", "add", "Jenkinsfile");
        run(dir, "git", "commit", "-q", "-m", "init");
        return dir.getAbsolutePath();
    }

    private static void run(File cwd, String... cmd) throws IOException, InterruptedException {
        Process p = new ProcessBuilder(cmd).directory(cwd).redirectErrorStream(true).start();
        String output = new String(p.getInputStream().readAllBytes(), StandardCharsets.UTF_8);
        int code = p.waitFor();
        if (code != 0) {
            throw new IOException("command " + Arrays.toString(cmd) + " failed (" + code + "): " + output);
        }
    }

    // ---- 5.12: a valid marker builds ----

    @Test
    @ConfiguredWithCode("pipeline-marker-flow-templates.yml")
    public void aValidMarkerBuilds() throws Exception {
        TestScmSource source = new TestScmSource("src");
        source.commit("r1", VarroaMarkerFlowDefinition.MARKER_PATH, VALID_MARKER);
        source.setTip("r1");
        WorkflowJob job = indexBranch(source);

        WorkflowRun run = j.buildAndAssertSuccess(job);

        VarroaTemplateValuesAction action = run.getAction(VarroaTemplateValuesAction.class);
        assertNotNull(action);
        assertFalse(action.isRejected());
        assertEquals("go-service", action.getTemplateName());
        assertEquals("1.0.0", action.getTemplateVersion());
        assertEquals(VarroaMarkerFlowDefinition.MARKER_PATH, action.getMarkerPath());
        assertEquals("r1", action.getMarkerRevision());
        assertEquals("1.24", action.getValues().get("goVersion"));
        j.assertLogContains("template ran goVersion=1.24", run);
    }

    // ---- 5.12a: rejection tests ----

    @Test
    @ConfiguredWithCode("pipeline-marker-flow-templates.yml")
    public void anInvalidValueFailsNamingTheProperty() throws Exception {
        TestScmSource source = new TestScmSource("src");
        String marker = "apiVersion: varroa.dev/v1\n"
                + "kind: Pipeline\n"
                + "template: go-service\n"
                + "values:\n"
                + "  goVersion: \"1.19\"\n";
        source.commit("r1", VarroaMarkerFlowDefinition.MARKER_PATH, marker);
        source.setTip("r1");
        WorkflowJob job = indexBranch(source);

        ActivitySink.get().drain(1000);
        WorkflowRun run = j.buildAndAssertStatus(Result.FAILURE, job);

        j.assertLogContains("goVersion", run);
        j.assertLogContains("1.19", run);
        j.assertLogContains("1.23", run);
        j.assertLogContains("1.24", run);

        VarroaTemplateValuesAction action = run.getAction(VarroaTemplateValuesAction.class);
        assertNotNull(action);
        assertTrue(action.isRejected());
        assertEquals("go-service", action.getTemplateName());

        ActivitySink.DrainResult drained = ActivitySink.get().drain(1000);
        boolean sawRejection = false;
        for (ActivityEvent e : drained.events) {
            if ("pipeline.marker.rejected".equals(e.getType())) {
                sawRejection = true;
                assertTrue(e.getMessage().contains("goVersion"));
            }
        }
        assertTrue("task 5.11: pipeline.marker.rejected must be emitted", sawRejection);
    }

    @Test
    @ConfiguredWithCode("pipeline-marker-flow-templates.yml")
    public void anUndeliveredTemplateFailsNamingTheDeliveredVersions() throws Exception {
        TestScmSource source = new TestScmSource("src");
        String marker = "apiVersion: varroa.dev/v1\n"
                + "kind: Pipeline\n"
                + "template: go-service\n"
                + "templateVersion: \"9\"\n"
                + "values:\n"
                + "  goVersion: \"1.24\"\n";
        source.commit("r1", VarroaMarkerFlowDefinition.MARKER_PATH, marker);
        source.setTip("r1");
        WorkflowJob job = indexBranch(source);

        WorkflowRun run = j.buildAndAssertStatus(Result.FAILURE, job);

        j.assertLogContains("go-service", run);
        j.assertLogContains("1.0.0", run);
    }

    // ---- 5.12b: isolation tests ----

    @Test
    @ConfiguredWithCode("pipeline-marker-flow-templates.yml")
    public void aTemplateScopedToAnotherFolderDoesNotResolve() throws Exception {
        TestScmSource source = new TestScmSource("src");
        String marker = "apiVersion: varroa.dev/v1\n"
                + "kind: Pipeline\n"
                + "template: tenant-a-only\n";
        source.commit("r1", VarroaMarkerFlowDefinition.MARKER_PATH, marker);
        source.setTip("r1");
        Folder folder = j.jenkins.createProject(Folder.class, "tenant-b");
        WorkflowJob job = indexBranch(folder, "repo", source);

        WorkflowRun run = j.buildAndAssertStatus(Result.FAILURE, job);

        j.assertLogContains("tenant-a-only", run);
        j.assertLogContains("tenant-b", run);
    }

    @Test
    @ConfiguredWithCode("pipeline-marker-flow-templates.yml")
    public void aValueHoldingGroovySyntaxStaysInert() throws Exception {
        TestScmSource source = new TestScmSource("src");
        String marker = "apiVersion: varroa.dev/v1\n"
                + "kind: Pipeline\n"
                + "template: go-service\n"
                + "values:\n"
                + "  goVersion: \"1.24\"\n"
                + "  message: \"; sh 'id' //\"\n";
        source.commit("r1", VarroaMarkerFlowDefinition.MARKER_PATH, marker);
        source.setTip("r1");
        WorkflowJob job = indexBranch(source);

        WorkflowRun run = j.buildAndAssertSuccess(job);

        j.assertLogContains("message=; sh 'id' //", run);
    }

    // ---- 5.12c: the fallback test ----

    @Test
    @ConfiguredWithCode("pipeline-marker-flow-templates.yml")
    public void aSourceWithNoLightweightFileSystemStillBuildsThroughCheckout() throws Exception {
        TestScmSource source = new TestScmSource("src");
        source.setLightweightSupported(false);
        source.commit("r1", VarroaMarkerFlowDefinition.MARKER_PATH, VALID_MARKER);
        source.setTip("r1");
        WorkflowJob job = indexBranch(source);

        WorkflowRun run = j.buildAndAssertSuccess(job);

        assertTrue("the checkout fallback must have run", source.wasCheckedOut());
        VarroaTemplateValuesAction action = run.getAction(VarroaTemplateValuesAction.class);
        assertNotNull(action);
        assertFalse(action.isRejected());
    }

    // ---- oversized marker: the read is bounded before the whole file is
    // allocated (D6, D8, pipeline-marker-file) ----

    /** One byte past {@link VarroaMarkerValidator#MAX_MARKER_BYTES}. */
    private static final String OVERSIZED_MARKER = "a".repeat(VarroaMarkerValidator.MAX_MARKER_BYTES + 1);

    @Test
    @ConfiguredWithCode("pipeline-marker-flow-templates.yml")
    public void anOversizedMarkerReadThroughTheLightweightFileSystemIsRejected() throws Exception {
        TestScmSource source = new TestScmSource("src");
        source.commit("r1", VarroaMarkerFlowDefinition.MARKER_PATH, OVERSIZED_MARKER);
        source.setTip("r1");
        WorkflowJob job = indexBranch(source);

        WorkflowRun run = j.buildAndAssertStatus(Result.FAILURE, job);

        j.assertLogContains("exceeds " + VarroaMarkerValidator.MAX_MARKER_BYTES + " bytes", run);
        assertFalse("an oversized lightweight marker is already known invalid; no checkout must run",
                source.wasCheckedOut());
    }

    @Test
    @ConfiguredWithCode("pipeline-marker-flow-templates.yml")
    public void anOversizedMarkerReadThroughTheCheckoutFallbackIsRejected() throws Exception {
        TestScmSource source = new TestScmSource("src");
        source.setLightweightSupported(false);
        source.commit("r1", VarroaMarkerFlowDefinition.MARKER_PATH, OVERSIZED_MARKER);
        source.setTip("r1");
        WorkflowJob job = indexBranch(source);

        WorkflowRun run = j.buildAndAssertStatus(Result.FAILURE, job);

        assertTrue("the checkout fallback must have run", source.wasCheckedOut());
        j.assertLogContains("exceeds " + VarroaMarkerValidator.MAX_MARKER_BYTES + " bytes", run);
    }

    // ---- 5.12d: credential-scope tests ----

    @Test
    @ConfiguredWithCode("pipeline-marker-flow-templates.yml")
    public void aTenantTemplateNamingASystemOnlyCredentialFailsNamingTheFolderStore() throws Exception {
        addTemplate(gitTemplate("tenant-git", "tenant-b", "sys-cred-1", "https://example.invalid/repo.git"));

        TestScmSource source = new TestScmSource("src");
        String marker = "apiVersion: varroa.dev/v1\n"
                + "kind: Pipeline\n"
                + "template: tenant-git\n";
        source.commit("r1", VarroaMarkerFlowDefinition.MARKER_PATH, marker);
        source.setTip("r1");
        Folder folder = j.jenkins.createProject(Folder.class, "tenant-b");
        WorkflowJob job = indexBranch(folder, "repo", source);

        WorkflowRun run = j.buildAndAssertStatus(Result.FAILURE, job);

        j.assertLogContains("sys-cred-1", run);
        j.assertLogContains("tenant-b", run);
    }

    @Test
    @ConfiguredWithCode("pipeline-marker-flow-templates.yml")
    public void anOperatorTemplateWithAnEmptyFolderResolvesASystemCredential() throws Exception {
        UsernamePasswordCredentialsImpl credential =
                new UsernamePasswordCredentialsImpl(CredentialsScope.GLOBAL, "sys-cred-2", "", "u", "p");
        SystemCredentialsProvider.getInstance().getCredentials().add(credential);
        SystemCredentialsProvider.getInstance().save();

        String remote = initGitRepo("node { echo 'system credential template ran' }\n");
        addTemplate(gitTemplate("operator-git", "", "sys-cred-2", remote));

        TestScmSource source = new TestScmSource("src");
        String marker = "apiVersion: varroa.dev/v1\n"
                + "kind: Pipeline\n"
                + "template: operator-git\n";
        source.commit("r1", VarroaMarkerFlowDefinition.MARKER_PATH, marker);
        source.setTip("r1");
        WorkflowJob job = indexBranch(source);

        WorkflowRun run = j.buildAndAssertSuccess(job);

        j.assertLogContains("system credential template ran", run);
    }

    private static VarroaPipelineTemplateStore.TemplateEntry gitTemplate(String name, String folder,
            String credentialsId, String remote) {
        VarroaPipelineTemplateStore.TemplateEntry entry =
                new VarroaPipelineTemplateStore.TemplateEntry(name, "1.0.0");
        entry.setFolder(folder);
        entry.setParameters("type: object\nadditionalProperties: false\n");
        VarroaPipelineTemplateStore.ScmDefinitionEntry scmDefinition =
                new VarroaPipelineTemplateStore.ScmDefinitionEntry(remote, "Jenkinsfile");
        scmDefinition.setCredentialsId(credentialsId);
        // The fixture's local test repository has no remote HEAD symref
        // for a bare "*/HEAD" branch spec to resolve; name the branch the
        // fixture actually commits to.
        scmDefinition.setRevision("*/main");
        entry.setScmDefinition(scmDefinition);
        return entry;
    }

    // ---- 5.12e: trust tests ----

    @Test
    @ConfiguredWithCode("pipeline-marker-flow-templates.yml")
    public void anUntrustedRevisionNamingADifferentTemplateBuildsTheTrustedTemplateAndWarns() throws Exception {
        TestScmSource source = new TestScmSource("src");
        source.commit("trusted", VarroaMarkerFlowDefinition.MARKER_PATH, VALID_MARKER);
        source.commit("untrusted", VarroaMarkerFlowDefinition.MARKER_PATH,
                "apiVersion: varroa.dev/v1\nkind: Pipeline\ntemplate: tenant-a-only\n");
        source.setTip("untrusted");
        source.setTrustOverride(id -> "trusted");
        WorkflowJob job = indexBranch(source);

        WorkflowRun run = j.buildAndAssertSuccess(job);

        j.assertLogContains("template ran goVersion=1.24", run);
        j.assertLogContains("modified in an untrusted revision", run);
        VarroaTemplateValuesAction action = run.getAction(VarroaTemplateValuesAction.class);
        assertNotNull(action);
        assertEquals("go-service", action.getTemplateName());
        assertEquals("trusted", action.getMarkerRevision());
    }

    @Test
    @ConfiguredWithCode("pipeline-marker-flow-templates.yml")
    public void anUntrustedRevisionThatAddsTheFirstMarkerFailsWithTheTrustRule() throws Exception {
        TestScmSource source = new TestScmSource("src");
        // The trusted revision ("trusted") holds no marker at all; only the
        // untrusted tip adds one. Indexing tests the tip, so the branch
        // still gets a job (branch-api's SCMSourceCriteria.Probe.exists
        // reads whichever revision indexing passes in, here the tip).
        source.commit("untrusted", VarroaMarkerFlowDefinition.MARKER_PATH, VALID_MARKER);
        source.setTip("untrusted");
        source.setTrustOverride(id -> "trusted");
        WorkflowJob job = indexBranch(source);

        WorkflowRun run = j.buildAndAssertStatus(Result.FAILURE, job);

        j.assertLogContains("trusted revision", run);
        j.assertLogContains("holds no marker", run);
    }

    @Test
    @ConfiguredWithCode("pipeline-marker-flow-templates.yml")
    public void aTrustedRevisionEqualToTheTipBuildsWithNoWarning() throws Exception {
        TestScmSource source = new TestScmSource("src");
        source.commit("r1", VarroaMarkerFlowDefinition.MARKER_PATH, VALID_MARKER);
        source.setTip("r1");
        // No trust override: getTrustedRevision returns its argument, the tip itself.
        WorkflowJob job = indexBranch(source);

        WorkflowRun run = j.buildAndAssertSuccess(job);

        VarroaTemplateValuesAction action = run.getAction(VarroaTemplateValuesAction.class);
        assertNotNull(action);
        assertEquals("r1", action.getMarkerRevision());
        assertFalse(hasLogLineContaining(run, "modified in an untrusted revision"));
    }

    @Test
    @ConfiguredWithCode("pipeline-marker-flow-templates.yml")
    public void aSourceWithNoTipRevisionFailsWithNoCheckout() throws Exception {
        TestScmSource source = new TestScmSource("src");
        source.commit("r1", VarroaMarkerFlowDefinition.MARKER_PATH, VALID_MARKER);
        source.setTip("r1");
        WorkflowJob job = indexBranch(source);
        // Index with a real tip so the branch job exists, then remove the
        // tip before the build resolves the marker at run start.
        source.setNoTip(true);

        WorkflowRun run = j.buildAndAssertStatus(Result.FAILURE, job);

        j.assertLogContains("no revision", run);
        assertFalse("no nondeterministic checkout must run", source.wasCheckedOut());
    }

    @Test
    @ConfiguredWithCode("pipeline-marker-flow-templates.yml")
    public void aTipReadThatThrowsLeavesTheBuildSuccessful() throws Exception {
        TestScmSource source = new TestScmSource("src");
        source.commit("trusted", VarroaMarkerFlowDefinition.MARKER_PATH, VALID_MARKER);
        source.commit("tip", VarroaMarkerFlowDefinition.MARKER_PATH, VALID_MARKER);
        source.setThrowsOnRead("tip");
        source.setTip("tip");
        source.setTrustOverride(id -> "trusted");
        WorkflowJob job = indexBranch(source);

        WorkflowRun run = j.buildAndAssertSuccess(job);

        VarroaTemplateValuesAction action = run.getAction(VarroaTemplateValuesAction.class);
        assertNotNull(action);
        assertEquals("trusted", action.getMarkerRevision());
    }

    private static boolean hasLogLineContaining(WorkflowRun run, String text) throws IOException {
        for (String line : run.getLog(10_000)) {
            if (line.contains(text)) {
                return true;
            }
        }
        return false;
    }
}
