package org.jenkinsci.plugins.varroamiteauth.template;

import hudson.Extension;

import jenkins.branch.BranchProjectFactoryDescriptor;
import jenkins.branch.MultiBranchProject;
import jenkins.scm.api.SCMFile;
import jenkins.scm.api.SCMProbeStat;
import jenkins.scm.api.SCMSource;
import jenkins.scm.api.SCMSourceCriteria;

import org.jenkinsci.plugins.workflow.flow.FlowDefinition;
import org.jenkinsci.plugins.workflow.multibranch.AbstractWorkflowBranchProjectFactory;
import org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject;

import org.kohsuke.stapler.DataBoundConstructor;

/**
 * Selects a branch by the marker's existence alone (D8, {@code
 * pipeline-marker-file}): {@link #getSCMSourceCriteria} tests only that
 * {@code .varroa/pipeline.yaml} exists, at the same per-branch cost as the
 * {@code Jenkinsfile} test {@code WorkflowBranchProjectFactory} makes
 * today. It never reads, parses, or validates the marker during indexing;
 * {@link #createDefinition} hands every branch a {@link
 * VarroaMarkerFlowDefinition}, which does that at run start.
 */
@Extension
public class VarroaTemplateBranchProjectFactory extends AbstractWorkflowBranchProjectFactory {

    @DataBoundConstructor
    public VarroaTemplateBranchProjectFactory() {
    }

    @Override
    protected FlowDefinition createDefinition() {
        return new VarroaMarkerFlowDefinition();
    }

    @Override
    protected SCMSourceCriteria getSCMSourceCriteria(SCMSource source) {
        return (probe, listener) -> {
            SCMProbeStat stat = probe.stat(VarroaMarkerFlowDefinition.MARKER_PATH);
            return stat.getType() != SCMFile.Type.NONEXISTENT && stat.getType() != SCMFile.Type.DIRECTORY;
        };
    }

    /**
     * {@code AbstractWorkflowBranchProjectFactory}'s own descriptor base
     * is package-protected, so this extends {@code
     * BranchProjectFactoryDescriptor} directly and restates the one check
     * that base applies: applicable to a {@link WorkflowMultiBranchProject}
     * only.
     */
    @Extension
    public static final class DescriptorImpl extends BranchProjectFactoryDescriptor {
        @Override
        public String getDisplayName() {
            return "by Varroa pipeline template marker";
        }

        @Override
        public boolean isApplicable(Class<? extends MultiBranchProject> clazz) {
            return WorkflowMultiBranchProject.class.isAssignableFrom(clazz);
        }
    }
}
