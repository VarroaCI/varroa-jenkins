package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import java.util.Arrays;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import org.junit.Test;

/**
 * Tests for the configured group and user claim names: value shapes, the
 * comma-separated name list, the local-prefix refusal on custom user claims,
 * and the group source of a local session beside OIDC.
 */
public class VarroaGroupClaimsTest {

    private static Map<String, Object> claims(Object... kv) {
        Map<String, Object> c = new HashMap<>();
        for (int i = 0; i < kv.length; i += 2) {
            c.put((String) kv[i], kv[i + 1]);
        }
        return c;
    }

    @Test
    public void getGroupsReadsListAndString() {
        Map<String, Object> c = claims("roles", List.of("a", "b"), "role", "ops");
        assertEquals(List.of("a", "b"), JWTValidator.getGroups(c, List.of("roles")));
        assertEquals(List.of("ops"), JWTValidator.getGroups(c, List.of("role")));
    }

    @Test
    public void getGroupsDoesNotSplitString() {
        Map<String, Object> c = claims("role", "a, b c");
        assertEquals(List.of("a, b c"), JWTValidator.getGroups(c, List.of("role")));
    }

    @Test
    public void getGroupsReadsSeveralNamesDedup() {
        Map<String, Object> c = claims("groups", List.of("a"), "roles", List.of("a", "b"));
        assertEquals(List.of("a", "b"), JWTValidator.getGroups(c, List.of("groups", "roles")));
    }

    @Test
    public void getGroupsIgnoresNonStringShapes() {
        Map<String, Object> c = claims("roles", Arrays.asList(1, "x", ""), "obj", Map.of("k", "v"));
        assertEquals(List.of("x"), JWTValidator.getGroups(c, List.of("roles")));
        assertEquals(List.of(), JWTValidator.getGroups(c, List.of("obj")));
    }

    @Test
    public void getGroupsColonName() {
        Map<String, Object> c = claims("cognito:groups", List.of("admins"));
        assertEquals(List.of("admins"), JWTValidator.getGroups(c, List.of("cognito:groups")));
    }

    @Test
    public void getGroupsNullOrEmptyNamesReadGroupsClaim() {
        Map<String, Object> c = claims("groups", List.of("g"));
        assertEquals(List.of("g"), JWTValidator.getGroups(c, null));
        assertEquals(List.of("g"), JWTValidator.getGroups(c, List.of()));
    }

    @Test
    public void parseClaimNamesTrimsDropsEmptyAndDedups() {
        assertEquals(List.of("groups", "roles"),
                VarroaSecurityRealm.parseClaimNames(" groups , roles,,groups "));
        assertEquals(List.of(), VarroaSecurityRealm.parseClaimNames(null));
    }

    @Test
    public void realmParsesCommaSeparatedGroupClaimNames() {
        VarroaSecurityRealm realm = new VarroaSecurityRealm(
                "https://dex.example", " login , sub ", "memberOf, roles");
        assertEquals("login,sub", realm.getUserClaimNames());
        assertEquals("memberOf,roles", realm.getGroupClaimNames());
    }

    @Test
    public void oidcSessionReadsConfiguredGroupClaims() {
        VarroaSecurityRealm realm = new VarroaSecurityRealm(
                "https://dex.example", "preferred_username,sub", "roles");
        realm.setLocalJwtValidatorForTest(new FixedIssuerValidator("https://varroa.example"));
        Map<String, Object> oidc = claims("iss", "https://dex.example", "roles", List.of("r"), "groups", List.of("x"));
        assertEquals(List.of("r"), realm.sessionGroups(oidc));
    }

    @Test
    public void localSessionAlwaysReadsGroupsClaim() {
        VarroaSecurityRealm realm = new VarroaSecurityRealm(
                "https://dex.example", "preferred_username,sub", "roles");
        realm.setLocalJwtValidatorForTest(new FixedIssuerValidator("https://varroa.example"));
        Map<String, Object> local = claims("iss", "https://varroa.example", "sub", "local:rescue",
                "groups", List.of("g"));
        assertEquals("a local session keeps its groups under any group claim", List.of("g"),
                realm.sessionGroups(local));
    }

    @Test
    public void oidcTokenWithLocalPrefixInCustomUserClaimRefused() {
        VarroaSecurityRealm realm = new VarroaSecurityRealm(
                "https://dex.example", "upn,sub", "groups");
        Map<String, Object> idp = claims("sub", "alice", "upn", "local:alice");
        realm.setJwtValidatorForTest(new FixedClaimsValidator("https://dex.example", idp));
        assertNull("an IdP token must not carry the local: prefix in a custom user claim",
                realm.validateSession(tokenWithIssuer("https://dex.example")));
    }

    @Test
    public void localPrefixInEmailUserClaimRefused() {
        VarroaSecurityRealm realm = new VarroaSecurityRealm(
                "https://dex.example", "email,sub", "groups");
        realm.setJwtValidatorForTest(new FixedClaimsValidator("https://dex.example",
                claims("sub", "alice", "email", "local:alice")));
        assertNull("an IdP email of local:alice must be refused",
                realm.validateSession(tokenWithIssuer("https://dex.example")));
    }

    @Test
    public void paddedMixedCaseLocalPrefixRefused() {
        VarroaSecurityRealm realm = new VarroaSecurityRealm(
                "https://dex.example", "upn,sub", "groups");
        realm.setJwtValidatorForTest(new FixedClaimsValidator("https://dex.example",
                claims("sub", "alice", "upn", " Local:alice")));
        assertNull("a padded, mixed-case local: value must be refused",
                realm.validateSession(tokenWithIssuer("https://dex.example")));
    }

    @Test
    public void normalEmailUserClaimAccepted() {
        VarroaSecurityRealm realm = new VarroaSecurityRealm(
                "https://dex.example", "email,sub", "groups");
        realm.setJwtValidatorForTest(new FixedClaimsValidator("https://dex.example",
                claims("sub", "alice", "email", "alice@corp.example")));
        assertNotNull("a normal email must pass",
                realm.validateSession(tokenWithIssuer("https://dex.example")));
    }

    private static String tokenWithIssuer(String iss) {
        String payload = "{\"iss\":\"" + iss + "\"}";
        return "e30." + java.util.Base64.getUrlEncoder().withoutPadding()
                .encodeToString(payload.getBytes(java.nio.charset.StandardCharsets.UTF_8)) + ".c2ln";
    }

    /** A validator with a fixed issuer and no claims, used as the local validator. */
    private static final class FixedIssuerValidator extends JWTValidator {
        FixedIssuerValidator(String issuer) {
            super(issuer, "varroa");
        }
    }

    /** A validator that returns fixed claims for every token. */
    private static final class FixedClaimsValidator extends JWTValidator {
        private final Map<String, Object> claims;

        FixedClaimsValidator(String issuer, Map<String, Object> claims) {
            super(issuer, "varroa");
            this.claims = claims;
        }

        @Override
        public Map<String, Object> validate(String token) {
            return claims;
        }
    }
}
