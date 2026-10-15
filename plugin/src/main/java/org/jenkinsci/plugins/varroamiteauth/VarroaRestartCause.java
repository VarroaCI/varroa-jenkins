package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.Cause;

/**
 * The cause a restarted build carries. It names the run the restart reran
 * from and the stage the restart began at.
 *
 * <p>Two causes that name one source run and one stage are equal, so {@link
 * hudson.model.CauseAction} keeps one.
 *
 * <p>The class is public because Jenkins writes a cause into {@code
 * build.xml} and loads it again by its class name.
 *
 * <p>The cause renders through {@link #getShortDescription()}, so it needs
 * no Jelly view.
 */
public class VarroaRestartCause extends Cause {

    private final String sourceRunId;
    private final String stage;

    /** Creates a cause for {@code sourceRunId} and {@code stage}. A null value reads as empty. */
    public VarroaRestartCause(String sourceRunId, String stage) {
        this.sourceRunId = sourceRunId != null ? sourceRunId : "";
        this.stage = stage != null ? stage : "";
    }

    /** Answers the identifier of the run the restart reran from. */
    public String getSourceRunId() {
        return sourceRunId;
    }

    /** Answers the stage the restart began at. */
    public String getStage() {
        return stage;
    }

    /**
     * Answers {@code Restarted from run <id> at stage <stage> through Varroa}.
     * An empty source run identifier names no run, so the text then reads
     * {@code Restarted through Varroa}.
     */
    @Override
    public String getShortDescription() {
        if (sourceRunId.isEmpty()) {
            return "Restarted through Varroa";
        }
        return "Restarted from run " + sourceRunId + " at stage " + stage + " through Varroa";
    }

    @Override
    public boolean equals(Object other) {
        if (!(other instanceof VarroaRestartCause)) {
            return false;
        }
        VarroaRestartCause that = (VarroaRestartCause) other;
        return sourceRunId.equals(that.sourceRunId) && stage.equals(that.stage);
    }

    @Override
    public int hashCode() {
        return 31 * sourceRunId.hashCode() + stage.hashCode();
    }
}
