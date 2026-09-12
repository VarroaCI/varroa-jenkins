package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Node;
import hudson.model.Queue;
import hudson.model.labels.LabelAtom;
import hudson.model.queue.CauseOfBlockage;
import hudson.model.queue.QueueTaskDispatcher;
import java.util.Set;
import java.util.TreeSet;

/**
 * Fences a tenant build to nodes labelled for that tenant.
 *
 * <p>Only {@code canTake(Node, BuildableItem)} is overridden. The fence
 * decides which node may take a build, not whether the build may run at
 * all: blocking with {@code canRun} would also stop the cloud from
 * provisioning a node the build is allowed to use.
 */
@Extension
public class VarroaQueueTaskDispatcher extends QueueTaskDispatcher {

    private static final String TENANT_LABEL_PREFIX = "varroa-tenant-";

    @Override
    public CauseOfBlockage canTake(Node node, Queue.BuildableItem item) {
        String tenant = VarroaTenantResolver.tenantOf(item.task);
        if (tenant == null) {
            // A build outside every tenant folder is not a tenant build.
            return null;
        }

        Set<String> nodeTenants = tenantLabelsOf(node);
        if (nodeTenants.isEmpty()) {
            // An unlabelled node has no proven owner. The fence fails
            // closed for every tenant build.
            return new CauseOfBlockage() {
                @Override
                public String getShortDescription() {
                    return "Varroa: node has no varroa-tenant- label, so it has no proven owner for tenant "
                        + tenant;
                }
            };
        }
        if (nodeTenants.size() > 1) {
            // A node carrying more than one reserved tenant label has no
            // single proven owner. The fence fails closed for every
            // tenant build the node is offered to, not only the one
            // whose label happens to match this request.
            return new CauseOfBlockage() {
                @Override
                public String getShortDescription() {
                    return "Varroa: node carries ambiguous tenant labels " + nodeTenants
                        + ", so it has no proven owner for tenant " + tenant;
                }
            };
        }
        String nodeTenant = nodeTenants.iterator().next();
        if (!nodeTenant.equals(tenant)) {
            return new CauseOfBlockage() {
                @Override
                public String getShortDescription() {
                    return "Varroa: node is reserved for tenant " + nodeTenant
                        + ", not tenant " + tenant;
                }
            };
        }
        return null;
    }

    /**
     * Returns every tenant named by the node's reserved
     * {@code varroa-tenant-<team>} labels. The set is empty when the node
     * carries no such label, and holds more than one entry when the node
     * carries labels for more than one tenant — an ambiguous node the
     * fence must trust for no tenant. Never parses the user-facing
     * {@code <team>-} label prefix: it is ambiguous on its own, because
     * the teams {@code pay} and {@code pay-prod} both match a node
     * labelled {@code pay-prod-linux}.
     */
    private static Set<String> tenantLabelsOf(Node node) {
        Set<String> tenants = new TreeSet<>();
        for (LabelAtom label : node.getAssignedLabels()) {
            String name = label.getName();
            if (name != null && name.startsWith(TENANT_LABEL_PREFIX)) {
                tenants.add(name.substring(TENANT_LABEL_PREFIX.length()));
            }
        }
        return tenants;
    }
}
