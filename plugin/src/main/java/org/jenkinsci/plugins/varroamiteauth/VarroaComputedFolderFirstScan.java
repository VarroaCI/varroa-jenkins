package org.jenkinsci.plugins.varroamiteauth;

import com.cloudbees.hudson.plugins.folder.computed.ComputedFolder;
import hudson.Extension;
import hudson.model.Item;
import hudson.model.listeners.ItemListener;
import hudson.security.ACL;
import hudson.security.ACLContext;
import java.util.logging.Level;
import java.util.logging.Logger;
import jenkins.branch.MultiBranchProject;
import jenkins.branch.OrganizationFolder;

/**
 * Starts the first folder computation of a multibranch project or an organization
 * folder when the item is created or its configuration is saved.
 *
 * <p>The items engine creates an item through the {@code createItem} endpoint with
 * config.xml. Jenkins saves that item without running the configuration submit step,
 * and that step is the one that schedules the first computation. So no scan starts
 * unless this listener starts it. A multibranch project inside an organization folder
 * is skipped, because its folder indexes it.
 */
@Extension
public class VarroaComputedFolderFirstScan extends ItemListener {

    private static final Logger LOGGER = Logger.getLogger(VarroaComputedFolderFirstScan.class.getName());

    @Override
    public void onCreated(Item item) {
        scheduleComputation(item);
    }

    @Override
    public void onUpdated(Item item) {
        scheduleComputation(item);
    }

    private static void scheduleComputation(Item item) {
        if (!(item instanceof OrganizationFolder) && !(item instanceof MultiBranchProject)) {
            return;
        }
        if (item.getParent() instanceof OrganizationFolder) {
            return;
        }
        try (ACLContext ignored = ACL.as2(ACL.SYSTEM2)) {
            ((ComputedFolder<?>) item).scheduleBuild();
        } catch (RuntimeException | LinkageError e) {
            LOGGER.log(Level.FINE, "the first folder computation of " + item.getFullName() + " did not start", e);
        }
    }
}
