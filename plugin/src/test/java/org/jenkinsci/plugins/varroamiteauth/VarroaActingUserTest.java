package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertNull;

import java.util.ArrayList;
import java.util.List;

import net.sf.json.JSONObject;

import org.junit.Test;
import org.springframework.security.core.Authentication;
import org.springframework.security.core.GrantedAuthority;

/**
 * Tests for {@link VarroaActingUser} (D10): the acting user a command body
 * carries, and the authentication the two input routes run their Jenkins calls
 * inside.
 */
public class VarroaActingUserTest {

    private static VarroaActingUser user(String json) {
        return VarroaActingUser.fromJson(JSONObject.fromObject(json));
    }

    private static List<String> authoritiesOf(Authentication authentication) {
        List<String> names = new ArrayList<>();
        for (GrantedAuthority authority : authentication.getAuthorities()) {
            names.add(authority.getAuthority());
        }
        return names;
    }

    @Test
    public void theAuthenticationCarriesAuthenticatedAndOneAuthorityPerGroup() {
        VarroaActingUser acting = user("{\"actingUser\":{\"name\":\"ada\",\"email\":\"ada@example.com\","
                + "\"groups\":[\"varroa-payments\",\"varroa-ops\"]}}");
        assertNotNull(acting);

        Authentication authentication = acting.authentication();

        assertEquals("ada", authentication.getName());
        assertEquals(List.of("authenticated", "varroa-payments", "varroa-ops"),
                authoritiesOf(authentication));
    }

    @Test
    public void theAuthenticationCarriesAuthenticatedAloneWithoutGroups() {
        Authentication authentication = user("{\"actingUser\":{\"name\":\"ada\"}}").authentication();

        assertEquals(List.of("authenticated"), authoritiesOf(authentication));
    }

    @Test
    public void anEmptyGroupAddsNoAuthority() {
        Authentication authentication = user("{\"actingUser\":{\"name\":\"ada\","
                + "\"groups\":[\"\",\"varroa-payments\"]}}").authentication();

        assertEquals(List.of("authenticated", "varroa-payments"), authoritiesOf(authentication));
    }

    @Test
    public void aBlankNameIsRefused() {
        assertNull(user("{\"actingUser\":{\"name\":\"\"}}"));
        assertNull(user("{\"actingUser\":{\"name\":\"   \"}}"));
        assertNull(user("{\"actingUser\":{}}"));
        assertNull(user("{}"));
    }

    @Test
    public void theEmailReachesNoAuthority() {
        VarroaActingUser acting = user("{\"actingUser\":{\"name\":\"ada\",\"email\":\"ada@example.com\","
                + "\"groups\":[\"varroa-ops\"]}}");

        assertEquals("ada@example.com", acting.getEmail());
        assertEquals(List.of("varroa-ops"), acting.getGroups());
        assertFalse(authoritiesOf(acting.authentication()).contains("ada@example.com"));
    }
    @Test
    public void aBlankGroupAddsNoAuthority() {
        JSONObject body = JSONObject.fromObject(
                "{\"actingUser\":{\"name\":\"ada\",\"groups\":[\"   \",\" varroa-payments \",\"\"]}}");

        VarroaActingUser user = VarroaActingUser.fromJson(body);

        assertEquals(List.of(" varroa-payments "), user.getGroups());
        assertEquals(2, user.authentication().getAuthorities().size());
    }
}
