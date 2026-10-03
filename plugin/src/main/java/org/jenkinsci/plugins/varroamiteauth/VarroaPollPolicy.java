package org.jenkinsci.plugins.varroamiteauth;

import java.util.Random;

/**
 * D8's shared poll cadence: 2 s for the first 30 s, then 5 s to 5 minutes,
 * then 15 s, each with plus or minus 20 percent jitter. Both {@link
 * VarroaBuildStep}'s wait and {@link VarroaLockStep}'s waiting poller use
 * this cadence (D11).
 */
final class VarroaPollPolicy {

    private static final long DEFAULT_PENDING_WARN_MILLIS = 10L * 60 * 1000;
    private static final long DEFAULT_PENDING_CEILING_MILLIS = 24L * 60 * 60 * 1000;
    private static final long DEFAULT_LOCK_RENEW_MILLIS = 60L * 1000;
    private static final long DEFAULT_LOCK_RESUME_GATE_MILLIS = VarroaPollPolicy.LOCK_LEASE_SECONDS * 1000;

    /** D8: a {@code pending} wait warns once it has run this long. Mutable so a test can shrink it. */
    static long PENDING_WARN_MILLIS = DEFAULT_PENDING_WARN_MILLIS;

    /** D8: a {@code pending} wait fails once it has run this long. Mutable so a test can shrink it. */
    static long PENDING_CEILING_MILLIS = DEFAULT_PENDING_CEILING_MILLIS;

    /** D11: the lease a lock acquire requests, in seconds. */
    static final long LOCK_LEASE_SECONDS = 300L;

    /** D11: the lock step renews on this cadence. Mutable so a test can shrink it. */
    static long LOCK_RENEW_MILLIS = DEFAULT_LOCK_RENEW_MILLIS;

    /**
     * A resumed lock step stops trying to confirm its hold after this long
     * and ends the hold. It equals the lease: past it, the recorder can
     * already have freed the row. Mutable so a test can shrink it.
     */
    static long LOCK_RESUME_GATE_MILLIS = DEFAULT_LOCK_RESUME_GATE_MILLIS;

    private VarroaPollPolicy() {
    }

    /** Test seam: shrinks the pending warn/ceiling bounds and the renew cadence. */
    static void setBoundsForTesting(long pendingWarnMillis, long pendingCeilingMillis, long lockRenewMillis) {
        PENDING_WARN_MILLIS = pendingWarnMillis;
        PENDING_CEILING_MILLIS = pendingCeilingMillis;
        LOCK_RENEW_MILLIS = lockRenewMillis;
    }

    /** Test seam: shrinks the bound on a resumed lock step's first renew. */
    static void setResumeGateForTesting(long resumeGateMillis) {
        LOCK_RESUME_GATE_MILLIS = resumeGateMillis;
    }

    /** Test seam: restores every bound to its production default. */
    static void resetBoundsForTesting() {
        PENDING_WARN_MILLIS = DEFAULT_PENDING_WARN_MILLIS;
        PENDING_CEILING_MILLIS = DEFAULT_PENDING_CEILING_MILLIS;
        LOCK_RENEW_MILLIS = DEFAULT_LOCK_RENEW_MILLIS;
        LOCK_RESUME_GATE_MILLIS = DEFAULT_LOCK_RESUME_GATE_MILLIS;
    }

    /** The base interval for a poll that has been running {@code elapsedMillis}. */
    static long baseIntervalMillis(long elapsedMillis) {
        if (elapsedMillis < 30_000L) {
            return 2_000L;
        }
        if (elapsedMillis < 5L * 60 * 1000) {
            return 5_000L;
        }
        return 15_000L;
    }

    /** Applies plus-or-minus 20 percent jitter to {@code baseMillis}. */
    static long withJitter(long baseMillis, Random random) {
        double factor = 0.8 + random.nextDouble() * 0.4;
        return Math.max(1L, Math.round(baseMillis * factor));
    }

    /** The jittered delay for a poll that has been running {@code elapsedMillis}. */
    static long nextDelayMillis(long elapsedMillis, Random random) {
        return withJitter(baseIntervalMillis(elapsedMillis), random);
    }
}
