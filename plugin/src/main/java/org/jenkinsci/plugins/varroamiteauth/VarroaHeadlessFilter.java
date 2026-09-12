package org.jenkinsci.plugins.varroamiteauth;

import hudson.init.InitMilestone;
import hudson.init.Initializer;
import hudson.util.PluginServletFilter;

import jakarta.servlet.Filter;
import jakarta.servlet.FilterChain;
import jakarta.servlet.FilterConfig;
import jakarta.servlet.ServletException;
import jakarta.servlet.ServletRequest;
import jakarta.servlet.ServletResponse;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;

import jenkins.model.Jenkins;

import java.io.IOException;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.logging.Level;
import java.util.logging.Logger;

import net.sf.json.JSONObject;
import org.springframework.security.core.GrantedAuthority;

/**
 * Servlet filter that blocks the Jenkins web UI on a headless controller for
 * every caller except the two Varroa system identities and an admin (D2).
 *
 * <p>Registered via {@link PluginServletFilter#addFilter} from an
 * {@link Initializer}, following {@link HttpActivityFilter}. Reads {@code
 * VARROA_HEADLESS} once, into a volatile flag. A value change needs a pod
 * restart, which the operator performs.
 *
 * <p>The filter only denies. It never grants a Jenkins permission and never
 * authenticates a request. The active authorization strategy, and any
 * endpoint's own access check, still guards every allowlisted path.
 *
 * <h3>Decision table (first match wins)</h3>
 * <ol>
 *   <li>{@code VARROA_HEADLESS} is not the exact string {@code true} — pass
 *   <li>Jenkins is absent, or its init level is before {@code COMPLETED} —
 *       apply the allowlist alone; otherwise answer {@code 503}
 *   <li>the active security realm is not a {@link VarroaSecurityRealm} —
 *       answer {@code 403}, and log one warning for the JVM
 *   <li>the path is on the infrastructure allowlist — pass
 *   <li>the caller holds {@code ROLE:varroa:system-mite} or {@code
 *       ROLE:varroa:system-operator}, matched exactly — pass
 *   <li>the caller holds {@code Jenkins.ADMINISTER} — pass, and the session
 *       is audited
 *   <li>otherwise — answer {@code 403}
 * </ol>
 *
 * <p>An exception raised while evaluating tests 2 to 6 counts as a denial
 * for that test, never as a pass.
 */
public class VarroaHeadlessFilter implements Filter {

    private static final Logger LOGGER = Logger.getLogger(VarroaHeadlessFilter.class.getName());

    /** Environment variable the operator stamps on a headless controller's pod. */
    static final String HEADLESS_ENV = "VARROA_HEADLESS";

    /** Environment variable carrying the dashboard banner URL and its {@code back} link. */
    static final String BANNER_ENV = "VARROA_BANNER_URL";

    /** The Jenkins permission a denial body names. */
    static final String ADMINISTER_PERMISSION_ID = "hudson.model.Hudson.Administer";

    private static final String MITE_AUTHORITY = "ROLE:varroa:system-mite";
    private static final String OPERATOR_AUTHORITY = "ROLE:varroa:system-operator";

    /** Prefix-matched allowlist entries (D2). */
    private static final String[] PREFIX_ALLOWLIST = {
        "/login",
        "/logout",
        "/crumbIssuer/",
        "/instance-identity/",
        "/tcpSlaveAgentListener/",
        "/wsagents/",
        "/jnlpJars/",
        "/github-webhook/",
        "/varroa-activity/",
        "/varroa-run",
        "/prometheus/",
        "/static/",
        "/adjuncts/",
        "/images/",
        "/css/",
        "/scripts/",
        "/userContent/",
    };

    /** Exact-matched allowlist entries (D2). */
    private static final String[] EXACT_ALLOWLIST = {
        "/j_spring_security_check",
        "/generic-webhook-trigger/invoke",
        "/favicon.ico",
        "/robots.txt",
    };

    private static final String COMPUTER_PREFIX = "/computer/";
    private static final String[] COMPUTER_SUFFIXES = {
        "/slave-agent.jnlp",
        "/jenkins-agent.jnlp",
    };

    private static volatile boolean headlessEnabled = readHeadlessEnv();

    private static final AtomicBoolean REALM_WARNING_LOGGED = new AtomicBoolean();

    private static boolean readHeadlessEnv() {
        return "true".equals(System.getenv(HEADLESS_ENV));
    }

    @Initializer
    public static void registerFilter() throws ServletException {
        PluginServletFilter.addFilter(new VarroaHeadlessFilter());
    }

    @Override
    public void init(FilterConfig filterConfig) {
        // Nothing to init. The static field already holds the environment value.
    }

    @Override
    public void doFilter(ServletRequest request, ServletResponse response, FilterChain chain)
            throws IOException, ServletException {
        if (!headlessEnabled) {
            chain.doFilter(request, response);
            return;
        }
        if (!(request instanceof HttpServletRequest) || !(response instanceof HttpServletResponse)) {
            chain.doFilter(request, response);
            return;
        }
        handle((HttpServletRequest) request, (HttpServletResponse) response, chain, safeInstance());
    }

    @Override
    public void destroy() {
        // No resources to release.
    }

    /**
     * Applies tests 2 to 7 against an explicit {@link Jenkins} reference.
     * Package-private so a test can pass {@code null} for the "instance
     * absent" branch of test 2 without controlling the real singleton.
     */
    static void handle(HttpServletRequest req, HttpServletResponse rsp, FilterChain chain, Jenkins jenkins)
            throws IOException, ServletException {
        if (isNotReady(jenkins)) {
            String path = getPath(req);
            if (safeAllowlisted(path)) {
                chain.doFilter(req, rsp);
            } else {
                denyStarting(req, rsp);
            }
            return;
        }

        if (!safeRealmIsVarroa(jenkins)) {
            if (REALM_WARNING_LOGGED.compareAndSet(false, true)) {
                LOGGER.log(Level.WARNING,
                        "VarroaHeadlessFilter: the active security realm is not VarroaSecurityRealm;"
                                + " every headless request is denied until it is");
            }
            denyForbidden(req, rsp);
            return;
        }

        String path = getPath(req);
        if (safeAllowlisted(path)) {
            chain.doFilter(req, rsp);
            return;
        }

        if (safeHasSystemAuthority()) {
            chain.doFilter(req, rsp);
            return;
        }

        if (safeHasAdminister(jenkins)) {
            VarroaBreakGlassAudit.record(safeActorName(), path);
            chain.doFilter(req, rsp);
            return;
        }

        denyForbidden(req, rsp);
    }

    // ---- Test 2: readiness ----

    static Jenkins safeInstance() {
        try {
            return Jenkins.getInstanceOrNull();
        } catch (Exception e) {
            return null;
        }
    }

    /** True when Jenkins is absent or has not reached {@code InitMilestone.COMPLETED}. */
    static boolean isNotReady(Jenkins jenkins) {
        if (jenkins == null) {
            return true;
        }
        try {
            return jenkins.getInitLevel() != InitMilestone.COMPLETED;
        } catch (Exception e) {
            return true;
        }
    }

    // ---- Test 3: realm ----

    static boolean safeRealmIsVarroa(Jenkins jenkins) {
        try {
            return jenkins.getSecurityRealm() instanceof VarroaSecurityRealm;
        } catch (Exception e) {
            return false;
        }
    }

    // ---- Test 4: allowlist ----

    static boolean safeAllowlisted(String path) {
        try {
            return isAllowlisted(path);
        } catch (Exception e) {
            return false;
        }
    }

    /** Package-private for testing: the pure allowlist match, path already context-stripped. */
    static boolean isAllowlisted(String path) {
        for (String prefix : PREFIX_ALLOWLIST) {
            if (path.startsWith(prefix)) {
                return true;
            }
        }
        for (String exact : EXACT_ALLOWLIST) {
            if (path.equals(exact)) {
                return true;
            }
        }
        if (path.startsWith(COMPUTER_PREFIX)) {
            for (String suffix : COMPUTER_SUFFIXES) {
                if (path.endsWith(suffix)) {
                    return true;
                }
            }
        }
        return false;
    }

    // ---- Test 5: system identity ----

    static boolean safeHasSystemAuthority() {
        try {
            return hasSystemAuthority();
        } catch (Exception e) {
            return false;
        }
    }

    static boolean hasSystemAuthority() {
        for (GrantedAuthority authority : Jenkins.getAuthentication2().getAuthorities()) {
            String a = authority.getAuthority();
            if (MITE_AUTHORITY.equals(a) || OPERATOR_AUTHORITY.equals(a)) {
                return true;
            }
        }
        return false;
    }

    // ---- Test 6: admin ----

    static boolean safeHasAdminister(Jenkins jenkins) {
        try {
            return jenkins.hasPermission(Jenkins.ADMINISTER);
        } catch (Exception e) {
            return false;
        }
    }

    static String safeActorName() {
        try {
            return Jenkins.getAuthentication2().getName();
        } catch (Exception e) {
            return "";
        }
    }

    // ---- Path extraction ----

    /** Strips the context path, exactly as {@link HttpActivityFilter#getPath} does. */
    static String getPath(HttpServletRequest req) {
        String uri = req.getRequestURI();
        String ctx = req.getContextPath();
        if (ctx != null && !ctx.isEmpty() && uri.startsWith(ctx)) {
            return uri.substring(ctx.length());
        }
        return uri;
    }

    // ---- Denial responses ----

    private static boolean wantsHtml(HttpServletRequest req) {
        String accept = req.getHeader("Accept");
        return accept != null && accept.contains("text/html");
    }

    private static void denyStarting(HttpServletRequest req, HttpServletResponse rsp) throws IOException {
        rsp.setStatus(503);
        if (wantsHtml(req)) {
            rsp.setContentType("text/html;charset=UTF-8");
            rsp.getWriter().write("<html><body><p>Jenkins is starting. Try again soon.</p></body></html>");
        } else {
            rsp.setContentType("application/json;charset=UTF-8");
            rsp.getWriter().write("{\"error\":\"starting\"}");
        }
    }

    private static void denyForbidden(HttpServletRequest req, HttpServletResponse rsp) throws IOException {
        rsp.setStatus(403);
        String pipelinesUrl = pipelinesUrlFromBannerEnv();
        if (wantsHtml(req)) {
            rsp.setContentType("text/html;charset=UTF-8");
            StringBuilder body = new StringBuilder();
            body.append("<html><body><p>This Jenkins UI is closed. You need the permission ")
                    .append(ADMINISTER_PERMISSION_ID).append(".</p>");
            if (pipelinesUrl != null) {
                body.append("<p>Use Pipelines instead: <a href=\"")
                        .append(escapeHtml(pipelinesUrl)).append("\">")
                        .append(escapeHtml(pipelinesUrl)).append("</a></p>");
            }
            body.append("</body></html>");
            rsp.getWriter().write(body.toString());
        } else {
            JSONObject json = new JSONObject();
            json.put("error", "headless");
            json.put("permission", ADMINISTER_PERMISSION_ID);
            if (pipelinesUrl != null) {
                json.put("pipelines", pipelinesUrl);
            }
            rsp.setContentType("application/json;charset=UTF-8");
            rsp.getWriter().write(json.toString());
        }
    }

    private static String escapeHtml(String s) {
        return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;").replace("\"", "&quot;");
    }

    /** Reads the {@code back} query parameter out of {@code VARROA_BANNER_URL}, or null. */
    static String pipelinesUrlFromBannerEnv() {
        return extractBackParam(System.getenv(BANNER_ENV));
    }

    /**
     * Package-private pure function for testing: extracts the {@code back} query
     * parameter value from a banner URL string. Returns null when the input is
     * empty, has no query string, or carries no {@code back} parameter.
     */
    static String extractBackParam(String bannerUrl) {
        if (bannerUrl == null || bannerUrl.isEmpty()) {
            return null;
        }
        int qIdx = bannerUrl.indexOf('?');
        if (qIdx < 0) {
            return null;
        }
        String query = bannerUrl.substring(qIdx + 1);
        int backIdx = query.indexOf("back=");
        if (backIdx < 0) {
            return null;
        }
        String value = query.substring(backIdx + "back=".length());
        return value.isEmpty() ? null : value;
    }

    // ---- Testing hooks ----

    /** Overrides the headless flag for testing. Package-private. */
    static void setHeadlessForTest(boolean value) {
        headlessEnabled = value;
    }

    /** Restores the headless flag from the real environment. Package-private. */
    static void resetHeadlessForTest() {
        headlessEnabled = readHeadlessEnv();
    }

    /** Resets the one-shot realm warning latch. Package-private. */
    static void resetRealmWarningForTest() {
        REALM_WARNING_LOGGED.set(false);
    }
}
