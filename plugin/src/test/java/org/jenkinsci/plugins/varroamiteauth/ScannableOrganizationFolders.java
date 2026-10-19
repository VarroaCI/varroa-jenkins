package org.jenkinsci.plugins.varroamiteauth;

import com.cloudbees.hudson.plugins.folder.computed.ComputedFolder;
import com.cloudbees.hudson.plugins.folder.computed.FolderComputation;
import java.io.IOException;
import java.util.ArrayList;
import jenkins.branch.OrganizationFolder;
import jenkins.scm.impl.SingleSCMNavigator;
import org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProjectFactory;

/**
 * Test fixtures for an organization folder. The folder reports itself buildable only
 * with a navigator and a project factory. The navigator here has no sources, so the
 * folder computes without any SCM server.
 */
final class ScannableOrganizationFolders {

    private ScannableOrganizationFolders() {
    }

    /** Gives the folder a navigator and a project factory, and saves its configuration. */
    static void makeScannable(OrganizationFolder folder) throws IOException {
        folder.getNavigators().add(new SingleSCMNavigator("varroa-test", new ArrayList<>()));
        folder.getProjectFactories().add(new WorkflowMultiBranchProjectFactory());
        folder.save();
    }

    /** True when the folder has run a computation that has an end time. */
    static boolean computed(ComputedFolder<?> folder) {
        FolderComputation<?> computation = folder.getComputation();
        return computation != null && computation.getTimestamp() != null;
    }
}
