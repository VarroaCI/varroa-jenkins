package org.jenkinsci.plugins.varroamiteauth;

import groovy.json.JsonSlurper;

import javax.net.ssl.SSLContext;
import javax.net.ssl.TrustManagerFactory;
import java.io.ByteArrayInputStream;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.security.KeyStore;
import java.security.MessageDigest;
import java.security.cert.CertificateFactory;
import java.security.cert.X509Certificate;
import java.time.Duration;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.logging.Logger;

/**
 * Validates Varroa API keys (vk_* tokens) against the gateway verify endpoint.
 * <p>
 * Caches verification results in-process keyed on SHA-256 of the full token.
 * Positive results are cached for 60 seconds; definitive 401 rejection for 10
 * seconds. Transport errors and 503 responses are never cached.
 */
public class ApiKeyValidator {

    private static final Logger LOGGER = Logger.getLogger(ApiKeyValidator.class.getName());

    private static final long POSITIVE_TTL_MILLIS = 60_000;
    private static final long NEGATIVE_TTL_MILLIS = 10_000;
    private static final int MAX_CACHE_SIZE = 10_000;
    private static final Duration CONNECT_TIMEOUT = Duration.ofSeconds(5);
    private static final Duration REQUEST_TIMEOUT = Duration.ofSeconds(5);

    private final String verifyURL;
    private final String caPEM;
    private transient volatile HttpClient httpClient;
    private transient ConcurrentHashMap<String, CacheEntry> cache;
    private transient volatile boolean enabled;
    private transient volatile boolean inited;
    private transient volatile long lastWarnMillis;

    private static class CacheEntry {
        final Map<String, Object> identity; // null means negative entry
        final long expiresAtMillis;

        CacheEntry(Map<String, Object> identity, long expiresAtMillis) {
            this.identity = identity;
            this.expiresAtMillis = expiresAtMillis;
        }

        boolean isLive() {
            return System.currentTimeMillis() < expiresAtMillis;
        }
    }

    public ApiKeyValidator() {
        this(System.getenv("VARROA_APIKEY_VERIFY_URL"), System.getenv("VARROA_CA_PEM"));
    }

    // Package-private constructor for testing.
    ApiKeyValidator(String verifyURL, String caPEM) {
        this.verifyURL = (verifyURL != null && !verifyURL.isEmpty()) ? verifyURL : null;
        this.caPEM = caPEM;
        // Defer HttpClient and cache construction to avoid XStream serialization
        // failures (jdk.internal.net.http.HttpClientFacade is blocked by Jenkins)
    }

    private synchronized void ensureInit() {
        if (inited) return;
        inited = true;
        this.cache = new ConcurrentHashMap<>();

        if (verifyURL == null) {
            this.enabled = false;
            LOGGER.info("varroa-mite-auth: ApiKeyValidator disabled (VARROA_APIKEY_VERIFY_URL not set)");
            return;
        }

        HttpClient.Builder builder = HttpClient.newBuilder()
                .connectTimeout(CONNECT_TIMEOUT);

        if (verifyURL.startsWith("https")) {
            if (caPEM == null || caPEM.isEmpty()) {
                this.enabled = false;
                LOGGER.warning("varroa-mite-auth: ApiKeyValidator disabled (VARROA_APIKEY_VERIFY_URL is https but VARROA_CA_PEM not set)");
                return;
            }
            try {
                CertificateFactory cf = CertificateFactory.getInstance("X.509");
                X509Certificate cert = (X509Certificate) cf.generateCertificate(
                        new ByteArrayInputStream(caPEM.getBytes()));

                KeyStore keyStore = KeyStore.getInstance(KeyStore.getDefaultType());
                keyStore.load(null, null);
                keyStore.setCertificateEntry("varroa-ca", cert);

                TrustManagerFactory tmf = TrustManagerFactory.getInstance(
                        TrustManagerFactory.getDefaultAlgorithm());
                tmf.init(keyStore);

                SSLContext sslContext = SSLContext.getInstance("TLSv1.3");
                sslContext.init(null, tmf.getTrustManagers(), null);

                builder.sslContext(sslContext);
            } catch (Exception e) {
                this.enabled = false;
                LOGGER.warning("varroa-mite-auth: ApiKeyValidator disabled (failed to configure TLS): " + e.getMessage());
                return;
            }
        }

        this.enabled = true;
        this.httpClient = builder.build();
        LOGGER.info("varroa-mite-auth: ApiKeyValidator enabled, verify URL: " + verifyURL);
    }

    /**
     * Validates a vk_ token against the gateway verify endpoint.
     *
     * @param token the full vk_ token string
     * @return the identity map (subject, preferredUsername, email, name, groups)
     *         or null if the token is invalid or verification unavailable
     */
    public Map<String, Object> validate(String token) {
        if (!inited) ensureInit();
        if (!enabled || token == null) {
            return null;
        }

        String cacheKey = sha256(token);

        // Check cache. Return a defensive copy so callers cannot mutate the
        // cached identity map.
        CacheEntry entry = cache.get(cacheKey);
        if (entry != null && entry.isLive()) {
            if (entry.identity != null) {
                return new java.util.HashMap<>(entry.identity);
            }
            return null;
        }

        // Perform verification.
        try {
            HttpRequest request = HttpRequest.newBuilder()
                    .uri(URI.create(verifyURL))
                    .header("Authorization", "Bearer " + token)
                    .timeout(REQUEST_TIMEOUT)
                    .POST(HttpRequest.BodyPublishers.noBody())
                    .build();

            HttpResponse<String> response = httpClient.send(request,
                    HttpResponse.BodyHandlers.ofString());

            if (response.statusCode() == 200) {
                JsonSlurper slurper = new JsonSlurper();
                Map<String, Object> identity = (Map<String, Object>) slurper.parseText(response.body());
                cache.put(cacheKey, new CacheEntry(identity, System.currentTimeMillis() + POSITIVE_TTL_MILLIS));
                evictStale();
                return new java.util.HashMap<>(identity);
            } else if (response.statusCode() == 401) {
                cache.put(cacheKey, new CacheEntry(null, System.currentTimeMillis() + NEGATIVE_TTL_MILLIS));
                evictStale();
                return null;
            } else {
                // 503 or other status — do not cache.
                logThrottled("verify endpoint returned " + response.statusCode());
                return null;
            }
        } catch (Exception e) {
            logThrottled("verify call failed: " + e.getMessage());
            return null;
        }
    }

    public boolean isEnabled() {
        if (!inited) ensureInit();
        return enabled;
    }

    private void logThrottled(String message) {
        long now = System.currentTimeMillis();
        if (now - lastWarnMillis > 60_000) {
            lastWarnMillis = now;
            LOGGER.warning("varroa-mite-auth: ApiKeyValidator: " + message);
        }
    }

    private void evictStale() {
        if (cache.size() > MAX_CACHE_SIZE) {
            long now = System.currentTimeMillis();
            cache.values().removeIf(e -> now >= e.expiresAtMillis);
        }
    }

    private static String sha256(String token) {
        try {
            MessageDigest md = MessageDigest.getInstance("SHA-256");
            byte[] hash = md.digest(token.getBytes());
            StringBuilder hex = new StringBuilder(64);
            for (byte b : hash) {
                hex.append(String.format("%02x", b & 0xff));
            }
            return hex.toString();
        } catch (Exception e) {
            throw new RuntimeException("SHA-256 not available", e);
        }
    }
}
