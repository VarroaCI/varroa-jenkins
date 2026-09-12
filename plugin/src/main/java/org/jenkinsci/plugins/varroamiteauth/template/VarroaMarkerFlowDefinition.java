package org.jenkinsci.plugins.varroamiteauth.template;

import hudson.AbortException;
import hudson.Extension;
import hudson.FilePath;
import hudson.Functions;
import hudson.Launcher;
import hudson.model.Action;
import hudson.model.Cause;
import hudson.model.ItemGroup;
import hudson.model.Queue;
import hudson.model.Run;
import hudson.model.TaskListener;
import hudson.scm.SCM;

import java.io.IOException;
import java.io.InputStream;
import java.lang.reflect.Constructor;
import java.nio.charset.StandardCharsets;
import java.util.Arrays;
import java.util.Collections;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.logging.Level;
import java.util.logging.Logger;

import jenkins.branch.Branch;
import jenkins.model.Jenkins;
import jenkins.scm.api.SCMFile;
import jenkins.scm.api.SCMFileSystem;
import jenkins.scm.api.SCMHead;
import jenkins.scm.api.SCMRevision;
import jenkins.scm.api.SCMSource;

import org.jenkinsci.plugins.varroamiteauth.ActivityEvent;
import org.jenkinsci.plugins.varroamiteauth.ActivitySink;
import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.cps.CpsScmFlowDefinition;
import org.jenkinsci.plugins.workflow.flow.FlowDefinition;
import org.jenkinsci.plugins.workflow.flow.FlowDefinitionDescriptor;
import org.jenkinsci.plugins.workflow.flow.FlowExecution;
import org.jenkinsci.plugins.workflow.flow.FlowExecutionOwner;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.multibranch.BranchJobProperty;
import org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject;

import org.kohsuke.stapler.DataBoundConstructor;

/**
 * Reads the marker at run start, resolves and validates it against
 * {@link VarroaPipelineTemplateStore}, and builds the execution (D8, D8a,
 * {@code pipeline-marker-file}). {@link VarroaTemplateBranchProjectFactory}
 * hands out one instance per branch job; the marker path is fixed, so this
 * class carries no field.
 *
 * <p>Steps 1 to 4 below run inside one try block. Any failure attaches a
 * rejected {@link VarroaTemplateValuesAction}, emits activity {@code
 * pipeline.marker.rejected}, and fails the build with a message the
 * console shows. No step lets an exception escape unexplained.
 */
public class VarroaMarkerFlowDefinition extends FlowDefinition {

    private static final Logger LOGGER = Logger.getLogger(VarroaMarkerFlowDefinition.class.getName());

    /** The marker's fixed path, per {@code pipeline-marker-file}. Never configurable. */
    static final String MARKER_PATH = ".varroa/pipeline.yaml";

    @DataBoundConstructor
    public VarroaMarkerFlowDefinition() {
    }

    @Override
    public FlowExecution create(FlowExecutionOwner owner, TaskListener listener, List<? extends Action> actions)
            throws Exception {
        Run<?, ?> run = requireRun(owner);
        String template = "";
        String templateVersion = "";
        String markerRevision = "";
        try {
            SourceInfo info = resolveSourceInfo(run);
            SCMRevision tip = fetchTip(info);
            SCMRevision trusted = resolveTrustedRevision(info, tip, listener);
            markerRevision = trusted.toString();

            byte[] markerBytes = readMarkerOrFail(info, trusted, run, listener);
            warnIfTipDiffers(info, tip, trusted, markerBytes, run, listener);

            VarroaMarkerValidator.Marker marker;
            try {
                marker = VarroaMarkerValidator.parseMarker(markerBytes);
            } catch (VarroaMarkerValidator.MarkerFormatException e) {
                throw new AbortException("marker \"" + MARKER_PATH + "\" is invalid: " + e.getMessage());
            }
            template = marker.getTemplate();
            templateVersion = marker.getTemplateVersion();

            VarroaPipelineTemplateStore store = VarroaPipelineTemplateStore.get();
            String tenantFolder = tenantFolderOf(run);
            VarroaPipelineTemplateStore.TemplateEntry entry;
            try {
                entry = store.resolve(tenantFolder, marker.getTemplate(), marker.getTemplateVersion());
            } catch (VarroaPipelineTemplateStore.TemplateResolutionException e) {
                throw new AbortException(e.getMessage());
            }

            Map<String, Object> schema = parseSchema(entry);
            VarroaMarkerValidator.ValuesResult result = VarroaMarkerValidator.validateValues(schema, marker.getValues());
            if (!result.getFindings().isEmpty()) {
                throw new AbortException(
                        valuesFailureMessage(MARKER_PATH, entry.getName(), result.getFindings(), result.getDefaulted()));
            }

            run.addAction(VarroaTemplateValuesAction.resolved(
                    entry.getName(), entry.getVersion(), MARKER_PATH, markerRevision, result.getDefaulted()));

            return buildExecution(store, entry, owner, listener, actions);
        } catch (Exception e) {
            String message = e.getMessage() != null ? e.getMessage() : e.toString();
            if (e instanceof AbortException) {
                listener.error(message);
            } else {
                listener.error(message).println(Functions.printThrowable(e).trim());
            }
            run.addAction(VarroaTemplateValuesAction.rejected(template, templateVersion, MARKER_PATH, markerRevision, message));
            emitRejectedActivity(run, message);
            throw e instanceof AbortException ? e : new AbortException(message);
        }
    }

    // ---- Step 1: the trusted revision and the marker read (D8a) ----

    private static final class SourceInfo {
        final SCMSource source;
        final SCMHead head;

        SourceInfo(SCMSource source, SCMHead head) {
            this.source = source;
            this.head = head;
        }
    }

    private static SourceInfo resolveSourceInfo(Run<?, ?> run) throws AbortException {
        if (!(run.getParent() instanceof WorkflowJob)) {
            throw new AbortException("Varroa pipeline template: inappropriate context (not a Pipeline job)");
        }
        WorkflowJob job = (WorkflowJob) run.getParent();
        BranchJobProperty property = job.getProperty(BranchJobProperty.class);
        if (property == null) {
            throw new AbortException("Varroa pipeline template: inappropriate context (not a multibranch branch job)");
        }
        Branch branch = property.getBranch();
        ItemGroup<?> parent = job.getParent();
        if (!(parent instanceof WorkflowMultiBranchProject)) {
            throw new AbortException("Varroa pipeline template: inappropriate context (not a multibranch project)");
        }
        WorkflowMultiBranchProject project = (WorkflowMultiBranchProject) parent;
        SCMSource source = project.getSCMSource(branch.getSourceId());
        if (source == null) {
            throw new AbortException("Varroa pipeline template: could not find SCM source \"" + branch.getSourceId() + "\"");
        }
        return new SourceInfo(source, branch.getHead());
    }

    private static SCMRevision fetchTip(SourceInfo info) throws AbortException {
        SCMRevision tip;
        try {
            tip = info.source.fetch(info.head, TaskListener.NULL);
        } catch (IOException | InterruptedException e) {
            throw new AbortException("Varroa pipeline template: could not fetch \"" + info.head.getName() + "\": "
                    + e.getMessage());
        }
        if (tip == null) {
            // D8a: no fallback to Branch.getScm(), which would check out an
            // unknown, possibly untrusted, revision.
            throw new AbortException("Varroa pipeline template: source reported no revision for \""
                    + info.head.getName() + "\"; the marker is not read with no nondeterministic checkout");
        }
        return tip;
    }

    private static SCMRevision resolveTrustedRevision(SourceInfo info, SCMRevision tip, TaskListener listener)
            throws AbortException {
        try {
            return info.source.getTrustedRevision(tip, listener);
        } catch (IOException | InterruptedException e) {
            throw new AbortException(
                    "Varroa pipeline template: could not resolve the trusted revision for \"" + info.head.getName()
                            + "\": " + e.getMessage());
        }
    }

    private static byte[] readMarkerOrFail(SourceInfo info, SCMRevision trusted, Run<?, ?> run, TaskListener listener)
            throws AbortException {
        MarkerRead read = readMarker(info, trusted, run, listener);
        if (read.error != null) {
            throw new AbortException(
                    "Varroa pipeline template: could not read \"" + MARKER_PATH + "\" from " + info.source + ": "
                            + read.error);
        }
        if (read.content == null) {
            throw new AbortException("Varroa pipeline template: the trusted revision " + trusted
                    + " holds no marker at \"" + MARKER_PATH + "\" (template selection reads only the branch's "
                    + "trusted revision, never an untrusted revision such as a fork pull request's own commits)");
        }
        return read.content;
    }

    /**
     * Reads the tip marker for the untrusted-edit warning (D8a) when the
     * trusted revision differs from the tip. A failure to read the tip
     * marker is a log line only: it never fails the build.
     */
    private static void warnIfTipDiffers(SourceInfo info, SCMRevision tip, SCMRevision trusted, byte[] trustedContent,
            Run<?, ?> run, TaskListener listener) {
        if (trusted.equals(tip)) {
            return;
        }
        try {
            MarkerRead tipRead = readMarker(info, tip, run, listener);
            if (tipRead.content != null && !Arrays.equals(tipRead.content, trustedContent)) {
                listener.getLogger().println("Varroa pipeline template: \"" + MARKER_PATH
                        + "\" has been modified in an untrusted revision; the build used the trusted revision "
                        + trusted + " (template selection trusts only SCMSource.getTrustedRevision, never the "
                        + "branch tip).");
            }
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaMarkerFlowDefinition: could not read the tip marker for the trust warning", e);
        }
    }

    private static final class MarkerRead {
        final byte[] content;
        final String error;

        private MarkerRead(byte[] content, String error) {
            this.content = content;
            this.error = error;
        }

        static MarkerRead found(byte[] content) {
            return new MarkerRead(content, null);
        }

        static MarkerRead notFound() {
            return new MarkerRead(null, null);
        }

        static MarkerRead error(String message) {
            return new MarkerRead(null, message);
        }
    }

    /**
     * Reads the marker at {@code revision}: the lightweight
     * {@link SCMFileSystem} path first, falling back to a checkout of
     * {@code revision} when {@link SCMFileSystem#of} returns null, exactly
     * as D8 states. Reports an outright read failure only when both paths
     * fail; a marker that plainly does not exist at {@code revision}
     * reports as {@link MarkerRead#notFound()}. An oversized marker is
     * never retried through a checkout: the size cap already proves the
     * marker is invalid, so {@link MarkerTooLargeException} from either
     * read path returns straight to the caller.
     */
    private static MarkerRead readMarker(SourceInfo info, SCMRevision revision, Run<?, ?> run, TaskListener listener) {
        String lightweightError = null;
        try (SCMFileSystem fs = SCMFileSystem.of(info.source, info.head, revision)) {
            if (fs != null) {
                SCMFile file = fs.child(MARKER_PATH);
                try {
                    if (!file.exists() || file.isDirectory()) {
                        return MarkerRead.notFound();
                    }
                    try (InputStream in = file.content()) {
                        return MarkerRead.found(readBounded(in));
                    }
                } catch (MarkerTooLargeException e) {
                    return MarkerRead.error(e.getMessage());
                } catch (IOException | InterruptedException e) {
                    lightweightError = e.getMessage();
                }
            }
        } catch (IOException | InterruptedException e) {
            lightweightError = e.getMessage();
        }
        try {
            byte[] content = readMarkerViaCheckout(info, revision, run, listener);
            return content != null ? MarkerRead.found(content) : MarkerRead.notFound();
        } catch (IOException | InterruptedException e) {
            String msg = e.getMessage() != null ? e.getMessage() : e.toString();
            return MarkerRead.error(lightweightError != null ? lightweightError + "; checkout also failed: " + msg : msg);
        }
    }

    /**
     * Falls back to a checkout of {@code revision} in a temporary
     * workspace and reads the marker from there (D8), the way {@code
     * SCMBinder} falls back to a heavyweight checkout when the lightweight
     * file API is unavailable. Returns {@code null} when the checkout
     * completes but the marker is absent from the checked-out tree.
     */
    private static byte[] readMarkerViaCheckout(SourceInfo info, SCMRevision revision, Run<?, ?> run,
            TaskListener listener) throws IOException, InterruptedException {
        SCM scm = info.source.build(info.head, revision);
        Jenkins jenkins = Jenkins.get();
        FilePath root = jenkins.getRootPath();
        if (root == null) {
            throw new IOException("the controller has no root path for a checkout");
        }
        FilePath dir = root.child("caches/varroa-pipeline-template/" + UUID.randomUUID());
        try {
            Launcher launcher = jenkins.createLauncher(listener);
            scm.checkout(run, launcher, dir, listener, null, null);
            FilePath marker = dir.child(MARKER_PATH);
            if (!marker.exists()) {
                return null;
            }
            try (InputStream in = marker.read()) {
                return readBounded(in);
            }
        } finally {
            try {
                dir.deleteRecursive();
            } catch (IOException | InterruptedException e) {
                LOGGER.log(Level.FINE, "VarroaMarkerFlowDefinition: could not clean up the temporary checkout", e);
            }
        }
    }

    /**
     * The most bytes {@link #readBounded} ever allocates: one more than
     * {@link VarroaMarkerValidator#MAX_MARKER_BYTES}, exactly enough to
     * prove the stream holds more than the marker size limit.
     */
    private static final int MAX_MARKER_READ_BYTES = VarroaMarkerValidator.MAX_MARKER_BYTES + 1;

    /**
     * A marker stream held more than {@link VarroaMarkerValidator#MAX_MARKER_BYTES}
     * bytes. A distinct type from a plain {@link IOException} so {@link
     * #readMarker} can tell this apart from a lightweight-read failure:
     * the size cap already proves the marker is invalid, so this
     * rejection is not retried through a checkout.
     */
    static final class MarkerTooLargeException extends IOException {
        MarkerTooLargeException(String message) {
            super(message);
        }
    }

    /**
     * Reads {@code in} into a byte array of at most {@link
     * #MAX_MARKER_READ_BYTES} bytes and rejects a stream that still has
     * more data past that cap, so an oversized marker is never fully
     * allocated before rejection (D6, D8, {@code pipeline-marker-file}).
     */
    private static byte[] readBounded(InputStream in) throws IOException {
        byte[] buf = new byte[MAX_MARKER_READ_BYTES];
        int total = 0;
        int n;
        while (total < buf.length && (n = in.read(buf, total, buf.length - total)) != -1) {
            total += n;
        }
        if (total >= MAX_MARKER_READ_BYTES) {
            throw new MarkerTooLargeException("marker exceeds " + VarroaMarkerValidator.MAX_MARKER_BYTES + " bytes");
        }
        return Arrays.copyOf(buf, total);
    }

    // ---- Shared helpers (also used by VarroaTemplateFlowDefinition) ----

    static Run<?, ?> requireRun(FlowExecutionOwner owner) throws AbortException {
        Queue.Executable executable;
        try {
            executable = owner.getExecutable();
        } catch (IOException e) {
            throw new AbortException("Varroa pipeline template: inappropriate context: " + e.getMessage());
        }
        if (!(executable instanceof Run)) {
            throw new AbortException("Varroa pipeline template: inappropriate context (not a run)");
        }
        return (Run<?, ?>) executable;
    }

    /** The job's tenant folder: the first segment of its full name, or {@code ""} with none (D7). */
    static String tenantFolderOf(Run<?, ?> run) {
        String fullName = run.getParent().getFullName();
        int slash = fullName.indexOf('/');
        return slash >= 0 ? fullName.substring(0, slash) : "";
    }

    /** Parses a resolved template's {@code spec.parameters} schema YAML into a map. */
    static Map<String, Object> parseSchema(VarroaPipelineTemplateStore.TemplateEntry entry) throws AbortException {
        try {
            Map<String, Object> schema =
                    VarroaMarkerValidator.parseMarkerYaml(entry.getParameters().getBytes(StandardCharsets.UTF_8));
            return schema != null ? schema : Collections.emptyMap();
        } catch (VarroaMarkerValidator.MarkerFormatException e) {
            throw new AbortException(
                    "template \"" + entry.getName() + "\" delivers an unreadable schema: " + e.getMessage());
        }
    }

    /** Builds the console-facing values-validation failure message (D3, D8a). */
    static String valuesFailureMessage(String markerPath, String templateName,
            List<VarroaMarkerValidator.Finding> findings, Map<String, Object> values) {
        StringBuilder sb = new StringBuilder();
        if (!markerPath.isEmpty()) {
            sb.append("marker \"").append(markerPath).append("\": ");
        }
        sb.append("template \"").append(templateName).append("\" values are invalid:");
        for (VarroaMarkerValidator.Finding f : findings) {
            String path = f.getPath().isEmpty() ? "(root)" : f.getPath();
            sb.append("\n  ").append(path).append('=').append(valueAt(values, f.getPath())).append(": ")
                    .append(f.getMessage());
        }
        return sb.toString();
    }

    private static Object valueAt(Map<String, Object> values, String path) {
        if (path.isEmpty()) {
            return values;
        }
        Object cur = values;
        for (String part : path.split("\\.")) {
            if (!(cur instanceof Map)) {
                return null;
            }
            cur = ((Map<?, ?>) cur).get(part);
        }
        return cur;
    }

    /** Emits the activity event {@code pipeline.marker.rejected} (task 5.11, {@code build-event-capture}). */
    static void emitRejectedActivity(Run<?, ?> run, String message) {
        try {
            ActivityEvent event = ActivityEvent.builder()
                    .type("pipeline.marker.rejected")
                    .itemPath(run.getParent().getFullName())
                    .buildNumber(run.getNumber())
                    .result("FAILURE")
                    .url(run.getUrl())
                    .actor(actorOf(run))
                    .message(message)
                    .build();
            ActivitySink.get().offer(event);
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "VarroaMarkerFlowDefinition: failed to emit pipeline.marker.rejected", e);
        }
    }

    private static String actorOf(Run<?, ?> run) {
        for (Cause cause : run.getCauses()) {
            if (cause instanceof Cause.UserIdCause) {
                String userId = ((Cause.UserIdCause) cause).getUserId();
                if (userId != null && !userId.isEmpty()) {
                    return userId;
                }
            }
        }
        return "unknown";
    }

    /**
     * Step 6: builds the execution from the resolved template's {@code
     * spec.jenkinsfile} or {@code spec.scmDefinition} (D8, E16). Shared
     * with {@link VarroaTemplateFlowDefinition}, whose values come from an
     * item rather than a marker but whose step 6 is identical.
     */
    static FlowExecution buildExecution(VarroaPipelineTemplateStore store, VarroaPipelineTemplateStore.TemplateEntry entry,
            FlowExecutionOwner owner, TaskListener listener, List<? extends Action> actions) throws Exception {
        if (!entry.getJenkinsfile().isEmpty()) {
            return new CpsFlowDefinition(entry.getJenkinsfile(), true).create(owner, listener, actions);
        }
        VarroaPipelineTemplateStore.ScmDefinitionEntry scmDefinition = entry.getScmDefinition();
        if (scmDefinition == null) {
            throw new AbortException(
                    "template \"" + entry.getName() + "\" carries neither a jenkinsfile nor an scmDefinition");
        }
        try {
            store.resolveCredentialScope(entry);
        } catch (VarroaPipelineTemplateStore.TemplateResolutionException e) {
            throw new AbortException(e.getMessage());
        }
        SCM scm = buildGitScm(scmDefinition);
        return new CpsScmFlowDefinition(scm, scmDefinition.getScriptPath()).create(owner, listener, actions);
    }

    /**
     * Builds a {@code hudson.plugins.git.GitSCM} for {@code
     * scmDefinition} reflectively (D2, D7): git-plugin is not a compile
     * dependency of this plugin, the same reason {@link
     * TemplateCredentialResolver} reaches the credentials plugin
     * reflectively.
     */
    private static SCM buildGitScm(VarroaPipelineTemplateStore.ScmDefinitionEntry scmDefinition) throws AbortException {
        try {
            ClassLoader loader = VarroaMarkerFlowDefinition.class.getClassLoader();
            Class<?> userRemoteConfigClass = Class.forName("hudson.plugins.git.UserRemoteConfig", false, loader);
            Class<?> branchSpecClass = Class.forName("hudson.plugins.git.BranchSpec", false, loader);
            Class<?> gitScmClass = Class.forName("hudson.plugins.git.GitSCM", false, loader);
            Class<?> browserClass = Class.forName("hudson.plugins.git.browser.GitRepositoryBrowser", false, loader);
            Class<?> extensionClass = Class.forName("hudson.plugins.git.extensions.GitSCMExtension", false, loader);

            Object remoteConfig = userRemoteConfigClass.getConstructor(String.class, String.class, String.class, String.class)
                    .newInstance(scmDefinition.getRemote(), null, null, scmDefinition.getCredentialsId());
            String revision = scmDefinition.getRevision();
            Object branchSpec = branchSpecClass.getConstructor(String.class)
                    .newInstance(revision.isEmpty() ? "*/HEAD" : revision);

            List<Object> remoteConfigs = List.of(remoteConfig);
            List<Object> branchSpecs = List.of(branchSpec);
            List<Object> extensions = List.of();

            Constructor<?> ctor = gitScmClass.getConstructor(List.class, List.class, browserClass, String.class, List.class);
            return (SCM) ctor.newInstance(remoteConfigs, branchSpecs, null, null, extensions);
        } catch (ReflectiveOperationException e) {
            throw new AbortException("could not build the git checkout for \"" + scmDefinition.getRemote() + "\": "
                    + e.getMessage());
        }
    }

    @Extension
    public static final class DescriptorImpl extends FlowDefinitionDescriptor {
        @Override
        public String getDisplayName() {
            return "Varroa pipeline template marker";
        }
    }
}
