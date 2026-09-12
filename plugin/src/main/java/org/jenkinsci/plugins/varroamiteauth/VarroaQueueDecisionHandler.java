package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Action;
import hudson.model.Cause;
import hudson.model.CauseAction;
import hudson.model.Job;
import hudson.model.Queue;
import hudson.security.ACL;

import java.util.ArrayList;
import java.util.List;
import java.util.logging.Level;
import java.util.logging.Logger;

import jenkins.model.Jenkins;

/**
 * Admits every queued item and stamps a fresh {@link VarroaIntentAction}
 * on it (D4 step order). Runs inside the Jenkins queue lock and performs
 * no network call and no other I/O; it returns at once on every path.
 *
 * <p>The claim itself starts later, in {@link VarroaClaimDispatcher}: at
 * this point {@code Queue.schedule2} has not yet created the {@link
 * Queue.Item}, so no claimer could act on it.
 *
 * <p>This handler also derives the claim's {@code actor} (D5): the
 * action's own actor when {@link VarroaRunTriggerAction} already stamped
 * one, otherwise the scheduling authentication's name, with {@code
 * anonymous} and {@link ACL#SYSTEM2} normalized to the empty string.
 */
@Extension
public class VarroaQueueDecisionHandler extends Queue.QueueDecisionHandler {

    private static final Logger LOGGER = Logger.getLogger(VarroaQueueDecisionHandler.class.getName());

    static final String GATE_ENV = "VARROA_CLAIM_GATE";

    @Override
    public boolean shouldSchedule(Queue.Task task, List<Action> actions) {
        return shouldSchedule(task, actions, System.getenv(GATE_ENV));
    }

    /**
     * Package-private overload that takes the gate value directly, so a
     * test can drive every path without mutating the process environment.
     */
    boolean shouldSchedule(Queue.Task task, List<Action> actions, String gateEnvValue) {
        try {
            return decide(task, actions, gateEnvValue);
        } catch (Exception e) {
            // A defect here must never block scheduling: the claim gate
            // is a veto mechanism, not a source of new build failures.
            LOGGER.log(Level.WARNING, "VarroaQueueDecisionHandler: admitting after an unexpected error", e);
            return true;
        }
    }

    private boolean decide(Queue.Task task, List<Action> actions, String gateEnvValue) {
        // Step 1: the gate is off.
        if (!gateEnabled(gateEnvValue)) {
            return true;
        }

        // Step 2: no job, no build record.
        if (!(task instanceof Job)) {
            return true;
        }
        Job<?, ?> job = (Job<?, ?>) task;

        VarroaIntentAction existing = findAction(actions);

        // Step 3: an already-granted intent always admits.
        if (existing != null && existing.isGranted()) {
            return true;
        }

        // Step 4: the one rejection the handler makes.
        if (existing != null && DuplicateRegistry.get().contains(existing.getRunId())) {
            return false;
        }

        // Step 5 and step 6: read the clock once, mint the run id, read
        // the delivery id and the admission revision, and stamp a fresh
        // action only when the list holds none.
        if (existing == null) {
            List<Cause> causes = causesFromActions(actions);
            VarroaIntentAction stamped = VarroaIntentAction.stampMissing(job, causes);
            stamped.setActor(currentAuthenticationActor());
            actions.add(stamped);
        } else if (existing.getActor().isEmpty()) {
            existing.setActor(currentAuthenticationActor());
        }

        // Step 7.
        return true;
    }

    private static boolean gateEnabled(String raw) {
        return raw != null && !raw.isEmpty() && !"0".equals(raw);
    }

    /**
     * Reads {@code Jenkins.getAuthentication2().getName()} once, and
     * normalizes {@code anonymous} and {@link ACL#SYSTEM2}'s name to the
     * empty string.
     */
    private static String currentAuthenticationActor() {
        String name = Jenkins.getAuthentication2().getName();
        if (name == null || "anonymous".equals(name) || ACL.SYSTEM2.getName().equals(name)) {
            return "";
        }
        return name;
    }

    private static VarroaIntentAction findAction(List<Action> actions) {
        for (Action action : actions) {
            if (action instanceof VarroaIntentAction) {
                return (VarroaIntentAction) action;
            }
        }
        return null;
    }

    private static List<Cause> causesFromActions(List<Action> actions) {
        List<Cause> causes = new ArrayList<>();
        for (Action action : actions) {
            if (action instanceof CauseAction) {
                causes.addAll(((CauseAction) action).getCauses());
            }
        }
        return causes;
    }
}
