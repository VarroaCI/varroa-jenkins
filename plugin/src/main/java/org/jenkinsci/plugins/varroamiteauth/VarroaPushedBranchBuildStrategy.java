package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Item;
import hudson.model.TaskListener;
import jenkins.branch.BranchBuildStrategy;
import jenkins.branch.BranchBuildStrategyDescriptor;
import jenkins.scm.api.SCMHead;
import jenkins.scm.api.SCMRevision;
import jenkins.scm.api.SCMSource;
import jenkins.scm.api.mixin.TagSCMHead;
import org.kohsuke.accmod.Restricted;
import org.kohsuke.accmod.restrictions.NoExternalUse;
import org.kohsuke.stapler.DataBoundConstructor;

/**
 * Builds a branch job once when a push created it. A scan that discovers a
 * branch builds nothing, so the skip strategy stays in force for scans.
 *
 * <p>The strategy answers true only for a branch job with no revision seen or
 * built, for a head that a recorded create or update event names at the
 * current revision. Each branch job consumes that event once. Tags never
 * build.
 */
@Restricted(NoExternalUse.class)
public class VarroaPushedBranchBuildStrategy extends BranchBuildStrategy {

    @DataBoundConstructor
    public VarroaPushedBranchBuildStrategy() {
    }

    @Override
    public boolean isAutomaticBuild(
            SCMSource source,
            SCMHead head,
            SCMRevision currRevision,
            SCMRevision lastBuiltRevision,
            SCMRevision lastSeenRevision,
            TaskListener listener) {
        if (lastSeenRevision != null || lastBuiltRevision != null || head instanceof TagSCMHead) {
            return false;
        }
        if (!PushedHeadRegistry.get().consume(source, head, currRevision, ownerKey(source, head))) {
            return false;
        }
        listener.getLogger().println(
                "Varroa: a push named " + head.getName() + " before its job existed; building it once");
        return true;
    }

    private static String ownerKey(SCMSource source, SCMHead head) {
        Object owner = source.getOwner();
        String ownerName = owner instanceof Item ? ((Item) owner).getFullName() : String.valueOf(owner);
        return ownerName + "\u0000" + source.getId() + "\u0000" + head.getName();
    }

    /** Descriptor for the strategy. The symbol is referenced by no configuration. */
    @Extension
    @Restricted(NoExternalUse.class)
    public static class DescriptorImpl extends BranchBuildStrategyDescriptor {

        @Override
        public String getDisplayName() {
            return "Build a branch that a push creates";
        }
    }
}
