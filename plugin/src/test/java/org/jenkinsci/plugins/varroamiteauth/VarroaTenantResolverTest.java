package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

import com.michelin.cio.hudson.plugins.rolestrategy.Role;
import com.michelin.cio.hudson.plugins.rolestrategy.RoleBasedAuthorizationStrategy;
import com.michelin.cio.hudson.plugins.rolestrategy.RoleMap;
import hudson.model.FreeStyleProject;
import hudson.model.Queue;
import java.util.Map;
import java.util.TreeMap;
import java.util.regex.Pattern;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests for {@link VarroaTenantResolver} (tasks 10.1, 10.3a): the shared
 * rule that both {@link VarroaQueueItemAuthenticator} and
 * {@link VarroaQueueTaskDispatcher} call.
 */
public class VarroaTenantResolverTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    /**
     * Installs an item-type role-strategy role for each named tenant, plus
     * a fixture-only agent-type role granting {@code Computer.BUILD} to
     * every authenticated identity. Core's own {@code Node.canTake} always
     * checks that permission before any {@link VarroaQueueTaskDispatcher}
     * runs; a real cell's role-strategy document grants it too, so the
     * fixture grants it broadly rather than asserting how.
     */
    static void hostTenants(JenkinsRule j, String... tenants) throws Exception {
        RoleMap itemRoles = new RoleMap(new TreeMap<>());
        for (String tenant : tenants) {
            Role role = new Role(
                "varroa:tenant:" + tenant,
                Pattern.compile("^" + tenant + "($|/.*)$"),
                java.util.Set.of(
                    hudson.model.Item.READ,
                    hudson.model.Item.CONFIGURE),
                "tenant role");
            itemRoles.addRole(role);
            itemRoles.assignRole(role, "ROLE:varroa:tenant:" + tenant);
        }
        RoleMap agentRoles = new RoleMap(new TreeMap<>());
        Role anyAgent = new Role(
            "test-fixture-any-agent",
            Pattern.compile(".*"),
            java.util.Set.of(hudson.model.Computer.BUILD),
            "fixture role: any authenticated identity may use any agent");
        agentRoles.addRole(anyAgent);
        agentRoles.assignRole(anyAgent, "authenticated");
        j.jenkins.setAuthorizationStrategy(new RoleBasedAuthorizationStrategy(
            Map.of(
                RoleBasedAuthorizationStrategy.PROJECT, itemRoles,
                RoleBasedAuthorizationStrategy.SLAVE, agentRoles)));
    }

    @Test
    public void hostsTenantIsFalseWithNoRoleStrategyDocument() {
        assertFalse(VarroaTenantResolver.hostsTenant("payments"));
    }

    @Test
    public void hostsTenantIsFalseForAnEmptyOrNullName() throws Exception {
        hostTenants(j, "payments");
        assertFalse(VarroaTenantResolver.hostsTenant(""));
        assertFalse(VarroaTenantResolver.hostsTenant(null));
    }

    @Test
    public void hostsTenantIsTrueOnlyForARoleTheDocumentCarries() throws Exception {
        hostTenants(j, "payments");
        assertTrue(VarroaTenantResolver.hostsTenant("payments"));
        assertFalse(VarroaTenantResolver.hostsTenant("search"));
    }

    @Test
    public void tenantOfReturnsNullWhenTheTaskResolvesNoOwningItem() {
        // Not an Item, and getOwnerTask() returns itself: PlaceholderTask's
        // own self-return path when it cannot resolve its Job.
        Queue.Task selfOwning = mock(Queue.Task.class);
        when(selfOwning.getOwnerTask()).thenReturn(selfOwning);

        assertNull(VarroaTenantResolver.tenantOf(selfOwning));
    }

    @Test
    public void tenantOfReturnsNullForAnUnknownFirstSegment() throws Exception {
        hostTenants(j, "payments");
        FreeStyleProject ghost = j.createFreeStyleProject("ghost");
        assertNull(VarroaTenantResolver.tenantOf(ghost));
    }

    @Test
    public void tenantOfReturnsTheHostedFirstSegment() throws Exception {
        hostTenants(j, "payments");
        FreeStyleProject p = j.jenkins.createProject(FreeStyleProject.class, "payments");
        assertEquals("payments", VarroaTenantResolver.tenantOf(p));
    }
}
