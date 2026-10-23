package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;
import static org.junit.Assert.fail;

import hudson.model.User;
import java.util.HashSet;
import java.util.Set;
import org.acegisecurity.GrantedAuthority;
import org.acegisecurity.userdetails.UserDetails;
import org.acegisecurity.userdetails.UsernameNotFoundException;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests for {@code VarroaSecurityRealm.VarroaUserDetailsService}'s tenant
 * service identity answer (task 10.3b): {@code svc-<tenant>} synthesizes an
 * authority set with no persisted {@code User} record, and an unhosted
 * tenant still throws.
 */
public class VarroaSecurityRealmServiceIdentityTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Test
    public void aHostedTenantIdentityGetsSynthesizedAuthoritiesWithNoPersistedRecord() throws Exception {
        VarroaTenantResolverTest.hostTenants(j, "payments");

        UserDetails details =
            new VarroaSecurityRealm.VarroaUserDetailsService().loadUserByUsername("svc-payments");

        assertEquals("svc-payments", details.getUsername());
        Set<String> authorities = new HashSet<>();
        for (GrantedAuthority a : details.getAuthorities()) {
            authorities.add(a.getAuthority());
        }
        assertTrue(authorities.contains("authenticated"));
        assertTrue(authorities.contains("ROLE:varroa:tenant:payments"));

        assertNull("no User record must be created for the synthesized identity",
            User.getById("svc-payments", false));
    }

    @Test
    public void anUnhostedTenantIdentityStillThrows() throws Exception {
        VarroaTenantResolverTest.hostTenants(j, "payments");

        try {
            new VarroaSecurityRealm.VarroaUserDetailsService().loadUserByUsername("svc-ghost");
            fail("an unhosted tenant identity must throw UsernameNotFoundException");
        } catch (UsernameNotFoundException expected) {
            // expected
        }
    }
}
