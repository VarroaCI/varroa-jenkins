package org.jenkinsci.plugins.varroamiteauth;

import hudson.console.AnnotatedLargeText;
import hudson.model.BuildListener;
import hudson.model.TaskListener;
import hudson.util.StreamTaskListener;

import java.io.IOException;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;
import java.util.concurrent.ConcurrentHashMap;

import org.jenkinsci.plugins.workflow.flow.FlowExecutionOwner;
import org.jenkinsci.plugins.workflow.graph.FlowNode;
import org.jenkinsci.plugins.workflow.log.LogStorage;

/**
 * One build's {@link LogStorage}: owns its {@link LogChunkBuffer} and reads
 * back through the shared {@link LogReadCache} (D1).
 *
 * <p>{@code overallListener()} and {@code nodeListener(FlowNode)} build each
 * listener's stream only with {@link LogStorage#wrapWithRemoteAutoFlushingListener}
 * and {@link LogStorage#wrapWithAutoFlushingBuffer}, so every byte already
 * passed the masking {@code TaskListenerDecorator} chain before it reaches
 * the buffer. This class reads no lower-level output and adds no masking of
 * its own.
 */
final class VarroaLogStorage implements LogStorage {

    private final String runId;
    private final String jobPath;
    private final long buildNumber;
    private final LogChunkBuffer buffer;
    private final LogReadCache cache;
    private final ConcurrentHashMap<String, String> stageIdCache = new ConcurrentHashMap<>();

    VarroaLogStorage(String runId, String jobPath, long buildNumber, LogChunkSender sender, LogReadCache cache,
            LogReadCache.ResumePoint resume) {
        this.runId = runId;
        this.jobPath = jobPath;
        this.buildNumber = buildNumber;
        this.cache = cache;
        this.buffer = new LogChunkBuffer(runId, jobPath, buildNumber, sender, cache, resume);
    }

    @Override
    public BuildListener overallListener() throws IOException {
        OutputStream sink = new BufferSink("", "");
        return LogStorage.wrapWithRemoteAutoFlushingListener(sink);
    }

    @Override
    public TaskListener nodeListener(FlowNode node) throws IOException {
        String stageId = resolveStageId(node);
        OutputStream sink = new BufferSink(node.getId(), stageId);
        return new StreamTaskListener(LogStorage.wrapWithAutoFlushingBuffer(sink), StandardCharsets.UTF_8);
    }

    @Override
    public AnnotatedLargeText<FlowExecutionOwner.Executable> overallLog(
            FlowExecutionOwner.Executable executable, boolean complete) {
        return cache.overallLog(runId, executable, pointerMessage());
    }

    @Override
    public AnnotatedLargeText<FlowNode> stepLog(FlowNode node, boolean complete) {
        return cache.stepLog(runId, node.getId(), node, pointerMessage());
    }

    /** Switches this build's flush interval. {@link TailEndpoint} calls this through {@link VarroaLogStorageFactory}. */
    void setTailActive(boolean active) {
        buffer.setTailActive(active);
    }

    /**
     * Runs the four-step end-of-build sequence (D2): closes the buffer,
     * appends the final chunk, and returns the log summary. Never waits on
     * the network. Idempotent.
     */
    LogSummary finish() {
        return buffer.finish();
    }

    private String resolveStageId(FlowNode node) {
        return stageIdCache.computeIfAbsent(node.getId(), id -> {
            // Reuses VarroaRunRecorder's own "recorded stage" walk so a
            // chunk's stage_id always joins to the same BuildStage row.
            if (VarroaRunRecorder.hasLabel(node)) {
                return node.getId();
            }
            return VarroaRunRecorder.walkToRecordedParent(node);
        });
    }

    private String pointerMessage() {
        return "This log is not cached on this Jenkins instance. Fetch it from Varroa by its run id: "
                + runId + "\n";
    }

    /** Appends every write to {@link #buffer} under the given node and stage id. */
    private final class BufferSink extends OutputStream {
        private final String nodeId;
        private final String stageId;

        BufferSink(String nodeId, String stageId) {
            this.nodeId = nodeId;
            this.stageId = stageId;
        }

        @Override
        public void write(int b) {
            buffer.append(nodeId, stageId, new byte[] {(byte) b}, 0, 1);
        }

        @Override
        public void write(byte[] b, int off, int len) {
            buffer.append(nodeId, stageId, b, off, len);
        }
    }
}
