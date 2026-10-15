package org.jenkinsci.plugins.varroamiteauth.template;

import hudson.FilePath;
import hudson.Launcher;
import hudson.model.Run;
import hudson.model.TaskListener;
import hudson.scm.ChangeLogParser;
import hudson.scm.SCM;
import hudson.scm.SCMRevisionState;

import java.io.File;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.Map;

/**
 * The {@link SCM} {@link TestScmSource#build} returns: {@link #checkout}
 * writes the named revision's committed files into the workspace, which
 * is all {@code VarroaMarkerFlowDefinition}'s checkout fallback (5.12c)
 * needs. Never used for polling or a real build orchestration.
 */
final class TestScm extends SCM {

    private final TestScmSource source;
    private final String revisionId;

    TestScm(TestScmSource source, String revisionId) {
        this.source = source;
        this.revisionId = revisionId;
    }

    @Override
    public void checkout(Run<?, ?> build, Launcher launcher, FilePath workspace, TaskListener listener,
            File changelogFile, SCMRevisionState baseline) throws IOException, InterruptedException {
        source.markCheckedOut();
        for (Map.Entry<String, byte[]> entry : source.filesAt(revisionId).entrySet()) {
            FilePath target = workspace.child(entry.getKey());
            target.getParent().mkdirs();
            target.write(new String(entry.getValue(), StandardCharsets.UTF_8), StandardCharsets.UTF_8.name());
        }
    }

    @Override
    public ChangeLogParser createChangeLogParser() {
        return null;
    }
}
