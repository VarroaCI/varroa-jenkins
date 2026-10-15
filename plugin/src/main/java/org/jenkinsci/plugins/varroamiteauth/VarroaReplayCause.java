package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.Cause;

/**
 * The cause a replayed build carries. It names the run the replay copied
 * its script from (D10).
 *
 * <p>Two causes naming one source run are equal, so {@link
 * hudson.model.CauseAction} keeps one.
 *
 * <p>The class is public because Jenkins writes a cause into {@code
 * build.xml} and loads it again by its class name.
 *
 * <p>The cause renders through {@link #getShortDescription()}, so it needs
 * no Jelly view.
 */
public class VarroaReplayCause extends Cause {

    private final String sourceRunId;

    /** Creates a cause for {@code sourceRunId}. A null value reads as empty. */
    public VarroaReplayCause(String sourceRunId) {
        this.sourceRunId = sourceRunId != null ? sourceRunId : "";
    }

    /** Answers the identifier of the run the replay copied its script from. */
    public String getSourceRunId() {
        return sourceRunId;
    }

    /**
     * Answers {@code Replayed from run <id> through Varroa}. An empty source
     * run identifier names no run, so the text is then {@code Replayed
     * through Varroa}.
     */
    @Override
    public String getShortDescription() {
        if (sourceRunId.isEmpty()) {
            return "Replayed through Varroa";
        }
        return "Replayed from run " + sourceRunId + " through Varroa";
    }

    @Override
    public boolean equals(Object other) {
        return other instanceof VarroaReplayCause
                && sourceRunId.equals(((VarroaReplayCause) other).sourceRunId);
    }

    @Override
    public int hashCode() {
        return sourceRunId.hashCode();
    }
}
