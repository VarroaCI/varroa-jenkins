package org.jenkinsci.plugins.varroamiteauth;

import hudson.AbortException;
import hudson.EnvVars;
import hudson.Extension;
import hudson.model.Job;
import hudson.model.Run;
import hudson.model.TaskListener;

import java.time.Instant;
import java.util.Random;
import java.util.Set;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.logging.Level;
import java.util.logging.Logger;

import org.jenkinsci.plugins.workflow.steps.BodyExecutionCallback;
import org.jenkinsci.plugins.workflow.steps.EnvironmentExpander;
import org.jenkinsci.plugins.workflow.steps.Step;
import org.jenkinsci.plugins.workflow.steps.StepContext;
import org.jenkinsci.plugins.workflow.steps.StepDescriptor;
import org.jenkinsci.plugins.workflow.steps.StepExecution;
import org.kohsuke.stapler.DataBoundConstructor;
import org.kohsuke.stapler.DataBoundSetter;

import jenkins.util.Timer;

/**
 * {@code varroaLock} (D1, D11): a block step that acquires a fleet lock
 * before the body and releases it in a {@code finally}. A bare name is
 * qualified with the calling tenant; a name already holding a {@code /} is
 * used unchanged.
 *
 * <p>The waiting phase and the 60-second renewal loop hold no CPS thread
 * and no dedicated thread, the same way {@link VarroaBuildStep} polls: both
 * reschedule themselves on {@link Timer}.
 *
 * <p>A {@code varroa.lock} call fails at once, before any acquire attempt,
 * when the run already holds a lock ({@link VarroaActiveLocksAction}) — see
 * that class for why a nested block cannot run correctly on this platform.
 */
public class VarroaLockStep extends Step {

    private static final Logger LOGGER = Logger.getLogger(VarroaLockStep.class.getName());

    private final String name;
    private int timeout;

    @DataBoundConstructor
    public VarroaLockStep(String name) {
        this.name = name;
    }

    public String getName() {
        return name;
    }

    /** Seconds; {@code 0} means unlimited, the default. */
    public int getTimeout() {
        return timeout;
    }

    @DataBoundSetter
    public void setTimeout(int timeout) {
        this.timeout = Math.max(timeout, 0);
    }

    @Override
    public StepExecution start(StepContext context) {
        return new Execution(this, context);
    }

    @Extension
    public static final class DescriptorImpl extends StepDescriptor {
        @Override
        public String getFunctionName() {
            return "varroaLock";
        }

        @Override
        public String getDisplayName() {
            return "Acquire a Varroa fleet lock";
        }

        @Override
        public boolean takesImplicitBlockArgument() {
            return true;
        }

        @Override
        public Set<Class<?>> getRequiredContext() {
            return Set.of(Run.class, TaskListener.class);
        }
    }

    static final class Execution extends StepExecution {

        private static final long serialVersionUID = 1L;

        private final String nameArg;
        private final int timeoutSeconds;

        private String qualifiedName = "";
        private String callerJobPath = "";
        private String runId = "";
        private long acquireStartedAtMillis;
        private long fenceToken;

        // Written by the body-completion callback or stop() (a CPS or
        // executor thread) and read by renew() on the shared Timer
        // thread. An AtomicBoolean gives both sides a happens-before
        // edge a plain field cannot: without one, renew() can spin
        // forever on a stale "not released" read and keep renewing a
        // lease the job already told the recorder to drop.
        private final AtomicBoolean released = new AtomicBoolean(false);
        private boolean releasingWarned;

        private transient Random random;

        Execution(VarroaLockStep step, StepContext context) {
            super(context);
            this.nameArg = step.getName();
            this.timeoutSeconds = step.getTimeout();
        }

        @Override
        public boolean start() throws Exception {
            Run<?, ?> run = getContext().get(Run.class);
            runId = callerRunId(run);
            callerJobPath = run.getParent().getFullName();
            qualifiedName = qualify(nameArg, ownTenant(run));

            acquireStartedAtMillis = System.currentTimeMillis();
            attemptAcquire();
            return false;
        }

        @Override
        public void onResume() {
            if (released.get()) {
                return;
            }
            if (fenceToken != 0) {
                // The action's stack is transient, so a resumed build
                // restores an empty one and varroa.lock.token reads null
                // inside a block that holds a lock. Push the held entry
                // back before the renewal resumes.
                try {
                    Run<?, ?> run = getContext().get(Run.class);
                    if (run != null) {
                        VarroaActiveLocksAction.of(run).push(qualifiedName, fenceToken);
                    }
                } catch (Exception e) {
                    LOGGER.log(Level.WARNING,
                            "varroaLock: could not restore the held token after a resume", e);
                }
                scheduleRenew();
            } else {
                attemptAcquire();
            }
        }

        @Override
        public void stop(Throwable cause) throws Exception {
            // Covers the waiting-for-acquire phase: no body has started
            // yet, so nothing else releases a row this step never entered.
            releaseLock();
            super.stop(cause);
        }

        private void attemptAcquire() {
            try {
                VarroaCrossTenantReply reply = VarroaCrossTenantReply.parse(
                        VarroaCrossTenantOps.client().crossTenantLockAcquire(callerJobPath, qualifiedName, runId,
                                VarroaPollPolicy.LOCK_LEASE_SECONDS, isoInstant(acquireStartedAtMillis)));
                if (reply.isFailure()) {
                    getContext().onFailure(
                            new AbortException("varroaLock: the recorder did not answer (" + reply.code + ")"));
                    return;
                }
                String state = reply.getString("state", "waiting");
                if ("held".equals(state)) {
                    fenceToken = reply.getLong("fence_token", 0L);
                    onAcquired();
                    return;
                }
                long elapsed = System.currentTimeMillis() - acquireStartedAtMillis;
                if (timeoutSeconds > 0 && elapsed >= timeoutSeconds * 1000L) {
                    releaseLock();
                    getContext().onFailure(
                            new AbortException("varroaLock: timed out waiting for " + qualifiedName));
                    return;
                }
                Timer.get().schedule(this::attemptAcquire,
                        VarroaPollPolicy.nextDelayMillis(elapsed, random()), TimeUnit.MILLISECONDS);
            } catch (Exception e) {
                try {
                    getContext().onFailure(e);
                } catch (Exception ignored) {
                    // The context is already closed.
                }
            }
        }

        private void onAcquired() throws Exception {
            Run<?, ?> run = getContext().get(Run.class);
            VarroaActiveLocksAction.of(run).push(qualifiedName, fenceToken);
            EnvironmentExpander original = getContext().get(EnvironmentExpander.class);
            EnvironmentExpander merged = EnvironmentExpander.merge(original, new LockTokenExpander(fenceToken));
            scheduleRenew();
            getContext().newBodyInvoker()
                    .withContext(merged)
                    .withCallback(new LockBodyCallback())
                    .start();
        }

        private void scheduleRenew() {
            Timer.get().schedule(this::renew, VarroaPollPolicy.LOCK_RENEW_MILLIS, TimeUnit.MILLISECONDS);
        }

        private void renew() {
            if (released.get()) {
                return;
            }
            try {
                VarroaCrossTenantReply reply = VarroaCrossTenantReply.parse(
                        VarroaCrossTenantOps.client().crossTenantLockRenew(callerJobPath, qualifiedName, runId,
                                VarroaPollPolicy.LOCK_LEASE_SECONDS));
                if (reply.isFailure()) {
                    scheduleRenew();
                    return;
                }
                String state = reply.getString("state", "");
                if ("lost".equals(state)) {
                    released.set(true);
                    try {
                        getContext().onFailure(
                                new AbortException("varroaLock: the lease for " + qualifiedName + " was lost"));
                    } catch (Exception ignored) {
                        // The context is already closed.
                    }
                    return;
                }
                if ("releasing".equals(state) && !releasingWarned) {
                    releasingWarned = true;
                    logWarning(qualifiedName + " is releasing after an interruption; "
                            + "the body keeps running until it ends or its cell dies");
                }
                scheduleRenew();
            } catch (Exception e) {
                scheduleRenew();
            }
        }

        private void logWarning(String message) {
            try {
                TaskListener listener = getContext().get(TaskListener.class);
                if (listener != null) {
                    listener.getLogger().println("WARNING: varroaLock: " + message);
                }
            } catch (Exception ignored) {
                // Best-effort logging only.
            }
        }

        private void releaseLock() {
            if (!released.compareAndSet(false, true)) {
                return;
            }
            try {
                VarroaCrossTenantOps.client().crossTenantLockRelease(callerJobPath, qualifiedName, runId);
            } catch (Exception ignored) {
                // Best effort: the recorder's own lease expiry is the backstop.
            }
        }

        /** {@code true} once the lock has released or lost its lease. Package-private for testing. */
        boolean isReleasedForTesting() {
            return released.get();
        }

        /** Package-private for testing: drives {@link #releaseLock()} directly, with no reflection. */
        void releaseLockForTesting() {
            releaseLock();
        }

        private Random random() {
            if (random == null) {
                random = new Random();
            }
            return random;
        }

        /** Runs the body, then always releases the lock before propagating the body's own outcome. */
        private final class LockBodyCallback extends BodyExecutionCallback {

            private static final long serialVersionUID = 1L;

            @Override
            public void onSuccess(StepContext context, Object result) {
                finish(context, null, result);
            }

            @Override
            public void onFailure(StepContext context, Throwable t) {
                finish(context, t, null);
            }

            private void finish(StepContext context, Throwable failure, Object result) {
                releaseLock();
                try {
                    Run<?, ?> run = context.get(Run.class);
                    if (run != null) {
                        VarroaActiveLocksAction.of(run).pop(qualifiedName);
                    }
                } catch (Exception ignored) {
                    // Best-effort bookkeeping only.
                }
                if (failure != null) {
                    context.onFailure(failure);
                } else {
                    context.onSuccess(result);
                }
            }
        }

        private static final class LockTokenExpander extends EnvironmentExpander {

            private static final long serialVersionUID = 1L;

            private final long token;

            LockTokenExpander(long token) {
                this.token = token;
            }

            @Override
            public void expand(EnvVars env) {
                env.override("VARROA_LOCK_TOKEN", Long.toString(token));
            }
        }

        private static String qualify(String name, String callerTenant) {
            return name.contains("/") ? name : callerTenant + "/" + name;
        }

        private static String isoInstant(long epochMillis) {
            return Instant.ofEpochMilli(epochMillis).toString();
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
