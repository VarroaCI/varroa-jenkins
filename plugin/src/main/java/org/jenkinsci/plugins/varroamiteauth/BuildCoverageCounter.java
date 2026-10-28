package org.jenkinsci.plugins.varroamiteauth;

import net.sf.json.JSONObject;

/**
 * Immutable value object mirroring the {@code BuildCoverageCounter} proto message.
 * One row holds the covered and total counts of one coverage metric. Field names
 * in {@link #toJson()} follow the proto JSON names exactly.
 *
 * <p>The builder cuts the metric name at 128 code points.
 */
public final class BuildCoverageCounter {

    private final String metric;
    private final int covered;
    private final int total;

    private BuildCoverageCounter(Builder b) {
        this.metric = b.metric;
        this.covered = b.covered;
        this.total = b.total;
    }

    public String getMetric() {
        return metric;
    }

    public int getCovered() {
        return covered;
    }

    public int getTotal() {
        return total;
    }

    /** Serializes the counter to a {@link JSONObject} with proto JSON field names. */
    public JSONObject toJson() {
        JSONObject o = new JSONObject();
        o.put("metric", metric);
        o.put("covered", covered);
        o.put("total", total);
        return o;
    }

    public static Builder builder() {
        return new Builder();
    }

    public static final class Builder {
        private String metric = "";
        private int covered;
        private int total;

        private Builder() {
        }

        public Builder metric(String v) {
            this.metric = VarroaRunQuality.truncateText(v);
            return this;
        }

        public Builder covered(int v) {
            this.covered = v;
            return this;
        }

        public Builder total(int v) {
            this.total = v;
            return this;
        }

        public BuildCoverageCounter build() {
            return new BuildCoverageCounter(this);
        }
    }
}
