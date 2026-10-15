package org.jenkinsci.plugins.varroamiteauth;

/**
 * One ordered console-output chunk of one build, ready to post to
 * {@code POST /v1/log} (D2, D3).
 *
 * <p>{@code data} is empty for the final chunk. A chunk never mixes node
 * ids: every byte in {@code data} came from writes to the same node
 * (or from {@code overallListener()}, whose node id is empty).
 */
final class LogChunk {

    final String runId;
    final String jobPath;
    final long buildNumber;
    final long seq;
    final String nodeId;
    final String stageId;
    final byte[] data;
    final long firstLineUnixMs;
    final long droppedBytes;
    final boolean isFinal;

    LogChunk(String runId, String jobPath, long buildNumber, long seq, String nodeId, String stageId,
            byte[] data, long firstLineUnixMs, long droppedBytes, boolean isFinal) {
        this.runId = runId;
        this.jobPath = jobPath;
        this.buildNumber = buildNumber;
        this.seq = seq;
        this.nodeId = nodeId;
        this.stageId = stageId;
        this.data = data;
        this.firstLineUnixMs = firstLineUnixMs;
        this.droppedBytes = droppedBytes;
        this.isFinal = isFinal;
    }

    /** Bytes this chunk accounts for against the outstanding-byte caps. */
    int accountedBytes() {
        return data.length;
    }
}
