package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.InvisibleAction;
import hudson.model.Run;

/**
 * Per-run sequence counter for the {@code varroa.build} step's intent-key
 * ordinal (D6): the n-th call on a run sends {@code s<n>}. A plain field
 * on a normal {@link hudson.model.Action}, so it persists across a run
 * reload the same way every other build action does.
 */
public final class VarroaCallSeqAction extends InvisibleAction {

    private int lastSeq;

    /** Returns the next 1-based ordinal, {@code s<n>}, for a call on {@code run}. */
    static int nextSeq(Run<?, ?> run) {
        synchronized (run) {
            VarroaCallSeqAction action = run.getAction(VarroaCallSeqAction.class);
            if (action == null) {
                action = new VarroaCallSeqAction();
                run.addAction(action);
            }
            return action.increment();
        }
    }

    synchronized int increment() {
        return ++lastSeq;
    }

    /** The most recently issued ordinal, {@code 0} before any call. Package-private for testing. */
    synchronized int lastSeqForTesting() {
        return lastSeq;
    }
}
