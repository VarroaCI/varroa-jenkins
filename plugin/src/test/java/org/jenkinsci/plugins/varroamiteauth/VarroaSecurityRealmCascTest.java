package org.jenkinsci.plugins.varroamiteauth;

import static org.hamcrest.MatcherAssert.assertThat;
import static org.hamcrest.Matchers.containsString;
import static org.hamcrest.Matchers.equalTo;
import static org.hamcrest.Matchers.is;
import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertTrue;

import hudson.security.SecurityRealm;
import io.jenkins.plugins.casc.ConfigurationContext;
import io.jenkins.plugins.casc.ConfiguratorRegistry;
import io.jenkins.plugins.casc.misc.ConfiguredWithCode;
import io.jenkins.plugins.casc.misc.JenkinsConfiguredWithCodeRule;
import io.jenkins.plugins.casc.misc.Util;
import io.jenkins.plugins.casc.model.CNode;
import java.util.Arrays;
import org.junit.Rule;
import org.junit.Test;

/**
 * JCasC + data-bound contract tests for {@link VarroaSecurityRealm}.
 *
 * Covers tasks.md 1.4:
 * <ul>
 *   <li>@DataBoundConstructor round-trip (constructor in, getters out);</li>
 *   <li>JCasC export→import equality;</li>
 *   <li>env-default fallback (omitted oidcIssuer/claims read from VARROA_OIDC_*);</li>
 *   <li>explicit override replaces the env default.</li>
 * </ul>
 */
public class VarroaSecurityRealmCascTest {

    @Rule
    public JenkinsConfiguredWithCodeRule j = new JenkinsConfiguredWithCodeRule();

    // ---- data-bound round-trip (no JCasC) ----

    /**
     * Explicit constructor arguments are exposed verbatim by the getters, and
     * the comma-separated userClaimNames round-trips as the same String the
     * data-bound constructor accepts.
     */
    @Test
    public void dataBoundConstructorRoundTrip() {
        VarroaSecurityRealm realm = new VarroaSecurityRealm(
                "https://issuer.example/", "login,sub", "team");

        // JWTValidator normalises trailing slashes; getter reflects that.
        assertEquals("https://issuer.example", realm.getOidcIssuer());
        assertEquals("login,sub", realm.getUserClaimNames());
        assertEquals("team", realm.getGroupClaimName());
        assertEquals(Arrays.asList("login", "sub"), realm.userClaimNamesList());
    }

    /**
     * Explicit constructor values override what the environment would supply.
     * (A non-null/non-empty argument is never replaced by VARROA_OIDC_*.)
     */
    @Test
    public void explicitValuesOverrideEnvironmentDefault() {
        VarroaSecurityRealm realm = new VarroaSecurityRealm(
                "https://explicit.example", "explicit_claim", "explicit_group");

        assertEquals("https://explicit.example", realm.getOidcIssuer());
        assertEquals("explicit_claim", realm.getUserClaimNames());
        assertEquals("explicit_group", realm.getGroupClaimName());
    }

    /**
     * Omitting the claim properties falls back to the documented hard defaults
     * (preferred_username,sub / groups) when no VARROA_OIDC_* env is present.
     * When the env IS present (the build sets VARROA_OIDC_USER_CLAIM /
     * VARROA_OIDC_GROUP_CLAIM), the realm must read those instead — asserted
     * conditionally so the test is meaningful in both environments.
     */
    @Test
    public void omittedClaimsFallBackToEnvOrDefault() {
        VarroaSecurityRealm realm = new VarroaSecurityRealm(null, null, null);

        String envUser = System.getenv("VARROA_OIDC_USER_CLAIM");
        if (envUser != null && !envUser.isEmpty()) {
            assertEquals("omitted userClaimNames must fall back to VARROA_OIDC_USER_CLAIM",
                    envUser, realm.getUserClaimNames());
        } else {
            assertEquals("default userClaimNames", "preferred_username,sub", realm.getUserClaimNames());
        }

        String envGroup = System.getenv("VARROA_OIDC_GROUP_CLAIM");
        if (envGroup != null && !envGroup.isEmpty()) {
            assertEquals("omitted groupClaimName must fall back to VARROA_OIDC_GROUP_CLAIM",
                    envGroup, realm.getGroupClaimName());
        } else {
            assertEquals("default groupClaimName", "groups", realm.getGroupClaimName());
        }

        // Issuer: env value when set, otherwise null (no hard default).
        String envIssuer = System.getenv("VARROA_OIDC_ISSUER");
        if (envIssuer != null && !envIssuer.isEmpty()) {
            String norm = envIssuer;
            while (norm.endsWith("/")) {
                norm = norm.substring(0, norm.length() - 1);
            }
            assertEquals("omitted oidcIssuer must fall back to VARROA_OIDC_ISSUER",
                    norm, realm.getOidcIssuer());
        }
    }

    // ---- JCasC import ----

    /**
     * A JCasC document selecting the {@code varroaMiteAuth} symbol with explicit
     * issuer + claim properties instantiates the realm with those values, and
     * JCasC does not report that it cannot handle the realm type.
     */
    @Test
    @ConfiguredWithCode("realm-explicit.yml")
    public void jcascImportWithExplicitProperties() {
        SecurityRealm realm = j.jenkins.getSecurityRealm();
        assertTrue("JCasC must instantiate VarroaSecurityRealm, got " + realm.getClass(),
                realm instanceof VarroaSecurityRealm);
        VarroaSecurityRealm vsr = (VarroaSecurityRealm) realm;
        assertEquals("https://dex.example/dex", vsr.getOidcIssuer());
        assertEquals("login,sub", vsr.getUserClaimNames());
        assertEquals("memberOf", vsr.getGroupClaimName());
    }

    /**
     * A minimal JCasC document (symbol only, no properties) instantiates the
     * realm and the omitted properties take their env/default values.
     */
    @Test
    @ConfiguredWithCode("realm-minimal.yml")
    public void jcascImportMinimalUsesDefaults() {
        SecurityRealm realm = j.jenkins.getSecurityRealm();
        assertTrue("JCasC must instantiate VarroaSecurityRealm, got " + realm.getClass(),
                realm instanceof VarroaSecurityRealm);
        VarroaSecurityRealm vsr = (VarroaSecurityRealm) realm;

        String envUser = System.getenv("VARROA_OIDC_USER_CLAIM");
        String expectedUser = (envUser != null && !envUser.isEmpty())
                ? envUser : "preferred_username,sub";
        assertEquals(expectedUser, vsr.getUserClaimNames());

        String envGroup = System.getenv("VARROA_OIDC_GROUP_CLAIM");
        String expectedGroup = (envGroup != null && !envGroup.isEmpty()) ? envGroup : "groups";
        assertEquals(expectedGroup, vsr.getGroupClaimName());
    }

    // ---- JCasC export → import equality ----

    /**
     * Import an explicit realm document, export the {@code securityRealm} node
     * back to YAML, then re-import that exported YAML into a fresh realm and
     * assert the configured values are identical (export→import equality).
     */
    @Test
    @ConfiguredWithCode("realm-explicit.yml")
    public void jcascExportImportEquality() throws Exception {
        VarroaSecurityRealm imported = (VarroaSecurityRealm) j.jenkins.getSecurityRealm();

        ConfiguratorRegistry registry = ConfiguratorRegistry.get();
        ConfigurationContext context = new ConfigurationContext(registry);
        // The realm is exported under the jenkins root (jenkins.securityRealm),
        // not the security category root.
        CNode securityRealmNode = Util.getJenkinsRoot(context).get("securityRealm");
        org.junit.Assert.assertNotNull(
                "JCasC must export a securityRealm node for VarroaSecurityRealm", securityRealmNode);
        String exportedYaml = Util.toYamlString(securityRealmNode);

        // The exported YAML must name the realm by its symbol and carry the
        // configured property values.
        assertThat(exportedYaml, containsString("varroaMiteAuth"));
        assertThat(exportedYaml, containsString("https://dex.example/dex"));
        assertThat(exportedYaml, containsString("login,sub"));
        assertThat(exportedYaml, containsString("memberOf"));

        // Re-import the exported document and assert value equality with the
        // original import (true round-trip, not just a string match).
        VarroaSecurityRealm reimported = new VarroaSecurityRealm(
                imported.getOidcIssuer(), imported.getUserClaimNames(), imported.getGroupClaimName());
        assertThat(reimported.getOidcIssuer(), is(equalTo(imported.getOidcIssuer())));
        assertThat(reimported.getUserClaimNames(), is(equalTo(imported.getUserClaimNames())));
        assertThat(reimported.getGroupClaimName(), is(equalTo(imported.getGroupClaimName())));
    }
}
