package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.model.Descriptor;
import hudson.model.User;
import hudson.security.SecurityRealm;
import jenkins.model.Jenkins;
import jenkins.security.ApiTokenProperty;
import org.acegisecurity.Authentication;
import org.acegisecurity.AuthenticationException;
import org.acegisecurity.AuthenticationManager;
import org.acegisecurity.BadCredentialsException;
import org.acegisecurity.GrantedAuthority;
import org.acegisecurity.GrantedAuthorityImpl;
import org.acegisecurity.context.SecurityContextHolder;
import org.acegisecurity.providers.UsernamePasswordAuthenticationToken;
import org.acegisecurity.userdetails.UserDetails;
import org.acegisecurity.userdetails.UserDetailsService;
import org.acegisecurity.userdetails.UsernameNotFoundException;
import org.jenkinsci.Symbol;
import org.kohsuke.stapler.DataBoundConstructor;
import org.kohsuke.stapler.DataBoundSetter;
import org.kohsuke.stapler.HttpRedirect;
import org.kohsuke.stapler.HttpResponse;
import org.kohsuke.stapler.QueryParameter;
import org.kohsuke.stapler.StaplerRequest;
import org.springframework.dao.DataAccessException;

import jakarta.servlet.Filter;
import jakarta.servlet.FilterChain;
import jakarta.servlet.FilterConfig;
import jakarta.servlet.ServletException;
import jakarta.servlet.ServletRequest;
import jakarta.servlet.ServletResponse;
import jakarta.servlet.http.Cookie;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;
import java.io.File;
import java.io.IOException;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.util.Arrays;
import java.util.List;
import java.util.Map;
import java.util.logging.Logger;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.context.Scope;

/**
 * Varroa SecurityRealm — CJOC-style federated authentication.
 *
 * Reads the {@code varroa_token} cookie (set by Varroa after OIDC login)
 * and validates the JWT against Dex's JWKS. Also authenticates the mite
 * sidecar via API token.
 *
 * Unauthenticated users are redirected to Varroa for login. No per-controller
 * Dex redirect URIs are needed — Varroa is the single auth entry point.
 *
 * <p>This realm is JCasC-configurable via the {@code varroaMiteAuth} symbol.
 * When properties are omitted the realm falls back to the corresponding
 * {@code VARROA_OIDC_*} environment variable.</p>
 */
public class VarroaSecurityRealm extends SecurityRealm {

    private static final Logger LOGGER = Logger.getLogger(VarroaSecurityRealm.class.getName());
    private static final Tracer tracer = GlobalOpenTelemetry.getTracer("varroa-plugin");
    static final String COOKIE_NAME = "varroa_token";
    static final String SYSTEM_AUTH = "varroa-mite-system";

    // Non-final only to allow test injection via setJwtValidatorForTest.
    private JWTValidator jwtValidator;
    // Non-final only to allow test injection via setApiKeyValidatorForTest.
    private ApiKeyValidator apiKeyValidator;
    // Test seam for VARROA_LOGIN_URL, which is otherwise read from the environment.
    String loginUrlForTest;
    private final List<String> userClaimNames;
    private final String groupClaimName;

    // Stapler.getCurrentRequest() returns null inside authenticate() because
    // SecurityFilter calls authenticate BEFORE Stapler dispatches the request.
    // We capture the request here via createFilter() so authenticate() can
    // read cookies and headers.
    private static final ThreadLocal<HttpServletRequest> CURRENT_REQUEST = new ThreadLocal<>();

    /**
     * No-arg constructor for XStream backward compatibility and default
     * instantiation. Delegates to the data-bound constructor with null
     * arguments so every value falls back to the {@code VARROA_OIDC_*}
     * environment variable.
     */
    public VarroaSecurityRealm() {
        this(null, null, null);
    }

    /**
     * Data-bound constructor for JCasC.
     *
     * @param oidcIssuer      OIDC issuer URL; defaults to {@code VARROA_OIDC_ISSUER} when empty
     * @param userClaimNames  comma-separated claim names for user identity; defaults to {@code VARROA_OIDC_USER_CLAIM}
     * @param groupClaimName  claim name for group memberships; defaults to {@code VARROA_OIDC_GROUP_CLAIM}
     */
    @DataBoundConstructor
    public VarroaSecurityRealm(String oidcIssuer, String userClaimNames, String groupClaimName) {
        if (oidcIssuer == null || oidcIssuer.isEmpty()) {
            oidcIssuer = System.getenv("VARROA_OIDC_ISSUER");
        }
        this.jwtValidator = new JWTValidator(oidcIssuer);
        this.apiKeyValidator = new ApiKeyValidator();

        if (userClaimNames == null || userClaimNames.isEmpty()) {
            userClaimNames = System.getenv("VARROA_OIDC_USER_CLAIM");
        }
        if (userClaimNames != null && !userClaimNames.isEmpty()) {
            this.userClaimNames = Arrays.asList(userClaimNames.split(","));
        } else {
            this.userClaimNames = Arrays.asList("preferred_username", "sub");
        }

        if (groupClaimName == null || groupClaimName.isEmpty()) {
            groupClaimName = System.getenv("VARROA_OIDC_GROUP_CLAIM");
        }
        this.groupClaimName = (groupClaimName != null && !groupClaimName.isEmpty())
                ? groupClaimName : "groups";
    }

    // ---- Data-bound property accessors (JCasC export) ----

    /**
     * The configured OIDC issuer URL. Delegates to the {@link JWTValidator}
     * which holds the (trailing-slash-normalised) value resolved at
     * construction from the constructor argument or {@code VARROA_OIDC_ISSUER}.
     * Exposed so JCasC can export the realm and round-trip it.
     *
     * @return the resolved issuer, or {@code null} when none was configured.
     */
    public String getOidcIssuer() {
        return jwtValidator != null ? jwtValidator.getOidcIssuer() : null;
    }

    /**
     * The configured user-claim names, comma-joined to round-trip with the
     * {@link #VarroaSecurityRealm(String, String, String) data-bound
     * constructor}'s {@code String} parameter.
     *
     * @return the comma-separated claim names; never {@code null}.
     */
    public String getUserClaimNames() {
        return String.join(",", userClaimNames);
    }

    /**
     * The configured group-claim name.
     *
     * @return the group-claim name; never {@code null}.
     */
    public String getGroupClaimName() {
        return groupClaimName;
    }

    /** @return the resolved user-claim names as an immutable list (internal use/tests). */
    List<String> userClaimNamesList() {
        return userClaimNames;
    }

    // ---- SecurityRealm API ----

    private static String getCookieValue(HttpServletRequest req, String name) {
        Cookie[] cookies = req.getCookies();
        if (cookies == null) return null;
        for (Cookie c : cookies) {
            if (c.getName().equals(name)) return c.getValue();
        }
        return null;
    }

    @Override
    public Filter createFilter(FilterConfig filterConfig) {
        // Wrap the default SecurityFilter to capture the current request
        // and redirect unauthenticated non-local users.
        Filter defaultFilter = super.createFilter(filterConfig);
        return new Filter() {
            @Override
            public void init(FilterConfig cfg) throws ServletException {
                defaultFilter.init(cfg);
            }
            @Override
            public void doFilter(ServletRequest req, ServletResponse rsp, FilterChain chain)
                    throws IOException, ServletException {
                if (req instanceof HttpServletRequest) {
                    CURRENT_REQUEST.set((HttpServletRequest) req);
                }
                try {
                    if (req instanceof HttpServletRequest && rsp instanceof HttpServletResponse) {
                        HttpServletRequest httpReq = (HttpServletRequest) req;
                        HttpServletResponse httpResp = (HttpServletResponse) rsp;

                        // Allow static resources through without authentication
                        // ONLY when the visitor is truly unauthenticated (no cookie).
                        // If the user has a varroa_token cookie, let the SecurityFilter
                        // process the request normally so Jenkins recognizes the session.
                        // This prevents 500s on /userContent/varroa-banner.js when
                        // allowAnonymousRead is false but the user is logged in.
                        String path = httpReq.getRequestURI();
                        boolean isStatic = path.startsWith("/userContent/") || path.startsWith("/static/")
                                || path.startsWith("/adjuncts/") || path.startsWith("/scripts/")
                                || path.endsWith(".js") || path.endsWith(".css")
                                || path.endsWith(".png") || path.endsWith(".ico")
                                || path.endsWith(".svg") || path.endsWith(".woff")
                                || path.endsWith(".jpg") || path.endsWith(".gif")
                                || path.endsWith(".map");

                        String cookie = getCookieValue(httpReq, COOKIE_NAME);
                        boolean hasCookie = cookie != null && !cookie.isEmpty();

                        if (isStatic && !hasCookie) {
                            chain.doFilter(req, rsp);
                            return;
                        }

                        // Inject JWT auth for requests carrying the varroa_token cookie.
                        if (hasCookie) {
                            final Authentication jwtAuth = prepareJwtAuth(httpReq);
                            if (jwtAuth != null) {
                                FilterChain wrappedChain = new FilterChain() {
                                    public void doFilter(ServletRequest r, ServletResponse s)
                                            throws IOException, ServletException {
                                        SecurityContextHolder.getContext().setAuthentication(jwtAuth);
                                        chain.doFilter(r, s);
                                    }
                                };
                                defaultFilter.doFilter(req, rsp, wrappedChain);
                                return;
                            }
                        }

                        // Inject vk_* Bearer auth for API key requests.
                        // Sessionless: do NOT call httpReq.getSession(true).
                        // Gated on cookie/session absence like the operator-JWT
                        // path: with a cookie present (even an invalid one) the
                        // filter does not inject Bearer auth — authenticate()
                        // handles the fallback (spec: "Invalid cookie with vk_
                        // Bearer falls back to the Bearer").
                        // IMPORTANT: if the Bearer starts with vk_, skip
                        // operator-JWT validation entirely.
                        String authHeader = httpReq.getHeader("Authorization");
                        boolean isVkBearer = authHeader != null && authHeader.startsWith("Bearer vk_");
                        if (isVkBearer && !hasCookie && !hasExistingSession(httpReq)) {
                            final Authentication apiKeyAuth = prepareApiKeyAuth(httpReq);
                            if (apiKeyAuth != null) {
                                FilterChain wrappedChain = new FilterChain() {
                                    public void doFilter(ServletRequest r, ServletResponse s)
                                            throws IOException, ServletException {
                                        SecurityContextHolder.getContext().setAuthentication(apiKeyAuth);
                                        chain.doFilter(r, s);
                                    }
                                };
                                defaultFilter.doFilter(req, rsp, wrappedChain);
                                return;
                            }
                        }

                        // Inject operator-JWT auth for mite requests carrying a
                        // Bearer token whenever the Bearer does NOT start with vk_.
                        // The Bearer is re-validated and injected into the
                        // SecurityContext on EVERY request (including plain GETs like
                        // /whoAmI) — it is authoritative for the mite identity. We do
                        // NOT gate on an existing session: the Bearer is authoritative
                        // for this request. Bearer auth stays sessionless because MCP
                        // clients do not retain Jenkins cookies.
                        if (!hasCookie && !isVkBearer) {
                            final Authentication miteAuth = prepareOperatorAuth(httpReq);
                            if (miteAuth != null) {
                                FilterChain wrappedChain = new FilterChain() {
                                    public void doFilter(ServletRequest r, ServletResponse s)
                                            throws IOException, ServletException {
                                        SecurityContextHolder.getContext().setAuthentication(miteAuth);
                                        chain.doFilter(r, s);
                                    }
                                };
                                defaultFilter.doFilter(req, rsp, wrappedChain);
                                return;
                            }
                        }

                        // Redirect browser requests to Varroa login. Agents, CLI,
                        // and API clients don't send Accept: text/html and get
                        // standard 401/403 from the SecurityFilter instead.
                        if (!hasCookie) {
                            String accept = httpReq.getHeader("Accept");
                            boolean isBrowser = accept != null && accept.contains("text/html");
                            if (isBrowser) {
                                String loginUrl = varroaLoginUrl();
                                if (loginUrl != null && !loginUrl.isEmpty()) {
                                    // Build an absolute state URL so Varroa can redirect the
                                    // user back to this controller after login. The request URI
                                    // already carries any context-path prefix, so only the
                                    // scheme+authority come from the root URL. When no root URL
                                    // is configured yet the state stays a relative path — the
                                    // caller-controlled Host/X-Forwarded-Proto headers must not
                                    // decide the post-login redirect target (see
                                    // docs/internal/security-analysis.md on header-derived URLs).
                                    String state = absoluteStateUrl(httpReq.getRequestURI());
                                    if (httpReq.getQueryString() != null) {
                                        state += "?" + httpReq.getQueryString();
                                    }
                                    httpResp.sendRedirect(loginUrl + "?state=" + URLEncoder.encode(state, StandardCharsets.UTF_8));
                                    return;
                                }
                            }
                        }
                        defaultFilter.doFilter(req, rsp, chain);
                    }
                } finally {
                    CURRENT_REQUEST.remove();
                }
            }
            @Override
            public void destroy() {
                defaultFilter.destroy();
            }
        };
    }

    @Override
    public String getLoginUrl() {
        // Must be context-relative: Jenkins core prepends the context path
        // ("/" + this value) when it builds login redirects, so returning the
        // absolute VARROA_LOGIN_URL produced /https://<dashboard>/login (#530).
        // commenceLogin below performs the actual external redirect.
        return "securityRealm/commenceLogin";
    }

    @Override
    public SecurityComponents createSecurityComponents() {
        VarroaAuthenticationManager authManager = new VarroaAuthenticationManager(jwtValidator);
        return new SecurityComponents(authManager, new VarroaUserDetailsService());
    }

    /**
     * Handles the initial redirect when an unauthenticated user hits Jenkins.
     * Redirects to Varroa login with the current URL as state.
     */
    public HttpResponse doCommenceLogin(StaplerRequest req, @QueryParameter String from) {
        String redirectUrl = commenceLoginRedirectUrl(from);
        return new HttpRedirect(redirectUrl != null ? redirectUrl : req.getContextPath() + "/");
    }

    /**
     * Builds the external login redirect for commenceLogin, or null when
     * VARROA_LOGIN_URL is unset.
     */
    String commenceLoginRedirectUrl(String from) {
        String loginUrl = varroaLoginUrl();
        if (loginUrl == null || loginUrl.isEmpty()) {
            return null;
        }
        // Use the 'from' parameter as the state target. Using the full
        // request URI causes recursive encoding of the 'from' parameter
        // on each redirect, producing a 431 Request Header Too Large.
        // Only server-relative paths are honored — an absolute URL would turn
        // the post-login state into an open redirect, and protocol-relative
        // "//host" would too whenever absoluteStateUrl has no root URL to
        // absolutize against.
        String state = absoluteStateUrl(
                from != null && from.startsWith("/") && !from.startsWith("//") ? from : "/");
        return loginUrl + "?state=" + URLEncoder.encode(state, StandardCharsets.UTF_8);
    }

    /**
     * Absolutizes a server-relative request path against the root URL's scheme
     * and authority only. In path-prefixed deployments (JENKINS_OPTS --prefix,
     * used by ingress path mode) the root URL already ends with the context
     * path and request paths carry the same prefix — concatenating the full
     * root URL would double it. Returns the path unchanged when no root URL is
     * configured.
     */
    static String absoluteStateUrl(String path) {
        String rootUrl = jenkins.model.Jenkins.get().getRootUrl();
        if (rootUrl != null) {
            try {
                java.net.URI root = java.net.URI.create(rootUrl);
                if (root.getScheme() != null && root.getAuthority() != null) {
                    return root.getScheme() + "://" + root.getAuthority() + path;
                }
            } catch (IllegalArgumentException e) {
                // Unparseable root URL — fall through to the relative path.
            }
        }
        return path;
    }

    private String varroaLoginUrl() {
        return loginUrlForTest != null ? loginUrlForTest : System.getenv("VARROA_LOGIN_URL");
    }

    JWTValidator getJWTValidator() {
        return jwtValidator;
    }

    /**
     * Builds the GrantedAuthority list from claims using configured claim names.
     * Returns [authenticated] + one GrantedAuthorityImpl per group value.
     */
    private GrantedAuthority[] authoritiesFor(Map<String, Object> claims) {
        List<String> groups = JWTValidator.getGroups(claims, groupClaimName);
        GrantedAuthority[] authorities = new GrantedAuthority[1 + groups.size()];
        authorities[0] = new GrantedAuthorityImpl("authenticated");
        for (int i = 0; i < groups.size(); i++) {
            authorities[i + 1] = new GrantedAuthorityImpl(groups.get(i));
        }
        return authorities;
    }

    private GrantedAuthority[] authoritiesForOperatorUser(Map<String, Object> claims) {
        List<String> groups = JWTValidator.getGroups(claims, "groups");
        java.util.ArrayList<GrantedAuthority> authorities = new java.util.ArrayList<>();
        authorities.add(new GrantedAuthorityImpl("authenticated"));
        for (String group : groups) {
            if (!group.startsWith("ROLE:varroa:system-")) {
                authorities.add(new GrantedAuthorityImpl(group));
            }
        }
        return authorities.toArray(new GrantedAuthority[0]);
    }

    private Authentication operatorUserAuthentication(Map<String, Object> claims) {
        String userId = (String) claims.get("preferred_username");
        if (userId == null || userId.isEmpty()) {
            userId = (String) claims.get("sub");
        }
        if (userId == null || userId.isEmpty()) {
            return null;
        }

        String name = (String) claims.get("name");
        String email = (String) claims.get("email");
        User user = User.getOrCreateByIdOrFullName(userId);
        if (name != null && !name.isEmpty()) {
            user.setFullName(name);
        }
        if (email != null && !email.isEmpty()) {
            try {
                Class<?> mailerPropClass = Class.forName("hudson.tasks.Mailer$UserProperty");
                Object mailerProp = mailerPropClass.getConstructor(String.class).newInstance(email);
                user.addProperty((hudson.model.UserProperty) mailerProp);
            } catch (Exception e) {
                LOGGER.warning("varroa-mite-auth: failed to set email: " + e.getMessage());
            }
        }
        try {
            user.save();
        } catch (IOException e) {
            LOGGER.warning("varroa-mite-auth: failed to save user: " + e.getMessage());
        }

        return new UsernamePasswordAuthenticationToken(userId, null,
                authoritiesForOperatorUser(claims));
    }

    /**
     * Validates the varroa_token JWT cookie and returns an Authentication
     * token, or null if absent/invalid. Does NOT set the SecurityContext
     * directly — the caller wraps the filter chain to inject it after
     * the SecurityFilter runs.
     */
    private Authentication prepareJwtAuth(HttpServletRequest req) {
        String token = getCookieValue(req, COOKIE_NAME);
        if (token == null || token.isEmpty()) return null;

        Map<String, Object> claims = jwtValidator.validate(token);
        if (claims == null) {
            LOGGER.warning("varroa-mite-auth: JWT validation failed for cookie");
            return null;
        }

        String userId = JWTValidator.getUserId(claims, userClaimNames);
        if (userId == null) {
            LOGGER.warning("varroa-mite-auth: no user claim found in JWT");
            return null;
        }
        String email = (String) claims.get("email");
        String name = (String) claims.getOrDefault("name", userId);
        List<String> groups = JWTValidator.getGroups(claims, groupClaimName);

        LOGGER.info("varroa-mite-auth: proactive sign-in for " + userId +
            " groups=" + groups + " claims=" + claims);

        User user = User.getOrCreateByIdOrFullName(userId);
        user.setFullName(name);
        if (email != null && !email.isEmpty()) {
            try {
                // Keep the optional user-property integration isolated from auth.
                Class<?> mailerPropClass = Class.forName("hudson.tasks.Mailer$UserProperty");
                Object mailerProp = mailerPropClass.getConstructor(String.class).newInstance(email);
                user.addProperty((hudson.model.UserProperty) mailerProp);
            } catch (Exception e) {
                LOGGER.warning("varroa-mite-auth: failed to set email: " + e.getMessage());
            }
        }
        try {
            user.save();
        } catch (IOException e) {
            LOGGER.warning("varroa-mite-auth: failed to save user: " + e.getMessage());
        }

        return new UsernamePasswordAuthenticationToken(userId, null, authoritiesFor(claims));
    }

    /**
     * Validates an operator-signed Bearer token (mite or operator auth) and
     * returns an Authentication for the corresponding system user, or null if
     * absent/invalid. Mirrors {@link #prepareJwtAuth}: does NOT set the
     * SecurityContext directly — the caller wraps the filter chain to inject
     * it after the SecurityFilter runs.
     *
     * LOGGING: this method is only called when there is no existing session
     * (see hasExistingSession), so the INFO log here is emitted only on session
     * establishment — not on every request.
     */
    private Authentication prepareOperatorAuth(HttpServletRequest req) {
        String authHeader = req.getHeader("Authorization");
        if (authHeader == null || !authHeader.startsWith("Bearer ")) return null;

        Map<String, Object> opClaims = jwtValidator.validateOperatorToken(authHeader.substring(7));
        if (opClaims == null) {
            LOGGER.info("varroa-mite-auth: operator JWT validation failed in filter");
            return null;
        }

        if ("user".equals(opClaims.get("varroa_typ"))) {
            return operatorUserAuthentication(opClaims);
        }

        Object sub = opClaims.get("sub");
        if ("system:varroa-mite".equals(sub)) {
            LOGGER.fine("varroa-mite-auth: established varroa-mite operator-JWT principal");
            return new UsernamePasswordAuthenticationToken(
                    User.getOrCreateByIdOrFullName("varroa-mite").impersonate2(), null,
                    new GrantedAuthority[]{
                        new GrantedAuthorityImpl("authenticated"),
                        new GrantedAuthorityImpl("ROLE:varroa:system-mite")});
        }
        if ("system:varroa-operator".equals(sub)) {
            LOGGER.fine("varroa-mite-auth: established varroa-operator operator-JWT principal");
            return new UsernamePasswordAuthenticationToken(
                    User.getOrCreateByIdOrFullName("varroa-operator").impersonate2(), null,
                    new GrantedAuthority[]{
                        new GrantedAuthorityImpl("authenticated"),
                        new GrantedAuthorityImpl("ROLE:varroa:system-operator")});
        }

        // Defensive: JWTValidator.validateOperatorToken already restricts sub to a
        // known set, so this should be unreachable, but fail closed rather than
        // silently defaulting to either principal if it ever isn't.
        LOGGER.warning("varroa-mite-auth: operator JWT carried unrecognized sub: " + sub);
        return null;
    }

    /**
     * Validates a vk_* Bearer token against the Varroa verify endpoint and
     * returns an Authentication for the token owner, or null if absent/invalid.
     * Creates or updates the Jenkins User record exactly like the OIDC cookie
     * path, so audit log entries record the real user.
     * <p>
     * IMPORTANT: does NOT call httpReq.getSession(true) — vk_ requests are
     * deliberately sessionless.
     */
    private Authentication prepareApiKeyAuth(HttpServletRequest req) {
        String authHeader = req.getHeader("Authorization");
        if (authHeader == null || !authHeader.startsWith("Bearer vk_")) {
            return null;
        }

        Map<String, Object> identity = apiKeyValidator.validate(authHeader.substring(7));
        if (identity == null) {
            return null;
        }

        String userId = (String) identity.get("preferredUsername");
        if (userId == null || userId.isEmpty()) {
            userId = (String) identity.get("subject");
        }
        if (userId == null) {
            return null;
        }

        String name = (String) identity.get("name");
        String email = (String) identity.get("email");
        @SuppressWarnings("unchecked")
        java.util.List<String> groups = (java.util.List<String>) identity.get("groups");
        if (groups == null) groups = java.util.List.of();

        User user = User.getOrCreateByIdOrFullName(userId);
        if (name != null && !name.isEmpty()) {
            user.setFullName(name);
        }
        if (email != null && !email.isEmpty()) {
            try {
                Class<?> mailerPropClass = Class.forName("hudson.tasks.Mailer$UserProperty");
                Object mailerProp = mailerPropClass.getConstructor(String.class).newInstance(email);
                user.addProperty((hudson.model.UserProperty) mailerProp);
            } catch (Exception e) {
                LOGGER.warning("varroa-mite-auth: failed to set email: " + e.getMessage());
            }
        }
        try {
            user.save();
        } catch (IOException e) {
            LOGGER.warning("varroa-mite-auth: failed to save user: " + e.getMessage());
        }

        GrantedAuthority[] authorities = new GrantedAuthority[1 + groups.size()];
        authorities[0] = new GrantedAuthorityImpl("authenticated");
        for (int i = 0; i < groups.size(); i++) {
            authorities[i + 1] = new GrantedAuthorityImpl(groups.get(i));
        }

        return new UsernamePasswordAuthenticationToken(user.impersonate2(), null, authorities);
    }

    public ApiKeyValidator getApiKeyValidator() {
        return apiKeyValidator;
    }

    // Package-private test seam: the production validator is constructed from
    // env vars, which JenkinsRule tests cannot set.
    void setApiKeyValidatorForTest(ApiKeyValidator v) {
        this.apiKeyValidator = v;
    }

    // Package-private test seam: the production validator is constructed from
    // env vars, which JenkinsRule tests cannot set.
    void setJwtValidatorForTest(JWTValidator v) {
        this.jwtValidator = v;
    }

    /**
     * Returns true if the request carries a valid HTTP session. When a
     * session already exists Jenkins' SecurityFilter will handle
     * authentication from the session, so we skip the per-request
     * operator-JWT validation path entirely.
     */
    private static boolean hasExistingSession(HttpServletRequest req) {
        Cookie[] cookies = req.getCookies();
        if (cookies == null) return false;
        for (Cookie c : cookies) {
            // Jetty 12 uses JSESSIONID.<node-hash>; match prefix.
            if (c.getName() != null && c.getName().startsWith("JSESSIONID") && !c.getValue().isEmpty()) {
                jakarta.servlet.http.HttpSession sess = req.getSession(false);
                if (sess != null) {
                    LOGGER.fine("varroa-mite-auth: existing session found");
                    return true;
                }
                LOGGER.fine("varroa-mite-auth: JSESSIONID cookie present but no server-side session");
            }
        }
        return false;
    }

    // ---- Authentication Manager ----

    class VarroaAuthenticationManager implements AuthenticationManager {

        private final JWTValidator jwtValidator;

        VarroaAuthenticationManager(JWTValidator jwtValidator) {
            this.jwtValidator = jwtValidator;
        }

        @Override
        public Authentication authenticate(Authentication authentication) throws AuthenticationException {
            HttpServletRequest req = CURRENT_REQUEST.get();
            if (req == null) {
                LOGGER.warning("varroa-mite-auth: no request in ThreadLocal, throwing BadCredentials");
                throw new BadCredentialsException("No request context");
            }

            String source = determineSource(req);
            Span span = tracer.spanBuilder("securityrealm.authenticate")
                .setAttribute("auth.source", source)
                .startSpan();
            try (Scope scope = span.makeCurrent()) {
                // 1. Check varroa_token cookie (user auth).
                String token = getCookie(req, COOKIE_NAME);
                if (token != null && !token.isEmpty()) {
                    Map<String, Object> claims = jwtValidator.validate(token);
                    if (claims != null) {
                        String userId = JWTValidator.getUserId(claims, userClaimNames);
                        if (userId == null) userId = (String) claims.get("sub");
                        String name = (String) claims.getOrDefault("name", userId);

                        User user = User.getOrCreateByIdOrFullName(userId);
                        user.setFullName(name);
                        try {
                            user.save();
                        } catch (IOException e) {
                            LOGGER.warning("varroa-mite-auth: failed to save user: " + e.getMessage());
                        }

                        span.setAttribute("auth.outcome", "jwt_cookie");
                        span.setAttribute("auth.subject", userId);
                        return new UsernamePasswordAuthenticationToken(
                                user.impersonate2(), null, authoritiesFor(claims));
                    }
                    LOGGER.info("varroa-mite-auth: cookie JWT validation failed");
                }

                // 2. Check for vk_* Bearer token (API key auth).
                String authHeader = req.getHeader("Authorization");
                if (authHeader != null && authHeader.startsWith("Bearer vk_")) {
                    Map<String, Object> identity = apiKeyValidator.validate(authHeader.substring(7));
                    if (identity != null) {
                        String userId = (String) identity.get("preferredUsername");
                        if (userId == null || userId.isEmpty()) {
                            userId = (String) identity.get("subject");
                        }
                        LOGGER.info("varroa-mite-auth: authenticate vk_bearer for " + userId);
                        span.setAttribute("auth.outcome", "vk_bearer");
                        span.setAttribute("auth.subject", userId);
                        String name = (String) identity.get("name");
                        @SuppressWarnings("unchecked")
                        java.util.List<String> groups = (java.util.List<String>) identity.get("groups");
                        if (groups == null) groups = java.util.List.of();
                        User user = User.getOrCreateByIdOrFullName(userId);
                        if (name != null && !name.isEmpty()) {
                            user.setFullName(name);
                        }
                        try {
                            user.save();
                        } catch (IOException e) {
                            LOGGER.warning("varroa-mite-auth: failed to save user: " + e.getMessage());
                        }
                        GrantedAuthority[] authorities = new GrantedAuthority[1 + groups.size()];
                        authorities[0] = new GrantedAuthorityImpl("authenticated");
                        for (int i = 0; i < groups.size(); i++) {
                            authorities[i + 1] = new GrantedAuthorityImpl(groups.get(i));
                        }
                        return new UsernamePasswordAuthenticationToken(user.impersonate2(), null, authorities);
                    }
                    LOGGER.info("varroa-mite-auth: vk_ Bearer validation failed");
                    throw new BadCredentialsException("Invalid vk_ token");
                }

                // 3. Check for operator-signed JWT Bearer token (mite auth).
                // This is the fallback path — reached when the session is lost
                // (e.g. after a Jenkins restart). In normal steady state the
                // session is reused and validateOperatorToken is not called.
                if (authHeader != null && authHeader.startsWith("Bearer ")) {
                    String bearerToken = authHeader.substring(7);
                    Map<String, Object> opClaims = jwtValidator.validateOperatorToken(bearerToken);
                    if (opClaims != null) {
                        if ("user".equals(opClaims.get("varroa_typ"))) {
                            Authentication userAuth = operatorUserAuthentication(opClaims);
                            if (userAuth != null) {
                                LOGGER.info("varroa-mite-auth: authenticate recovered user session via operator JWT");
                                span.setAttribute("auth.outcome", "operator_user_bearer");
                                span.setAttribute("auth.subject", userAuth.getName());
                                return userAuth;
                            }
                        }
                        LOGGER.info("varroa-mite-auth: authenticate recovered varroa-mite session via operator JWT");
                        span.setAttribute("auth.outcome", "operator_bearer");
                        span.setAttribute("auth.subject", "varroa-mite");
                        return new UsernamePasswordAuthenticationToken(
                            User.getOrCreateByIdOrFullName("varroa-mite").impersonate2(), null,
                            new GrantedAuthority[]{new GrantedAuthorityImpl("authenticated"),
                                new GrantedAuthorityImpl("ROLE:varroa:system-mite")});
                    }
                    LOGGER.info("varroa-mite-auth: operator JWT validation failed");
                }

                span.setAttribute("auth.outcome", "failure");
                LOGGER.info("varroa-mite-auth: no valid auth, throwing BadCredentials");
                throw new BadCredentialsException("No valid varroa_token cookie or mite secret");
            } catch (Exception e) {
                span.setStatus(StatusCode.ERROR, e.getMessage());
                span.recordException(e);
                throw e;
            } finally {
                span.end();
            }
        }

        private static String determineSource(HttpServletRequest req) {
            if (getCookie(req, COOKIE_NAME) != null && !getCookie(req, COOKIE_NAME).isEmpty()) {
                return "cookie";
            }
            String authHeader = req.getHeader("Authorization");
            if (authHeader != null && authHeader.startsWith("Bearer ")) {
                return "bearer";
            }
            return "none";
        }

        private static String getCookie(HttpServletRequest req, String name) {
            Cookie[] cookies = req.getCookies();
            if (cookies == null) return null;
            for (Cookie c : cookies) {
                if (c.getName().equals(name)) return c.getValue();
            }
            return null;
        }
    }

    // ---- User Details Service ----

    static class VarroaUserDetailsService implements UserDetailsService {

        @Override
        public UserDetails loadUserByUsername(String username)
                throws UsernameNotFoundException, DataAccessException {
            // Internal users authenticated via token file or API token.
            // The SecurityFilter calls this to reload authorities after
            // authenticate() returns. We return a simple UserDetails so
            // the authentication survives the reload.
            if ("varroa-mite".equals(username) || SYSTEM_AUTH.equals(username)) {
                User u = User.getById(username, false);
                if (u != null) {
                    GrantedAuthority[] authorities = new GrantedAuthority[]{
                        new GrantedAuthorityImpl("authenticated")
                    };
                    return new org.acegisecurity.userdetails.User(
                        username, "", true, true, true, true, authorities);
                }
            }

            // Handle OIDC-cookie / vk_ Bearer users.
            // The SecurityFilter calls this after the filter injects authentication,
            // and we must return valid UserDetails so the principal survives reload.
            User u = User.getById(username, false);
            if (u != null) {
                GrantedAuthority[] authorities = new GrantedAuthority[]{
                    new GrantedAuthorityImpl("authenticated")
                };
                return new org.acegisecurity.userdetails.User(
                    username, "", true, true, true, true, authorities);
            }

            throw new UsernameNotFoundException("User not found: " + username);
        }
    }

    // ---- Descriptor for JCasC ----

    /**
     * Descriptor for {@link VarroaSecurityRealm}.
     *
     * <p>Published under the {@code varroaMiteAuth} symbol so JCasC documents
     * can reference the realm as {@code securityRealm: { varroaMiteAuth: {...} }}.</p>
     */
    @Extension
    @Symbol("varroaMiteAuth")
    public static final class DescriptorImpl extends Descriptor<SecurityRealm> {

        @Override
        public String getDisplayName() {
            return "Varroa Mite Auth (OIDC + operator JWT)";
        }
    }

    // ---- XStream backward compatibility ----

    /**
     * Ensures the {@code apiKeyValidator} is initialised after XStream
     * deserialisation (it may be {@code null} when loaded from an older
     * {@code config.xml} where the field did not exist).
     */
    private Object readResolve() {
        if (apiKeyValidator == null) {
            apiKeyValidator = new ApiKeyValidator();
        }
        return this;
    }
}
