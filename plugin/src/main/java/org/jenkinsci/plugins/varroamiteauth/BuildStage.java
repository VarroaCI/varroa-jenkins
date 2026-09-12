package org.jenkinsci.plugins.varroamiteauth;

import net.sf.json.JSONArray;
import net.sf.json.JSONObject;

import java.util.ArrayList;
import java.util.List;

/**
 * Immutable value object mirroring the {@code BuildStage} proto message.
 * Field names in {@link #toJson()} follow the proto JSON names exactly.
 *
 * <p>{@code stage_id} equals the {@code FlowNode} id. The status
 * {@code IN_PROGRESS} marks a stage still running.
 */
public final class BuildStage {

    /** Status a live stage carries until it ends. */
    public static final String STATUS_IN_PROGRESS = "IN_PROGRESS";

    private final String stageId;
    private final String parentId;
    private final List<String> parentIds;
    private final String enclosingId;
    private final String name;
    private final String nodeType;
    private final String parallelBranchName;
    private final String status;
    private final long startedAtMillis;
    private final long endedAtMillis;
    private final long durationMillis;
    private final long pauseDurationMillis;
    private final String nodeName;
    private final String agentLabel;
    private final String error;

    private BuildStage(Builder b) {
        this.stageId = orEmpty(b.stageId);
        this.parentId = orEmpty(b.parentId);
        this.parentIds = List.copyOf(b.parentIds);
        this.enclosingId = orEmpty(b.enclosingId);
        this.name = orEmpty(b.name);
        this.nodeType = orEmpty(b.nodeType);
        this.parallelBranchName = orEmpty(b.parallelBranchName);
        this.status = orEmpty(b.status);
        this.startedAtMillis = b.startedAtMillis;
        this.endedAtMillis = b.endedAtMillis;
        this.durationMillis = b.durationMillis;
        this.pauseDurationMillis = b.pauseDurationMillis;
        this.nodeName = orEmpty(b.nodeName);
        this.agentLabel = orEmpty(b.agentLabel);
        this.error = orEmpty(b.error);
    }

    private static String orEmpty(String s) {
        return s != null ? s : "";
    }

    public String getStageId() {
        return stageId;
    }

    public String getParentId() {
        return parentId;
    }

    public String getStatus() {
        return status;
    }

    public String getNodeType() {
        return nodeType;
    }

    public List<String> getParentIds() {
        return parentIds;
    }

    public String getEnclosingId() {
        return enclosingId;
    }

    public String getName() {
        return name;
    }

    public String getParallelBranchName() {
        return parallelBranchName;
    }

    public long getStartedAtMillis() {
        return startedAtMillis;
    }

    public long getEndedAtMillis() {
        return endedAtMillis;
    }

    public long getDurationMillis() {
        return durationMillis;
    }

    public long getPauseDurationMillis() {
        return pauseDurationMillis;
    }

    public String getNodeName() {
        return nodeName;
    }

    public String getAgentLabel() {
        return agentLabel;
    }

    public String getError() {
        return error;
    }

    /** Returns a builder pre-populated with every field of {@code this}. */
    public Builder toBuilder() {
        return new Builder()
                .stageId(stageId)
                .parentId(parentId)
                .parentIds(parentIds)
                .enclosingId(enclosingId)
                .name(name)
                .nodeType(nodeType)
                .parallelBranchName(parallelBranchName)
                .status(status)
                .startedAtMillis(startedAtMillis)
                .endedAtMillis(endedAtMillis)
                .durationMillis(durationMillis)
                .pauseDurationMillis(pauseDurationMillis)
                .nodeName(nodeName)
                .agentLabel(agentLabel)
                .error(error);
    }

    public JSONObject toJson() {
        JSONObject o = new JSONObject();
        o.put("stage_id", stageId);
        o.put("parent_id", parentId);
        JSONArray parentIdsArr = new JSONArray();
        for (String p : parentIds) {
            parentIdsArr.add(p);
        }
        o.put("parent_ids", parentIdsArr);
        o.put("enclosing_id", enclosingId);
        o.put("name", name);
        o.put("node_type", nodeType);
        o.put("parallel_branch_name", parallelBranchName);
        o.put("status", status);
        o.put("started_at_millis", startedAtMillis);
        o.put("ended_at_millis", endedAtMillis);
        o.put("duration_millis", durationMillis);
        o.put("pause_duration_millis", pauseDurationMillis);
        o.put("node_name", nodeName);
        o.put("agent_label", agentLabel);
        o.put("error", error);
        return o;
    }

    public static Builder builder() {
        return new Builder();
    }

    public static final class Builder {
        private String stageId;
        private String parentId;
        private List<String> parentIds = new ArrayList<>();
        private String enclosingId;
        private String name;
        private String nodeType;
        private String parallelBranchName;
        private String status;
        private long startedAtMillis;
        private long endedAtMillis;
        private long durationMillis;
        private long pauseDurationMillis;
        private String nodeName;
        private String agentLabel;
        private String error;

        private Builder() {
        }

        public Builder stageId(String v) {
            this.stageId = v;
            return this;
        }

        public Builder parentId(String v) {
            this.parentId = v;
            return this;
        }

        public Builder parentIds(List<String> v) {
            this.parentIds = new ArrayList<>(v);
            return this;
        }

        public Builder enclosingId(String v) {
            this.enclosingId = v;
            return this;
        }

        public Builder name(String v) {
            this.name = v;
            return this;
        }

        public Builder nodeType(String v) {
            this.nodeType = v;
            return this;
        }

        public Builder parallelBranchName(String v) {
            this.parallelBranchName = v;
            return this;
        }

        public Builder status(String v) {
            this.status = v;
            return this;
        }

        public Builder startedAtMillis(long v) {
            this.startedAtMillis = v;
            return this;
        }

        public Builder endedAtMillis(long v) {
            this.endedAtMillis = v;
            return this;
        }

        public Builder durationMillis(long v) {
            this.durationMillis = v;
            return this;
        }

        public Builder pauseDurationMillis(long v) {
            this.pauseDurationMillis = v;
            return this;
        }

        public Builder nodeName(String v) {
            this.nodeName = v;
            return this;
        }

        public Builder agentLabel(String v) {
            this.agentLabel = v;
            return this;
        }

        public Builder error(String v) {
            this.error = v;
            return this;
        }

        public BuildStage build() {
            return new BuildStage(this);
        }
    }
}
