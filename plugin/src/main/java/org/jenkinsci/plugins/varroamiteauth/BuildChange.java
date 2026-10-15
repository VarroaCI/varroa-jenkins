package org.jenkinsci.plugins.varroamiteauth;

import net.sf.json.JSONObject;

/**
 * Immutable value object mirroring the {@code BuildChange} proto message.
 * Field names in {@link #toJson()} follow the proto JSON names exactly.
 */
public final class BuildChange {

    private final String commitId;
    private final String authorId;
    private final String authorDisplayName;
    private final String message;
    private final long timestampMillis;
    private final int affectedPathCount;

    private BuildChange(Builder b) {
        this.commitId = orEmpty(b.commitId);
        this.authorId = orEmpty(b.authorId);
        this.authorDisplayName = orEmpty(b.authorDisplayName);
        this.message = orEmpty(b.message);
        this.timestampMillis = b.timestampMillis;
        this.affectedPathCount = b.affectedPathCount;
    }

    private static String orEmpty(String s) {
        return s != null ? s : "";
    }

    public String getCommitId() {
        return commitId;
    }

    public String getAuthorId() {
        return authorId;
    }

    public String getAuthorDisplayName() {
        return authorDisplayName;
    }

    public String getMessage() {
        return message;
    }

    public long getTimestampMillis() {
        return timestampMillis;
    }

    public int getAffectedPathCount() {
        return affectedPathCount;
    }

    public JSONObject toJson() {
        JSONObject o = new JSONObject();
        o.put("commit_id", commitId);
        o.put("author_id", authorId);
        o.put("author_display_name", authorDisplayName);
        o.put("message", message);
        o.put("timestamp_millis", timestampMillis);
        o.put("affected_path_count", affectedPathCount);
        return o;
    }

    public static Builder builder() {
        return new Builder();
    }

    public static final class Builder {
        private String commitId;
        private String authorId;
        private String authorDisplayName;
        private String message;
        private long timestampMillis;
        private int affectedPathCount;

        private Builder() {
        }

        public Builder commitId(String v) {
            this.commitId = v;
            return this;
        }

        public Builder authorId(String v) {
            this.authorId = v;
            return this;
        }

        public Builder authorDisplayName(String v) {
            this.authorDisplayName = v;
            return this;
        }

        public Builder message(String v) {
            this.message = v;
            return this;
        }

        public Builder timestampMillis(long v) {
            this.timestampMillis = v;
            return this;
        }

        public Builder affectedPathCount(int v) {
            this.affectedPathCount = v;
            return this;
        }

        public BuildChange build() {
            return new BuildChange(this);
        }
    }
}
