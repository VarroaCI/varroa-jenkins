package org.jenkinsci.plugins.varroamiteauth;

import net.sf.json.JSONObject;

/**
 * Immutable value object mirroring the {@code BuildTestCase} proto message.
 * Field names in {@link #toJson()} follow the proto JSON names exactly.
 *
 * <p>The builder cuts every string at 512 UTF-8 bytes on a whole character
 * boundary, because each value crosses the wire as a proto3 string (D3).
 */
public final class BuildTestCase {

    /** The byte cap of each string field. */
    static final int MAX_STRING_BYTES = 512;

    private final String suite;
    private final String className;
    private final String name;
    private final String status;
    private final long durationMs;
    private final String errorDetails;

    private BuildTestCase(Builder b) {
        this.suite = orEmpty(b.suite);
        this.className = orEmpty(b.className);
        this.name = orEmpty(b.name);
        this.status = orEmpty(b.status);
        this.durationMs = b.durationMs;
        this.errorDetails = orEmpty(b.errorDetails);
    }

    private static String orEmpty(String s) {
        return s != null ? s : "";
    }

    private static String cut(String s) {
        return VarroaRunRecorder.truncateUtf8Bytes(s, MAX_STRING_BYTES);
    }

    public String getSuite() {
        return suite;
    }

    public String getClassName() {
        return className;
    }

    public String getName() {
        return name;
    }

    public String getStatus() {
        return status;
    }

    public long getDurationMs() {
        return durationMs;
    }

    public String getErrorDetails() {
        return errorDetails;
    }

    /** Serializes the case to a {@link JSONObject} with proto JSON field names. */
    public JSONObject toJson() {
        JSONObject o = new JSONObject();
        o.put("suite", suite);
        o.put("class_name", className);
        o.put("name", name);
        o.put("status", status);
        o.put("duration_ms", durationMs);
        o.put("error_details", errorDetails);
        return o;
    }

    public static Builder builder() {
        return new Builder();
    }

    public static final class Builder {
        private String suite;
        private String className;
        private String name;
        private String status;
        private long durationMs;
        private String errorDetails;

        private Builder() {
        }

        public Builder suite(String v) {
            this.suite = cut(v);
            return this;
        }

        public Builder className(String v) {
            this.className = cut(v);
            return this;
        }

        public Builder name(String v) {
            this.name = cut(v);
            return this;
        }

        public Builder status(String v) {
            this.status = cut(v);
            return this;
        }

        public Builder durationMs(long v) {
            this.durationMs = v;
            return this;
        }

        public Builder errorDetails(String v) {
            this.errorDetails = cut(v);
            return this;
        }

        public BuildTestCase build() {
            return new BuildTestCase(this);
        }
    }
}
