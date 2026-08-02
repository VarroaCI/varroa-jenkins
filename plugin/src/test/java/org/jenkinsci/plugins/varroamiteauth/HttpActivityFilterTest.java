package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import hudson.security.ACL;
import hudson.security.ACLContext;

import java.util.List;

import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.springframework.security.authentication.UsernamePasswordAuthenticationToken;
import org.springframework.security.core.authority.SimpleGrantedAuthority;

import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpSession;

import static org.mockito.Mockito.*;

/**
 * Tests for {@link HttpActivityFilter} — exclusion table (D1 rules 1-8).
 *
 * <p>Uses {@link JenkinsRule} for authentication context (rule 1) and
 * plain Mockito for request path/method/session stubbing.
 */
public class HttpActivityFilterTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Before
    public void setUp() {
        HttpActivityFilter.resetForTesting();
        HttpActivityFilter.compileIgnorePattern(); // reload from env
    }

    @After
    public void tearDown() {
        HttpActivityFilter.resetForTesting();
    }

    // ---- Rule 1: mite principal ----

    @Test
    public void mitePrincipalIsExcluded() {
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "varroa-mite", "", List.of()))) {
            HttpServletRequest req = mock(HttpServletRequest.class);
            when(req.getRequestURI()).thenReturn("/jenkins/");
            when(req.getContextPath()).thenReturn("");
            when(req.getMethod()).thenReturn("GET");

            assertTrue("varroa-mite principal should be excluded",
                    HttpActivityFilter.isExcluded(req));
        }
    }

    @Test
    public void miteSystemPrincipalIsExcluded() {
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "varroa-mite-system", "", List.of()))) {
            HttpServletRequest req = mock(HttpServletRequest.class);
            when(req.getRequestURI()).thenReturn("/jenkins/");
            when(req.getContextPath()).thenReturn("");
            when(req.getMethod()).thenReturn("GET");

            assertTrue("varroa-mite-system principal should be excluded",
                    HttpActivityFilter.isExcluded(req));
        }
    }

    @Test
    public void userPageViewIsActivity() {
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "alice", "", List.of(new SimpleGrantedAuthority("ROLE:varroa:developer"))))) {
            HttpServletRequest req = mock(HttpServletRequest.class);
            when(req.getRequestURI()).thenReturn("/jenkins/");
            when(req.getContextPath()).thenReturn("");
            when(req.getMethod()).thenReturn("GET");

            assertFalse("user page view should NOT be excluded",
                    HttpActivityFilter.isExcluded(req));
        }
    }

    // ---- Rule 2: /varroa-activity/ ----

    @Test
    public void varroaActivityPathIsExcluded() {
        HttpServletRequest req = mock(HttpServletRequest.class);
        when(req.getRequestURI()).thenReturn("/varroa-activity/events");
        when(req.getContextPath()).thenReturn("");
        when(req.getMethod()).thenReturn("GET");

        // No auth context — rule 1 doesn't apply, rule 2 should match.
        assertTrue("/varroa-activity/ path should be excluded",
                HttpActivityFilter.isExcluded(req));
    }

    // ---- Rule 3: static resources ----

    @Test
    public void staticPrefixIsExcluded() {
        String[] prefixes = {"/static/", "/adjuncts/", "/images/", "/css/", "/scripts/"};
        for (String prefix : prefixes) {
            HttpServletRequest req = mock(HttpServletRequest.class);
            when(req.getRequestURI()).thenReturn(prefix + "some/resource");
            when(req.getContextPath()).thenReturn("");
            when(req.getMethod()).thenReturn("GET");
            assertTrue(prefix + " prefix should be excluded",
                    HttpActivityFilter.isExcluded(req));
        }
    }

    // ---- Rule 4: /favicon.ico ----

    @Test
    public void faviconExactPathIsExcluded() {
        HttpServletRequest req = mock(HttpServletRequest.class);
        when(req.getRequestURI()).thenReturn("/favicon.ico");
        when(req.getContextPath()).thenReturn("");
        when(req.getMethod()).thenReturn("GET");

        assertTrue("/favicon.ico should be excluded",
                HttpActivityFilter.isExcluded(req));
    }

    @Test
    public void faviconSubPathIsNotExcluded() {
        HttpServletRequest req = mock(HttpServletRequest.class);
        when(req.getRequestURI()).thenReturn("/favicon.ico/sub");
        when(req.getContextPath()).thenReturn("");
        when(req.getMethod()).thenReturn("GET");

        assertFalse("/favicon.ico/sub should NOT be excluded (exact match only)",
                HttpActivityFilter.isExcluded(req));
    }

    // ---- Rule 5: /crumbIssuer/ ----

    @Test
    public void crumbIssuerPathIsExcluded() {
        HttpServletRequest req = mock(HttpServletRequest.class);
        when(req.getRequestURI()).thenReturn("/crumbIssuer/");
        when(req.getContextPath()).thenReturn("");
        when(req.getMethod()).thenReturn("GET");

        assertTrue("/crumbIssuer/ should be excluded",
                HttpActivityFilter.isExcluded(req));
    }

    // ---- Rule 6: /wsagents/ ----

    @Test
    public void wsagentsPathIsExcluded() {
        HttpServletRequest req = mock(HttpServletRequest.class);
        when(req.getRequestURI()).thenReturn("/wsagents/");
        when(req.getContextPath()).thenReturn("");
        when(req.getMethod()).thenReturn("GET");

        assertTrue("/wsagents/ should be excluded",
                HttpActivityFilter.isExcluded(req));
    }

    // ---- Rule 7: GET/HEAD /login without session ----

    @Test
    public void loginGetWithoutSessionIsExcluded() {
        HttpServletRequest req = mock(HttpServletRequest.class);
        when(req.getRequestURI()).thenReturn("/login");
        when(req.getContextPath()).thenReturn("");
        when(req.getMethod()).thenReturn("GET");
        when(req.getSession(false)).thenReturn(null);

        assertTrue("GET /login without session should be excluded (probe)",
                HttpActivityFilter.isExcluded(req));
    }

    @Test
    public void loginHeadWithoutSessionIsExcluded() {
        HttpServletRequest req = mock(HttpServletRequest.class);
        when(req.getRequestURI()).thenReturn("/login");
        when(req.getContextPath()).thenReturn("");
        when(req.getMethod()).thenReturn("HEAD");
        when(req.getSession(false)).thenReturn(null);

        assertTrue("HEAD /login without session should be excluded (probe)",
                HttpActivityFilter.isExcluded(req));
    }

    @Test
    public void loginPostIsNotExcludedByRule7() {
        HttpServletRequest req = mock(HttpServletRequest.class);
        when(req.getRequestURI()).thenReturn("/login");
        when(req.getContextPath()).thenReturn("");
        when(req.getMethod()).thenReturn("POST");
        // Sessionless POST /login is a login attempt — not probe traffic.
        when(req.getSession(false)).thenReturn(null);

        assertFalse("POST /login without session should NOT be excluded",
                HttpActivityFilter.isExcluded(req));
    }

    @Test
    public void loginGetWithSessionIsNotExcluded() {
        HttpSession session = mock(HttpSession.class);
        HttpServletRequest req = mock(HttpServletRequest.class);
        when(req.getRequestURI()).thenReturn("/login");
        when(req.getContextPath()).thenReturn("");
        when(req.getMethod()).thenReturn("GET");
        when(req.getSession(false)).thenReturn(session);

        assertFalse("GET /login with session should NOT be excluded",
                HttpActivityFilter.isExcluded(req));
    }

    // ---- Rule 8: ignore regex ----

    @Test
    public void pathMatchingIgnoreRegexIsExcluded() {
        HttpActivityFilter.setIgnorePatternForTest(java.util.regex.Pattern.compile("/health|/metrics"));

        try {
            HttpServletRequest req = mock(HttpServletRequest.class);
            when(req.getRequestURI()).thenReturn("/health");
            when(req.getContextPath()).thenReturn("");
            when(req.getMethod()).thenReturn("GET");

            assertTrue("path matching ignore regex should be excluded",
                    HttpActivityFilter.isExcluded(req));

            req = mock(HttpServletRequest.class);
            when(req.getRequestURI()).thenReturn("/metrics/foo");
            when(req.getContextPath()).thenReturn("");
            when(req.getMethod()).thenReturn("GET");

            assertTrue("path matching ignore regex should be excluded",
                    HttpActivityFilter.isExcluded(req));

            req = mock(HttpServletRequest.class);
            when(req.getRequestURI()).thenReturn("/job/my-job");
            when(req.getContextPath()).thenReturn("");
            when(req.getMethod()).thenReturn("GET");

            assertFalse("path NOT matching ignore regex should NOT be excluded",
                    HttpActivityFilter.isExcluded(req));
        } finally {
            HttpActivityFilter.setIgnorePatternForTest(null);
        }
    }

    @Test
    public void invalidRegexLogsWarningOnceAndTreatsAsUnset() throws Exception {
        // Install a handler to capture log output on the filter's logger.
        java.util.logging.Logger logger = java.util.logging.Logger.getLogger(
                HttpActivityFilter.class.getName());
        java.util.List<String> capturedRecords = new java.util.ArrayList<>();
        java.util.logging.Handler handler = new java.util.logging.Handler() {
            @Override public void publish(java.util.logging.LogRecord r) {
                capturedRecords.add(r.getLevel() + ": " + r.getMessage());
            }
            @Override public void flush() {}
            @Override public void close() {}
        };
        logger.addHandler(handler);
        try {
            HttpActivityFilter.resetForTesting();

            // First call with invalid regex — should log one WARNING.
            HttpActivityFilter.compileIgnorePatternFromString("[invalid");
            assertTrue("first invalid regex should log a WARNING",
                    capturedRecords.stream().anyMatch(r -> r.startsWith("WARNING:")));
            int warningCount = (int) capturedRecords.stream()
                    .filter(r -> r.startsWith("WARNING:")).count();
            assertEquals("should log exactly one WARNING on first invalid regex", 1, warningCount);

            // Second call with a different invalid regex — should NOT log again.
            capturedRecords.clear();
            HttpActivityFilter.compileIgnorePatternFromString("(unclosed");
            assertEquals("second invalid regex should NOT log again", 0, capturedRecords.size());

            // After invalid regex, exclusion should not break and pattern should be null.
            // Since compileIgnorePatternFromString sets ignorePattern = null on error,
            // rule 8 should not exclude anything.
            HttpServletRequest req = mock(HttpServletRequest.class);
            when(req.getRequestURI()).thenReturn("/any/path");
            when(req.getContextPath()).thenReturn("");
            when(req.getMethod()).thenReturn("GET");

            assertFalse("invalid regex should not affect exclusion",
                    HttpActivityFilter.isExcluded(req));

            // Verify the filter never breaks the chain even with invalid state.
            HttpActivityFilter filter = new HttpActivityFilter();
            req = mock(HttpServletRequest.class);
            when(req.getRequestURI()).thenThrow(new RuntimeException("simulated"));
            jakarta.servlet.ServletResponse resp = mock(jakarta.servlet.ServletResponse.class);
            jakarta.servlet.FilterChain chain = mock(jakarta.servlet.FilterChain.class);
            filter.doFilter(req, resp, chain);
            verify(chain).doFilter(req, resp);
        } finally {
            logger.removeHandler(handler);
        }
    }

    // ---- Filter never breaks the chain ----

    @Test
    public void filterNeverThrowsPastDoFilter() throws Exception {
        HttpActivityFilter filter = new HttpActivityFilter();
        HttpServletRequest req = mock(HttpServletRequest.class);
        // Simulate an exception-inducing scenario: null request URI
        when(req.getRequestURI()).thenThrow(new RuntimeException("simulated"));

        jakarta.servlet.ServletResponse resp = mock(jakarta.servlet.ServletResponse.class);
        jakarta.servlet.FilterChain chain = mock(jakarta.servlet.FilterChain.class);

        // Must not throw — the exception is caught inside doFilter.
        filter.doFilter(req, resp, chain);

        // chain.doFilter must still have been called.
        verify(chain).doFilter(req, resp);
    }

    @Test
    public void nonHttpRequestPassesThrough() throws Exception {
        HttpActivityFilter filter = new HttpActivityFilter();
        jakarta.servlet.ServletRequest req = mock(jakarta.servlet.ServletRequest.class);
        jakarta.servlet.ServletResponse resp = mock(jakarta.servlet.ServletResponse.class);
        jakarta.servlet.FilterChain chain = mock(jakarta.servlet.FilterChain.class);

        filter.doFilter(req, resp, chain);
        verify(chain).doFilter(req, resp);
    }

    // ---- Conventions: mite poll not activity ----

    @Test
    public void mitePollNotActivity() {
        // The mite drain endpoint: /varroa-activity/events.
        // This should be excluded by rule 2 regardless of who calls it.
        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "varroa-mite", "", List.of()))) {
            HttpServletRequest req = mock(HttpServletRequest.class);
            when(req.getRequestURI()).thenReturn("/varroa-activity/events");
            when(req.getContextPath()).thenReturn("");
            when(req.getMethod()).thenReturn("GET");

            assertTrue("mite drain poll should be excluded",
                    HttpActivityFilter.isExcluded(req));
        }
    }

    @Test
    public void userPageViewRecordsActivity() throws Exception {
        HttpActivityFilter.resetForTesting();
        assertEquals(0, HttpActivityFilter.getLastHttpActivityUnixMillis());

        try (ACLContext ctx = ACL.as2(new UsernamePasswordAuthenticationToken(
                "alice", "", List.of()))) {
            HttpServletRequest req = mock(HttpServletRequest.class);
            when(req.getRequestURI()).thenReturn("/job/my-job/");
            when(req.getContextPath()).thenReturn("");
            when(req.getMethod()).thenReturn("GET");

            // Run through doFilter, not just isExcluded, to verify the timestamp is set.
            HttpActivityFilter filter = new HttpActivityFilter();
            filter.doFilter(req, mock(jakarta.servlet.ServletResponse.class),
                    mock(jakarta.servlet.FilterChain.class));

            assertTrue("lastHttpActivityUnixMillis should be > 0 after doFilter on non-excluded request",
                    HttpActivityFilter.getLastHttpActivityUnixMillis() > 0);
        }
    }
}
