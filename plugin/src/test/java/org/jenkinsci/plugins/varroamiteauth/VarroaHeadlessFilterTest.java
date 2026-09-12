package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;

import com.michelin.cio.hudson.plugins.rolestrategy.Role;
import com.michelin.cio.hudson.plugins.rolestrategy.RoleBasedAuthorizationStrategy;
import com.michelin.cio.hudson.plugins.rolestrategy.RoleMap;

import hudson.security.ACL;
import hudson.security.ACLContext;

import jakarta.servlet.FilterChain;
import jakarta.servlet.ServletRequest;
import jakarta.servlet.ServletResponse;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;

import jenkins.model.Jenkins;

import java.io.PrintWriter;
import java.io.StringWriter;
import java.lang.reflect.InvocationHandler;
import java.lang.reflect.Proxy;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeMap;
import java.util.regex.Pattern;

import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.springframework.security.authentication.UsernamePasswordAuthenticationToken;
import org.springframework.security.core.Authentication;
import org.springframework.security.core.authority.SimpleGrantedAuthority;

/**
 * {@code JenkinsRule} tests for {@link VarroaHeadlessFilter} (D2, tasks 4.1
 * to 4.6): the seven-row decision table, the allowlist, the denial bodies,
 * and the invariant that the filter never grants a Jenkins permission.
 */
public class VarroaHeadlessFilterTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Before
    public void setUp() {
        VarroaHeadlessFilter.setHeadlessForTest(true);
        VarroaHeadlessFilter.resetRealmWarningForTest();
        VarroaBreakGlassAudit.resetForTesting();
        ActivitySink.get().resetForTesting();
    }

    @After
    public void tearDown() {
        VarroaHeadlessFilter.resetHeadlessForTest();
        VarroaBreakGlassAudit.resetForTesting();
        ActivitySink.get().resetForTesting();
    }

    // ---- Fixtures ----

    private static Authentication authFor(String id, String... authorities) {
        List<SimpleGrantedAuthority> granted = new ArrayList<>();
        for (String a : authorities) {
            granted.add(new SimpleGrantedAuthority(a));
        }
        return new UsernamePasswordAuthenticationToken(id, "", granted);
    }

    /** Grants {@code Jenkins.ADMINISTER} to exactly one user id. */
    private void grantAdminTo(String userId) {
        Role adminRole = new Role("varroa:admin", Pattern.compile(".*"),
                Set.of(Jenkins.ADMINISTER), "admin");
        RoleMap roleMap = new RoleMap(new TreeMap<>());
        roleMap.addRole(adminRole);
        roleMap.assignRole(adminRole, userId);
        j.jenkins.setAuthorizationStrategy(new RoleBasedAuthorizationStrategy(
                Map.of(RoleBasedAuthorizationStrategy.GLOBAL, roleMap)));
    }

    /** Grants only {@code Jenkins.SYSTEM_READ} (never ADMINISTER) to one user id. */
    private void grantSystemReadOnlyTo(String userId) {
        Role readRole = new Role("varroa:viewer", Pattern.compile(".*"),
                Set.of(Jenkins.SYSTEM_READ), "reader");
        RoleMap roleMap = new RoleMap(new TreeMap<>());
        roleMap.addRole(readRole);
        roleMap.assignRole(readRole, userId);
        j.jenkins.setAuthorizationStrategy(new RoleBasedAuthorizationStrategy(
                Map.of(RoleBasedAuthorizationStrategy.GLOBAL, roleMap)));
    }

    private void useVarroaRealm() {
        j.jenkins.setSecurityRealm(new VarroaSecurityRealm());
    }

    /**
     * Installs a role-strategy with no roles at all, so no principal holds
     * any Jenkins permission. JenkinsRule's own default strategy grants full
     * control to any logged-in user, which would let every "non-admin" test
     * pass for the wrong reason.
     */
    private void denyAllByDefault() {
        j.jenkins.setAuthorizationStrategy(new RoleBasedAuthorizationStrategy(
                Map.of(RoleBasedAuthorizationStrategy.GLOBAL, new RoleMap(new TreeMap<>()))));
    }

    @SuppressWarnings("unchecked")
    private static HttpServletRequest fakeRequest(String uri, String accept) {
        InvocationHandler h = (proxy, method, args) -> {
            switch (method.getName()) {
                case "getRequestURI": return uri;
                case "getContextPath": return "";
                case "getHeader":
                    String name = (String) args[0];
                    return "Accept".equalsIgnoreCase(name) ? accept : null;
                default:
                    Class<?> rt = method.getReturnType();
                    if (rt == boolean.class) return false;
                    if (rt == int.class) return 0;
                    return null;
            }
        };
        return (HttpServletRequest) Proxy.newProxyInstance(
                VarroaHeadlessFilterTest.class.getClassLoader(),
                new Class<?>[]{HttpServletRequest.class}, h);
    }

    /** A response double recording the status, the content type, and the written body. */
    private static final class RecordingResponse {
        int status = -1;
        String contentType;
        final StringWriter body = new StringWriter();
        final PrintWriter writer = new PrintWriter(body);

        @SuppressWarnings("unchecked")
        HttpServletResponse asProxy() {
            InvocationHandler h = (proxy, method, args) -> {
                switch (method.getName()) {
                    case "setStatus": status = (int) args[0]; return null;
                    case "getStatus": return status;
                    case "setContentType": contentType = (String) args[0]; return null;
                    case "getContentType": return contentType;
                    case "getWriter": return writer;
                    default:
                        Class<?> rt = method.getReturnType();
                        if (rt == boolean.class) return false;
                        if (rt == int.class) return 0;
                        return null;
                }
            };
            return (HttpServletResponse) Proxy.newProxyInstance(
                    VarroaHeadlessFilterTest.class.getClassLoader(),
                    new Class<?>[]{HttpServletResponse.class}, h);
        }
    }

    private static final class RecordingChain implements FilterChain {
        boolean called;

        @Override
        public void doFilter(ServletRequest request, ServletResponse response) {
            called = true;
        }
    }

    // ---- Row 1: not headless ----

    @Test
    public void row1PassesEveryRequestWhenNotHeadless() throws Exception {
        VarroaHeadlessFilter.setHeadlessForTest(false);
        VarroaHeadlessFilter filter = new VarroaHeadlessFilter();
        RecordingChain chain = new RecordingChain();

        filter.doFilter(fakeRequest("/job/team-a/", null), new RecordingResponse().asProxy(), chain);

        assertTrue("a non-headless controller passes every request", chain.called);
    }

    // ---- Row 2: readiness ----

    @Test
    public void row2InstanceAbsentAppliesTheAllowlistAlone_allowlistedPasses() throws Exception {
        RecordingChain chain = new RecordingChain();
        RecordingResponse rsp = new RecordingResponse();

        VarroaHeadlessFilter.handle(fakeRequest("/login", null), rsp.asProxy(), chain, null);

        assertTrue("an allowlisted path passes while Jenkins is absent", chain.called);
        assertEquals(-1, rsp.status);
    }

    @Test
    public void row2InstanceAbsentAppliesTheAllowlistAlone_othersAnswer503() throws Exception {
        RecordingChain chain = new RecordingChain();
        RecordingResponse rsp = new RecordingResponse();

        VarroaHeadlessFilter.handle(fakeRequest("/job/team-a/", null), rsp.asProxy(), chain, null);

        assertFalse(chain.called);
        assertEquals(503, rsp.status);
        assertTrue(rsp.body.toString().contains("\"error\":\"starting\""));
    }

    @Test
    public void row2ReadyCompletedInstanceIsNotStarting() {
        assertFalse("a booted JenkinsRule instance is COMPLETED",
                VarroaHeadlessFilter.isNotReady(j.jenkins));
        assertTrue("a null instance is always not ready", VarroaHeadlessFilter.isNotReady(null));
    }

    // ---- Row 3: realm ----

    @Test
    public void row3NonVarroaRealmDeniesEvenAnAllowlistedPath() throws Exception {
        // JenkinsRule's default realm is not VarroaSecurityRealm.
        RecordingChain chain = new RecordingChain();
        RecordingResponse rsp = new RecordingResponse();

        VarroaHeadlessFilter.handle(fakeRequest("/tcpSlaveAgentListener/", null), rsp.asProxy(), chain, j.jenkins);

        assertFalse("the realm gate fires before the allowlist", chain.called);
        assertEquals(403, rsp.status);
    }

    @Test
    public void row3VarroaRealmLetsAnAllowlistedPathThrough() throws Exception {
        useVarroaRealm();
        RecordingChain chain = new RecordingChain();
        RecordingResponse rsp = new RecordingResponse();

        VarroaHeadlessFilter.handle(fakeRequest("/tcpSlaveAgentListener/", null), rsp.asProxy(), chain, j.jenkins);

        assertTrue("the same path passes once the realm is Varroa", chain.called);
    }

    // ---- Row 4: allowlist families ----

    private void assertAllowlisted(String rowLabel, String path) {
        assertTrue(rowLabel + " (" + path + ") must be allowlisted", VarroaHeadlessFilter.isAllowlisted(path));
    }

    @Test
    public void allowlistTableRows() {
        assertAllowlisted("login", "/login");
        assertAllowlisted("logout", "/logout");
        assertAllowlisted("form login post", "/j_spring_security_check");
        assertAllowlisted("crumbIssuer", "/crumbIssuer/api/json");
        assertAllowlisted("instance-identity", "/instance-identity/");
        assertAllowlisted("tcpSlaveAgentListener", "/tcpSlaveAgentListener/");
        assertAllowlisted("wsagents", "/wsagents/");
        assertAllowlisted("jnlpJars", "/jnlpJars/agent.jar");
        assertAllowlisted("computer slave-agent.jnlp", "/computer/agent-1/slave-agent.jnlp");
        assertAllowlisted("computer jenkins-agent.jnlp", "/computer/agent-1/jenkins-agent.jnlp");
        assertAllowlisted("github-webhook", "/github-webhook/");
        assertAllowlisted("generic-webhook-trigger", "/generic-webhook-trigger/invoke");
        assertAllowlisted("varroa-activity", "/varroa-activity/events");
        assertAllowlisted("varroa-run", "/varroa-run/trigger");
        assertAllowlisted("prometheus", "/prometheus/metrics");
        assertAllowlisted("static", "/static/abc123/images/x.png");
        assertAllowlisted("adjuncts", "/adjuncts/abc/x.js");
        assertAllowlisted("images", "/images/16x16/error.png");
        assertAllowlisted("css", "/css/style.css");
        assertAllowlisted("scripts", "/scripts/hudson-behavior.js");
        assertAllowlisted("userContent", "/userContent/logo.png");
        assertAllowlisted("favicon", "/favicon.ico");
        assertAllowlisted("robots", "/robots.txt");
    }

    @Test
    public void computerPagesAreNotAllowlistedAndAreDenied() throws Exception {
        assertFalse(VarroaHeadlessFilter.isAllowlisted("/computer/agent-1/"));

        useVarroaRealm();
        denyAllByDefault();
        RecordingChain chain = new RecordingChain();
        RecordingResponse rsp = new RecordingResponse();
        try (ACLContext ignored = ACL.as2(authFor("bob", "authenticated"))) {
            VarroaHeadlessFilter.handle(fakeRequest("/computer/agent-1/", null), rsp.asProxy(), chain, j.jenkins);
        }

        assertFalse(chain.called);
        assertEquals(403, rsp.status);
    }

    @Test
    public void jcasExportIsDeniedForACallerHoldingOnlySystemRead() throws Exception {
        useVarroaRealm();
        grantSystemReadOnlyTo("bob");
        RecordingChain chain = new RecordingChain();
        RecordingResponse rsp = new RecordingResponse();

        try (ACLContext ignored = ACL.as2(authFor("bob", "authenticated"))) {
            VarroaHeadlessFilter.handle(
                    fakeRequest("/configuration-as-code/export", null), rsp.asProxy(), chain, j.jenkins);
        }

        assertFalse("SystemRead alone is not ADMINISTER", chain.called);
        assertEquals(403, rsp.status);
    }

    // ---- Row 5: system identity, matched exactly ----

    @Test
    public void row5MitePasses() throws Exception {
        useVarroaRealm();
        denyAllByDefault();
        RecordingChain chain = new RecordingChain();
        RecordingResponse rsp = new RecordingResponse();

        try (ACLContext ignored = ACL.as2(authFor("varroa-mite", "authenticated", "ROLE:varroa:system-mite"))) {
            VarroaHeadlessFilter.handle(fakeRequest("/api/json", null), rsp.asProxy(), chain, j.jenkins);
        }

        assertTrue("the mite must pass", chain.called);
    }

    @Test
    public void row5SystemOperatorPasses_executeGroovyKeepsWorking() throws Exception {
        useVarroaRealm();
        denyAllByDefault();
        RecordingChain chain = new RecordingChain();
        RecordingResponse rsp = new RecordingResponse();

        try (ACLContext ignored = ACL.as2(
                authFor("varroa-operator", "authenticated", "ROLE:varroa:system-operator"))) {
            VarroaHeadlessFilter.handle(fakeRequest("/scriptText", null), rsp.asProxy(), chain, j.jenkins);
        }

        assertTrue("the operator's executeGroovy call must pass", chain.called);
    }

    @Test
    public void row5ForgedGroupAuthorityDoesNotBypassTheFilter() throws Exception {
        useVarroaRealm();
        denyAllByDefault();
        RecordingChain chain = new RecordingChain();
        RecordingResponse rsp = new RecordingResponse();

        try (ACLContext ignored = ACL.as2(
                authFor("evil", "authenticated", "ROLE:varroa:system-forged"))) {
            VarroaHeadlessFilter.handle(fakeRequest("/job/team-a/", null), rsp.asProxy(), chain, j.jenkins);
        }

        assertFalse("a look-alike group authority must not match exactly", chain.called);
        assertEquals(403, rsp.status);
    }

    // ---- Row 6: admin ----

    @Test
    public void row6AdminPasses() throws Exception {
        useVarroaRealm();
        grantAdminTo("alice");
        RecordingChain chain = new RecordingChain();
        RecordingResponse rsp = new RecordingResponse();

        try (ACLContext ignored = ACL.as2(authFor("alice", "authenticated"))) {
            VarroaHeadlessFilter.handle(fakeRequest("/job/team-a/", null), rsp.asProxy(), chain, j.jenkins);
        }

        assertTrue("an admin reaches the UI", chain.called);

        List<ActivityEvent> events = ActivitySink.get().drain(1024).events;
        int sessionEvents = 0;
        for (ActivityEvent e : events) {
            if ("controller.breakglass.session".equals(e.getType())) {
                sessionEvents++;
            }
        }
        assertEquals("the admin pass must audit exactly one session event", 1, sessionEvents);
    }

    // ---- Row 7: fallback denial ----

    @Test
    public void row7NonAdminNonSystemIsDenied() throws Exception {
        useVarroaRealm();
        grantAdminTo("alice");
        RecordingChain chain = new RecordingChain();
        RecordingResponse rsp = new RecordingResponse();

        try (ACLContext ignored = ACL.as2(authFor("bob", "authenticated"))) {
            VarroaHeadlessFilter.handle(fakeRequest("/job/team-a/", null), rsp.asProxy(), chain, j.jenkins);
        }

        assertFalse(chain.called);
        assertEquals(403, rsp.status);
    }

    // ---- Denial response formats (task 4.5) ----

    @Test
    public void startingDenialAnswersHtmlWhenAccepted() throws Exception {
        RecordingResponse rsp = new RecordingResponse();
        VarroaHeadlessFilter.handle(
                fakeRequest("/job/team-a/", "text/html,*/*"), rsp.asProxy(), new RecordingChain(), null);

        assertEquals(503, rsp.status);
        assertTrue(rsp.contentType.contains("text/html"));
        assertTrue(rsp.body.toString().contains("starting"));
    }

    @Test
    public void startingDenialAnswersJsonOtherwise() throws Exception {
        RecordingResponse rsp = new RecordingResponse();
        VarroaHeadlessFilter.handle(
                fakeRequest("/job/team-a/", "application/json"), rsp.asProxy(), new RecordingChain(), null);

        assertEquals(503, rsp.status);
        assertTrue(rsp.contentType.contains("application/json"));
        assertEquals("{\"error\":\"starting\"}", rsp.body.toString());
    }

    @Test
    public void forbiddenDenialAnswersHtmlWhenAccepted() throws Exception {
        useVarroaRealm();
        denyAllByDefault();
        RecordingResponse rsp = new RecordingResponse();
        try (ACLContext ignored = ACL.as2(authFor("bob", "authenticated"))) {
            VarroaHeadlessFilter.handle(
                    fakeRequest("/job/team-a/", "text/html,*/*"), rsp.asProxy(), new RecordingChain(), j.jenkins);
        }

        assertEquals(403, rsp.status);
        assertTrue(rsp.contentType.contains("text/html"));
        assertTrue(rsp.body.toString().contains("hudson.model.Hudson.Administer"));
    }

    @Test
    public void forbiddenDenialAnswersJsonNamingThePermission() throws Exception {
        useVarroaRealm();
        denyAllByDefault();
        RecordingResponse rsp = new RecordingResponse();
        try (ACLContext ignored = ACL.as2(authFor("bob", "authenticated"))) {
            VarroaHeadlessFilter.handle(
                    fakeRequest("/job/team-a/", "application/json"), rsp.asProxy(), new RecordingChain(), j.jenkins);
        }

        assertEquals(403, rsp.status);
        assertTrue(rsp.contentType.contains("application/json"));
        String body = rsp.body.toString();
        assertTrue(body.contains("\"error\":\"headless\""));
        assertTrue(body.contains("\"permission\":\"hudson.model.Hudson.Administer\""));
    }

    @Test
    public void forbiddenDenialOmitsPipelinesWhenTheBannerVariableIsUnset() throws Exception {
        // VARROA_BANNER_URL is not set in this test process.
        assertNull(VarroaHeadlessFilter.pipelinesUrlFromBannerEnv());

        useVarroaRealm();
        denyAllByDefault();
        RecordingResponse rsp = new RecordingResponse();
        try (ACLContext ignored = ACL.as2(authFor("bob", "authenticated"))) {
            VarroaHeadlessFilter.handle(
                    fakeRequest("/job/team-a/", "application/json"), rsp.asProxy(), new RecordingChain(), j.jenkins);
        }

        assertEquals(403, rsp.status);
        assertFalse(rsp.body.toString().contains("pipelines"));
    }

    @Test
    public void backParamIsExtractedFromTheBannerUrl() {
        String bannerUrl = "https://dash.example?controller=ci-1&namespace=team-a"
                + "&back=https://dash.example/controllers/core/team-a/ci-1";
        assertEquals("https://dash.example/controllers/core/team-a/ci-1",
                VarroaHeadlessFilter.extractBackParam(bannerUrl));
        assertNull(VarroaHeadlessFilter.extractBackParam(null));
        assertNull(VarroaHeadlessFilter.extractBackParam(""));
        assertNull(VarroaHeadlessFilter.extractBackParam("https://dash.example?controller=ci-1"));
    }

    @Test
    public void denialBodiesLeakNoItemOrUserName() throws Exception {
        j.createFreeStyleProject("super-secret-job");
        useVarroaRealm();
        denyAllByDefault();
        RecordingResponse htmlRsp = new RecordingResponse();
        RecordingResponse jsonRsp = new RecordingResponse();

        try (ACLContext ignored = ACL.as2(authFor("victim-user", "authenticated"))) {
            VarroaHeadlessFilter.handle(fakeRequest("/job/super-secret-job/", "text/html"),
                    htmlRsp.asProxy(), new RecordingChain(), j.jenkins);
            VarroaHeadlessFilter.handle(fakeRequest("/job/super-secret-job/", "application/json"),
                    jsonRsp.asProxy(), new RecordingChain(), j.jenkins);
        }

        for (RecordingResponse rsp : List.of(htmlRsp, jsonRsp)) {
            assertEquals(403, rsp.status);
            String body = rsp.body.toString();
            assertFalse("must not name the item", body.contains("super-secret-job"));
            assertFalse("must not name the user", body.contains("victim-user"));
        }
    }

    // ---- The invariant (task 4.6) ----

    /**
     * An allowlisted path still obeys whatever access check sits behind the
     * filter: {@code /varroa-activity/} passes the headless gate, but
     * {@link ActivityEndpoint}'s own {@code ROLE:varroa:system-mite} check
     * still denies an anonymous caller. The filter therefore grants nothing.
     */
    @Test
    public void allowlistedPathStillObeysItsOwnAccessCheck() throws Exception {
        useVarroaRealm();

        JenkinsRule.WebClient wc = j.createWebClient();
        wc.setThrowExceptionOnFailingStatusCode(false);
        org.htmlunit.Page p = wc.goTo("varroa-activity/events", null);
        org.junit.Assume.assumeTrue("harness web stack unavailable (503)",
                p.getWebResponse().getStatusCode() != 503);

        assertEquals(403, p.getWebResponse().getStatusCode());
        assertTrue("the denial must be ActivityEndpoint's own, not the filter's",
                p.getWebResponse().getContentAsString().contains("ROLE:varroa:system-mite"));
    }

    /**
     * The registered filter, reached through the real servlet chain (not
     * {@code handle} called directly): an anonymous GET to {@code /} is
     * denied, and an anonymous GET to {@code /login} still passes.
     */
    @Test
    public void productionServletChainDeniesRootAndPassesLogin() throws Exception {
        useVarroaRealm();
        denyAllByDefault();

        JenkinsRule.WebClient wc = j.createWebClient();
        wc.setThrowExceptionOnFailingStatusCode(false);

        org.htmlunit.Page root = wc.goTo("", null);
        int rootStatus = root.getWebResponse().getStatusCode();
        assertTrue("anonymous GET / must be denied by the production servlet chain, got "
                        + rootStatus,
                rootStatus == 403 || rootStatus == 503);

        org.htmlunit.Page login = wc.goTo("login", null);
        assertEquals(200, login.getWebResponse().getStatusCode());
    }
}
