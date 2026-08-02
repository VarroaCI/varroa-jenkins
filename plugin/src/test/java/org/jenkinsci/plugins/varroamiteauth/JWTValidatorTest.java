package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import org.junit.After;
import org.junit.Before;
import org.junit.Test;

import java.io.OutputStream;
import java.math.BigInteger;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.security.KeyPair;
import java.security.KeyPairGenerator;
import java.security.PrivateKey;
import java.security.Signature;
import java.security.interfaces.RSAPublicKey;
import java.util.Base64;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * Unit tests for JWTValidator fail-closed validation against a local
 * plain-HTTP server (com.sun.net.httpserver ships with the JDK).
 */
public class JWTValidatorTest {

    private static final String KID = "test-key-1";

    private HttpServer server;
    private KeyPair keyPair;
    private String baseUrl;

    @Before
    public void startServer() throws Exception {
        KeyPairGenerator gen = KeyPairGenerator.getInstance("RSA");
        gen.initialize(2048);
        keyPair = gen.generateKeyPair();

        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/.well-known/openid-configuration", this::handleDiscovery);
        server.createContext("/jwks", this::handleJwks);
        server.start();

        int port = server.getAddress().getPort();
        baseUrl = "http://127.0.0.1:" + port;
    }

    @After
    public void stopServer() {
        if (server != null) {
            server.stop(0);
        }
    }

    private void handleDiscovery(HttpExchange exchange) throws java.io.IOException {
        String jwksUri = baseUrl + "/jwks";
        String body = "{\"issuer\":\"" + baseUrl + "\",\"jwks_uri\":\"" + jwksUri + "\"}";
        byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "application/json");
        exchange.sendResponseHeaders(200, bytes.length);
        try (OutputStream os = exchange.getResponseBody()) {
            os.write(bytes);
        }
        exchange.close();
    }

    private void handleJwks(HttpExchange exchange) throws java.io.IOException {
        RSAPublicKey pub = (RSAPublicKey) keyPair.getPublic();
        String n = Base64.getUrlEncoder().withoutPadding().encodeToString(toUnsignedBytes(pub.getModulus()));
        String e = Base64.getUrlEncoder().withoutPadding().encodeToString(toUnsignedBytes(pub.getPublicExponent()));
        String body = "{\"keys\":[{\"kty\":\"RSA\",\"use\":\"sig\",\"kid\":\"" + KID
                + "\",\"n\":\"" + n + "\",\"e\":\"" + e + "\"}]}";
        byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "application/json");
        exchange.sendResponseHeaders(200, bytes.length);
        try (OutputStream os = exchange.getResponseBody()) {
            os.write(bytes);
        }
        exchange.close();
    }

    /**
     * Converts a BigInteger to a byte array suitable for base64url encoding,
     * stripping a leading 0x00 sign byte if present.
     */
    private static byte[] toUnsignedBytes(BigInteger value) {
        byte[] raw = value.toByteArray();
        if (raw.length > 1 && raw[0] == 0) {
            byte[] trimmed = new byte[raw.length - 1];
            System.arraycopy(raw, 1, trimmed, 0, trimmed.length);
            return trimmed;
        }
        return raw;
    }

    /**
     * Builds and signs a JWT with the given claims using RS256.
     * Header: {"alg":"RS256","kid":"<KID>"}
     * Returns the three-part base64url-encoded JWT string.
     */
    private String sign(Map<String, Object> claims) throws Exception {
        // Header
        String headerJson = "{\"alg\":\"RS256\",\"kid\":\"" + KID + "\"}";
        String headerB64 = Base64.getUrlEncoder().withoutPadding()
                .encodeToString(headerJson.getBytes(StandardCharsets.UTF_8));

        // Payload
        StringBuilder payloadJson = new StringBuilder("{");
        boolean first = true;
        for (Map.Entry<String, Object> entry : claims.entrySet()) {
            if (!first) payloadJson.append(",");
            first = false;
            payloadJson.append("\"").append(entry.getKey()).append("\":");
            Object val = entry.getValue();
            if (val == null) {
                payloadJson.append("null");
            } else if (val instanceof String) {
                payloadJson.append("\"").append(val).append("\"");
            } else if (val instanceof Number) {
                payloadJson.append(val.toString());
            } else if (val instanceof List) {
                payloadJson.append("[");
                List<?> list = (List<?>) val;
                for (int i = 0; i < list.size(); i++) {
                    if (i > 0) payloadJson.append(",");
                    payloadJson.append("\"").append(list.get(i)).append("\"");
                }
                payloadJson.append("]");
            } else {
                payloadJson.append("\"").append(val.toString()).append("\"");
            }
        }
        payloadJson.append("}");
        String payloadB64 = Base64.getUrlEncoder().withoutPadding()
                .encodeToString(payloadJson.toString().getBytes(StandardCharsets.UTF_8));

        // Sign
        String message = headerB64 + "." + payloadB64;
        Signature sig = Signature.getInstance("SHA256withRSA");
        sig.initSign((PrivateKey) keyPair.getPrivate());
        sig.update(message.getBytes(StandardCharsets.UTF_8));
        byte[] signature = sig.sign();
        String sigB64 = Base64.getUrlEncoder().withoutPadding().encodeToString(signature);

        return headerB64 + "." + payloadB64 + "." + sigB64;
    }

    private Map<String, Object> validClaims() {
        Map<String, Object> claims = new HashMap<>();
        claims.put("exp", System.currentTimeMillis() / 1000 + 3600);
        claims.put("iss", baseUrl);
        claims.put("aud", "test-client");
        return claims;
    }

    private String publicKeyPEM() {
        return "-----BEGIN PUBLIC KEY-----\n"
                + Base64.getEncoder().encodeToString(keyPair.getPublic().getEncoded())
                + "\n-----END PUBLIC KEY-----";
    }

    private Map<String, Object> operatorClaims(String sub) {
        Map<String, Object> claims = new HashMap<>();
        claims.put("iss", "varroa-operator");
        claims.put("sub", sub);
        claims.put("aud", "team-a/controller");
        claims.put("exp", System.currentTimeMillis() / 1000 + 3600);
        return claims;
    }

    // ---------------------------------------------------------------
    // Tests
    // ---------------------------------------------------------------

    @Test
    public void validTokenReturnsNonNull() throws Exception {
        JWTValidator v = new JWTValidator(baseUrl, "test-client");
        String token = sign(validClaims());
        assertNotNull("valid token should be accepted", v.validate(token));
    }

    @Test
    public void missingExpReturnsNull() throws Exception {
        JWTValidator v = new JWTValidator(baseUrl, "test-client");
        Map<String, Object> claims = validClaims();
        claims.remove("exp");
        String token = sign(claims);
        assertNull("token missing exp should be rejected", v.validate(token));
    }

    @Test
    public void expiredExpReturnsNull() throws Exception {
        JWTValidator v = new JWTValidator(baseUrl, "test-client");
        Map<String, Object> claims = validClaims();
        claims.put("exp", System.currentTimeMillis() / 1000 - 10);
        String token = sign(claims);
        assertNull("expired token should be rejected", v.validate(token));
    }

    @Test
    public void missingIssReturnsNull() throws Exception {
        JWTValidator v = new JWTValidator(baseUrl, "test-client");
        Map<String, Object> claims = validClaims();
        claims.remove("iss");
        String token = sign(claims);
        assertNull("token missing iss should be rejected", v.validate(token));
    }

    @Test
    public void wrongIssReturnsNull() throws Exception {
        JWTValidator v = new JWTValidator(baseUrl, "test-client");
        Map<String, Object> claims = validClaims();
        claims.put("iss", "http://evil.example.com");
        String token = sign(claims);
        assertNull("token with wrong iss should be rejected", v.validate(token));
    }

    @Test
    public void missingAudReturnsNull() throws Exception {
        JWTValidator v = new JWTValidator(baseUrl, "test-client");
        Map<String, Object> claims = validClaims();
        claims.remove("aud");
        String token = sign(claims);
        assertNull("token missing aud should be rejected", v.validate(token));
    }

    @Test
    public void wrongAudReturnsNull() throws Exception {
        JWTValidator v = new JWTValidator(baseUrl, "test-client");
        Map<String, Object> claims = validClaims();
        claims.put("aud", "wrong-client");
        String token = sign(claims);
        assertNull("token with wrong aud should be rejected", v.validate(token));
    }

    @Test
    public void audAsListContainingClientIdReturnsNonNull() throws Exception {
        JWTValidator v = new JWTValidator(baseUrl, "test-client");
        Map<String, Object> claims = validClaims();
        claims.put("aud", List.of("test-client"));
        String token = sign(claims);
        assertNotNull("token with aud as list containing client id should be accepted",
                v.validate(token));
    }

    @Test
    public void userOperatorTokenIsAcceptedWithoutSystemSubject() throws Exception {
        JWTValidator v = new JWTValidator(null);
        Map<String, Object> claims = operatorClaims("oidc-subject");
        claims.put("varroa_typ", "user");
        claims.put("groups", List.of("platform-team", "ROLE:varroa:system-mite"));

        Map<String, Object> validated = v.validateOperatorToken(sign(claims), publicKeyPEM(), KID,
                "team-a/controller");
        assertNotNull("user token should not require a system subject", validated);
        assertEquals("user", validated.get("varroa_typ"));
        assertEquals(claims.get("groups"), validated.get("groups"));
    }

    @Test
    public void userOperatorTokenRequiresSubject() throws Exception {
        JWTValidator v = new JWTValidator(null);
        Map<String, Object> claims = operatorClaims("");
        claims.put("varroa_typ", "user");

        assertNull("user token without sub must be rejected",
                v.validateOperatorToken(sign(claims), publicKeyPEM(), KID, "team-a/controller"));
    }

    @Test
    public void unknownOperatorTokenTypeIsRejected() throws Exception {
        JWTValidator v = new JWTValidator(null);
        Map<String, Object> claims = operatorClaims("oidc-subject");
        claims.put("varroa_typ", "service");

        assertNull("unknown varroa_typ must be rejected",
                v.validateOperatorToken(sign(claims), publicKeyPEM(), KID, "team-a/controller"));
    }

    @Test
    public void nullOperatorTokenTypeIsRejected() throws Exception {
        JWTValidator v = new JWTValidator(null);
        Map<String, Object> claims = operatorClaims("system:varroa-mite");
        claims.put("varroa_typ", null);

        assertNull("an explicitly present null varroa_typ must be rejected",
                v.validateOperatorToken(sign(claims), publicKeyPEM(), KID, "team-a/controller"));
    }

    @Test
    public void systemOperatorTokensRemainAcceptedWithoutType() throws Exception {
        JWTValidator v = new JWTValidator(null);
        for (String sub : List.of("system:varroa-mite", "system:varroa-operator")) {
            Map<String, Object> claims = operatorClaims(sub);
            Map<String, Object> validated = v.validateOperatorToken(sign(claims), publicKeyPEM(), KID,
                    "team-a/controller");
            assertNotNull("system token should remain accepted for " + sub, validated);
            assertFalse("system token must not gain varroa_typ", validated.containsKey("varroa_typ"));
        }
    }
}
