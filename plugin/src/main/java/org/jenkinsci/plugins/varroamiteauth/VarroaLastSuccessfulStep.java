package org.jenkinsci.plugins.varroamiteauth;

import hudson.AbortException;
import hudson.Extension;
import hudson.model.Job;
import hudson.model.Run;
import hudson.model.TaskListener;

import java.util.Set;

import org.jenkinsci.plugins.workflow.steps.Step;
import org.jenkinsci.plugins.workflow.steps.StepContext;
import org.jenkinsci.plugins.workflow.steps.StepDescriptor;
import org.jenkinsci.plugins.workflow.steps.StepExecution;
import org.jenkinsci.plugins.workflow.steps.SynchronousNonBlockingStepExecution;
import org.kohsuke.stapler.DataBoundConstructor;
import org.kohsuke.stapler.DataBoundSetter;

/**
 * {@code varroaLastSuccessful} (D1): a single read of the last successful
 * run of a job, on another tenant or the caller's own. Synchronous: one
 * round trip, run off the CPS thread on {@link SynchronousNonBlockingStepExecution}'s
 * own pool.
 */
public class VarroaLastSuccessfulStep extends Step {

    private final String job;
    private String tenant = "";

    @DataBoundConstructor
    public VarroaLastSuccessfulStep(String job) {
        this.job = job;
    }

    public String getJob() {
        return job;
    }

    public String getTenant() {
        return tenant;
    }

    @DataBoundSetter
    public void setTenant(String tenant) {
        this.tenant = tenant != null ? tenant : "";
    }

    @Override
    public StepExecution start(StepContext context) {
        return new Execution(this, context);
    }

    @Extension
    public static final class DescriptorImpl extends StepDescriptor {
        @Override
        public String getFunctionName() {
            return "varroaLastSuccessful";
        }

        @Override
        public String getDisplayName() {
            return "Read the last successful Varroa run";
        }

        @Override
        public Set<Class<?>> getRequiredContext() {
            return Set.of(Run.class, TaskListener.class);
        }
    }

    static final class Execution extends SynchronousNonBlockingStepExecution<Object> {

        private static final long serialVersionUID = 1L;

        private final String tenantArg;
        private final String jobPath;

        Execution(VarroaLastSuccessfulStep step, StepContext context) {
            super(context);
            this.tenantArg = step.getTenant();
            this.jobPath = step.getJob();
        }

        @Override
        protected Object run() throws Exception {
            Run<?, ?> run = getContext().get(Run.class);
            String callerJobPath = run.getParent().getFullName();
            String targetTenant = tenantArg.isEmpty() ? ownTenant(run) : tenantArg;

            VarroaCrossTenantReply reply = VarroaCrossTenantReply.parse(
                    VarroaCrossTenantOps.client().crossTenantLastSuccessful(callerJobPath, targetTenant, jobPath));
            if (reply.isFailure()) {
                throw new AbortException(
                        "varroaLastSuccessful: the recorder did not answer (" + reply.code + ")");
            }
            String runId = reply.getString("run_id", "");
            if (runId.isEmpty()) {
                return null;
            }
            return VarroaCrossTenantOps.handleMap(reply, targetTenant, jobPath, callerJobPath, "", "completed");
        }

        private static String ownTenant(Run<?, ?> run) {
            Job<?, ?> job = run.getParent();
            if (!(job instanceof hudson.model.Queue.Task)) {
                return "";
            }
            String tenant = VarroaTenantResolver.tenantOf((hudson.model.Queue.Task) job);
            return tenant != null ? tenant : "";
        }
    }
}
