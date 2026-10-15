package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertNull;

import io.jenkins.plugins.artifact_manager_jclouds.s3.S3BlobStoreConfig;
import io.jenkins.plugins.aws.global_configuration.CredentialsAwsGlobalConfiguration;
import io.jenkins.plugins.casc.ConfigurationAsCode;

import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.nio.file.StandardCopyOption;
import java.nio.file.attribute.FileTime;
import java.time.Instant;

import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import software.amazon.awssdk.auth.credentials.AwsSessionCredentials;

/**
 * Proves the STS-mode contract of the operator's artifact-storage fragment.
 *
 * <p>In STS mode the fragment names no credential. artifact-manager-s3 then
 * asks {@code CredentialsAwsGlobalConfiguration} for session credentials,
 * which reads the AWS SDK default chain. The operator sets the JVM property
 * {@code aws.sharedCredentialsFile} to the projected credentials file and
 * rewrites that file before the credential expires. The SDK must return
 * the new credential without a restart and without a JCasC apply.
 *
 * <p>The surefire configuration sets the property for the whole test JVM,
 * because the SDK reads it once, when it first builds its default chain.
 */
public class ArtifactSessionCredentialsReloadTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Test
    public void stsFragmentSelectsTheDefaultChain() throws Exception {
        applyStsFragment();
        assertNull("STS mode must name no Jenkins credential",
                CredentialsAwsGlobalConfiguration.get().getCredentialsId());
        assertFalse("STS mode must keep session tokens",
                S3BlobStoreConfig.get().getDisableSessionToken());
        assertEquals("artifacts/core/varroa/cell-a/", S3BlobStoreConfig.get().getPrefix());
    }

    @Test
    public void sessionCredentialsFollowTheRewrittenFile() throws Exception {
        String property = System.getProperty("aws.sharedCredentialsFile");
        assertNotNull("surefire must set aws.sharedCredentialsFile", property);
        Path file = Paths.get(property);
        Files.createDirectories(file.getParent());

        writeCredentials(file, "ASIAFIRST", "first-secret", "first-token", Instant.now().minusSeconds(60));
        applyStsFragment();

        AwsSessionCredentials first = sessionCredentials();
        assertEquals("ASIAFIRST", first.accessKeyId());
        assertEquals("first-token", first.sessionToken());

        // The SDK checks the file modification time at most once a second
        // (ProfileFileRefresher STALE_TIME_MS). The operator renews minutes
        // apart, so the test waits past that window before it reads again.
        Thread.sleep(1500);
        writeCredentials(file, "ASIASECOND", "second-secret", "second-token", Instant.now());

        AwsSessionCredentials second = sessionCredentials();
        assertEquals("ASIASECOND", second.accessKeyId());
        assertEquals("second-secret", second.secretAccessKey());
        assertEquals("second-token", second.sessionToken());
    }

    private void applyStsFragment() throws Exception {
        Path fixture = Paths.get(getClass().getResource("artifact-storage-fragment-sts.yml").toURI());
        ConfigurationAsCode.get().configure(fixture.toString());
    }

    private static AwsSessionCredentials sessionCredentials() throws Exception {
        CredentialsAwsGlobalConfiguration config = CredentialsAwsGlobalConfiguration.get();
        return config.sessionCredentials(config.getRegion(), config.getCredentialsId());
    }

    /**
     * Replaces the file with an atomic rename, as the kubelet replaces a
     * projected Secret volume, and sets an explicit modification time.
     */
    private static void writeCredentials(Path file, String accessKey, String secretKey, String token, Instant modified)
            throws Exception {
        Path tmp = file.resolveSibling(file.getFileName() + ".tmp");
        String body = "[default]\n"
                + "aws_access_key_id = " + accessKey + "\n"
                + "aws_secret_access_key = " + secretKey + "\n"
                + "aws_session_token = " + token + "\n";
        Files.write(tmp, body.getBytes(StandardCharsets.UTF_8));
        Files.setLastModifiedTime(tmp, FileTime.from(modified));
        Files.move(tmp, file, StandardCopyOption.REPLACE_EXISTING, StandardCopyOption.ATOMIC_MOVE);
    }
}
