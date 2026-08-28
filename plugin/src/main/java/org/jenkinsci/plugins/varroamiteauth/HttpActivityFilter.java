package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.init.Initializer;
import hudson.util.PluginServletFilter;

import jakarta.servlet.Filter;
import jakarta.servlet.FilterChain;
import jakarta.servlet.FilterConfig;
import jakarta.servlet.ServletException;
import jakarta.servlet.ServletRequest;
import jakarta.servlet.ServletResponse;
import jakarta.servlet.http.HttpServletRequest;

import jenkins.model.Jenkins;

import java.io.IOException;
import java.util.logging.Level;
import java.util.logging.Logger;
import java.util.regex.Matcher;
import java.util.regex.Pattern;
import java.util.regex.PatternSyntaxException;

/**
 * Servlet filter that records the Unix timestamp of non-excluded HTTP requests.
 *
 * <p>Registered via {@link PluginServletFilter#addFilter} from an {@link Initializer}.
 * The exclusion table (D1) is evaluated in order; first match excludes the request
 * from activity tracking. Never throws past {@link FilterChain#doFilter}.
 *
 * <p>The recorded timestamp is published via {@link #getLastHttpActivityUnix()}.
 * All gauges for the {@code /varroa-activity/} drain response are published from
 * this class and {@link ActivitySink}.
 *
 * <h3>Exclusion rules (D1)</h3>
 * <ol>
 *   <li>Authenticated principal is {@code varroa-mite} or {@code varroa-mite-system}
 *   <li>Path prefix {@code /varroa-activity/}
 *   <li>Static resource prefixes: {@code /static/}, {@code /adjuncts/}, {@code /images/}, {@code /css/}, {@code /scripts/}
 *   <li>Path equals {@code /favicon.ico}
 *   <li>Path prefix {@code /crumbIssuer/}
 *   <li>Path prefix {@code /wsagents/}
 *   <li>{@code GET}/{@code HEAD} {@code /login} with no session cookie
 *   <li>Path matches {@code VARROA_HIBERNATION_IGNORE_REGEX} (operator-supplied, step-limited watchdog)
 * </ol>
 */
@Extension
public class HttpActivityFilter implements Filter {

    private static final Logger LOGGER = Logger.getLogger(HttpActivityFilter.class.getName());

    /** System property / env var for the operator-supplied ignore regex. */
    static final String IGNORE_REGEX_ENV = "VARROA_HIBERNATION_IGNORE_REGEX";

    /** Mite principal names used by VarroaSecurityRealm. */
    static final String MITE_USER = "varroa-mite";
    static final String MITE_SYSTEM_USER = "varroa-mite-system";

    /** Static resource path prefixes (rule 3). */
    private static final String[] STATIC_PREFIXES = {
        "/static/", "/adjuncts/", "/images/", "/css/", "/scripts/"
    };

    /** Max characters to match the ignore regex against per request (ReDoS guard). */
    private static final int REGEX_STEP_LIMIT = 4096;

    /** The compiled ignore regex, or null if unset or invalid. */
    private static volatile Pattern ignorePattern;

    /** Whether we've logged the invalid-regex warning (one-shot). */
    private static volatile boolean invalidRegexWarningLogged;

    /**
     * The Unix epoch millis (not seconds!) of the last non-excluded HTTP request.
     * Read by the mite gauges.
     */
    private static volatile long lastHttpActivityUnixMillis;

    static {
        compileIgnorePattern();
    }

    // ---- Filter registration ----

    @Initializer
    public static void registerFilter() throws ServletException {
        PluginServletFilter.addFilter(new HttpActivityFilter());
    }

    // ---- Filter implementation ----

    @Override
    public void init(FilterConfig filterConfig) {
        // Nothing to init — the static block handles regex compilation.
    }

    @Override
    public void doFilter(ServletRequest request, ServletResponse response, FilterChain chain)
            throws IOException, ServletException {
        try {
            if (request instanceof HttpServletRequest) {
                HttpServletRequest req = (HttpServletRequest) request;
                if (!isExcluded(req)) {
                    // Record activity (epoch millis).
                    lastHttpActivityUnixMillis = System.currentTimeMillis();
                }
            }
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "HttpActivityFilter: unexpected error in exclusion check", e);
            // Never break the filter chain.
        }

        chain.doFilter(request, response);
    }

    @Override
    public void destroy() {
        // No resources to release.
    }

    // ---- Exclusion rules (evaluated in order, first match excludes) ----

    /**
     * Returns {@code true} if the request should be excluded from activity
     * tracking. The 8 rules from D1 are evaluated in order; the first match
     * wins.
     */
    static boolean isExcluded(HttpServletRequest req) {
        // Rule 1: authenticated principal is varroa-mite or varroa-mite-system.
        if (isMitePrincipal(req)) {
            return true;
        }

        String path = getPath(req);

        // Rule 2: path prefix /varroa-activity/.
        if (path.startsWith("/varroa-activity/")) {
            return true;
        }

        // Rule 3: static resource prefixes.
        for (String prefix : STATIC_PREFIXES) {
            if (path.startsWith(prefix)) {
                return true;
            }
        }

        // Rule 4: path equals /favicon.ico.
        if ("/favicon.ico".equals(path)) {
            return true;
        }

        // Rule 5: path prefix /crumbIssuer/.
        if (path.startsWith("/crumbIssuer/")) {
            return true;
        }

        // Rule 6: path prefix /wsagents/.
        if (path.startsWith("/wsagents/")) {
            return true;
        }

        // Rule 7: GET/HEAD /login with no session cookie.
        if (isLoginProbe(req, path)) {
            return true;
        }

        // Rule 8: path matches the operator-supplied regex (step-limited watchdog).
        if (matchesIgnoreRegex(path)) {
            return true;
        }

        return false;
    }

    // ---- Rule implementations ----

    /**
     * Rule 1: Returns true if the authenticated principal is {@code varroa-mite}
     * or {@code varroa-mite-system}.
     */
    private static boolean isMitePrincipal(HttpServletRequest req) {
        // Try the current security context first (preferred for authenticated requests).
        try {
            String name = Jenkins.getAuthentication2().getName();
            if (MITE_USER.equals(name) || MITE_SYSTEM_USER.equals(name)) {
                return true;
            }
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "HttpActivityFilter: error reading authentication", e);
        }
        return false;
    }

    /**
     * Rule 7: Returns true for GET/HEAD /login without a session cookie (probe traffic).
     */
    private static boolean isLoginProbe(HttpServletRequest req, String path) {
        if (!"/login".equals(path)) {
            return false;
        }
        String method = req.getMethod();
        if (!"GET".equalsIgnoreCase(method) && !"HEAD".equalsIgnoreCase(method)) {
            return false;
        }
        // No session cookie means no established session — this is probe traffic.
        return req.getSession(false) == null;
    }

    /**
     * Rule 8: Returns true if the path matches the compiled ignore regex.
     * Uses a step-limited CharSequence to guard against ReDoS.
     */
    private static boolean matchesIgnoreRegex(String path) {
        Pattern p = ignorePattern;
        if (p == null) {
            return false;
        }
        try {
            CharSequence limited = new StepLimitedCharSequence(path, REGEX_STEP_LIMIT);
            Matcher m = p.matcher(limited);
            return m.find();
        } catch (Exception e) {
            // ReDoS watchdog aborted or other error — fail open (not excluded).
            LOGGER.log(Level.FINE, "HttpActivityFilter: regex match aborted", e);
            return false;
        }
    }

    // ---- Path extraction ----

    /**
     * Returns the servlet path portion from the request, normalised.
     */
    private static String getPath(HttpServletRequest req) {
        String uri = req.getRequestURI();
        String ctx = req.getContextPath();
        if (ctx != null && !ctx.isEmpty() && uri.startsWith(ctx)) {
            return uri.substring(ctx.length());
        }
        return uri;
    }

    // ---- Public accessors for gauge collection ----

    /**
     * Returns the Unix epoch millis of the last non-excluded HTTP request,
     * or 0 if no activity has been recorded.
     */
    public static long getLastHttpActivityUnixMillis() {
        return lastHttpActivityUnixMillis;
    }

    /**
     * Resets the last activity time to 0. Used in testing.
     */
    static void resetForTesting() {
        lastHttpActivityUnixMillis = 0;
    }

    // ---- Regex compilation (read once at startup) ----

    /**
     * Compiles the ignore regex from the environment variable.
     * If the regex is invalid, logs a single WARNING and treats it as unset.
     */
    static void compileIgnorePattern() {
        String raw = System.getenv(IGNORE_REGEX_ENV);
        compileIgnorePatternFromString(raw);
    }

    /**
     * Compiles the given raw regex string, logging a single WARNING on failure
     * and treating it as unset. Package-private for testing.
     */
    static void compileIgnorePatternFromString(String raw) {
        if (raw == null || raw.isEmpty()) {
            ignorePattern = null;
            return;
        }
        try {
            ignorePattern = Pattern.compile(raw);
            LOGGER.log(Level.INFO, "HttpActivityFilter: compiled ignore regex from {0}", IGNORE_REGEX_ENV);
        } catch (PatternSyntaxException e) {
            if (!invalidRegexWarningLogged) {
                invalidRegexWarningLogged = true;
                LOGGER.log(Level.WARNING,
                    "HttpActivityFilter: invalid regex in " + IGNORE_REGEX_ENV
                    + " (treated as unset): " + e.getMessage());
            }
            ignorePattern = null;
        }
    }

    /**
     * Sets the ignore regex pattern for testing. Pass {@code null} to clear.
     */
    static void setIgnorePatternForTest(Pattern pattern) {
        ignorePattern = pattern;
        invalidRegexWarningLogged = false;
    }

    /**
     * Simulates an invalid regex for testing the single-warning behavior.
     */
    static void setInvalidRegexForTest() {
        invalidRegexWarningLogged = false;
        ignorePattern = null;
    }

    // ---- Step-limited CharSequence for ReDoS protection ----

    /**
     * A {@link CharSequence} wrapper that tracks character access steps and
     * throws after exceeding a limit, preventing ReDoS via pathological regex
     * input. Used only for the path (bounded input), so the limit is generous.
     */
    static class StepLimitedCharSequence implements CharSequence {
        private final CharSequence wrapped;
        private final int maxSteps;
        // Shared step counter (single-element holder) so subsequences the regex
        // engine derives internally count against the SAME budget — a per-instance
        // counter would reset on every subSequence() and defeat the ReDoS guard.
        private final int[] steps;

        StepLimitedCharSequence(CharSequence wrapped, int maxSteps) {
            this(wrapped, maxSteps, new int[1]);
        }

        private StepLimitedCharSequence(CharSequence wrapped, int maxSteps, int[] steps) {
            this.wrapped = wrapped;
            this.maxSteps = maxSteps;
            this.steps = steps;
        }

        @Override
        public int length() {
            return wrapped.length();
        }

        @Override
        public char charAt(int index) {
            if (++steps[0] > maxSteps) {
                throw new RuntimeException("Regex match aborted after " + maxSteps + " steps (ReDoS guard)");
            }
            return wrapped.charAt(index);
        }

        @Override
        public CharSequence subSequence(int start, int end) {
            return new StepLimitedCharSequence(wrapped.subSequence(start, end), maxSteps, steps);
        }
    }
}
