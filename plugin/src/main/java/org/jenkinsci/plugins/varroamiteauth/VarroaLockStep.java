package org.jenkinsci.plugins.varroamiteauth;

import hudson.AbortException;
import hudson.EnvVars;
import hudson.Extension;
import hudson.model.Run;
import hudson.model.TaskListener;

import java.time.Instant;
import java.util.Random;
import java.util.Set;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.logging.Level;
import java.util.logging.Logger;

import org.jenkinsci.plugins.workflow.steps.BodyExecution;
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
 * before the body and releases it in a {@code finally}. {@code name} is
 * always a bare name, and a name that holds a {@code /} fails the step
 * before any call. {@code tenant} names the tenant that owns the lock. An
 * empty {@code tenant} names the caller's own tenant, which the recorder
 * resolves from the identity the gateway stamps. The step never builds a
 * qualified name itself: a controller tenant holds a {@code /} of its own,
 * so a qualified name is ambiguous.
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
    private String tenant = "";
    private int timeout;

    @DataBoundConstructor
    public VarroaLockStep(String name) {
        this.name = name;
    }

    public String getName() {
        return name;
    }

    /** The tenant that owns the lock; empty names the caller's own tenant. */
    public String getTenant() {
        return tenant;
    }

    @DataBoundSetter
    public void setTenant(String tenant) {
        this.tenant = tenant != null ? tenant : "";
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
        private final String tenantArg;
        private final int timeoutSeconds;

        // The tenant that owns the lock, as the acquire reply names it.
        // It feeds varroa.lock.tenant and the log lines only, never the
        // wire: the wire carries tenantArg as the caller wrote it.
        private String ownerTenant = "";
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

        // Set by renew() when the recorder ends the hold (a lost lease or
        // a permanent veto). It stops the renewals only. It never marks
        // the lock released, so the body callback still sends the release
        // and a waiter does not wait out the lease.
        private final AtomicBoolean holdEnded = new AtomicBoolean(false);
        private boolean releasingWarned;

        private transient Random random;

        // The running body, once the lock is held. A hold that ends early
        // (a lost lease or a permanent veto) cancels it, so the body stops
        // its protected side effects instead of running on unfenced.
        private volatile BodyExecution body;

        Execution(VarroaLockStep step, StepContext context) {
            super(context);
            this.nameArg = step.getName();
            this.tenantArg = step.getTenant();
            this.timeoutSeconds = step.getTimeout();
        }

        @Override
        public boolean start() throws Exception {
            checkName(nameArg);
            Run<?, ?> run = getContext().get(Run.class);
            runId = callerRunId(run);
            callerJobPath = run.getParent().getFullName();
            ownerTenant = tenantArg;

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
                        VarroaActiveLocksAction.of(run).push(ownerTenant, nameArg, fenceToken);
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
                        VarroaCrossTenantOps.client().crossTenantLockAcquire(callerJobPath, tenantArg, nameArg, runId,
                                VarroaPollPolicy.LOCK_LEASE_SECONDS, isoInstant(acquireStartedAtMillis)));
                if (reply.isFailure()) {
                    getContext().onFailure(
                            new AbortException("varroaLock: the recorder did not answer (" + reply.code + ")"));
                    return;
                }
                String reasonCode = reply.getString("reason_code", "");
                if (isPermanentVeto(reasonCode)) {
                    releaseLock();
                    getContext().onFailure(vetoFailure(reply, reasonCode));
                    return;
                }
                ownerTenant = reply.getString("tenant", tenantArg);
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
                            new AbortException("varroaLock: timed out waiting for " + displayName()));
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
            VarroaActiveLocksAction.of(run).push(ownerTenant, nameArg, fenceToken);
            EnvironmentExpander original = getContext().get(EnvironmentExpander.class);
            EnvironmentExpander merged = EnvironmentExpander.merge(original, new LockTokenExpander(fenceToken));
            body = getContext().newBodyInvoker()
                    .withContext(merged)
                    .withCallback(new LockBodyCallback())
                    .start();
            // Renewals start only after body is set, so endHold always
            // finds the running body to cancel.
            scheduleRenew();
        }

        private void scheduleRenew() {
            Timer.get().schedule(this::renew, VarroaPollPolicy.LOCK_RENEW_MILLIS, TimeUnit.MILLISECONDS);
        }

        private void renew() {
            if (released.get() || holdEnded.get()) {
                return;
            }
            try {
                VarroaCrossTenantReply reply = VarroaCrossTenantReply.parse(
                        VarroaCrossTenantOps.client().crossTenantLockRenew(callerJobPath, tenantArg, nameArg, runId,
                                VarroaPollPolicy.LOCK_LEASE_SECONDS));
                if (reply.isFailure()) {
                    scheduleRenew();
                    return;
                }
                String reasonCode = reply.getString("reason_code", "");
                if (isPermanentVeto(reasonCode)) {
                    // A revoked grant or a fenced cell never renews again,
                    // so the hold ends here, the same as a lost lease.
                    endHold(vetoFailure(reply, reasonCode));
                    return;
                }
                String state = reply.getString("state", "");
                if ("lost".equals(state)) {
                    endHold(new AbortException("varroaLock: the lease for " + displayName() + " was lost"));
                    return;
                }
                if ("releasing".equals(state) && !releasingWarned) {
                    releasingWarned = true;
                    logWarning(displayName() + " is releasing after an interruption; "
                            + "the body keeps running until it ends or its cell dies");
                }
                scheduleRenew();
            } catch (Exception e) {
                scheduleRenew();
            }
        }

        /**
         * Ends a hold the recorder no longer honors. It cancels the running
         * body with {@code cause}, and the body callback then releases the
         * lock and fails the step with it. A body that already completed
         * keeps its own outcome: it ran while the last renew still held
         * the lock. With no body, it releases and fails the step directly.
         */
        private void endHold(Throwable cause) {
            if (!holdEnded.compareAndSet(false, true)) {
                return;
            }
            BodyExecution running = body;
            if (running != null) {
                running.cancel(cause);
                return;
            }
            releaseLock();
            try {
                getContext().onFailure(cause);
            } catch (Exception ignored) {
                // The context is already closed.
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
                VarroaCrossTenantOps.client().crossTenantLockRelease(callerJobPath, tenantArg, nameArg, runId);
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
                        VarroaActiveLocksAction.of(run).pop(ownerTenant, nameArg);
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

        /**
         * Refuses a name the recorder would refuse, before any call: an
         * empty name, or a name that holds a {@code /}. Another tenant's
         * lock is named through the {@code tenant} argument.
         */
        static void checkName(String name) throws AbortException {
            if (name == null || name.isEmpty()) {
                throw new AbortException("varroaLock: name must not be empty");
            }
            if (name.contains("/")) {
                throw new AbortException("varroaLock: name '" + name + "' must not contain '/'. "
                        + "Name another tenant's lock with the tenant argument.");
            }
        }

        /**
         * Reports whether a recorder veto can never turn into a grant by
         * polling again. {@code NOT_READY} and {@code THROTTLED} are
         * retryable, so the step keeps polling on them.
         */
        static boolean isPermanentVeto(String reasonCode) {
            switch (reasonCode) {
                case "FENCED":
                case "CROSS_TENANT":
                case "NOT_HOME":
                case "INVALID_NAME":
                    return true;
                default:
                    return false;
            }
        }

        private AbortException vetoFailure(VarroaCrossTenantReply reply, String reasonCode) {
            return new AbortException("varroaLock: the recorder refused " + displayName() + " ("
                    + reasonCode + "): " + reply.getString("message", ""));
        }

        /** The lock as a log line names it: the owner tenant, when known, then the bare name. */
        private String displayName() {
            return ownerTenant == null || ownerTenant.isEmpty() ? nameArg : ownerTenant + "/" + nameArg;
        }

        private static String isoInstant(long epochMillis) {
            return Instant.ofEpochMilli(epochMillis).toString();
        }

        private static String callerRunId(Run<?, ?> run) {
            VarroaIntentAction action = run.getAction(VarroaIntentAction.class);
            return action != null ? action.getRunId() : "";
        }
    }
}
