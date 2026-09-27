package org.jenkinsci.plugins.varroamiteauth;

import hudson.AbortException;
import hudson.Extension;
import hudson.FilePath;
import hudson.model.Run;
import hudson.model.TaskListener;
import hudson.remoting.VirtualChannel;

import java.io.File;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.URI;
import java.util.Set;

import org.jenkinsci.plugins.workflow.steps.Step;
import org.jenkinsci.plugins.workflow.steps.StepContext;
import org.jenkinsci.plugins.workflow.steps.StepDescriptor;
import org.jenkinsci.plugins.workflow.steps.StepExecution;
import org.jenkinsci.plugins.workflow.steps.SynchronousNonBlockingStepExecution;
import org.kohsuke.stapler.DataBoundConstructor;
import org.kohsuke.stapler.DataBoundSetter;

import jenkins.MasterToSlaveFileCallable;

/**
 * {@code varroaArtifact}, backing {@code handle.copyArtifact} (D1, D12):
 * mints a signed URL on the controller, then downloads it on the agent's
 * own workspace channel. The URL crosses the channel; the bytes never do.
 */
public class VarroaArtifactStep extends Step {

    private final String runId;
    private final String path;
    private String target = "";

    @DataBoundConstructor
    public VarroaArtifactStep(String runId, String path) {
        this.runId = runId;
        this.path = path;
    }

    public String getRunId() {
        return runId;
    }

    public String getPath() {
        return path;
    }

    public String getTarget() {
        return target;
    }

    @DataBoundSetter
    public void setTarget(String target) {
        this.target = target != null ? target : "";
    }

    @Override
    public StepExecution start(StepContext context) {
        return new Execution(this, context);
    }

    @Extension
    public static final class DescriptorImpl extends StepDescriptor {
        @Override
        public String getFunctionName() {
            return "varroaArtifact";
        }

        @Override
        public String getDisplayName() {
            return "Copy a Varroa cross-tenant artifact";
        }

        @Override
        public Set<Class<?>> getRequiredContext() {
            return Set.of(Run.class, FilePath.class, TaskListener.class);
        }
    }

    static final class Execution extends SynchronousNonBlockingStepExecution<String> {

        private static final long serialVersionUID = 1L;

        private final String runId;
        private final String path;
        private final String target;

        Execution(VarroaArtifactStep step, StepContext context) {
            super(context);
            this.runId = step.getRunId();
            this.path = step.getPath();
            this.target = step.getTarget();
        }

        @Override
        protected String run() throws Exception {
            Run<?, ?> run = getContext().get(Run.class);
            FilePath workspace = getContext().get(FilePath.class);
            String callerJobPath = run.getParent().getFullName();

            VarroaCrossTenantReply reply = VarroaCrossTenantReply.parse(
                    VarroaCrossTenantOps.client().crossTenantArtifactUrl(callerJobPath, runId, path));
            if (reply.isFailure()) {
                throw new AbortException("varroaArtifact: " + reply.describe());
            }
            if (!reply.reasonCode().isEmpty()) {
                throw new AbortException("varroaArtifact: " + reply.describeVeto());
            }
            String url = reply.getString("url", "");
            if (url.isEmpty()) {
                throw new AbortException("varroaArtifact: no signed URL for " + path + " on run " + runId);
            }
            String targetPath = target.isEmpty() ? path : target;
            workspace.child(targetPath).act(new Download(url));
            return targetPath;
        }
    }

    /**
     * Downloads {@code url} straight to the agent's disk (D12): the signed
     * URL is the only thing marshalled to the agent, so the artifact's
     * bytes never transit the controller's workspace channel object.
     */
    private static final class Download extends MasterToSlaveFileCallable<Void> {

        private static final long serialVersionUID = 1L;

        private final String url;

        Download(String url) {
            this.url = url;
        }

        @Override
        public Void invoke(File f, VirtualChannel channel) throws IOException {
            File parent = f.getParentFile();
            if (parent != null) {
                parent.mkdirs();
            }
            try (InputStream in = URI.create(url).toURL().openStream();
                    OutputStream out = new FileOutputStream(f)) {
                in.transferTo(out);
            }
            return null;
        }
    }
}
