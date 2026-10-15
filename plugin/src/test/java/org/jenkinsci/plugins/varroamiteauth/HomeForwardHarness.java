package org.jenkinsci.plugins.varroamiteauth;

import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Map;

import org.junit.rules.ExternalResource;

/**
 * Shared harness for the forwarding tests: it starts a {@link
 * StubLoopback}, builds a client against it, and seeds a {@link
 * VarroaHomeCache} through the route the plugin itself reads.
 *
 * <p>A test declares this rule beside its own {@code JenkinsRule}. The
 * cache is seeded by a real refresh, so every test reads the same wire
 * shape the mite serves.
 */
final class HomeForwardHarness extends ExternalResource {

    private final StubLoopback stub = new StubLoopback();

    private Path tempDir;
    private MiteLoopbackClient client;
    private VarroaHomeCache cache;

    @Override
    protected void before() throws Throwable {
        stub.start();
        tempDir = Files.createTempDirectory("varroa-home-forward");
        client = stub.client(tempDir.resolve("token"));
        cache = new VarroaHomeCache(client);
    }

    @Override
    protected void after() {
        stub.stop();
    }

    StubLoopback stub() {
        return stub;
    }

    MiteLoopbackClient client() {
        return client;
    }

    VarroaHomeCache cache() {
        return cache;
    }

    /**
     * Seeds one hosted tenant and refreshes the cache. {@code jobHomes}
     * maps a full job path to its home slot.
     */
    void seedHomes(String tenant, int heldSlot, String role, Map<String, Integer> jobHomes) {
        StringBuilder homes = new StringBuilder();
        for (Map.Entry<String, Integer> entry : jobHomes.entrySet()) {
            if (homes.length() > 0) {
                homes.append(',');
            }
            homes.append('"').append(entry.getKey()).append("\":").append(entry.getValue());
        }
        stub.setHomesBody("{\"entries\":[{\"tenant\":\"" + tenant + "\",\"slot\":" + heldSlot
                + ",\"role\":\"" + role + "\",\"job_homes\":{" + homes + "}}]}");
        cache.refresh();
    }

    /** Seeds an empty entry list and refreshes, so the cache holds no opinion. */
    void seedNoOpinion() {
        stub.setHomesBody("{\"entries\":[]}");
        cache.refresh();
    }
}
