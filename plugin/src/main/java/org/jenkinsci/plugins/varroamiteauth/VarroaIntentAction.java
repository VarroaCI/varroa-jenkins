package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.Action;
import hudson.model.Cause;
import hudson.model.InvisibleAction;
import hudson.model.Job;
import hudson.model.Queue;

import java.util.List;

/**
 * Carries the claim gate's per-item state from admission through dispatch
 * to the claim (D1, D4, D5).
 *
 * <p>{@link VarroaQueueDecisionHandler} sets the run id, the delivery id,
 * the admission revision, and the admitted-at time when it admits the
 * item. The claimer thread in {@link VarroaClaimQueue} later sets the
 * origin, the intent key, the revision, and the granted flag.
 *
 * <p>{@code actor} carries the calling identity the claim reports to the
 * ledger (D5). {@link VarroaRunTriggerAction} stamps a caller-supplied
 * value on a cross-tenant trigger; {@link VarroaQueueDecisionHandler}
 * derives one from the scheduling authentication when the action carries
 * none.
 *
 * <p>This class also implements {@link Queue.QueueAction} so two granted
 * intents for the same job never collapse into one queue item, and one
 * intent scheduled twice never queues twice (D4).
 *
 * <p>That fold reads queued items only. A build that already left the queue
 * is not a queued item, so a second schedule of its run id passes the fold.
 * {@link VarroaRunTriggerAction#findBuildForRunId} covers that case on the
 * JVM that holds the build. The ledger covers it on every other JVM: the
 * item leaves the queue only after the ledger stored its start mark, and a
 * repeat claim from another JVM for a marked run answers {@code duplicate}.
 */
public class VarroaIntentAction extends InvisibleAction implements Queue.QueueAction {

    private final String runId;
    private final String deliveryId;
    private final String admissionRevision;
    private final long admittedAtMillis;

    private volatile String origin = "";
    private volatile String intentKey = "";
    private volatile String revision = "";
    private volatile String forgeRevision = "";
    private volatile boolean granted;
    private volatile String actor = "";
    private volatile long grantedNumber;
    private volatile long numberConflict;
    private transient volatile boolean applyPending;
    private transient volatile int markFailures;
    private transient volatile long claimNotBeforeMillis;

    /**
     * Constructs the action the handler stamps at admission (D4 step 6).
     *
     * @param runId              a fresh UUIDv7, minted by the handler
     * @param deliveryId         the newest {@code X-Varroa-Delivery} value in the window, or empty
     * @param admissionRevision  the job's current built revision, or {@code null} when it has none
     * @param admittedAtMillis   the wall clock read once, at admission
     */
    public VarroaIntentAction(String runId, String deliveryId, String admissionRevision, long admittedAtMillis) {
        this.runId = runId != null ? runId : "";
        this.deliveryId = deliveryId != null ? deliveryId : "";
        this.admissionRevision = admissionRevision;
        this.admittedAtMillis = admittedAtMillis;
    }

    /**
     * Stamps a fresh action with a new run id, the newest delivery id in
     * {@code causes}, and the job's current built revision, read the same
     * way {@link VarroaQueueDecisionHandler} reads them at admission.
     *
     * <p>{@link VarroaClaimDispatcher} calls this for an item that reaches
     * dispatch with no action — restored from disk, or scheduled while the
     * gate was off — so the claimer always has a run id to claim (D4 step
     * 6; ruling p).
     */
    static VarroaIntentAction stampMissing(Job<?, ?> job, List<Cause> causes) {
        long admittedAtMillis = System.currentTimeMillis();
        String runId = Uuid7.next();
        String deliveryId = IntentKey.deliveryIdFor(causes);
        String admissionRevision = IntentKey.readAdmissionRevision(job);
        return new VarroaIntentAction(runId, deliveryId, admissionRevision, admittedAtMillis);
    }

    public String getRunId() {
        return runId;
    }

    public String getDeliveryId() {
        return deliveryId;
    }

    /** Returns the job's built revision at admission, or {@code null} when it had none. */
    public String getAdmissionRevision() {
        return admissionRevision;
    }

    public long getAdmittedAtMillis() {
        return admittedAtMillis;
    }

    public String getOrigin() {
        return origin;
    }

    /** Set by the claimer thread (D5). Package-private: only this package's claimer writes it. */
    void setOrigin(String origin) {
        this.origin = origin != null ? origin : "";
    }

    public String getIntentKey() {
        return intentKey;
    }

    /** Set by the claimer thread (D5). Package-private: only this package's claimer writes it. */
    void setIntentKey(String intentKey) {
        this.intentKey = intentKey != null ? intentKey : "";
    }

    public String getRevision() {
        return revision;
    }

    /** Set by the claimer thread (D5). Package-private: only this package's claimer writes it. */
    void setRevision(String revision) {
        this.revision = revision != null ? revision : "";
    }

    /**
     * Returns the commit the forge knows for the claimed revision, or
     * {@code ""} when the revision object carries none. The start event
     * sends it as {@code scm_revision}.
     */
    public String getForgeRevision() {
        return forgeRevision != null ? forgeRevision : "";
    }

    /** Set by the claimer thread (D5). Package-private: only this package's claimer writes it. */
    void setForgeRevision(String forgeRevision) {
        this.forgeRevision = forgeRevision != null ? forgeRevision : "";
    }

    public boolean isGranted() {
        return granted;
    }

    /** Set by the claimer thread after a {@code granted} decision. Package-private. */
    void setGranted(boolean granted) {
        this.granted = granted;
    }

    /** Returns the build number the grant applied, or zero before a grant applied. */
    public long getGrantedNumber() {
        return grantedNumber;
    }

    /** Set by the apply after the job's next build number matches the grant. Package-private. */
    void setGrantedNumber(long grantedNumber) {
        this.grantedNumber = grantedNumber;
    }

    /**
     * Returns a granted number the job could not apply because its counter
     * already passed it, or zero. The next claim names it as {@code
     * number_conflict}, so the ledger answers a fresh number.
     */
    public long getNumberConflict() {
        return numberConflict;
    }

    /** Set by the apply on {@code NUMBER_CONFLICT}, cleared by a matching apply. Package-private. */
    void setNumberConflict(long numberConflict) {
        this.numberConflict = numberConflict;
    }

    /**
     * Returns true while a grant waits for the job's apply gate. The
     * dispatcher then blocks the item and offers no claim.
     */
    public boolean isApplyPending() {
        return applyPending;
    }

    /** Set by the claimer when it hands a grant to the gate. Package-private. */
    void setApplyPending(boolean applyPending) {
        this.applyPending = applyPending;
    }

    /**
     * Counts one failed start mark and answers the new count. Only the
     * claimer writes the count, one attempt at a time. Package-private.
     */
    int addMarkFailure() {
        markFailures = markFailures + 1;
        return markFailures;
    }

    /** Clears the failed start-mark count after a granted mark. Package-private. */
    void clearMarkFailures() {
        markFailures = 0;
    }

    /** Returns the count of failed start marks since the last granted one. */
    public int getMarkFailures() {
        return markFailures;
    }

    /**
     * Returns the wall-clock time before which the dispatcher offers no
     * claim for this item, or zero. A failed start mark sets it.
     */
    public long getClaimNotBeforeMillis() {
        return claimNotBeforeMillis;
    }

    /** Set by the claimer after a failed start mark. Package-private. */
    void setClaimNotBeforeMillis(long claimNotBeforeMillis) {
        this.claimNotBeforeMillis = claimNotBeforeMillis;
    }

    /**
     * Returns the calling identity the claim carries, or the empty string
     * when no identity was stamped. {@link VarroaRunTriggerAction} stamps a
     * caller-supplied value; {@link VarroaQueueDecisionHandler} derives one
     * from the scheduling authentication when this stays empty.
     */
    public String getActor() {
        return actor;
    }

    /**
     * Set by {@link VarroaRunTriggerAction} at construction, or by {@link
     * VarroaQueueDecisionHandler} when the action carried none. Package-private.
     */
    void setActor(String actor) {
        this.actor = actor != null ? actor : "";
    }

    /**
     * Returns {@code false} only when {@code actions} holds a {@link
     * VarroaIntentAction} carrying the same run id, which folds the new
     * schedule into the queued item instead of creating a second one.
     */
    @Override
    public boolean shouldSchedule(List<Action> actions) {
        for (Action action : actions) {
            if (action instanceof VarroaIntentAction
                    && runId.equals(((VarroaIntentAction) action).getRunId())) {
                return false;
            }
        }
        return true;
    }
}
