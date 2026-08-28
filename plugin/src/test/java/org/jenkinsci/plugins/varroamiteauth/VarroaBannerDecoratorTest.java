package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotNull;

import hudson.model.User;
import hudson.security.ACL;
import hudson.security.ACLContext;
import jenkins.model.Jenkins;
import java.util.ArrayList;
import java.util.List;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.springframework.security.authentication.UsernamePasswordAuthenticationToken;
import org.springframework.security.core.Authentication;
import org.springframework.security.core.authority.SimpleGrantedAuthority;

/**
 * Tests for the {@link VarroaBannerDecorator} identity getters. The decorator reads identity
 * from {@code Jenkins.getAuthentication2()} (Spring Security), so the tests drive the Spring
 * security context via {@link ACL#as2} rather than poking the acegi shim.
 */
public class VarroaBannerDecoratorTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private static Authentication authFor(String id, String... authorities) {
        List<SimpleGrantedAuthority> granted = new ArrayList<>();
        for (String a : authorities) {
            granted.add(new SimpleGrantedAuthority(a));
        }
        return new UsernamePasswordAuthenticationToken(id, "", granted);
    }

    /**
     * getCurrentUserName returns the full name when set, the id when the full name is blank,
     * and "" when no user is in context.
     */
    @Test
    public void getCurrentUserNameReturnsFullNameOrIdOrEmpty() throws Exception {
        VarroaBannerDecorator decorator = new VarroaBannerDecorator();

        // Anonymous — no current user (the default JenkinsRule context is SYSTEM, so
        // impersonate the anonymous principal to exercise the unauthenticated path).
        try (ACLContext ignored = ACL.as2(Jenkins.ANONYMOUS2)) {
            assertEquals("", decorator.getCurrentUserId());
            assertEquals("", decorator.getCurrentUserName());
        }

        User alice = User.getById("alice", true);
        alice.setFullName("Alice Smith");

        try (ACLContext ignored = ACL.as2(authFor("alice", "authenticated"))) {
            assertEquals("alice", decorator.getCurrentUserId());
            assertEquals("Alice Smith", decorator.getCurrentUserName());

            alice.setFullName("");
            assertEquals("alice", decorator.getCurrentUserName());

            alice.setFullName(null);
            assertEquals("alice", decorator.getCurrentUserName());
        }
    }

    /**
     * getCurrentUserGroups excludes the authenticated/anonymous literals and any ROLE:*
     * authority, de-duplicating while preserving order.
     */
    @Test
    public void getCurrentUserGroupsExcludesSystemAuthorities() {
        VarroaBannerDecorator decorator = new VarroaBannerDecorator();

        try (ACLContext ignored = ACL.as2(authFor("bob",
                "authenticated", "anonymous", "ROLE:varroa:system-mite", "ROLE:jenkins-admin",
                "team-ops", "team-dev", "everyone", "team-ops"))) {
            assertEquals("team-ops,team-dev,everyone", decorator.getCurrentUserGroups());
        }
    }

    /**
     * getCurrentUserRole degrades to "" when the active authorization strategy is not
     * role-strategy (the default JenkinsRule strategy / role-strategy not on the classpath).
     */
    @Test
    public void getCurrentUserRoleReturnsEmptyWhenNotRoleStrategy() {
        VarroaBannerDecorator decorator = new VarroaBannerDecorator();

        try (ACLContext ignored = ACL.as2(authFor("carol", "authenticated"))) {
            assertEquals("", decorator.getCurrentUserRole());
        }
    }

    /** getCurrentUserEmail returns "" when no user is in context. */
    @Test
    public void getCurrentUserEmailReturnsEmptyWhenNoUser() {
        VarroaBannerDecorator decorator = new VarroaBannerDecorator();
        try (ACLContext ignored = ACL.as2(Jenkins.ANONYMOUS2)) {
            assertEquals("", decorator.getCurrentUserEmail());
        }
    }

    /**
     * The crumb getters never throw off a request thread: the value is "" (no current request)
     * and the field name resolves without error.
     */
    @Test
    public void crumbGettersAreSafeOffRequestThread() {
        VarroaBannerDecorator decorator = new VarroaBannerDecorator();

        assertEquals("", decorator.getCrumbValue());
        assertNotNull(decorator.getCrumbFieldName());
    }

    /**
     * Regression for #533: role-strategy's RoleType lives in the
     * com.synopsys.arc package (unlike RoleMap/Role, which are under
     * com.michelin.cio); resolving it from the wrong package made the
     * reflective lookup throw and the banner role render empty for everyone.
     */
    @Test
    public void getCurrentUserRoleResolvesRoleViaRoleStrategy() throws Exception {
        com.michelin.cio.hudson.plugins.rolestrategy.Role role =
            new com.michelin.cio.hudson.plugins.rolestrategy.Role(
                "varroa:admin",
                java.util.regex.Pattern.compile(".*"),
                java.util.Set.of(Jenkins.ADMINISTER),
                "regression role");
        com.michelin.cio.hudson.plugins.rolestrategy.RoleMap roleMap =
            new com.michelin.cio.hudson.plugins.rolestrategy.RoleMap(new java.util.TreeMap<>());
        roleMap.addRole(role);
        roleMap.assignRole(role, "alice");
        j.jenkins.setAuthorizationStrategy(
            new com.michelin.cio.hudson.plugins.rolestrategy.RoleBasedAuthorizationStrategy(
                java.util.Map.of(
                    com.michelin.cio.hudson.plugins.rolestrategy.RoleBasedAuthorizationStrategy.GLOBAL,
                    roleMap)));
        User.getById("alice", true);

        VarroaBannerDecorator decorator = new VarroaBannerDecorator();
        try (ACLContext ignored = ACL.as2(authFor("alice", "authenticated"))) {
            assertEquals("Admin", decorator.getCurrentUserRole());
        }
    }
}
