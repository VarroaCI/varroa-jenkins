package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.Action;
import hudson.model.Cause;
import hudson.model.Job;
import hudson.model.ParameterValue;
import hudson.model.ParametersAction;
import hudson.model.Queue;
import hudson.triggers.SCMTrigger;
import hudson.triggers.TimerTrigger;

import java.lang.reflect.Method;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.time.Instant;
import java.time.format.DateTimeFormatter;
import java.time.temporal.ChronoUnit;
import java.util.ArrayList;
import java.util.Collections;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * D5: derives the origin, the intent key, the accepted revision, the
 * params digest, and the delivery id, from a queue item at claim time.
 *
 * <p>The revision read has two reflective sources — an {@code
 * SCMRevisionAction} on the item, or the job's branch project factory —
 * because C2 adds no {@code scm-api} or {@code branch-api} compile
 * dependency.
 */
public final class IntentKey {

    private static final Logger LOGGER = Logger.getLogger(IntentKey.class.getName());

    /** Closed set of {@code ClaimRequest.origin} values (D5, D9). */
    public static final String ORIGIN_VARROA_INTENT = "VARROA_INTENT";
    public static final String ORIGIN_SCM = "SCM";
    public static final String ORIGIN_CRON = "CRON";
    public static final String ORIGIN_CELL_LOCAL = "CELL_LOCAL";

    private static final String SCHEME_SCM = "scm:";
    private static final String SCHEME_CRON = "cron:";
    private static final String SCHEME_API = "api:";

    private static final int MAX_REVISION_LENGTH = 128;
    private static final int MAX_CAUSE_LENGTH = 512;

    /** The wait between two reads of the branch project factory. */
    static final long WAIT_INTERVAL_MILLIS = 2000L;

    /** The budget of the revision wait; at its end the key falls back to {@code api:<uuid>}. */
    static final long WAIT_DEADLINE_MILLIS = 30_000L;

    private IntentKey() {
    }

    /** The derived origin, intent key, and accepted revision (empty when none applies). */
    public static final class Result {
        public final String origin;
        public final String intentKey;
        public final String revision;

        Result(String origin, String intentKey, String revision) {
            this.origin = origin;
            this.intentKey = intentKey;
            this.revision = revision != null ? revision : "";
        }
    }

    /**
     * Derives the claim-time origin and intent key (D5 table), in order:
     * an existing key on the action, a revision-bearing action, the
     * branch project factory, a timer cause, and finally the
     * {@code api:<uuid>} fallback.
     *
     * <p>For a branch cause, each call reads the factory once and never
     * waits. It answers {@code null} while the factory revision equals
     * the admission revision and {@code nowMillis} is before
     * {@code revisionDeadlineMillis}. The caller then reads again after
     * {@link #WAIT_INTERVAL_MILLIS}. At the deadline, the key falls back
     * to {@code api:<uuid>}.
     */
    public static Result derive(Queue.Item item, Job<?, ?> job, VarroaIntentAction action, long nowMillis,
            long revisionDeadlineMillis) {
        return derive(item, job, action, nowMillis, revisionDeadlineMillis, job::getParent);
    }

    /**
     * Package-private test seam: takes the job's parent from {@code
     * parentSupplier} instead of calling {@code job.getParent()}
     * directly. {@code AbstractItem} carries a second, covariant {@code
     * getParent()} declaration (returning {@code Jenkins}) alongside its
     * primary one (returning {@code ItemGroup}); Mockito's invocation
     * bookkeeping conflates the two on a raw-typed mock, so a test cannot
     * stub {@code Job.getParent()} directly. The production overload
     * above passes the real {@code job::getParent} method reference, so
     * behavior is identical.
     */
    static Result derive(Queue.Item item, Job<?, ?> job, VarroaIntentAction action, long nowMillis,
            long revisionDeadlineMillis, java.util.function.Supplier<Object> parentSupplier) {
        if (action != null && !action.getIntentKey().isEmpty()) {
            return new Result(action.getOrigin(), action.getIntentKey(), action.getRevision());
        }

        String jobPath = job.getFullName();
        List<Cause> causes = item.getCauses();

        String actionRevision = readRevisionFromAction(item);
        if (actionRevision != null) {
            return new Result(ORIGIN_SCM, SCHEME_SCM + jobPath + ":" + actionRevision, actionRevision);
        }

        String admissionRevision = action != null ? action.getAdmissionRevision() : null;
        if (hasBranchCause(causes)) {
            Object factory = branchProjectFactory(parentSupplier);
            if (factory != null) {
                String candidate = readFactoryRevision(factory, job);
                if (candidate != null && !candidate.equals(admissionRevision)) {
                    return new Result(ORIGIN_SCM, SCHEME_SCM + jobPath + ":" + candidate, candidate);
                }
                if (nowMillis < revisionDeadlineMillis) {
                    return null;
                }
            }
            LOGGER.log(Level.WARNING,
                    "IntentKey: no accepted revision for {0} after the 30 s wait; falling back to api:<uuid>",
                    jobPath);
            PluginCounters.increment(PluginCounters.CLAIMS_NO_REVISION);
            return new Result(ORIGIN_CELL_LOCAL, SCHEME_API + Uuid7.next(), "");
        }

        if (hasTimerCause(causes)) {
            long fireMillis = action != null ? action.getAdmittedAtMillis() : nowMillis;
            return new Result(ORIGIN_CRON, SCHEME_CRON + jobPath + ":" + fireMinute(fireMillis), "");
        }

        LOGGER.log(Level.FINE, "IntentKey: no revision for {0}; falling back to api:<uuid>", jobPath);
        PluginCounters.increment(PluginCounters.CLAIMS_NO_REVISION);
        return new Result(ORIGIN_CELL_LOCAL, SCHEME_API + Uuid7.next(), "");
    }

    /**
     * Reads the job's current built revision for {@link
     * VarroaIntentAction#getAdmissionRevision()}. An in-memory field read:
     * the branch project factory returns an already-attached job
     * property, so this performs no I/O.
     */
    public static String readAdmissionRevision(Job<?, ?> job) {
        Object factory = branchProjectFactory(job::getParent);
        if (factory == null) {
            return null;
        }
        return readFactoryRevision(factory, job);
    }

    // --- Revision sources ---

    private static String readRevisionFromAction(Queue.Item item) {
        for (Action action : item.getActions()) {
            if (hasSimpleNameInHierarchy(action.getClass(), "SCMRevisionAction")) {
                Object revision = invokeNoArg(action, "getRevision");
                if (revision != null) {
                    return normalizeRevision(String.valueOf(revision));
                }
            }
        }
        return null;
    }

    private static Object branchProjectFactory(java.util.function.Supplier<Object> parentSupplier) {
        Object parent = parentSupplier.get();
        if (parent == null || !hasSimpleNameInHierarchy(parent.getClass(), "MultiBranchProject")) {
            return null;
        }
        return invokeNoArg(parent, "getProjectFactory");
    }

    private static String readFactoryRevision(Object factory, Job<?, ?> job) {
        Object revision = invokeWithArg(factory, "getRevision", job);
        if (revision == null) {
            revision = invokeWithArg(factory, "getLastSeenRevision", job);
        }
        return revision != null ? normalizeRevision(String.valueOf(revision)) : null;
    }

    // --- Cause classification ---

    private static boolean hasBranchCause(List<Cause> causes) {
        for (Cause cause : causes) {
            if (hasSimpleNameInHierarchy(cause.getClass(), "BranchEventCause")
                    || hasSimpleNameInHierarchy(cause.getClass(), "BranchIndexingCause")) {
                return true;
            }
        }
        return false;
    }

    private static boolean hasTimerCause(List<Cause> causes) {
        for (Cause cause : causes) {
            if (cause instanceof TimerTrigger.TimerTriggerCause) {
                return true;
            }
        }
        return false;
    }

    /** The first cause's short description, truncated to 512 characters. */
    public static String causeDescription(List<Cause> causes) {
        if (causes == null || causes.isEmpty()) {
            return "";
        }
        String description = causes.get(0).getShortDescription();
        if (description == null) {
            return "";
        }
        return description.length() > MAX_CAUSE_LENGTH ? description.substring(0, MAX_CAUSE_LENGTH) : description;
    }

    // --- Revision normalization ---

    static String normalizeRevision(String raw) {
        String trimmed = String.valueOf(raw).trim();
        if (trimmed.isEmpty() || containsWhitespace(trimmed) || trimmed.length() > MAX_REVISION_LENGTH) {
            return "sha256:" + sha256Hex(trimmed);
        }
        return trimmed;
    }

    private static boolean containsWhitespace(String value) {
        for (int i = 0; i < value.length(); i++) {
            if (Character.isWhitespace(value.charAt(i))) {
                return true;
            }
        }
        return false;
    }

    // --- Cron fire-minute ---

    static String fireMinute(long unixMillis) {
        Instant truncated = Instant.ofEpochMilli(unixMillis).truncatedTo(ChronoUnit.MINUTES);
        return DateTimeFormatter.ISO_INSTANT.format(truncated);
    }

    /**
     * Returns the job's branch name for a job under a multi-branch
     * parent, or {@code ""} for any other job. Jenkins branch-api names a
     * branch job by its branch, so the leaf name is a reasonable
     * best-effort value with no {@code branch-api} compile dependency.
     */
    public static String branchNameFor(Job<?, ?> job) {
        Object parent = job.getParent();
        if (parent == null || !hasSimpleNameInHierarchy(parent.getClass(), "MultiBranchProject")) {
            return "";
        }
        return job.getName();
    }

    // --- Delivery id ---

    /**
     * Returns {@code true} when the item's cause names a branch event or
     * an SCM trigger. The handler attaches a delivery id only to such an
     * item (D5); it never enters an intent key and never changes a
     * decision.
     */
    public static boolean isBranchEventOrScmTriggerCause(List<Cause> causes) {
        for (Cause cause : causes) {
            if (cause instanceof SCMTrigger.SCMTriggerCause) {
                return true;
            }
            if (hasSimpleNameInHierarchy(cause.getClass(), "BranchEventCause")) {
                return true;
            }
        }
        return false;
    }

    /**
     * Returns the newest live {@link DeliveryRegistry} entry for an item
     * whose cause names a branch event or an SCM trigger, or {@code ""}
     * otherwise.
     */
    public static String deliveryIdFor(List<Cause> causes) {
        if (!isBranchEventOrScmTriggerCause(causes)) {
            return "";
        }
        String newest = DeliveryRegistry.get().newest();
        return newest != null ? newest : "";
    }

    // --- params_digest ---

    /** Extracts the queue item's parameters, masking a sensitive value, then digests them. */
    public static String paramsDigest(Queue.Item item) {
        Map<String, String> values = new LinkedHashMap<>();
        Set<String> masked = new HashSet<>();
        collectParameters(item, values, masked);
        return paramsDigest(values, masked);
    }

    /**
     * Reads the queue item's parameters into {@code values}, name to
     * string value, and names each sensitive parameter in {@code masked}.
     *
     * <p>{@code ParameterValue} declares no {@code getValue}; each
     * subclass declares its own, so the read is reflective.
     */
    public static void collectParameters(Queue.Item item, Map<String, String> values, Set<String> masked) {
        for (Action action : item.getActions()) {
            if (action instanceof ParametersAction) {
                for (ParameterValue pv : ((ParametersAction) action).getParameters()) {
                    values.put(pv.getName(), stringValue(pv));
                    if (pv.isSensitive()) {
                        masked.add(pv.getName());
                    }
                }
            }
        }
    }

    /**
     * Digests a canonical string: {@code <name>=<value>} pairs sorted by
     * name and joined with {@code \n}. A masked name contributes
     * {@code <name>=<masked>}. The digest carries no secret value.
     */
    public static String paramsDigest(Map<String, String> values, Set<String> maskedNames) {
        List<String> names = new ArrayList<>(values.keySet());
        Collections.sort(names);
        StringBuilder canonical = new StringBuilder();
        for (String name : names) {
            if (canonical.length() > 0) {
                canonical.append('\n');
            }
            String value = maskedNames.contains(name) ? "<masked>" : values.get(name);
            canonical.append(name).append('=').append(value);
        }
        return sha256Hex(canonical.toString());
    }

    private static String stringValue(ParameterValue pv) {
        Object value = invokeNoArg(pv, "getValue");
        return value != null ? String.valueOf(value) : String.valueOf(pv);
    }

    // --- Reflection helpers ---

    private static boolean hasSimpleNameInHierarchy(Class<?> clazz, String simpleName) {
        Class<?> current = clazz;
        while (current != null && current != Object.class) {
            if (current.getSimpleName().equals(simpleName)) {
                return true;
            }
            for (Class<?> iface : current.getInterfaces()) {
                if (hasSimpleNameInHierarchy(iface, simpleName)) {
                    return true;
                }
            }
            current = current.getSuperclass();
        }
        return false;
    }

    private static Object invokeNoArg(Object target, String methodName) {
        if (target == null) {
            return null;
        }
        try {
            Method method = target.getClass().getMethod(methodName);
            return method.invoke(target);
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "IntentKey: reflective call " + methodName + " failed", e);
            return null;
        }
    }

    private static Object invokeWithArg(Object target, String methodName, Object arg) {
        if (target == null) {
            return null;
        }
        try {
            for (Method method : target.getClass().getMethods()) {
                if (method.getName().equals(methodName) && method.getParameterCount() == 1
                        && method.getParameterTypes()[0].isInstance(arg)) {
                    return method.invoke(target, arg);
                }
            }
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "IntentKey: reflective call " + methodName + " failed", e);
        }
        return null;
    }

    // --- SHA-256 ---

    static String sha256Hex(String value) {
        try {
            MessageDigest digest = MessageDigest.getInstance("SHA-256");
            byte[] hash = digest.digest(value.getBytes(StandardCharsets.UTF_8));
            StringBuilder hex = new StringBuilder(hash.length * 2);
            for (byte b : hash) {
                hex.append(String.format(Locale.ROOT, "%02x", b & 0xff));
            }
            return hex.toString();
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException("SHA-256 not available", e);
        }
    }
}
