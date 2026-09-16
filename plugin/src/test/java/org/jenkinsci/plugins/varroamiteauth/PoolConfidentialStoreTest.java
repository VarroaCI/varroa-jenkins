package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertArrayEquals;
import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotNull;

import hudson.util.Secret;

import java.io.File;
import java.io.IOException;
import java.io.InputStream;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Properties;

import jenkins.slaves.JnlpAgentReceiver;

import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Proves the byte format of the operator's confidential-store writer against
 * a real Jenkins.
 *
 * <p>Every cell of a pool boots on one operator-minted {@code secrets/}
 * directory, so an inbound agent secret minted by one cell stays valid on
 * every other cell of that pool. {@code DefaultConfidentialStore} answers
 * {@code null} for a file whose magic trailer does not verify, and Jenkins
 * then generates a fresh key with no error, so a wrong byte would fail
 * silently in a cluster. This test is the guard.
 *
 * <p>The fixture under {@code confidentialstore/} is the Go writer's own
 * output. Regenerate it with {@code go run ./hack/gen-cell-secrets-fixture}.
 */
public class PoolConfidentialStoreTest {

    /** The four keys a complete pool store holds. */
    private static final String[] KEYS = {
        "master.key",
        "jenkins.slaves.JnlpSlaveAgentProtocol.secret",
        "hudson.util.Secret",
        "org.jenkinsci.main.modules.instance_identity.InstanceIdentity.KEY",
    };

    /** The node name the fixture records an agent secret for. */
    private static final String MAC_MESSAGE = "x";

    @Rule
    public JenkinsRule rule = new JenkinsRule().with(PoolConfidentialStoreTest::seededHome);

    /** Builds a JENKINS_HOME whose secrets/ directory holds the fixture. */
    private static File seededHome() throws IOException {
        File home = Files.createTempDirectory("varroa-pool-secrets").toFile();
        Path secrets = home.toPath().resolve("secrets");
        Files.createDirectories(secrets);
        for (String key : KEYS) {
            Files.write(secrets.resolve(key), fixture(key));
        }
        return home;
    }

    /** Reads one fixture file from the test resources. */
    private static byte[] fixture(String name) throws IOException {
        try (InputStream in = PoolConfidentialStoreTest.class.getResourceAsStream("confidentialstore/" + name)) {
            assertNotNull("the fixture " + name + " is absent", in);
            return in.readAllBytes();
        }
    }

    /** Reads the value the Go writer recorded for the agent secret. */
    private static String expectedMac() throws IOException {
        Properties props = new Properties();
        try (InputStream in =
                PoolConfidentialStoreTest.class.getResourceAsStream("confidentialstore/expected.properties")) {
            assertNotNull("the fixture expected.properties is absent", in);
            props.load(in);
        }
        String mac = props.getProperty("jnlpMacFor" + MAC_MESSAGE);
        assertNotNull("the fixture records no agent secret", mac);
        return mac;
    }

    /**
     * Jenkins loads every seeded key, regenerates none, and computes the
     * agent secret the operator's own writer computes.
     */
    @Test
    public void jenkinsLoadsEverySeededKey() throws Exception {
        assertEquals("the agent secret differs from the writer's value",
                expectedMac(), JnlpAgentReceiver.SLAVE_SECRET.mac(MAC_MESSAGE));

        // Force a use of hudson.util.Secret, whose key loads lazily. A round
        // trip that works over an unchanged file proves Jenkins used the
        // seeded key.
        Secret secret = Secret.fromString("varroa");
        assertEquals("varroa", Secret.toString(Secret.decrypt(secret.getEncryptedValue())));

        Path secrets = rule.jenkins.getRootDir().toPath().resolve("secrets");
        for (String key : KEYS) {
            assertArrayEquals("Jenkins replaced the key " + key,
                    fixture(key), Files.readAllBytes(secrets.resolve(key)));
        }
    }
}
