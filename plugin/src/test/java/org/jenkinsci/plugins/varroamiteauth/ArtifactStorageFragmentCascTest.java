package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertTrue;

import io.jenkins.plugins.casc.ConfigurationAsCode;

import java.io.ByteArrayOutputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;

import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.yaml.snakeyaml.Yaml;

/**
 * JCasC round-trip for the operator-generated artifact-storage fragment
 * (D6, task 6.10, ruling u).
 *
 * <p>Boots a real Jenkins with the pinned {@code artifact-manager-s3} and
 * {@code aws-global-configuration} test-scope plugins, applies the
 * fixture through {@code ConfigurationAsCode.get().configure(...)}
 * exactly as the operator's rendered fragment would apply on a
 * controller, exports the live configuration, and asserts every key
 * path survives. Those key paths are the eleven this plugin's own
 * fragment writer uses, copied here from {@code
 * internal/controller/artifactcasc_keys.go} so an {@code
 * artifact-manager-s3} upgrade that renames or drops one breaks this
 * test, not a controller in production.
 */
public class ArtifactStorageFragmentCascTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private static final String FIXTURE = "artifact-storage-fragment.yml";

    /**
     * The eleven key paths {@code internal/controller/artifactcasc_keys.go}
     * (`ArtifactManagerJCasCKeys`) names. A trailing {@code []} segment
     * walks into the first (and only) entry of a list.
     */
    private static final String[] EXPECTED_PATHS = {
        "unclassified.artifactManager.artifactManagerFactories[].jclouds.provider",
        "aws.awsCredentials.credentialsId",
        "aws.awsCredentials.region",
        "aws.s3.container",
        "aws.s3.prefix",
        "aws.s3.customEndpoint",
        "aws.s3.usePathStyleUrl",
        "aws.s3.useHttp",
        "aws.s3.disableSessionToken",
        "aws.s3.customSigningRegion",
        "aws.s3.useTransferAcceleration",
    };

    @Test
    public void fragmentRoundTripsThroughTheRealConfigurators() throws Exception {
        Path fixture = Paths.get(getClass().getResource(FIXTURE).toURI());
        ConfigurationAsCode.get().configure(fixture.toString());

        ByteArrayOutputStream out = new ByteArrayOutputStream();
        ConfigurationAsCode.get().export(out);
        Map<String, Object> exported = new Yaml().load(out.toString(StandardCharsets.UTF_8));

        Set<String> missing = new LinkedHashSet<>();
        for (String path : EXPECTED_PATHS) {
            if (walk(exported, path) == null) {
                missing.add(path);
            }
        }
        assertTrue("the exported configuration is missing key paths: " + missing, missing.isEmpty());
        assertEquals("s3", walk(exported,
                "unclassified.artifactManager.artifactManagerFactories[].jclouds.provider"));
        assertEquals("varroa-artifact-store", walk(exported, "aws.awsCredentials.credentialsId"));
        assertEquals("artifacts/core/varroa/cell-a/", walk(exported, "aws.s3.prefix"));
    }

    @SuppressWarnings("unchecked")
    private static Object walk(Map<String, Object> doc, String dotPath) {
        Object current = doc;
        for (String segment : dotPath.split("\\.")) {
            boolean intoList = segment.endsWith("[]");
            String key = intoList ? segment.substring(0, segment.length() - 2) : segment;
            if (!(current instanceof Map)) {
                return null;
            }
            current = ((Map<String, Object>) current).get(key);
            if (current == null) {
                return null;
            }
            if (intoList) {
                if (!(current instanceof List) || ((List<?>) current).isEmpty()) {
                    return null;
                }
                current = ((List<?>) current).get(0);
            }
        }
        return current;
    }
}
