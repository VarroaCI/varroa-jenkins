package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.Cause;
import hudson.triggers.SCMTrigger;
import hudson.triggers.TimerTrigger;

import java.util.List;

/**
 * The cause a build carries when Varroa triggers it.
 *
 * <p>The cause holds the Varroa actor, the identity the run record names. A
 * headless cell holds no Jenkins {@code User} record for that identity, so
 * {@link Cause.UserIdCause} answers the literal {@code anonymous} for it. This
 * cause answers the actor itself, so the build header and console line 1 name
 * the actor.
 *
 * <p>Two causes with the same actor are equal. {@link hudson.model.CauseAction}
 * merges equal causes, so the equality keeps one cause for one actor.
 *
 * <p>The cause renders through {@link #getShortDescription()}, so it needs no
 * Jelly view.
 *
 * <p>The class also holds the one copy of the cause-to-actor precedence, in
 * {@link #actorFromCauses}.
 */
public class VarroaUserCause extends Cause {

    private final String userId;

    /** Creates a cause for {@code userId}. A null value reads as empty. */
    public VarroaUserCause(String userId) {
        this.userId = userId != null ? userId : "";
    }

    /** Answers the Varroa actor that triggered the build. */
    public String getUserId() {
        return userId;
    }

    /**
     * Normalizes a caller-supplied actor name to the value the run record
     * carries. It trims surrounding whitespace, drops control characters and
     * newlines, and reads the literal {@code anonymous} as empty.
     *
     * <p>A headless cell holds no Jenkins {@code User} record for the actor,
     * so a name that reads as {@code anonymous} names no one. One copy of this
     * rule keeps the stamped actor and the Varroa cause equal for one request.
     *
     * @param actor the raw actor name, possibly null
     * @return the normalized actor, never null
     */
    public static String normalizeActor(String actor) {
        if (actor == null) {
            return "";
        }
        StringBuilder cleaned = new StringBuilder(actor.length());
        for (int i = 0; i < actor.length(); i++) {
            char c = actor.charAt(i);
            if (!Character.isISOControl(c)) {
                cleaned.append(c);
            }
        }
        String normalized = cleaned.toString().trim();
        return "anonymous".equals(normalized) ? "" : normalized;
    }

    /**
     * Resolves the actor from a list of causes. One class holds the precedence,
     * so a build event, a claim, and a rejected pipeline marker name the same
     * actor for the same causes.
     *
     * <p>The precedence is:
     * <ol>
     *   <li>the first non-empty {@link VarroaUserCause#getUserId()}
     *   <li>then the first non-empty {@link Cause.UserIdCause#getUserId()}
     *   <li>{@link SCMTrigger.SCMTriggerCause} or {@link Cause.RemoteCause} → {@code "scm"}
     *   <li>{@link TimerTrigger.TimerTriggerCause} → {@code "timer"}
     *   <li>{@link Cause.UpstreamCause} → {@code "upstream:<upstreamProject>"}
     * </ol>
     *
     * <p>The Varroa cause wins over a user cause. A headless cell holds no
     * Jenkins {@code User} record for the actor, so a user cause answers the
     * literal {@code anonymous}. The list order decides only between two causes
     * of one kind.
     *
     * @param causes the causes of a run or a queue item, possibly null
     * @return the actor, or null when no cause names one
     */
    public static String actorFromCauses(List<Cause> causes) {
        if (causes == null) {
            return null;
        }
        for (Cause cause : causes) {
            if (cause instanceof VarroaUserCause) {
                String actor = ((VarroaUserCause) cause).getUserId();
                if (!actor.isEmpty()) {
                    return actor;
                }
            }
        }
        for (Cause cause : causes) {
            if (cause instanceof Cause.UserIdCause) {
                String actor = ((Cause.UserIdCause) cause).getUserId();
                if (actor != null && !actor.isEmpty()) {
                    return actor;
                }
            }
        }
        for (Cause cause : causes) {
            if (cause instanceof SCMTrigger.SCMTriggerCause || cause instanceof Cause.RemoteCause) {
                return "scm";
            }
        }
        for (Cause cause : causes) {
            if (cause instanceof TimerTrigger.TimerTriggerCause) {
                return "timer";
            }
        }
        for (Cause cause : causes) {
            if (cause instanceof Cause.UpstreamCause) {
                return "upstream:" + ((Cause.UpstreamCause) cause).getUpstreamProject();
            }
        }
        return null;
    }

    @Override
    public String getShortDescription() {
        return "Started by " + userId + " through Varroa";
    }

    @Override
    public boolean equals(Object other) {
        return other instanceof VarroaUserCause
                && userId.equals(((VarroaUserCause) other).userId);
    }

    @Override
    public int hashCode() {
        return userId.hashCode();
    }
}
