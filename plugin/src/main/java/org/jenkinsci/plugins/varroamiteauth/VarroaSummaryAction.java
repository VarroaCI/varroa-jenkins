package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.InvisibleAction;

import java.io.Serializable;

/**
 * Holds the one summary line a run attaches to itself through {@code
 * varroa.summary}. The action lives in the run's own {@code
 * build.xml}, so the text survives a build that resumes in another
 * process. It is invisible: it adds no link to the run page, because the
 * recorder reads its value and the run page reads the record store.
 */
public final class VarroaSummaryAction extends InvisibleAction implements Serializable {

    private static final long serialVersionUID = 1L;

    private final String summary;

    public VarroaSummaryAction(String summary) {
        this.summary = summary;
    }

    public String getSummary() {
        return summary;
    }
}
