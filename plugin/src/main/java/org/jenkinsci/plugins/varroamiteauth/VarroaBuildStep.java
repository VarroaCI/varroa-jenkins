package org.jenkinsci.plugins.varroamiteauth;

import hudson.AbortException;
import hudson.Extension;
import hudson.model.Job;
import hudson.model.Run;
import hudson.model.TaskListener;

import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Random;
import java.util.Set;
import java.util.concurrent.TimeUnit;

import org.jenkinsci.plugins.workflow.steps.Step;
import org.jenkinsci.plugins.workflow.steps.StepContext;
import org.jenkinsci.plugins.workflow.steps.StepDescriptor;
import org.jenkinsci.plugins.workflow.steps.StepExecution;
import org.kohsuke.stapler.DataBoundConstructor;
import org.kohsuke.stapler.DataBoundSetter;

import jenkins.util.Timer;

/**
 * {@code varroaBuild} (D1, D8): triggers a job on another tenant, or the
 * caller's own, and optionally waits for it to finish.
 *
 * <p>With {@code wait: true} the {@link Execution} holds no CPS thread and
 * no executor slot beyond the flyweight the step already occupies: it
 * returns {@code false} from {@link StepExecution#start()} and reschedules
 * itself on {@link Timer} between polls.
 */
public class VarroaBuildStep extends Step {

    private final String job;
    private String tenant = "";
    private Map<String, Object> params = Map.of();
    private boolean wait = true;
    private boolean propagate = true;

    @DataBoundConstructor
    public VarroaBuildStep(String job) {
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

    public Map<String, Object> getParams() {
        return params;
    }

    @DataBoundSetter
    public void setParams(Map<String, Object> params) {
        this.params = params != null ? Map.copyOf(params) : Map.of();
    }

    public boolean isWait() {
        return wait;
    }

    @DataBoundSetter
    public void setWait(boolean wait) {
        this.wait = wait;
    }

    public boolean isPropagate() {
        return propagate;
    }

    @DataBoundSetter
    public void setPropagate(boolean propagate) {
        this.propagate = propagate;
    }

    @Override
    public StepExecution start(StepContext context) {
        return new Execution(this, context);
    }

    @Extension
    public static final class DescriptorImpl extends StepDescriptor {
        @Override
        public String getFunctionName() {
            return "varroaBuild";
        }

        @Override
        public String getDisplayName() {
            return "Trigger a Varroa cross-tenant build";
        }

        @Override
        public Set<Class<?>> getRequiredContext() {
            return Set.of(Run.class, TaskListener.class);
        }
    }

    /**
     * Asynchronous execution (D8): posts the trigger once in {@link #start()},
     * then polls {@code /v1/cross-tenant/status} on {@link VarroaPollPolicy}'s
     * cadence until the target reaches a terminal state, warning at 10
     * minutes and failing at 24 hours while the state stays {@code pending}.
     */
    static final class Execution extends StepExecution {

        private static final long serialVersionUID = 1L;

        private final String tenantArg;
        private final String jobPath;
        private final Map<String, Object> params;
        private final boolean wait;
        private final boolean propagate;

        private String callerJobPath = "";
        private String targetTenant = "";
        private String intentKey = "";
        private String runId = "";
        private long startedAtMillis;
        private boolean warned;

        private transient Random random;

        Execution(VarroaBuildStep step, StepContext context) {
            super(context);
            this.tenantArg = step.getTenant();
            this.jobPath = step.getJob();
            this.params = step.getParams();
            this.wait = step.isWait();
            this.propagate = step.isPropagate();
        }

        @Override
        public boolean start() throws Exception {
            Run<?, ?> run = getContext().get(Run.class);
            startedAtMillis = System.currentTimeMillis();
            callerJobPath = run.getParent().getFullName();
            String callerRunId = callerRunId(run);
            int seq = VarroaCallSeqAction.nextSeq(run);
            intentKey = "upstream:" + callerRunId + ":s" + seq;
            targetTenant = tenantArg.isEmpty() ? ownTenant(run) : tenantArg;

            VarroaCrossTenantReply reply = VarroaCrossTenantReply.parse(
                    VarroaCrossTenantOps.client().crossTenantBuild(
                            callerJobPath, callerRunId, seq, targetTenant, jobPath, params, ""));
            if (reply.isFailure()) {
                getContext().onFailure(new AbortException("varroaBuild: " + reply.describe()));
                return true;
            }
            if (!reply.reasonCode().isEmpty()) {
                getContext().onFailure(new AbortException("varroaBuild: " + reply.describeVeto()));
                return true;
            }
            runId = reply.getString("run_id", "");

            if (!wait) {
                getContext().onSuccess(pendingHandleMap());
                return true;
            }

            String state = reply.getString("state", runId.isEmpty() ? VarroaCrossTenantOps.STATE_PENDING : "queued");
            return handlePolledState(state, reply);
        }

        @Override
        public void onResume() {
            if (!wait) {
                return;
            }
            scheduleNext(0);
        }

        private void poll() {
            try {
                VarroaCrossTenantReply reply = runId.isEmpty()
                        ? VarroaCrossTenantReply.parse(VarroaCrossTenantOps.client()
                                .crossTenantStatusByIntentKey(callerJobPath, intentKey))
                        : VarroaCrossTenantReply.parse(VarroaCrossTenantOps.client()
                                .crossTenantStatus(callerJobPath, runId));
                // The step polls by run_id only after a reply named one, and a
                // reply names one only once its run_intents row or its
                // TRIGGER_FAILED row exists. A later NOT_FOUND means the row is
                // gone, and no poll brings it back.
                if (reply.isFailure() && VarroaCrossTenantReply.CODE_NOT_FOUND.equals(reply.code)) {
                    getContext().onFailure(new AbortException("varroaBuild: " + reply.describe()));
                    return;
                }
                if (reply.isFailure()) {
                    // Any other failure re-polls on the same cadence rather than
                    // failing the step: the recorder may be restarting.
                    scheduleNext(elapsedMillis());
                    return;
                }
                // A revoked grant or a fenced caller never clears by waiting, so
                // a permanent veto fails the step. A retryable veto polls again.
                if (reply.isPermanentVeto()) {
                    getContext().onFailure(new AbortException("varroaBuild: " + reply.describeVeto()));
                    return;
                }
                if (!reply.reasonCode().isEmpty()) {
                    scheduleNext(elapsedMillis());
                    return;
                }
                String replyRunId = reply.getString("run_id", "");
                if (!replyRunId.isEmpty()) {
                    runId = replyRunId;
                }
                String state = reply.getString("state", runId.isEmpty() ? VarroaCrossTenantOps.STATE_PENDING : "queued");
                handlePolledState(state, reply);
            } catch (Exception e) {
                try {
                    getContext().onFailure(e);
                } catch (Exception ignored) {
                    // The context is already closed.
                }
            }
        }

        /** Returns {@code true} when the step finished (matches {@link StepExecution#start()}'s contract). */
        private boolean handlePolledState(String state, VarroaCrossTenantReply reply) throws Exception {
            long elapsed = elapsedMillis();
            if (VarroaCrossTenantOps.STATE_PENDING.equals(state)) {
                TaskListener listener = getContext().get(TaskListener.class);
                if (elapsed >= VarroaPollPolicy.PENDING_CEILING_MILLIS) {
                    getContext().onFailure(new AbortException("varroaBuild: no run appeared for " + jobPath
                            + " within 24 hours; a lost trigger, or a target cell still waking, are the two causes"));
                    return true;
                }
                if (!warned && elapsed >= VarroaPollPolicy.PENDING_WARN_MILLIS && listener != null) {
                    warned = true;
                    listener.getLogger().println("WARNING: varroaBuild: " + jobPath
                            + " has stayed pending for 10 minutes; a lost trigger, or a target cell still waking, "
                            + "are the two causes");
                }
                scheduleNext(elapsed);
                return false;
            }
            if ("interrupted".equals(state)) {
                getContext().onFailure(new AbortException(
                        "varroaBuild: " + jobPath + " (run " + runId + ") was interrupted"));
                return true;
            }
            if ("completed".equals(state)) {
                Map<String, Object> handle = VarroaCrossTenantOps.handleMap(reply, targetTenant, jobPath,
                        callerJobPath, intentKey);
                String result = reply.getString("result", "");
                if (propagate && !"SUCCESS".equals(result)) {
                    getContext().onFailure(new AbortException(
                            "varroaBuild: " + jobPath + " (run " + runId + ") finished " + result
                                    + reply.describeTriggerFailure()));
                    return true;
                }
                getContext().onSuccess(handle);
                return true;
            }
            // queued or running: keep polling, on the same cadence, no ceiling.
            scheduleNext(elapsed);
            return false;
        }

        private void scheduleNext(long elapsedMillis) {
            long delay = VarroaPollPolicy.nextDelayMillis(elapsedMillis, random());
            Timer.get().schedule(this::poll, delay, TimeUnit.MILLISECONDS);
        }

        private long elapsedMillis() {
            return System.currentTimeMillis() - startedAtMillis;
        }

        private Random random() {
            if (random == null) {
                random = new Random();
            }
            return random;
        }

        private Map<String, Object> pendingHandleMap() {
            Map<String, Object> m = new LinkedHashMap<>();
            m.put("runId", runId);
            m.put("tenant", targetTenant);
            m.put("job", jobPath);
            m.put("buildNumber", null);
            m.put("state", VarroaCrossTenantOps.STATE_PENDING);
            m.put("result", null);
            m.put("startedAt", null);
            m.put("endedAt", null);
            m.put("url", "");
            m.put("artifacts", List.of());
            m.put("intentKey", intentKey);
            m.put("callerJobPath", callerJobPath);
            return m;
        }

        private static String callerRunId(Run<?, ?> run) {
            VarroaIntentAction action = run.getAction(VarroaIntentAction.class);
            return action != null ? action.getRunId() : "";
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
