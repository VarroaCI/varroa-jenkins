package org.jenkinsci.plugins.varroamiteauth;

import hudson.ExtensionList;
import hudson.model.Run;

import java.util.ArrayList;
import java.util.List;
import java.util.function.Supplier;
import java.util.logging.Level;
import java.util.logging.Logger;

import jenkins.model.Jenkins;

/**
 * Walks {@link Run#getArtifacts()} and builds one {@link BuildArtifact} per
 * entry (D6, task 6.11).
 *
 * <p>{@code artifact-manager-s3} is never a compile dependency of this
 * plugin. The configured object prefix is read reflectively from {@code
 * S3BlobStoreConfig}'s own {@code get()} singleton, so this class works
 * whether or not that plugin is installed.
 */
final class ArtifactCollector {

    private static final Logger LOGGER = Logger.getLogger(ArtifactCollector.class.getName());

    private static final String S3_CONFIG_CLASS = "io.jenkins.plugins.artifact_manager_jclouds.s3.S3BlobStoreConfig";

    /**
     * Overrides the reflective {@code S3BlobStoreConfig} prefix read for a
     * test, so a test can prove the exact key layout without installing
     * {@code artifact-manager-s3}. {@code null} restores the reflective
     * path.
     */
    private static volatile Supplier<String> prefixSupplierForTesting;

    private ArtifactCollector() {
    }

    /**
     * Returns one {@link BuildArtifact} per archived artifact of {@code run}.
     * {@code blob_key} is empty when no S3 prefix is configured (local
     * mode). {@code size_bytes} is {@link BuildArtifact#SIZE_UNKNOWN} when
     * the cached length is absent, matching the artifact manager's own
     * "no size without a network call" contract.
     */
    @SuppressWarnings("rawtypes")
    static List<BuildArtifact> collect(Run<?, ?> run) {
        List<BuildArtifact> result = new ArrayList<>();
        String prefix = resolveConfiguredPrefix();
        String jobFullName = run.getParent().getFullName();
        long buildNumber = run.getNumber();
        boolean s3 = prefix != null && !prefix.isEmpty();
        for (Run.Artifact a : run.getArtifacts()) {
            String path = a.relativePath != null ? a.relativePath : "";
            String displayPath = a.getDisplayPath() != null ? a.getDisplayPath() : "";
            long size = parseSize(a.getLength());
            String storage = s3 ? BuildArtifact.STORAGE_S3 : BuildArtifact.STORAGE_LOCAL;
            String blobKey = s3 ? prefix + jobFullName + "/" + buildNumber + "/artifacts/" + path : "";
            result.add(BuildArtifact.builder()
                    .path(path)
                    .displayPath(displayPath)
                    .sizeBytes(size)
                    .storage(storage)
                    .blobKey(blobKey)
                    .build());
        }
        return result;
    }

    private static long parseSize(String length) {
        if (length == null || length.isEmpty()) {
            return BuildArtifact.SIZE_UNKNOWN;
        }
        try {
            return Long.parseLong(length);
        } catch (NumberFormatException e) {
            return BuildArtifact.SIZE_UNKNOWN;
        }
    }

    /**
     * Sets or clears the test override for {@link #resolveConfiguredPrefix()}.
     * Package-private for testing.
     */
    static void setPrefixSupplierForTesting(Supplier<String> supplier) {
        prefixSupplierForTesting = supplier;
    }

    /**
     * Reads {@code S3BlobStoreConfig.get().getPrefix()} by reflection,
     * unless a test installed {@link #prefixSupplierForTesting}. Returns
     * {@code null} when the plugin class is absent (the expected "local
     * mode" case) or its prefix is unset. The class is loaded through the
     * plugin manager's uber class loader: {@code artifact-manager-s3} is
     * not a dependency of this plugin, so this plugin's own class loader
     * cannot see it even when it is installed.
     */
    private static String resolveConfiguredPrefix() {
        Supplier<String> override = prefixSupplierForTesting;
        if (override != null) {
            return override.get();
        }
        try {
            Jenkins jenkins = Jenkins.getInstanceOrNull();
            if (jenkins == null) {
                return null;
            }
            Class<?> clazz = Class.forName(S3_CONFIG_CLASS, false, jenkins.getPluginManager().uberClassLoader);
            ExtensionList<?> list = jenkins.getExtensionList(clazz);
            if (list.isEmpty()) {
                return null;
            }
            Object config = list.iterator().next();
            Object value = clazz.getMethod("getPrefix").invoke(config);
            return value instanceof String ? (String) value : null;
        } catch (ClassNotFoundException e) {
            // artifact-manager-s3 is not installed: local mode, expected.
            return null;
        } catch (Exception e) {
            LOGGER.log(Level.FINE, "ArtifactCollector: could not read the S3 artifact prefix", e);
            return null;
        }
    }
}
