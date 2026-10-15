package org.jenkinsci.plugins.varroamiteauth;

import net.sf.json.JSONObject;

/**
 * Immutable value object mirroring the {@code BuildTestSuite} proto message.
 * One row holds the counters of one test suite of a run. Field names in
 * {@link #toJson()} follow the proto JSON names exactly.
 *
 * <p>The builder cuts the name at 512 UTF-8 bytes on a whole character
 * boundary, because the name crosses the wire as a proto3 string.
 */
public final class BuildTestSuite {

    /** The byte cap of the suite name. */
    static final int MAX_STRING_BYTES = 512;

    private final String name;
    private final int total;
    private final int failed;
    private final int skipped;
    private final long durationMs;

    private BuildTestSuite(Builder b) {
        this.name = b.name != null ? b.name : "";
        this.total = b.total;
        this.failed = b.failed;
        this.skipped = b.skipped;
        this.durationMs = b.durationMs;
    }

    public String getName() {
        return name;
    }

    public int getTotal() {
        return total;
    }

    public int getFailed() {
        return failed;
    }

    public int getSkipped() {
        return skipped;
    }

    public long getDurationMs() {
        return durationMs;
    }

    /** Serializes the suite to a {@link JSONObject} with proto JSON field names. */
    public JSONObject toJson() {
        JSONObject o = new JSONObject();
        o.put("name", name);
        o.put("total", total);
        o.put("failed", failed);
        o.put("skipped", skipped);
        o.put("duration_ms", durationMs);
        return o;
    }

    public static Builder builder() {
        return new Builder();
    }

    public static final class Builder {
        private String name;
        private int total;
        private int failed;
        private int skipped;
        private long durationMs;

        private Builder() {
        }

        public Builder name(String v) {
            this.name = VarroaRunRecorder.truncateUtf8Bytes(v, MAX_STRING_BYTES);
            return this;
        }

        public Builder total(int v) {
            this.total = v;
            return this;
        }

        public Builder failed(int v) {
            this.failed = v;
            return this;
        }

        public Builder skipped(int v) {
            this.skipped = v;
            return this;
        }

        public Builder durationMs(long v) {
            this.durationMs = v;
            return this;
        }

        public BuildTestSuite build() {
            return new BuildTestSuite(this);
        }
    }
}
