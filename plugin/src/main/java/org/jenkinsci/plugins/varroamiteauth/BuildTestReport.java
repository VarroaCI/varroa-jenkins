package org.jenkinsci.plugins.varroamiteauth;

import net.sf.json.JSONObject;

/**
 * Immutable value object mirroring the {@code BuildTestReport} proto message.
 * Field names in {@link #toJson()} follow the proto JSON names exactly.
 *
 * <p>{@code blob_key} is empty for a report that the controller stores
 * locally, and for a report that the collected artifact list does not show.
 */
public final class BuildTestReport {

    private final String blobKey;
    private final long sizeBytes;
    private final int caseCount;

    private BuildTestReport(Builder b) {
        this.blobKey = orEmpty(b.blobKey);
        this.sizeBytes = b.sizeBytes;
        this.caseCount = b.caseCount;
    }

    private static String orEmpty(String s) {
        return s != null ? s : "";
    }

    public String getBlobKey() {
        return blobKey;
    }

    public long getSizeBytes() {
        return sizeBytes;
    }

    public int getCaseCount() {
        return caseCount;
    }

    /** Serializes the report pointer to a {@link JSONObject} with proto JSON field names. */
    public JSONObject toJson() {
        JSONObject o = new JSONObject();
        o.put("blob_key", blobKey);
        o.put("size_bytes", sizeBytes);
        o.put("case_count", caseCount);
        return o;
    }

    public static Builder builder() {
        return new Builder();
    }

    public static final class Builder {
        private String blobKey;
        private long sizeBytes;
        private int caseCount;

        private Builder() {
        }

        public Builder blobKey(String v) {
            this.blobKey = v;
            return this;
        }

        public Builder sizeBytes(long v) {
            this.sizeBytes = v;
            return this;
        }

        public Builder caseCount(int v) {
            this.caseCount = v;
            return this;
        }

        public BuildTestReport build() {
            return new BuildTestReport(this);
        }
    }
}
