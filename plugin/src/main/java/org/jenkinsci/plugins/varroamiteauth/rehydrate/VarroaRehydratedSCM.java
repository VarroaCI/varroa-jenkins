package org.jenkinsci.plugins.varroamiteauth.rehydrate;

import java.io.File;
import java.io.IOException;

import hudson.FilePath;
import hudson.Launcher;
import hudson.model.Run;
import hudson.model.TaskListener;
import hudson.scm.ChangeLogParser;
import hudson.scm.SCM;
import hudson.scm.SCMRevisionState;

/**
 * The SCM every rehydrated build's {@code &lt;checkouts&gt;} element names,
 * so {@link Run#getChangeSets()} finds a changelog parser Varroa owns. The
 * operator writes this class's name into {@code build.xml}; it is never
 * configured through the Jenkins UI and carries no field.
 *
 * <p>{@link #checkout} always throws: a rehydrated run is already terminal
 * on load, and a terminal run never checks out.
 */
public final class VarroaRehydratedSCM extends SCM {

    @Override
    public ChangeLogParser createChangeLogParser() {
        return new VarroaChangeLogParser();
    }

    @Override
    public void checkout(Run<?, ?> build, Launcher launcher, FilePath workspace, TaskListener listener,
            File changelogFile, SCMRevisionState baseline) throws IOException {
        throw new IOException("VarroaRehydratedSCM never checks out: the run it belongs to is already terminal");
    }
}
