package org.jenkinsci.plugins.varroamiteauth;

import java.math.BigInteger;
import java.net.URI;
import java.net.URL;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.security.KeyFactory;
import java.security.PublicKey;
import java.security.Signature;
import java.security.spec.RSAPublicKeySpec;
import java.time.Duration;
import java.util.Base64;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.logging.Logger;

import groovy.json.JsonSlurper;

/**
 * Validates JWT ID tokens using pure JDK (no external libraries).
 * Fetches JWKS from the OIDC issuer's discovery endpoint and caches keys.
 */
public class JWTValidator {

    private static final Logger LOGGER = Logger.getLogger(JWTValidator.class.getName());

    /** Subjects accepted for the operator-signed Bearer JWT (mite and operator identities). */
    private static final List<String> ACCEPTED_OPERATOR_SUBJECTS =
            List.of("system:varroa-mite", "system:varroa-operator");

    private final String oidcIssuer;
    private final String oidcClientId;
    private transient Map<String, PublicKey> jwksKeys;
    private transient volatile long jwksLoaded;

    public JWTValidator(String oidcIssuer) {
        this(oidcIssuer, System.getenv("VARROA_OIDC_CLIENT_ID"));
    }

    public JWTValidator(String oidcIssuer, String oidcClientId) {
        // Normalize: strip trailing slashes. The BFF adds them for go-oidc strict
        // issuer matching, but constructing URLs like issuer + "/.well-known/..."
        // would produce double slashes. The JWT iss claim also lacks a trailing slash
        // from some providers.
        while (oidcIssuer != null && oidcIssuer.endsWith("/")) {
            oidcIssuer = oidcIssuer.substring(0, oidcIssuer.length() - 1);
        }
        this.oidcIssuer = oidcIssuer;
        this.oidcClientId = oidcClientId != null ? oidcClientId : "";
    }

    private Map<String, PublicKey> jwksKeys() {
        // Lazily initialise: field initialisers do not run after XStream
        // deserialisation, so jwksKeys may be null on a reloaded realm.
        if (jwksKeys == null) {
            jwksKeys = new ConcurrentHashMap<>();
        }
        return jwksKeys;
    }

    public String getOidcIssuer() {
        return oidcIssuer;
    }

    /**
     * Load JWKS keys from the OIDC issuer's discovery endpoint.
     */
    public void loadJWKS() {
        if (oidcIssuer == null || oidcIssuer.isEmpty()) return;
        try {
            HttpClient client = HttpClient.newBuilder()
                    .connectTimeout(Duration.ofSeconds(10))
                    .build();

            // Fetch OIDC discovery
            String discoUrl = oidcIssuer + "/.well-known/openid-configuration";
            HttpRequest req = HttpRequest.newBuilder()
                    .uri(URI.create(discoUrl))
                    .timeout(Duration.ofSeconds(10))
                    .GET()
                    .build();
            HttpResponse<String> resp = client.send(req, HttpResponse.BodyHandlers.ofString());
            if (resp.statusCode() != 200) {
                LOGGER.warning("varroa-mite-auth: OIDC discovery returned " + resp.statusCode());
                return;
            }

            JsonSlurper slurper = new JsonSlurper();
            Map<String, Object> disco = (Map<String, Object>) slurper.parseText(resp.body());
            String jwksUri = (String) disco.get("jwks_uri");
            if (jwksUri == null) {
                LOGGER.warning("varroa-mite-auth: no jwks_uri in discovery");
                return;
            }

            // Fetch JWKS
            req = HttpRequest.newBuilder()
                    .uri(URI.create(jwksUri))
                    .timeout(Duration.ofSeconds(10))
                    .GET()
                    .build();
            resp = client.send(req, HttpResponse.BodyHandlers.ofString());
            if (resp.statusCode() != 200) {
                LOGGER.warning("varroa-mite-auth: JWKS fetch returned " + resp.statusCode());
                return;
            }

            Map<String, Object> jwks = (Map<String, Object>) slurper.parseText(resp.body());
            List<Map<String, Object>> keys = (List<Map<String, Object>>) jwks.get("keys");
            if (keys == null) {
                LOGGER.warning("varroa-mite-auth: no keys in JWKS");
                return;
            }

            jwksKeys().clear();
            for (Map<String, Object> k : keys) {
                String kty = (String) k.get("kty");
                String use = (String) k.get("use");
                if ("RSA".equals(kty) && "sig".equals(use)) {
                    BigInteger modulus = base64ToBigInt((String) k.get("n"));
                    BigInteger exponent = base64ToBigInt((String) k.get("e"));
                    RSAPublicKeySpec spec = new RSAPublicKeySpec(modulus, exponent);
                    PublicKey key = KeyFactory.getInstance("RSA").generatePublic(spec);
                    jwksKeys().put((String) k.get("kid"), key);
                }
            }
            jwksLoaded = System.currentTimeMillis();
            LOGGER.info("varroa-mite-auth: loaded " + jwksKeys().size() + " JWKS keys");
        } catch (Exception e) {
            LOGGER.warning("varroa-mite-auth: JWKS load failed: " + e.getMessage());
        }
    }

    /**
     * Validates a JWT ID token and returns the claims map, or null if invalid.
     */
    public Map<String, Object> validate(String token) {
        if (jwksKeys().isEmpty()) loadJWKS();
        try {
            String[] parts = token.split("\\.");
            if (parts.length != 3) return null;

            // Parse header to get kid
            String headerJson = new String(Base64.getUrlDecoder().decode(parts[0]));
            Map<String, Object> header = (Map<String, Object>) new JsonSlurper().parseText(headerJson);
            String kid = (String) header.get("kid");

            PublicKey key = jwksKeys().get(kid);
            if (key == null) {
                loadJWKS();
                key = jwksKeys().get(kid);
            }
            if (key == null) {
                LOGGER.warning("varroa-mite-auth: no key for kid=" + kid);
                return null;
            }

            // Verify signature
            Signature sig = Signature.getInstance("SHA256withRSA");
            sig.initVerify(key);
            sig.update((parts[0] + "." + parts[1]).getBytes());
            if (!sig.verify(Base64.getUrlDecoder().decode(parts[2]))) {
                LOGGER.warning("varroa-mite-auth: JWT signature invalid");
                return null;
            }

            // Parse payload
            String payloadJson = new String(Base64.getUrlDecoder().decode(parts[1]));
            Map<String, Object> payload = (Map<String, Object>) new JsonSlurper().parseText(payloadJson);

            // Check expiry — REQUIRED (fail closed). A token with no exp must be rejected.
            if (!payload.containsKey("exp")) {
                LOGGER.warning("varroa-mite-auth: JWT missing exp");
                return null;
            }
            long exp = ((Number) payload.get("exp")).longValue();
            if (exp < System.currentTimeMillis() / 1000) {
                LOGGER.warning("varroa-mite-auth: JWT expired");
                return null;
            }

            // Validate issuer — REQUIRED when an issuer is configured (fail closed).
            if (oidcIssuer != null && !oidcIssuer.isEmpty()) {
                String iss = (String) payload.get("iss");
                if (iss == null) {
                    LOGGER.warning("varroa-mite-auth: JWT missing iss");
                    return null;
                }
                String normIss = iss;
                while (normIss.endsWith("/")) {
                    normIss = normIss.substring(0, normIss.length() - 1);
                }
                if (!oidcIssuer.equals(normIss)) {
                    LOGGER.warning("varroa-mite-auth: JWT iss mismatch: " + iss + " != " + oidcIssuer);
                    return null;
                }
            }

            // Validate audience — REQUIRED when a client id is configured (fail closed).
            if (oidcClientId != null && !oidcClientId.isEmpty()) {
                Object aud = payload.get("aud");
                if (aud instanceof String) {
                    if (!oidcClientId.equals(aud)) {
                        LOGGER.warning("varroa-mite-auth: JWT aud mismatch: " + aud + " != " + oidcClientId);
                        return null;
                    }
                } else if (aud instanceof List) {
                    if (!((List<?>) aud).contains(oidcClientId)) {
                        LOGGER.warning("varroa-mite-auth: JWT aud does not contain " + oidcClientId);
                        return null;
                    }
                } else {
                    // aud missing or unexpected type → reject.
                    LOGGER.warning("varroa-mite-auth: JWT missing or invalid aud");
                    return null;
                }
            }

            return payload;
        } catch (Exception e) {
            LOGGER.warning("varroa-mite-auth: JWT validation error: " + e.getMessage());
            return null;
        }
    }

    /**
     * Extracts the first present user claim from a validated JWT claims map.
     * Tries each claim name in order; returns the first non-empty string value.
     * Default claimNames: ["preferred_username", "sub"].
     */
    public static String getUserId(Map<String, Object> claims, List<String> claimNames) {
        for (String c : claimNames) {
            Object v = claims.get(c);
            if (v instanceof String && !((String) v).isEmpty()) return (String) v;
        }
        return null;
    }

    /**
     * Extracts the groups claim from a validated JWT claims map using a
     * configurable claim name. The OIDC provider returns groups as a JSON array
     * of strings.
     */
    public static List<String> getGroups(Map<String, Object> claims, String claimName) {
        Object groupsObj = claims.get(claimName != null ? claimName : "groups");
        if (!(groupsObj instanceof List)) {
            return List.of();
        }
        List<String> groups = new java.util.ArrayList<>();
        for (Object g : (List<?>) groupsObj) {
            if (g instanceof String && !((String) g).isEmpty()) {
                groups.add((String) g);
            }
        }
        return groups;
    }

    /**
     * Validates an operator-signed RS256 JWT (mite Jenkins auth).
     * Uses the operator's RSA public key from VARROA_MITE_PUBKEY_PEM.
     * Checks iss=="varroa-operator", expiry, and aud matching
     * VARROA_MITE_AUD (controller identity). Tokens without varroa_typ use
     * the system-subject allowlist; user tokens require a non-empty sub.
     * Returns claims map on success, null on failure.
     */
    public Map<String, Object> validateOperatorToken(String token) {
        String pubKeyPEM = System.getenv("VARROA_MITE_PUBKEY_PEM");
        if (pubKeyPEM == null || pubKeyPEM.isEmpty()) {
            LOGGER.warning("varroa-mite-auth: VARROA_MITE_PUBKEY_PEM not set");
            return null;
        }
        return validateOperatorToken(token, pubKeyPEM,
                System.getenv("VARROA_MITE_PUBKEY_KID"), System.getenv("VARROA_MITE_AUD"));
    }

    Map<String, Object> validateOperatorToken(String token, String pubKeyPEM,
            String expectedKid, String expectedAud) {
        try {
            // Parse PEM
            String pem = pubKeyPEM
                .replace("-----BEGIN PUBLIC KEY-----", "")
                .replace("-----END PUBLIC KEY-----", "")
                .replaceAll("\\s", "");
            byte[] der = Base64.getDecoder().decode(pem);
            java.security.spec.X509EncodedKeySpec spec =
                new java.security.spec.X509EncodedKeySpec(der);
            PublicKey pubKey = KeyFactory.getInstance("RSA").generatePublic(spec);

            String[] parts = token.split("\\.");
            if (parts.length != 3) return null;

            // Verify signature
            Signature sig = Signature.getInstance("SHA256withRSA");
            sig.initVerify(pubKey);
            sig.update((parts[0] + "." + parts[1]).getBytes());
            if (!sig.verify(Base64.getUrlDecoder().decode(parts[2]))) {
                LOGGER.warning("varroa-mite-auth: operator JWT signature invalid");
                return null;
            }

            // Verify kid matches configured key (prerequisite for key rotation).
            String opHeaderJson = new String(Base64.getUrlDecoder().decode(parts[0]));
            Map<String, Object> opHeader = (Map<String, Object>) new JsonSlurper().parseText(opHeaderJson);
            if (expectedKid != null && !expectedKid.isEmpty()) {
                String opKid = (String) opHeader.get("kid");
                if (!expectedKid.equals(opKid)) {
                    LOGGER.warning("varroa-mite-auth: operator JWT kid mismatch: " + opKid + " != " + expectedKid);
                    return null;
                }
            }

            // Parse payload
            String payloadJson = new String(Base64.getUrlDecoder().decode(parts[1]));
            Map<String, Object> payload = (Map<String, Object>) new JsonSlurper().parseText(payloadJson);

            // Check expiry (required — tokens without exp are rejected).
            if (!payload.containsKey("exp")) {
                LOGGER.warning("varroa-mite-auth: operator JWT missing exp");
                return null;
            }
            long exp = ((Number) payload.get("exp")).longValue();
            if (exp < System.currentTimeMillis() / 1000) {
                LOGGER.warning("varroa-mite-auth: operator JWT expired");
                return null;
            }

            // Validate claims
            if (!"varroa-operator".equals(payload.get("iss"))) {
                LOGGER.warning("varroa-mite-auth: operator JWT iss mismatch");
                return null;
            }
            Object sub = payload.get("sub");
            Object tokenType = payload.get("varroa_typ");
            if (!payload.containsKey("varroa_typ")) {
                if (!ACCEPTED_OPERATOR_SUBJECTS.contains(sub)) {
                    LOGGER.warning("varroa-mite-auth: operator JWT sub mismatch: " + sub);
                    return null;
                }
            } else if ("user".equals(tokenType)) {
                if (!(sub instanceof String) || ((String) sub).isEmpty()) {
                    LOGGER.warning("varroa-mite-auth: user operator JWT missing sub");
                    return null;
                }
            } else {
                LOGGER.warning("varroa-mite-auth: operator JWT unknown varroa_typ: " + tokenType);
                return null;
            }

            // Validate audience (required — tokens without matching aud are rejected).
            if (expectedAud == null || expectedAud.isEmpty()) {
                LOGGER.warning("varroa-mite-auth: VARROA_MITE_AUD not set; rejecting operator JWT");
                return null;
            }
            if (!expectedAud.equals(payload.get("aud"))) {
                LOGGER.warning("varroa-mite-auth: operator JWT aud mismatch");
                return null;
            }

            return payload;
        } catch (Exception e) {
            LOGGER.warning("varroa-mite-auth: operator JWT validation error: " + e.getMessage());
            return null;
        }
    }

    private static BigInteger base64ToBigInt(String b64) {
        return new BigInteger(1, Base64.getUrlDecoder().decode(b64));
    }
}
