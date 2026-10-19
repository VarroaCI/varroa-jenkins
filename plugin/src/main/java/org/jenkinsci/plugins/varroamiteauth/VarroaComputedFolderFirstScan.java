package org.jenkinsci.plugins.varroamiteauth;

import com.cloudbees.hudson.plugins.folder.computed.ComputedFolder;
import hudson.Extension;
import hudson.model.Item;
import hudson.model.Items;
import hudson.model.listeners.ItemListener;
import hudson.security.ACL;
import hudson.security.ACLContext;
import java.util.logging.Level;
import java.util.logging.Logger;
import jenkins.branch.MultiBranchProject;
import jenkins.branch.OrganizationFolder;
import org.kohsuke.stapler.Stapler;
import org.kohsuke.stapler.StaplerRequest2;

/**
 * Starts the first folder computation of a multibranch project or an organization
 * folder. The listener starts a computation when the item is created. It also starts
 * one when a POST to the item's config.xml or configuration form replaces its configuration.
 *
 * <p>A save of the folder fires the same update event during a scan, during creation,
 * and during an SCM event. The listener ignores those saves. A save during a load of
 * the item is also ignored. A multibranch project inside an organization folder is
 * skipped, because its folder indexes it.
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
        if (Items.currentlyUpdatingByXml() || !isConfigXmlPost(item)) {
            return;
        }
        scheduleComputation(item);
    }

    private static boolean isConfigXmlPost(Item item) {
        StaplerRequest2 req = Stapler.getCurrentRequest2();
        return req != null
                && "POST".equals(req.getMethod())
                && isConfigurationSubmit(req.getRequestURI())
                && req.findAncestorObject(Item.class) == item;
    }

    private static boolean isConfigurationSubmit(String uri) {
        return uri.endsWith("/config.xml") || uri.endsWith("/configSubmit");
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
