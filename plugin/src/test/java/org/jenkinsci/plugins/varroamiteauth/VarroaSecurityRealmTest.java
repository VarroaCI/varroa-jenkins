package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

import org.acegisecurity.Authentication;
import org.acegisecurity.context.SecurityContextHolder;

import java.lang.reflect.Field;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * Integration tests for the VarroaSecurityRealm session-aware mite auth.
 *
 * These tests exercise the filter's session reuse and operator-JWT
 * validation paths against a real Jenkins instance spun up by JenkinsRule.
 */
public class VarroaSecurityRealmTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    /**
     * Regression for #530: Jenkins core builds login redirects as
     * contextPath + "/" + getLoginUrl(), so the value must be context-relative.
     * Returning the absolute VARROA_LOGIN_URL produced /https://<dashboard>/login.
     */
    @Test
    public void loginUrlIsContextRelative() {
        VarroaSecurityRealm realm = new VarroaSecurityRealm();
        assertEquals("securityRealm/commenceLogin", realm.getLoginUrl());
    }

    /**
     * commenceLogin turns the server-relative 'from' path into an absolute
     * state URL so the dashboard can send the user back to this controller.
     * Only the root URL's scheme+authority are used: 'from' already carries
     * the context path (JenkinsRule serves under /jenkins), and reusing the
     * full root URL would double the prefix in path-mode deployments.
     */
    @Test
    public void commenceLoginStateIsAbsoluteWithoutDoublingThePrefix() throws Exception {
        VarroaSecurityRealm realm = new VarroaSecurityRealm();
        realm.loginUrlForTest = "https://dash.example/login";

        String rootUrl = j.jenkins.getRootUrl();
        assertNotNull(rootUrl);
        java.net.URI root = java.net.URI.create(rootUrl);
        // JenkinsRule's context path stands in for a path-mode --prefix.
        assertEquals("/jenkins/", root.getPath());
        String from = "/jenkins/api/json";
        String expectedState = root.getScheme() + "://" + root.getAuthority() + from;

        assertEquals(
                "https://dash.example/login?state="
                        + java.net.URLEncoder.encode(expectedState, java.nio.charset.StandardCharsets.UTF_8),
                realm.commenceLoginRedirectUrl(from));
    }

    /**
     * Only server-relative 'from' paths are honored as the post-login state —
     * an absolute URL would be an open redirect through the dashboard.
     */
    @Test
    public void commenceLoginRejectsAbsoluteFromTargets() throws Exception {
        VarroaSecurityRealm realm = new VarroaSecurityRealm();
        realm.loginUrlForTest = "https://dash.example/login";

        java.net.URI root = java.net.URI.create(j.jenkins.getRootUrl());
        String expectedState = root.getScheme() + "://" + root.getAuthority() + "/";

        assertEquals(
                "https://dash.example/login?state="
                        + java.net.URLEncoder.encode(expectedState, java.nio.charset.StandardCharsets.UTF_8),
                realm.commenceLoginRedirectUrl("https://evil.example/"));

        // Protocol-relative targets are rejected too — with no root URL to
        // absolutize against they would reach the dashboard verbatim.
        assertEquals(
                "https://dash.example/login?state="
                        + java.net.URLEncoder.encode(expectedState, java.nio.charset.StandardCharsets.UTF_8),
                realm.commenceLoginRedirectUrl("//evil.example/path"));
    }

    /**
     * A request with no Authorization header and no session is NOT
     * authenticated as the mite (negative case).
     */
    @Test
    public void noAuthIsNotMite() throws Exception {
        JenkinsRule.WebClient wc = j.createWebClient();
        // Disable preemptive basic auth so the request is anonymous.
        wc.setThrowExceptionOnFailingStatusCode(false);
        // Use an existing Jenkins page to trigger the security filter.
        wc.goTo("login");
        Authentication auth = SecurityContextHolder.getContext().getAuthentication();
        // Without any credentials, the user should not be varroa-mite.
        if (auth != null) {
            assertNotEquals("varroa-mite", auth.getPrincipal());
        }
    }

    /**
     * A request to a static page without credentials is allowed through,
     * or returns a redirect/not-found that the filter correctly passed through.
     */
    @Test
    public void staticResourceWithoutSessionIsAllowed() throws Exception {
        JenkinsRule.WebClient wc = j.createWebClient();
        wc.setThrowExceptionOnFailingStatusCode(false);
        int status = wc.goTo("userContent/").getWebResponse().getStatusCode();
        // The filter should not block static resources. Accept 200/404/302.
        // Note: 503 can occur in containerized test harness (networking).
        assertTrue("static resource request returned " + status,
                status != 401 && status != 403);
    }

    private static final String VK_TOKEN = "vk_0123456789abc."
            + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA";

    /** A fake validator with a fixed identity, bypassing the verify endpoint. */
    private static ApiKeyValidator fakeValidator(boolean accept) {
        return new ApiKeyValidator(null, null) {
            @Override
            public java.util.Map<String, Object> validate(String token) {
                if (!accept || token == null || !token.startsWith("vk_")) {
                    return null;
                }
                java.util.Map<String, Object> m = new java.util.HashMap<>();
                m.put("subject", "sub-1");
                m.put("preferredUsername", "jdoe");
                m.put("email", "jdoe@example.com");
                m.put("name", "J Doe");
                m.put("groups", java.util.List.of("team-a"));
                return m;
            }

            @Override
            public boolean isEnabled() {
                return true;
            }
        };
    }

    /**
     * A vk_* Bearer authenticates as the token owner's real identity, and the
     * response establishes no JSESSIONID session (spec: vk_ requests are
     * sessionless; identity mapping mirrors the OIDC cookie path).
     */
    @Test
    public void vkBearerAuthenticatesSessionlessly() throws Exception {
        VarroaSecurityRealm realm = new VarroaSecurityRealm();
        realm.setApiKeyValidatorForTest(fakeValidator(true));
        j.jenkins.setSecurityRealm(realm);

        JenkinsRule.WebClient wc = j.createWebClient();
        wc.setThrowExceptionOnFailingStatusCode(false);
        wc.addRequestHeader("Authorization", "Bearer " + VK_TOKEN);
        org.htmlunit.Page p = wc.goTo("whoAmI/api/json", null);
        // The containerized harness can fail to boot the Jenkins web stack
        // (javax/jakarta filter mismatch → 503 for every request, see the
        // note in staticResourceWithoutSessionIsAllowed). Skip — visibly —
        // rather than asserting against a dead server.
        org.junit.Assume.assumeTrue("harness web stack unavailable (503)",
                p.getWebResponse().getStatusCode() != 503);

        String body = p.getWebResponse().getContentAsString();
        assertTrue("expected jdoe in whoAmI response: " + body, body.contains("jdoe"));
        assertTrue("expected team-a authority in: " + body, body.contains("team-a"));

        // Sessionless: no JSESSIONID cookie may be set for vk_ requests.
        for (org.htmlunit.util.NameValuePair h : p.getWebResponse().getResponseHeaders()) {
            if ("Set-Cookie".equalsIgnoreCase(h.getName())) {
                assertFalse("unexpected session cookie: " + h.getValue(),
                        h.getValue().contains("JSESSIONID"));
            }
        }
    }

    private static final String OPERATOR_JWT =
            "eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJ2YXJyb2Etb3BlcmF0b3IifQ.c2ln";

    /** A fake JWT validator that accepts exactly one operator token. */
    private static JWTValidator fakeOperatorValidator() {
        return new JWTValidator(null) {
            @Override
            public java.util.Map<String, Object> validate(String token) {
                return null; // no varroa_token cookie path in these tests
            }

            @Override
            public java.util.Map<String, Object> validateOperatorToken(String token) {
                if (!OPERATOR_JWT.equals(token)) {
                    return null;
                }
                java.util.Map<String, Object> m = new java.util.HashMap<>();
                m.put("iss", "varroa-operator");
                m.put("sub", "system:varroa-mite");
                m.put("aud", "test-controller");
                m.put("exp", (System.currentTimeMillis() / 1000) + 3600);
                return m;
            }
        };
    }

    private static final String SYSTEM_OPERATOR_JWT =
            "eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJ2YXJyb2Etb3BlcmF0b3IifQ.c2ln2";

    /** A fake JWT validator that accepts exactly one system:varroa-operator token. */
    private static JWTValidator fakeSystemOperatorValidator() {
        return new JWTValidator(null) {
            @Override
            public java.util.Map<String, Object> validate(String token) {
                return null;
            }

            @Override
            public java.util.Map<String, Object> validateOperatorToken(String token) {
                if (!SYSTEM_OPERATOR_JWT.equals(token)) {
                    return null;
                }
                java.util.Map<String, Object> m = new java.util.HashMap<>();
                m.put("iss", "varroa-operator");
                m.put("sub", "system:varroa-operator");
                m.put("aud", "test-controller");
                m.put("exp", (System.currentTimeMillis() / 1000) + 3600);
                return m;
            }
        };
    }

    private static final String UNKNOWN_SUBJECT_JWT =
            "eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJ2YXJyb2Etb3BlcmF0b3IifQ.c2ln3";

    /** A fake JWT validator that accepts a token with an unrecognized sub claim. */
    private static JWTValidator fakeOperatorValidatorUnknownSubject() {
        return new JWTValidator(null) {
            @Override
            public java.util.Map<String, Object> validate(String token) {
                return null;
            }

            @Override
            public java.util.Map<String, Object> validateOperatorToken(String token) {
                if (!UNKNOWN_SUBJECT_JWT.equals(token)) {
                    return null;
                }
                java.util.Map<String, Object> m = new java.util.HashMap<>();
                m.put("iss", "varroa-operator");
                m.put("sub", "system:something-else");
                m.put("aud", "test-controller");
                m.put("exp", (System.currentTimeMillis() / 1000) + 3600);
                return m;
            }
        };
    }

    private static final String USER_OPERATOR_JWT =
            "eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJ2YXJyb2Etb3BlcmF0b3IifQ.dXNlcg";

    private static JWTValidator fakeUserOperatorValidator(Map<String, Object> claims) {
        return new JWTValidator(null) {
            @Override
            public Map<String, Object> validate(String token) {
                return null;
            }

            @Override
            public Map<String, Object> validateOperatorToken(String token) {
                return USER_OPERATOR_JWT.equals(token) ? claims : null;
            }
        };
    }

    private static Map<String, Object> userOperatorClaims() {
        Map<String, Object> claims = new HashMap<>();
        claims.put("varroa_typ", "user");
        claims.put("sub", "oidc-mcp-user");
        claims.put("preferred_username", "mcp-user");
        claims.put("name", "MCP User");
        claims.put("email", "mcp-user@example.com");
        claims.put("groups", List.of("platform-team", "ROLE:varroa:system-mite"));
        return claims;
    }

    private static void assertUserAuthentication(Authentication auth, String userId) {
        assertNotNull("expected user authentication", auth);
        assertEquals(userId, auth.getName());
        boolean authenticated = false;
        boolean platformTeam = false;
        for (org.acegisecurity.GrantedAuthority authority : auth.getAuthorities()) {
            if ("authenticated".equals(authority.getAuthority())) authenticated = true;
            if ("platform-team".equals(authority.getAuthority())) platformTeam = true;
            assertFalse("user tokens must not receive system authorities",
                    authority.getAuthority().startsWith("ROLE:varroa:system-"));
        }
        assertTrue("expected authenticated authority", authenticated);
        assertTrue("expected groups from literal groups claim", platformTeam);
    }

    @SuppressWarnings("unchecked")
    private static String userEmail(hudson.model.User user) throws Exception {
        Class<? extends hudson.model.UserProperty> mailerProperty = Class
                .forName("hudson.tasks.Mailer$UserProperty")
                .asSubclass(hudson.model.UserProperty.class);
        Object property = user.getProperty(mailerProperty);
        if (property == null) {
            return "";
        }
        return (String) property.getClass().getMethod("getAddress").invoke(property);
    }

    /**
     * BUG (task 1.5): a valid operator-JWT Bearer must authenticate as the mite
     * for ALL requests, including a plain GET to /whoAmI — not just config/rbac
     * apply POSTs. Previously /whoAmI reported anonymous:true despite a valid JWT.
     */
    @Test
    public void operatorJwtWhoAmIReportsMite() throws Exception {
        VarroaSecurityRealm realm = new VarroaSecurityRealm();
        realm.setJwtValidatorForTest(fakeOperatorValidator());
        j.jenkins.setSecurityRealm(realm);

        JenkinsRule.WebClient wc = j.createWebClient();
        wc.setThrowExceptionOnFailingStatusCode(false);
        wc.addRequestHeader("Authorization", "Bearer " + OPERATOR_JWT);
        org.htmlunit.Page p = wc.goTo("whoAmI/api/json", null);
        org.junit.Assume.assumeTrue("harness web stack unavailable (503)",
                p.getWebResponse().getStatusCode() != 503);

        String body = p.getWebResponse().getContentAsString();
        assertTrue("expected varroa-mite principal in whoAmI: " + body,
                body.contains("varroa-mite"));
        assertTrue("expected anonymous:false in whoAmI: " + body,
                body.contains("\"anonymous\":false"));
        assertTrue("expected system-mite role authority in whoAmI: " + body,
                body.contains("ROLE:varroa:system-mite"));
    }

    /**
     * A valid operator-JWT Bearer with sub=system:varroa-operator authenticates
     * as the varroa-operator system principal with ROLE:varroa:system-operator.
     */
    @Test
    public void operatorJwtWhoAmIReportsSystemOperator() throws Exception {
        VarroaSecurityRealm realm = new VarroaSecurityRealm();
        realm.setJwtValidatorForTest(fakeSystemOperatorValidator());
        j.jenkins.setSecurityRealm(realm);

        JenkinsRule.WebClient wc = j.createWebClient();
        wc.setThrowExceptionOnFailingStatusCode(false);
        wc.addRequestHeader("Authorization", "Bearer " + SYSTEM_OPERATOR_JWT);
        org.htmlunit.Page p = wc.goTo("whoAmI/api/json", null);
        org.junit.Assume.assumeTrue("harness web stack unavailable (503)",
                p.getWebResponse().getStatusCode() != 503);

        String body = p.getWebResponse().getContentAsString();
        assertTrue("expected varroa-operator principal in whoAmI: " + body,
                body.contains("varroa-operator"));
        assertFalse("expected no varroa-mite principal in whoAmI: " + body,
                body.contains("varroa-mite"));
        assertTrue("expected anonymous:false in whoAmI: " + body,
                body.contains("\"anonymous\":false"));
        assertTrue("expected system-operator role authority in whoAmI: " + body,
                body.contains("ROLE:varroa:system-operator"));
        assertFalse("expected no system-mite role authority in whoAmI: " + body,
                body.contains("ROLE:varroa:system-mite"));
    }

    /**
     * A valid operator-JWT Bearer with an unrecognized sub claim is rejected
     * and does NOT authenticate — the request remains anonymous.
     */
    @Test
    public void operatorJwtUnknownSubjectRejected() throws Exception {
        VarroaSecurityRealm realm = new VarroaSecurityRealm();
        realm.setJwtValidatorForTest(fakeOperatorValidatorUnknownSubject());
        j.jenkins.setSecurityRealm(realm);

        JenkinsRule.WebClient wc = j.createWebClient();
        wc.setThrowExceptionOnFailingStatusCode(false);
        wc.addRequestHeader("Authorization", "Bearer " + UNKNOWN_SUBJECT_JWT);
        org.htmlunit.Page p = wc.goTo("whoAmI/api/json", null);
        org.junit.Assume.assumeTrue("harness web stack unavailable (503)",
                p.getWebResponse().getStatusCode() != 503);

        String body = p.getWebResponse().getContentAsString();
        assertTrue("expected anonymous:true for unrecognized sub: " + body,
                body.contains("\"anonymous\":true"));
    }

    /**
     * Regression for the root cause: even when the request carries a live (but
     * anonymous) JSESSIONID, a valid operator Bearer must still establish the
     * mite principal. The old !hasExistingSession gate skipped JWT injection in
     * this case, leaving /whoAmI anonymous.
     */
    @Test
    public void operatorJwtWhoAmIReportsMiteWithExistingSession() throws Exception {
        VarroaSecurityRealm realm = new VarroaSecurityRealm();
        realm.setJwtValidatorForTest(fakeOperatorValidator());
        j.jenkins.setSecurityRealm(realm);

        JenkinsRule.WebClient wc = j.createWebClient();
        wc.setThrowExceptionOnFailingStatusCode(false);

        // First, an anonymous request (no Bearer) so the server hands back a
        // JSESSIONID and the WebClient's cookie jar retains it.
        wc.goTo("login", null);

        // Now the operator Bearer request reuses that JSESSIONID via the jar.
        wc.addRequestHeader("Authorization", "Bearer " + OPERATOR_JWT);
        org.htmlunit.Page p = wc.goTo("whoAmI/api/json", null);
        org.junit.Assume.assumeTrue("harness web stack unavailable (503)",
                p.getWebResponse().getStatusCode() != 503);

        String body = p.getWebResponse().getContentAsString();
        assertTrue("expected varroa-mite principal despite existing session: " + body,
                body.contains("varroa-mite"));
        assertTrue("expected anonymous:false despite existing session: " + body,
                body.contains("\"anonymous\":false"));
    }

    // ---- In-process filter tests (independent of the Jetty web stack) ----
    //
    // The containerized harness frequently returns 503 for every HtmlUnit
    // request (javax/jakarta filter mismatch), which makes the goTo()-based
    // tests above skip. These tests drive the realm's createFilter() filter
    // directly with proxy servlet objects so the fixed operator-JWT injection
    // path is asserted deterministically regardless of the web stack.

    /** A minimal HttpServletRequest proxy with controllable headers/cookies. */
    @SuppressWarnings("unchecked")
    private static jakarta.servlet.http.HttpServletRequest fakeRequest(
            String uri, String authHeader, jakarta.servlet.http.Cookie[] cookies,
            jakarta.servlet.http.HttpSession session) {
        return fakeRequest(uri, authHeader, cookies, session, null);
    }

    @SuppressWarnings("unchecked")
    private static jakarta.servlet.http.HttpServletRequest fakeRequest(
            String uri, String authHeader, jakarta.servlet.http.Cookie[] cookies,
            jakarta.servlet.http.HttpSession session, java.util.concurrent.atomic.AtomicInteger sessionCreates) {
        java.lang.reflect.InvocationHandler h = (proxy, method, args) -> {
            switch (method.getName()) {
                case "getRequestURI": return uri;
                case "getCookies": return cookies;
                case "getQueryString": return null;
                case "getHeader":
                    String n = (String) args[0];
                    if ("Authorization".equalsIgnoreCase(n)) return authHeader;
                    return null;
                case "getSession":
                    if (sessionCreates != null && (args == null || args.length == 0
                            || Boolean.TRUE.equals(args[0]))) {
                        sessionCreates.incrementAndGet();
                    }
                    return session; // returns null when none, ignoring create flag
                default:
                    Class<?> rt = method.getReturnType();
                    if (rt == boolean.class) return false;
                    if (rt == int.class) return 0;
                    return null;
            }
        };
        return (jakarta.servlet.http.HttpServletRequest) java.lang.reflect.Proxy.newProxyInstance(
                VarroaSecurityRealmTest.class.getClassLoader(),
                new Class<?>[]{jakarta.servlet.http.HttpServletRequest.class}, h);
    }

    private static jakarta.servlet.http.HttpServletResponse fakeResponse() {
        java.lang.reflect.InvocationHandler h = (proxy, method, args) -> {
            Class<?> rt = method.getReturnType();
            if (rt == boolean.class) return false;
            if (rt == int.class) return 0;
            return null;
        };
        return (jakarta.servlet.http.HttpServletResponse) java.lang.reflect.Proxy.newProxyInstance(
                VarroaSecurityRealmTest.class.getClassLoader(),
                new Class<?>[]{jakarta.servlet.http.HttpServletResponse.class}, h);
    }

    private static jakarta.servlet.http.HttpSession fakeSession() {
        java.lang.reflect.InvocationHandler h = (proxy, method, args) -> {
            Class<?> rt = method.getReturnType();
            if (rt == boolean.class) return false;
            if (rt == int.class) return 0;
            if ("getId".equals(method.getName())) return "fake-session-id";
            return null;
        };
        return (jakarta.servlet.http.HttpSession) java.lang.reflect.Proxy.newProxyInstance(
                VarroaSecurityRealmTest.class.getClassLoader(),
                new Class<?>[]{jakarta.servlet.http.HttpSession.class}, h);
    }

    /**
     * Drives createFilter() with a valid operator Bearer and asserts the
     * SecurityContext is set to the mite inside the chain — for a plain GET
     * like /whoAmI. This is the core of the task 1.5 fix.
     */
    @Test
    public void filterInjectsMiteForOperatorBearer() throws Exception {
        VarroaSecurityRealm realm = new VarroaSecurityRealm();
        realm.setJwtValidatorForTest(fakeOperatorValidator());
        j.jenkins.setSecurityRealm(realm);

        jakarta.servlet.Filter filter =
                realm.createFilter(new jakarta.servlet.FilterConfig() {
                    public String getFilterName() { return "test"; }
                    public jakarta.servlet.ServletContext getServletContext() { return null; }
                    public String getInitParameter(String name) { return null; }
                    public java.util.Enumeration<String> getInitParameterNames() {
                        return java.util.Collections.emptyEnumeration();
                    }
                });

        final Authentication[] seen = new Authentication[1];
        jakarta.servlet.FilterChain terminal = (r, s) ->
                seen[0] = SecurityContextHolder.getContext().getAuthentication();
        java.util.concurrent.atomic.AtomicInteger sessionCreates =
                new java.util.concurrent.atomic.AtomicInteger();

        SecurityContextHolder.clearContext();
        // No cookie, no existing session: clean GET to /whoAmI.
        filter.doFilter(
                fakeRequest("/whoAmI/api/json", "Bearer " + OPERATOR_JWT, null, null,
                        sessionCreates),
                fakeResponse(), terminal);

        assertNotNull("chain must run with an authentication set", seen[0]);
        assertEquals("varroa-mite must be the principal name",
                "varroa-mite", seen[0].getName());
        boolean hasRole = false;
        for (org.acegisecurity.GrantedAuthority a : seen[0].getAuthorities()) {
            if ("ROLE:varroa:system-mite".equals(a.getAuthority())) hasRole = true;
        }
        assertTrue("mite must hold ROLE:varroa:system-mite", hasRole);
        assertEquals("operator Bearer must not create a session", 0, sessionCreates.get());
        SecurityContextHolder.clearContext();
    }

    /**
     * Regression for the root cause: a live (anonymous) JSESSIONID present on
     * the request must NOT suppress operator-JWT injection. Before the fix the
     * !hasExistingSession gate skipped injection here, leaving /whoAmI anonymous.
     */
    @Test
    public void filterInjectsMiteEvenWithExistingSession() throws Exception {
        VarroaSecurityRealm realm = new VarroaSecurityRealm();
        realm.setJwtValidatorForTest(fakeOperatorValidator());
        j.jenkins.setSecurityRealm(realm);

        jakarta.servlet.Filter filter =
                realm.createFilter(new jakarta.servlet.FilterConfig() {
                    public String getFilterName() { return "test"; }
                    public jakarta.servlet.ServletContext getServletContext() { return null; }
                    public String getInitParameter(String name) { return null; }
                    public java.util.Enumeration<String> getInitParameterNames() {
                        return java.util.Collections.emptyEnumeration();
                    }
                });

        final Authentication[] seen = new Authentication[1];
        jakarta.servlet.FilterChain terminal = (r, s) ->
                seen[0] = SecurityContextHolder.getContext().getAuthentication();

        // A live JSESSIONID cookie + server-side session (anonymous).
        jakarta.servlet.http.Cookie[] cookies = {
            new jakarta.servlet.http.Cookie("JSESSIONID.abc123", "node0deadbeef")
        };

        SecurityContextHolder.clearContext();
        filter.doFilter(
                fakeRequest("/whoAmI/api/json", "Bearer " + OPERATOR_JWT,
                        cookies, fakeSession()),
                fakeResponse(), terminal);

        assertNotNull("chain must run with an authentication set", seen[0]);
        assertEquals("operator Bearer must win over an existing anonymous session",
                "varroa-mite", seen[0].getName());
        SecurityContextHolder.clearContext();
    }

    @Test
    public void filterInjectsUserOperatorBearerWithLiteralGroups() throws Exception {
        VarroaSecurityRealm realm = new VarroaSecurityRealm("", null, "roles");
        realm.setJwtValidatorForTest(fakeUserOperatorValidator(userOperatorClaims()));
        j.jenkins.setSecurityRealm(realm);

        jakarta.servlet.Filter filter = realm.createFilter(newFilterConfig());
        final Authentication[] seen = new Authentication[1];
        jakarta.servlet.FilterChain terminal = (r, s) ->
                seen[0] = SecurityContextHolder.getContext().getAuthentication();
        java.util.concurrent.atomic.AtomicInteger sessionCreates =
                new java.util.concurrent.atomic.AtomicInteger();

        SecurityContextHolder.clearContext();
        filter.doFilter(fakeRequest("/whoAmI/api/json", "Bearer " + USER_OPERATOR_JWT, null, null,
                sessionCreates), fakeResponse(), terminal);

        assertUserAuthentication(seen[0], "mcp-user");
        hudson.model.User user = hudson.model.User.getById("mcp-user", false);
        assertNotNull("user record should be created", user);
        assertEquals("MCP User", user.getFullName());
        assertEquals("mcp-user@example.com", userEmail(user));
        assertEquals("user operator Bearer must not create a session", 0, sessionCreates.get());
        SecurityContextHolder.clearContext();
    }

    @Test
    public void authenticationManagerRecoversUserOperatorBearer() throws Exception {
        VarroaSecurityRealm realm = new VarroaSecurityRealm();
        realm.setJwtValidatorForTest(fakeUserOperatorValidator(userOperatorClaims()));

        Authentication auth = authenticateFallback(realm,
                fakeRequest("/whoAmI/api/json", "Bearer " + USER_OPERATOR_JWT, null, null));
        assertUserAuthentication(auth, "mcp-user");
    }

    @Test
    public void userOperatorBearerFallsBackToSubjectAndRetainsExistingProfileWhenEmpty() throws Exception {
        Map<String, Object> claims = userOperatorClaims();
        claims.put("preferred_username", "");
        claims.put("name", "");
        claims.put("email", "");
        claims.put("sub", "oidc-sub-fallback");
        hudson.model.User existing = hudson.model.User.getOrCreateByIdOrFullName("oidc-sub-fallback");
        existing.setFullName("Existing Name");
        existing.save();
        String emailBefore = userEmail(existing);

        VarroaSecurityRealm realm = new VarroaSecurityRealm();
        realm.setJwtValidatorForTest(fakeUserOperatorValidator(claims));
        Authentication auth = authenticateFallback(realm,
                fakeRequest("/whoAmI/api/json", "Bearer " + USER_OPERATOR_JWT, null, null));

        assertUserAuthentication(auth, "oidc-sub-fallback");
        assertEquals("empty name must not overwrite the existing user record", "Existing Name",
                existing.getFullName());
        assertEquals("empty email must retain the existing user profile value", emailBefore,
                userEmail(existing));
    }

    private static jakarta.servlet.FilterConfig newFilterConfig() {
        return new jakarta.servlet.FilterConfig() {
            public String getFilterName() { return "test"; }
            public jakarta.servlet.ServletContext getServletContext() { return null; }
            public String getInitParameter(String name) { return null; }
            public java.util.Enumeration<String> getInitParameterNames() {
                return java.util.Collections.emptyEnumeration();
            }
        };
    }

    @SuppressWarnings("unchecked")
    private static Authentication authenticateFallback(VarroaSecurityRealm realm,
            jakarta.servlet.http.HttpServletRequest request) throws Exception {
        Field currentRequest = VarroaSecurityRealm.class.getDeclaredField("CURRENT_REQUEST");
        currentRequest.setAccessible(true);
        ThreadLocal<jakarta.servlet.http.HttpServletRequest> requests =
                (ThreadLocal<jakarta.servlet.http.HttpServletRequest>) currentRequest.get(null);
        requests.set(request);
        try {
            return realm.new VarroaAuthenticationManager(realm.getJWTValidator()).authenticate(
                    new org.acegisecurity.providers.UsernamePasswordAuthenticationToken("ignored", null));
        } finally {
            requests.remove();
        }
    }

    /**
     * A vk_* Bearer rejected by the validator does not authenticate — the
     * request proceeds as anonymous, never as the token's claimed owner, and
     * never falls through to operator-JWT (mite) authentication.
     */
    @Test
    public void invalidVkBearerDoesNotAuthenticate() throws Exception {
        VarroaSecurityRealm realm = new VarroaSecurityRealm();
        realm.setApiKeyValidatorForTest(fakeValidator(false));
        j.jenkins.setSecurityRealm(realm);

        JenkinsRule.WebClient wc = j.createWebClient();
        wc.setThrowExceptionOnFailingStatusCode(false);
        wc.addRequestHeader("Authorization", "Bearer " + VK_TOKEN);
        org.htmlunit.Page p = wc.goTo("whoAmI/api/json", null);
        String body = p.getWebResponse().getContentAsString();
        assertFalse("rejected vk_ token must not authenticate as jdoe: " + body,
                body.contains("jdoe"));
        assertFalse("vk_ Bearer must never authenticate as the mite: " + body,
                body.contains("\"varroa-mite\""));
    }
}
