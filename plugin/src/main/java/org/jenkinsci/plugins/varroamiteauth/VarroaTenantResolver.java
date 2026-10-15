package org.jenkinsci.plugins.varroamiteauth;

import hudson.model.Item;
import hudson.model.Queue;
import jenkins.model.Jenkins;

/**
 * Derives the tenant that owns a queued task, and reports whether the cell
 * hosts a named tenant. {@link VarroaQueueItemAuthenticator} and
 * {@link VarroaQueueTaskDispatcher} call {@link #tenantOf}.
 * {@code VarroaSecurityRealm} holds only a username, so it calls
 * {@link #hostsTenant}. One rule decides what a tenant is; two copies of
 * the rule would drift.
 */
final class VarroaTenantResolver {

    private VarroaTenantResolver() {
    }

    /**
     * Returns the tenant of the given task, or {@code null} when the task
     * resolves to no owning item, or when the resolved item's first path
     * segment names no tenant the cell hosts.
     */
    static String tenantOf(Queue.Task task) {
        Item item = ownerItem(task);
        if (item == null) {
            return null;
        }
        String fullName = item.getFullName();
        if (fullName == null || fullName.isEmpty()) {
            return null;
        }
        int slash = fullName.indexOf('/');
        String tenant = slash < 0 ? fullName : fullName.substring(0, slash);
        return hostsTenant(tenant) ? tenant : null;
    }

    /**
     * Resolves a task to its owning {@link Item} in two steps: the task
     * itself when it is an {@link Item}, otherwise the result of
     * {@link Queue.Task#getOwnerTask()} when that result is an
     * {@link Item} and is not the task itself. A durable-task
     * {@code PlaceholderTask} returns itself from {@code getOwnerTask()}
     * when it cannot resolve its job, and that case answers {@code null}
     * here.
     */
    private static Item ownerItem(Queue.Task task) {
        if (task instanceof Item) {
            return (Item) task;
        }
        Queue.Task owner = task.getOwnerTask();
        if (owner != task && owner instanceof Item) {
            return (Item) owner;
        }
        return null;
    }

    /**
     * Reports whether the cell hosts the named tenant. The answer comes
     * from the cell's applied role-strategy document: the cell hosts a
     * tenant exactly when that document carries an item-type role named
     * {@code varroa:tenant:<tenant>}. The operator's RBAC pipeline
     * synthesizes that role for every tenant in the cell's tenant list, so
     * the document is the live record of which tenants the cell hosts.
     *
     * <p>role-strategy is not a compile-time dependency; every step below
     * is reflection wrapped in a catch-all so API drift, or role-strategy
     * being absent, degrades to "not hosted" rather than a plugin failure.
     */
    static boolean hostsTenant(String tenant) {
        if (tenant == null || tenant.isEmpty()) {
            return false;
        }
        try {
            Object strategy = Jenkins.get().getAuthorizationStrategy();
            ClassLoader loader = strategy.getClass().getClassLoader();

            Class<?> rbasClass = Class.forName(
                "com.michelin.cio.hudson.plugins.rolestrategy.RoleBasedAuthorizationStrategy",
                false, loader);
            if (!rbasClass.isInstance(strategy)) {
                return false;
            }

            // RoleType.Project is role-strategy's historical name for an
            // item-scoped role; it lives in the synopsys.arc package,
            // unlike RoleMap/Role (com.michelin.cio).
            Class<?> roleTypeClass = Class.forName(
                "com.synopsys.arc.jenkins.plugins.rolestrategy.RoleType",
                false, loader);
            Object itemType = roleTypeClass.getField("Project").get(null);

            Object roleMap = rbasClass.getMethod("getRoleMap", roleTypeClass).invoke(strategy, itemType);
            Object role = roleMap.getClass().getMethod("getRole", String.class)
                .invoke(roleMap, "varroa:tenant:" + tenant);
            return role != null;
        } catch (Exception e) {
            return false;
        }
    }
}
