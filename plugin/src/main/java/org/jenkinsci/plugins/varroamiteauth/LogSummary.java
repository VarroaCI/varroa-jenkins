package org.jenkinsci.plugins.varroamiteauth;

import net.sf.json.JSONObject;

/**
 * Immutable value object mirroring the {@code LogSummary} proto message.
 * Field names in {@link #toJson()} follow the proto JSON names exactly.
 *
 * <p>{@link VarroaLogStorage#finish(String)} sets {@code chunk_count},
 * {@code dropped_bytes}, {@code incomplete}, and {@code incomplete_reason}.
 * It never sets {@code finalized} true and never sets {@code
 * writer_heartbeat_unix_ms}: those two fields belong to the gateway, which
 * learns the durable-write evidence the plugin cannot see.
 */
public final class LogSummary {

    /** {@code incomplete_reason} for a per-build or JVM-wide cap drop before {@code finish}. */
    public static final String REASON_PLUGIN_DROP = "plugin-drop";

    /** {@code incomplete_reason} for a chunk that ended on a terminal 400 or 413 before {@code finish}. */
    public static final String REASON_TERMINAL_RESPONSE = "terminal-response";

    /**
     * {@code incomplete_reason} for a completion that the boot reconcile
     * rebuilt after a Jenkins stop lost it. The process that counted the
     * run's log chunks, and any cap drop, is gone with the original summary.
     */
    public static final String REASON_JENKINS_RESTART = "jenkins-restart";

    private final long chunkCount;
    private final long droppedBytes;
    private final boolean incomplete;
    private final String incompleteReason;
    private final boolean finalized;
    private final long writerHeartbeatUnixMs;

    private LogSummary(Builder b) {
        this.chunkCount = b.chunkCount;
        this.droppedBytes = b.droppedBytes;
        this.incomplete = b.incomplete;
        this.incompleteReason = b.incompleteReason != null ? b.incompleteReason : "";
        this.finalized = b.finalized;
        this.writerHeartbeatUnixMs = b.writerHeartbeatUnixMs;
    }

    public long getChunkCount() {
        return chunkCount;
    }

    public long getDroppedBytes() {
        return droppedBytes;
    }

    public boolean isIncomplete() {
        return incomplete;
    }

    public String getIncompleteReason() {
        return incompleteReason;
    }

    public boolean isFinalized() {
        return finalized;
    }

    public long getWriterHeartbeatUnixMs() {
        return writerHeartbeatUnixMs;
    }

    /** Serializes to a {@link JSONObject} using proto JSON field names. */
    public JSONObject toJson() {
        JSONObject o = new JSONObject();
        o.put("chunk_count", chunkCount);
        o.put("dropped_bytes", droppedBytes);
        o.put("incomplete", incomplete);
        o.put("incomplete_reason", incompleteReason);
        o.put("finalized", finalized);
        o.put("writer_heartbeat_unix_ms", writerHeartbeatUnixMs);
        return o;
    }

    public static Builder builder() {
        return new Builder();
    }

    public static final class Builder {
        private long chunkCount;
        private long droppedBytes;
        private boolean incomplete;
        private String incompleteReason = "";
        private boolean finalized;
        private long writerHeartbeatUnixMs;

        private Builder() {
        }

        public Builder chunkCount(long v) {
            this.chunkCount = v;
            return this;
        }

        public Builder droppedBytes(long v) {
            this.droppedBytes = v;
            return this;
        }

        public Builder incomplete(boolean v) {
            this.incomplete = v;
            return this;
        }

        public Builder incompleteReason(String v) {
            this.incompleteReason = v;
            return this;
        }

        /** Package-private: only a gateway-originated value may ever set this true. Not used by the plugin. */
        Builder finalized(boolean v) {
            this.finalized = v;
            return this;
        }

        /** Package-private: the plugin never sets this. Reserved for a gateway-originated value. */
        Builder writerHeartbeatUnixMs(long v) {
            this.writerHeartbeatUnixMs = v;
            return this;
        }

        public LogSummary build() {
            return new LogSummary(this);
        }
    }
}
