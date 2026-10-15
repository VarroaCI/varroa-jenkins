package org.jenkinsci.plugins.varroamiteauth;

import java.util.ArrayList;
import java.util.List;

import net.sf.json.JSONArray;
import net.sf.json.JSONObject;

import org.springframework.security.authentication.UsernamePasswordAuthenticationToken;
import org.springframework.security.core.Authentication;
import org.springframework.security.core.GrantedAuthority;
import org.springframework.security.core.authority.SimpleGrantedAuthority;

/**
 * The user a mite-delivered command acts as (D10).
 *
 * <p>The command body carries the value, never the request authentication:
 * the request authenticates the mite, not the person.
 *
 * <p>{@link #authentication()} is the one builder of that identity in this
 * plugin. Any later route that acts as a person reuses this class unchanged.
 */
public final class VarroaActingUser {

    /** The authority every signed-in user carries. */
    private static final String AUTHENTICATED = "authenticated";

    private final String name;
    private final String email;
    private final List<String> groups;

    private VarroaActingUser(String name, String email, List<String> groups) {
        this.name = name;
        this.email = email;
        this.groups = groups;
    }

    /**
     * Reads the acting user from a command body's {@code actingUser} object.
     *
     * <p>It answers {@code null} when the body holds no such object, and when
     * the name is blank. A check against an empty principal has no meaning, so
     * a route answers {@code 400} for a null answer.
     *
     * <p>The email never reaches an authority and no check reads it. It is an
     * audit value.
     */
    static VarroaActingUser fromJson(JSONObject body) {
        if (body == null) {
            return null;
        }
        JSONObject acting = body.optJSONObject("actingUser");
        if (acting == null) {
            return null;
        }
        String name = acting.optString("name", "").trim();
        if (name.isEmpty()) {
            return null;
        }
        List<String> groups = new ArrayList<>();
        JSONArray raw = acting.optJSONArray("groups");
        if (raw != null) {
            for (Object group : raw) {
                // A blank group names no authority, so it never joins the
                // list. A non-blank group keeps its exact value, because
                // the security realm keeps a group's value exactly at
                // sign-in, and the authority must match that value.
                String value = group == null ? "" : String.valueOf(group);
                if (!value.trim().isEmpty()) {
                    groups.add(value);
                }
            }
        }
        return new VarroaActingUser(name, acting.optString("email", ""), List.copyOf(groups));
    }

    /** Returns the acting user's name, which is never empty. */
    String getName() {
        return name;
    }

    /** Returns the acting user's email, or the empty string. */
    String getEmail() {
        return email;
    }

    /** Returns the acting user's non-blank groups, in the order the body named them. */
    List<String> getGroups() {
        return groups;
    }

    /**
     * Builds the authentication the Jenkins calls run as. The authorities are
     * {@code authenticated} plus one per non-empty group, which is the mapping
     * the Varroa security realm applies at sign-in.
     *
     * <p>A persisted user carries {@code authenticated} alone, so an
     * impersonated user matches no group-scoped {@code submitter} check. This
     * authentication carries the groups the command reported.
     */
    Authentication authentication() {
        List<GrantedAuthority> authorities = new ArrayList<>();
        authorities.add(new SimpleGrantedAuthority(AUTHENTICATED));
        for (String group : groups) {
            if (!group.isEmpty()) {
                authorities.add(new SimpleGrantedAuthority(group));
            }
        }
        return new UsernamePasswordAuthenticationToken(name, null, authorities);
    }
}
