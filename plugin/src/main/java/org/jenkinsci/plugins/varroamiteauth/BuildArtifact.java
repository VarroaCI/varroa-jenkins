package org.jenkinsci.plugins.varroamiteauth;

import net.sf.json.JSONObject;

/**
 * Immutable value object mirroring the {@code BuildArtifact} proto message.
 * Field names in {@link #toJson()} follow the proto JSON names exactly.
 *
 * <p>Carries no checksum, because Jenkins publishes no digest for an
 * archived artifact. Carries no URL, because a signed URL expires and must
 * never reach a durable record.
 */
public final class BuildArtifact {

    /** {@code storage} value when the plugin computed a blob key under the S3 prefix. */
    public static final String STORAGE_S3 = "s3";

    /** {@code storage} value when no S3 prefix is configured and the artifact stays local. */
    public static final String STORAGE_LOCAL = "local";

    /** {@code size_bytes} value when the artifact manager reports no size without a network call. */
    public static final long SIZE_UNKNOWN = -1L;

    private final String path;
    private final String displayPath;
    private final long sizeBytes;
    private final String storage;
    private final String blobKey;

    private BuildArtifact(Builder b) {
        this.path = orEmpty(b.path);
        this.displayPath = orEmpty(b.displayPath);
        this.sizeBytes = b.sizeBytes;
        this.storage = orEmpty(b.storage);
        this.blobKey = orEmpty(b.blobKey);
    }

    private static String orEmpty(String s) {
        return s != null ? s : "";
    }

    public String getPath() {
        return path;
    }

    public String getDisplayPath() {
        return displayPath;
    }

    public long getSizeBytes() {
        return sizeBytes;
    }

    public String getStorage() {
        return storage;
    }

    public String getBlobKey() {
        return blobKey;
    }

    /** Serializes to a {@link JSONObject} using proto JSON field names. */
    public JSONObject toJson() {
        JSONObject o = new JSONObject();
        o.put("path", path);
        o.put("display_path", displayPath);
        o.put("size_bytes", sizeBytes);
        o.put("storage", storage);
        o.put("blob_key", blobKey);
        return o;
    }

    public static Builder builder() {
        return new Builder();
    }

    public static final class Builder {
        private String path;
        private String displayPath;
        private long sizeBytes = SIZE_UNKNOWN;
        private String storage;
        private String blobKey;

        private Builder() {
        }

        public Builder path(String v) {
            this.path = v;
            return this;
        }

        public Builder displayPath(String v) {
            this.displayPath = v;
            return this;
        }

        public Builder sizeBytes(long v) {
            this.sizeBytes = v;
            return this;
        }

        public Builder storage(String v) {
            this.storage = v;
            return this;
        }

        public Builder blobKey(String v) {
            this.blobKey = v;
            return this;
        }

        public BuildArtifact build() {
            return new BuildArtifact(this);
        }
    }
}
