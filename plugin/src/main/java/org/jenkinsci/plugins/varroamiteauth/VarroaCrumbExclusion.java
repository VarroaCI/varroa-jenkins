package org.jenkinsci.plugins.varroamiteauth;

import hudson.Extension;
import hudson.security.SecurityRealm;
import hudson.security.csrf.CrumbExclusion;
import jenkins.model.Jenkins;

import jakarta.servlet.FilterChain;
import jakarta.servlet.ServletException;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;
import java.io.IOException;
import java.util.logging.Logger;

/**
 * Exempts mite requests and vk_* API key requests from CSRF crumb validation.
 *
 * The mite authenticates with an operator-signed RS256 JWT sent as an
 * {@code Authorization: Bearer} header — a stateless, non-interactive
 * credential that cannot be exploited via CSRF (a browser never attaches it
 * automatically). Similarly, vk_* tokens are non-interactive credentials that
 * browsers never send automatically.
 *
 * Routing order: if the Bearer starts with {@code vk_}, exempt iff the
 * (cached) ApiKeyValidator validation succeeds. Otherwise, perform the
 * existing operator-token validation for non-vk_ Bearers.
 */
@Extension
public class VarroaCrumbExclusion extends CrumbExclusion {

    private static final Logger LOGGER = Logger.getLogger(VarroaCrumbExclusion.class.getName());

    @Override
    public boolean process(HttpServletRequest req, HttpServletResponse resp, FilterChain chain)
            throws IOException, ServletException {
        String authHeader = req.getHeader("Authorization");
        if (authHeader == null || !authHeader.startsWith("Bearer ")) {
            return false;
        }

        Jenkins jenkins = Jenkins.getInstanceOrNull();
        if (jenkins == null) {
            return false;
        }
        SecurityRealm realm = jenkins.getSecurityRealm();
        if (!(realm instanceof VarroaSecurityRealm)) {
            return false;
        }

        VarroaSecurityRealm vsr = (VarroaSecurityRealm) realm;
        String token = authHeader.substring(7);

        // vk_* Bearer: exempt iff cached validation succeeds.
        if (token.startsWith("vk_")) {
            if (vsr.getApiKeyValidator().validate(token) != null) {
                LOGGER.fine("varroa-mite-auth: crumb-exempt request for valid vk_ token");
                chain.doFilter(req, resp);
                return true;
            }
            return false;
        }

        // Non-vk_ Bearer: existing operator-JWT validation.
        JWTValidator validator = vsr.getJWTValidator();
        if (validator.validateOperatorToken(token) == null) {
            return false;
        }

        LOGGER.fine("varroa-mite-auth: crumb-exempt request for valid operator token");
        chain.doFilter(req, resp);
        return true;
    }
}
