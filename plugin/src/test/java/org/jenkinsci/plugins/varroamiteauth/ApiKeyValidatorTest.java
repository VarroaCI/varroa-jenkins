package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import org.junit.After;
import org.junit.Before;
import org.junit.Test;

import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.Map;
import java.util.concurrent.atomic.AtomicInteger;

/**
 * Unit tests for ApiKeyValidator cache semantics against a local plain-HTTP
 * server (com.sun.net.httpserver ships with the JDK — no new dependency).
 * The validator accepts http:// URLs without a CA PEM precisely to enable
 * these tests; production always configures https.
 */
public class ApiKeyValidatorTest {

    private static final String IDENTITY_JSON =
            "{\"subject\":\"sub-1\",\"preferredUsername\":\"jdoe\","
            + "\"email\":\"jdoe@example.com\",\"name\":\"J Doe\",\"groups\":[\"team-a\"]}";

    // Well-formed vk_ tokens: 13-char base32 prefix + "." + 43-char secret.
    private static final String PREFIX = "0123456789abc";
    private static final String SECRET_A = "A".repeat(43);
    private static final String SECRET_B = "B".repeat(43);
    private static final String TOKEN_A = "vk_" + PREFIX + "." + SECRET_A;
    private static final String TOKEN_B = "vk_" + PREFIX + "." + SECRET_B;

    private HttpServer server;
    private final AtomicInteger hits = new AtomicInteger();
    private volatile int responseStatus = 200;
    private volatile String responseBody = IDENTITY_JSON;

    @Before
    public void startServer() throws Exception {
        hits.set(0);
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/v1/verify-apikey", this::handle);
        server.start();
    }

    @After
    public void stopServer() {
        if (server != null) {
            server.stop(0);
        }
    }

    private void handle(HttpExchange exchange) throws java.io.IOException {
        hits.incrementAndGet();
        byte[] body = responseStatus == 200
                ? responseBody.getBytes(StandardCharsets.UTF_8) : new byte[0];
        exchange.getResponseHeaders().set("Content-Type", "application/json");
        exchange.sendResponseHeaders(responseStatus, body.length == 0 ? -1 : body.length);
        if (body.length > 0) {
            try (OutputStream os = exchange.getResponseBody()) {
                os.write(body);
            }
        }
        exchange.close();
    }

    private ApiKeyValidator newValidator() {
        String url = "http://127.0.0.1:" + server.getAddress().getPort() + "/v1/verify-apikey";
        return new ApiKeyValidator(url, null);
    }

    @Test
    public void validTokenReturnsIdentityAndCaches() {
        ApiKeyValidator v = newValidator();
        Map<String, Object> first = v.validate(TOKEN_A);
        assertNotNull(first);
        assertEquals("jdoe", first.get("preferredUsername"));
        assertEquals("sub-1", first.get("subject"));

        // Second call within the positive TTL must be served from cache.
        Map<String, Object> second = v.validate(TOKEN_A);
        assertNotNull(second);
        assertEquals(1, hits.get());
    }

    @Test
    public void samePrefixDifferentSecretMissesCache() {
        ApiKeyValidator v = newValidator();
        assertNotNull(v.validate(TOKEN_A));
        // TOKEN_B shares the public prefix but differs in secret: the cache is
        // keyed on SHA-256 of the full token, so this must NOT hit TOKEN_A's
        // entry — the server must be consulted again.
        assertNotNull(v.validate(TOKEN_B));
        assertEquals(2, hits.get());
    }

    @Test
    public void rejectionIsNegativeCached() {
        responseStatus = 401;
        ApiKeyValidator v = newValidator();
        assertNull(v.validate(TOKEN_A));
        // Immediate retry rides the 10s negative cache — no second call.
        assertNull(v.validate(TOKEN_A));
        assertEquals(1, hits.get());
    }

    @Test
    public void unavailableIsNotCached() {
        responseStatus = 503;
        ApiKeyValidator v = newValidator();
        assertNull(v.validate(TOKEN_A));
        // 503 must not be cached: the next request hits the server again.
        assertNull(v.validate(TOKEN_A));
        assertEquals(2, hits.get());
    }

    @Test
    public void disabledModeMakesNoCalls() {
        ApiKeyValidator v = new ApiKeyValidator(null, null);
        assertFalse(v.isEnabled());
        assertNull(v.validate(TOKEN_A));
        assertEquals(0, hits.get());
    }

    @Test
    public void httpsWithoutCaPemIsDisabled() {
        ApiKeyValidator v = new ApiKeyValidator("https://example.invalid/v1/verify-apikey", null);
        assertFalse(v.isEnabled());
        assertNull(v.validate(TOKEN_A));
        assertEquals(0, hits.get());
    }

    @Test
    public void cachedIdentityIsACopy() {
        ApiKeyValidator v = newValidator();
        Map<String, Object> first = v.validate(TOKEN_A);
        assertNotNull(first);
        first.put("preferredUsername", "tampered");
        Map<String, Object> second = v.validate(TOKEN_A);
        assertEquals("jdoe", second.get("preferredUsername"));
    }
}
