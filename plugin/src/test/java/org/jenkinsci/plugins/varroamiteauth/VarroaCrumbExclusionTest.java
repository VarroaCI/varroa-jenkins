package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import jakarta.servlet.FilterChain;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;
import java.lang.reflect.InvocationHandler;
import java.lang.reflect.Method;
import java.lang.reflect.Proxy;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests that {@link VarroaCrumbExclusion} exempts a valid Bearer credential
 * from CSRF crumb validation <em>regardless of the request path</em> — in
 * particular for {@code POST /configuration-as-code/reload}, which the mite
 * uses with its operator JWT (tasks.md 1.3 / 1.4).
 *
 * <p>The exclusion's {@code process()} never inspects the request URI: the
 * decision is driven solely by the Authorization header and the realm's
 * validators. We assert that with a valid Bearer the request is exempt for
 * {@code /configuration-as-code/reload} exactly as for any other path, and that
 * without a valid Bearer it is never exempt.</p>
 *
 * <p>No Mockito on the plugin test classpath, so the request is a minimal JDK
 * dynamic proxy that answers only {@code getRequestURI} and {@code getHeader}.</p>
 */
public class VarroaCrumbExclusionTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private static final String VK_TOKEN =
            "vk_0123456789abc." + "A".repeat(43);

    /** A vk_ validator with a fixed identity, bypassing the verify endpoint. */
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
                return m;
            }

            @Override
            public boolean isEnabled() {
                return true;
            }
        };
    }

    private static final String USER_OPERATOR_JWT = "operator-user-jwt";

    private static JWTValidator fakeUserOperatorValidator() {
        return new JWTValidator(null) {
            @Override
            public Map<String, Object> validateOperatorToken(String token) {
                if (!USER_OPERATOR_JWT.equals(token)) {
                    return null;
                }
                Map<String, Object> claims = new HashMap<>();
                claims.put("varroa_typ", "user");
                claims.put("sub", "oidc-subject");
                claims.put("groups", List.of("dev"));
                return claims;
            }
        };
    }

    /** Minimal POST request answering getRequestURI / getHeader(Authorization). */
    private static HttpServletRequest requestFor(String uri, String authHeader) {
        InvocationHandler h = (proxy, method, args) -> {
            switch (method.getName()) {
                case "getRequestURI":
                    return uri;
                case "getMethod":
                    return "POST";
                case "getHeader":
                    return "Authorization".equals(args[0]) ? authHeader : null;
                case "toString":
                    return "StubRequest[" + uri + "]";
                case "hashCode":
                    return System.identityHashCode(proxy);
                case "equals":
                    return proxy == args[0];
                default:
                    Class<?> rt = method.getReturnType();
                    if (rt == boolean.class) {
                        return false;
                    }
                    if (rt.isPrimitive()) {
                        return 0;
                    }
                    return null;
            }
        };
        return (HttpServletRequest) Proxy.newProxyInstance(
                VarroaCrumbExclusionTest.class.getClassLoader(),
                new Class<?>[] {HttpServletRequest.class}, h);
    }

    /** Minimal HttpServletResponse (never touched by an exempt path). */
    private static HttpServletResponse stubResponse() {
        InvocationHandler h = (proxy, method, args) -> {
            Class<?> rt = method.getReturnType();
            if (rt == boolean.class) {
                return false;
            }
            if (rt.isPrimitive()) {
                return 0;
            }
            return null;
        };
        return (HttpServletResponse) Proxy.newProxyInstance(
                VarroaCrumbExclusionTest.class.getClassLoader(),
                new Class<?>[] {HttpServletResponse.class}, h);
    }

    /**
     * A valid Bearer is crumb-exempt for {@code /configuration-as-code/reload}
     * (the path the operator JWT POSTs to) and the filter chain is continued.
     */
    @Test
    public void validBearerIsExemptOnReloadPath() throws Exception {
        VarroaSecurityRealm realm = new VarroaSecurityRealm();
        realm.setApiKeyValidatorForTest(fakeValidator(true));
        j.jenkins.setSecurityRealm(realm);

        VarroaCrumbExclusion exclusion = new VarroaCrumbExclusion();
        AtomicInteger chained = new AtomicInteger();
        FilterChain chain = (rq, rs) -> chained.incrementAndGet();

        boolean exempt = exclusion.process(
                requestFor("/configuration-as-code/reload", "Bearer " + VK_TOKEN),
                stubResponse(), chain);
        assertTrue("valid Bearer must be crumb-exempt on /configuration-as-code/reload", exempt);
        assertTrue("the filter chain must be continued when exempt", chained.get() == 1);
    }

    /**
     * Path-independence: the exemption decision for a valid Bearer is identical
     * across {@code /configuration-as-code/reload} and an unrelated path. The
     * exclusion exempts all valid Bearer requests regardless of path.
     */
    @Test
    public void exemptionIsPathIndependent() throws Exception {
        VarroaSecurityRealm realm = new VarroaSecurityRealm();
        realm.setApiKeyValidatorForTest(fakeValidator(true));
        j.jenkins.setSecurityRealm(realm);

        VarroaCrumbExclusion exclusion = new VarroaCrumbExclusion();
        FilterChain noop = (rq, rs) -> { };

        boolean reloadExempt = exclusion.process(
                requestFor("/configuration-as-code/reload", "Bearer " + VK_TOKEN),
                stubResponse(), noop);
        boolean otherExempt = exclusion.process(
                requestFor("/some/other/api/json", "Bearer " + VK_TOKEN),
                stubResponse(), noop);

        assertTrue("reload path must be exempt", reloadExempt);
        assertTrue("unrelated path must be exempt identically", otherExempt);
    }

    /**
     * A missing/invalid Bearer is never exempt — on the reload path or any
     * other. (A rejected vk_ token must not be crumb-exempt.)
     */
    @Test
    public void invalidBearerIsNotExempt() throws Exception {
        VarroaSecurityRealm realm = new VarroaSecurityRealm();
        realm.setApiKeyValidatorForTest(fakeValidator(false));
        j.jenkins.setSecurityRealm(realm);

        VarroaCrumbExclusion exclusion = new VarroaCrumbExclusion();
        FilterChain noop = (rq, rs) -> { };

        assertFalse("rejected vk_ Bearer must not be exempt",
                exclusion.process(
                        requestFor("/configuration-as-code/reload", "Bearer " + VK_TOKEN),
                        stubResponse(), noop));

        assertFalse("missing Bearer must not be exempt",
                exclusion.process(
                        requestFor("/configuration-as-code/reload", null),
                        stubResponse(), noop));
    }

    @Test
    public void validUserOperatorBearerIsCrumbExempt() throws Exception {
        VarroaSecurityRealm realm = new VarroaSecurityRealm();
        realm.setJwtValidatorForTest(fakeUserOperatorValidator());
        j.jenkins.setSecurityRealm(realm);

        VarroaCrumbExclusion exclusion = new VarroaCrumbExclusion();
        AtomicInteger chained = new AtomicInteger();
        boolean exempt = exclusion.process(
                requestFor("/mcp-server/mcp", "Bearer " + USER_OPERATOR_JWT),
                stubResponse(), (rq, rs) -> chained.incrementAndGet());

        assertTrue("valid user operator Bearer must be crumb-exempt", exempt);
        assertTrue("the filter chain must continue for a user token", chained.get() == 1);
    }
}
