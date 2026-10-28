package org.jenkinsci.plugins.varroamiteauth;

import net.sf.json.JSONObject;

/**
 * Immutable value object mirroring the {@code BuildWarningsReport} proto message.
 * One row summarizes one warnings report of a run. Field names in
 * {@link #toJson()} follow the proto JSON names exactly.
 *
 * <p>{@code new_total} and {@code fixed_total} are written only when the
 * report has a reference build. {@code gate_status} is empty when the gate
 * is inactive.
 */
public final class BuildWarningsReport {

    private final String id;
    private final String name;
    private final int total;
    private final int error;
    private final int high;
    private final int normal;
    private final int low;
    private final boolean hasReference;
    private final int newTotal;
    private final int fixedTotal;
    private final String gateStatus;

    private BuildWarningsReport(Builder b) {
        this.id = b.id;
        this.name = b.name;
        this.total = b.total;
        this.error = b.error;
        this.high = b.high;
        this.normal = b.normal;
        this.low = b.low;
        this.hasReference = b.hasReference;
        this.newTotal = b.newTotal;
        this.fixedTotal = b.fixedTotal;
        this.gateStatus = b.gateStatus;
    }

    public String getId() {
        return id;
    }

    public String getName() {
        return name;
    }

    public int getTotal() {
        return total;
    }

    public int getError() {
        return error;
    }

    public int getHigh() {
        return high;
    }

    public int getNormal() {
        return normal;
    }

    public int getLow() {
        return low;
    }

    public boolean hasReference() {
        return hasReference;
    }

    public int getNewTotal() {
        return newTotal;
    }

    public int getFixedTotal() {
        return fixedTotal;
    }

    public String getGateStatus() {
        return gateStatus;
    }

    /** Serializes the report to a {@link JSONObject} with proto JSON field names. */
    public JSONObject toJson() {
        JSONObject o = new JSONObject();
        o.put("id", id);
        o.put("name", name);
        o.put("total", total);
        o.put("error", error);
        o.put("high", high);
        o.put("normal", normal);
        o.put("low", low);
        o.put("has_reference", hasReference);
        if (hasReference) {
            o.put("new_total", newTotal);
            o.put("fixed_total", fixedTotal);
        }
        if (!gateStatus.isEmpty()) {
            o.put("gate_status", gateStatus);
        }
        return o;
    }

    public static Builder builder() {
        return new Builder();
    }

    public static final class Builder {
        private String id = "";
        private String name = "";
        private int total;
        private int error;
        private int high;
        private int normal;
        private int low;
        private boolean hasReference;
        private int newTotal;
        private int fixedTotal;
        private String gateStatus = "";

        private Builder() {
        }

        public Builder id(String v) {
            this.id = VarroaRunQuality.truncateText(v);
            return this;
        }

        public Builder name(String v) {
            this.name = VarroaRunQuality.truncateText(v);
            return this;
        }

        public Builder total(int v) {
            this.total = v;
            return this;
        }

        public Builder error(int v) {
            this.error = v;
            return this;
        }

        public Builder high(int v) {
            this.high = v;
            return this;
        }

        public Builder normal(int v) {
            this.normal = v;
            return this;
        }

        public Builder low(int v) {
            this.low = v;
            return this;
        }

        public Builder hasReference(boolean v) {
            this.hasReference = v;
            return this;
        }

        public Builder newTotal(int v) {
            this.newTotal = v;
            return this;
        }

        public Builder fixedTotal(int v) {
            this.fixedTotal = v;
            return this;
        }

        public Builder gateStatus(String v) {
            this.gateStatus = v != null ? v : "";
            return this;
        }

        public BuildWarningsReport build() {
            return new BuildWarningsReport(this);
        }
    }
}
