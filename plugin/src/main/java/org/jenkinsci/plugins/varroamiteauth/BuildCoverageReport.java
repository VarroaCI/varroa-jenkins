package org.jenkinsci.plugins.varroamiteauth;

import java.util.ArrayList;
import java.util.List;
import net.sf.json.JSONArray;
import net.sf.json.JSONObject;

/**
 * Immutable value object mirroring the {@code BuildCoverageReport} proto message.
 * One row summarizes one coverage report of a run. Field names in
 * {@link #toJson()} follow the proto JSON names exactly.
 *
 * <p>{@code line}, {@code branch}, and {@code modified_lines} are absent when
 * the report holds no such value. A report with only metrics has no line or
 * branch counter. {@code gate_status} is empty when the gate is inactive.
 */
public final class BuildCoverageReport {

    private final String id;
    private final String name;
    private final BuildCoverageCounter line;
    private final BuildCoverageCounter branch;
    private final BuildCoverageCounter modifiedLines;
    private final List<BuildCoverageCounter> metrics;
    private final String gateStatus;
    private final int metricsDropped;

    private BuildCoverageReport(Builder b) {
        this.id = b.id;
        this.name = b.name;
        this.line = b.line;
        this.branch = b.branch;
        this.modifiedLines = b.modifiedLines;
        this.metrics = List.copyOf(b.metrics);
        this.gateStatus = b.gateStatus;
        this.metricsDropped = b.metricsDropped;
    }

    public String getId() {
        return id;
    }

    public String getName() {
        return name;
    }

    public BuildCoverageCounter getLine() {
        return line;
    }

    public BuildCoverageCounter getBranch() {
        return branch;
    }

    public BuildCoverageCounter getModifiedLines() {
        return modifiedLines;
    }

    public List<BuildCoverageCounter> getMetrics() {
        return metrics;
    }

    public String getGateStatus() {
        return gateStatus;
    }

    /** The coverage metrics past the bound that the plugin did not send. */
    public int getMetricsDropped() {
        return metricsDropped;
    }

    /** Serializes the report to a {@link JSONObject} with proto JSON field names. */
    public JSONObject toJson() {
        JSONObject o = new JSONObject();
        o.put("id", id);
        o.put("name", name);
        if (line != null) {
            o.put("line", line.toJson());
        }
        if (branch != null) {
            o.put("branch", branch.toJson());
        }
        if (modifiedLines != null) {
            o.put("modified_lines", modifiedLines.toJson());
        }
        if (!metrics.isEmpty()) {
            JSONArray arr = new JSONArray();
            for (BuildCoverageCounter c : metrics) {
                arr.add(c.toJson());
            }
            o.put("metrics", arr);
        }
        if (!gateStatus.isEmpty()) {
            o.put("gate_status", gateStatus);
        }
        if (metricsDropped > 0) {
            o.put("metrics_dropped", metricsDropped);
        }
        return o;
    }

    public static Builder builder() {
        return new Builder();
    }

    public static final class Builder {
        private String id = "";
        private String name = "";
        private BuildCoverageCounter line;
        private BuildCoverageCounter branch;
        private BuildCoverageCounter modifiedLines;
        private List<BuildCoverageCounter> metrics = new ArrayList<>();
        private String gateStatus = "";
        private int metricsDropped;

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

        public Builder line(BuildCoverageCounter v) {
            this.line = v;
            return this;
        }

        public Builder branch(BuildCoverageCounter v) {
            this.branch = v;
            return this;
        }

        public Builder modifiedLines(BuildCoverageCounter v) {
            this.modifiedLines = v;
            return this;
        }

        public Builder metrics(List<BuildCoverageCounter> v) {
            this.metrics = v != null ? new ArrayList<>(v) : new ArrayList<>();
            return this;
        }

        public Builder gateStatus(String v) {
            this.gateStatus = v != null ? v : "";
            return this;
        }

        public Builder metricsDropped(int v) {
            this.metricsDropped = Math.max(0, v);
            return this;
        }

        public BuildCoverageReport build() {
            return new BuildCoverageReport(this);
        }
    }
}
