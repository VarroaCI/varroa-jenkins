package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

import hudson.model.Result;

import java.util.List;
import org.jenkinsci.plugins.workflow.actions.ErrorAction;
import org.jenkinsci.plugins.workflow.actions.LabelAction;
import org.jenkinsci.plugins.workflow.actions.NotExecutedNodeAction;
import org.jenkinsci.plugins.workflow.actions.ThreadNameAction;
import org.jenkinsci.plugins.workflow.actions.WarningAction;
import org.jenkinsci.plugins.workflow.graph.BlockStartNode;
import org.jenkinsci.plugins.workflow.graph.FlowNode;
import org.jenkinsci.plugins.workflow.pipelinegraphanalysis.GenericStatus;
import org.junit.Test;

/**
 * Tests for {@link VarroaRunRecorder}'s D2 block classification, the
 * action-based status derivation table, and the {@code
 * computeChunkStatus2} refinement mapping — plain JUnit with Mockito
 * doubles for {@code FlowNode}, no JenkinsRule.
 */
public class VarroaRunRecorderStatusTest {

    /** Matched by simple class name only (D2), not by the real workflow-cps type. */
    static class FlowInterruptedException extends RuntimeException {
    }

    private static BlockStartNode labelledStart(String id, boolean parallel) {
        BlockStartNode start = mock(BlockStartNode.class);
        when(start.getId()).thenReturn(id);
        when(start.getAction(LabelAction.class)).thenReturn(new LabelAction("build"));
        if (parallel) {
            when(start.getAction(ThreadNameAction.class)).thenReturn(threadName("unit"));
        }
        return start;
    }

    private static ThreadNameAction threadName(String name) {
        return new ThreadNameAction() {
            @Override
            public String getThreadName() {
                return name;
            }

            @Override
            public String getIconFileName() {
                return null;
            }

            @Override
            public String getDisplayName() {
                return name;
            }

            @Override
            public String getUrlName() {
                return null;
            }
        };
    }

    // --- Block classification (5.10) ---

    @Test
    public void aLabelledBlockWithNoThreadNameIsAStage() {
        BlockStartNode start = labelledStart("s1", false);

        BuildStage stage = VarroaRunRecorder.baseStageBuilder(start).status("SUCCESS").build();

        assertEquals("stage", stage.getNodeType());
        assertEquals("", stage.getParallelBranchName());
    }

    @Test
    public void aLabelledBlockWithAThreadNameIsParallel() {
        BlockStartNode start = labelledStart("s2", true);

        BuildStage stage = VarroaRunRecorder.baseStageBuilder(start).status("SUCCESS").build();

        assertEquals("parallel", stage.getNodeType());
        assertEquals("unit", stage.getParallelBranchName());
    }

    // --- Parent walk ---

    @Test
    public void theParentWalkSkipsAStructuralBlockAndFindsTheNearestLabelledOne() {
        BlockStartNode stageBlock = mock(BlockStartNode.class);
        when(stageBlock.getId()).thenReturn("stage-1");
        when(stageBlock.getAction(LabelAction.class)).thenReturn(new LabelAction("build"));

        BlockStartNode structuralBlock = mock(BlockStartNode.class);
        when(structuralBlock.getId()).thenReturn("parallel-1");
        // No LabelAction: this is the structural block the walk must skip.

        FlowNode branchStart = mock(FlowNode.class);
        when(branchStart.getEnclosingId()).thenReturn("parallel-1");
        doReturn(List.of(structuralBlock, stageBlock)).when(branchStart).getEnclosingBlocks();

        String parentId = VarroaRunRecorder.walkToRecordedParent(branchStart);

        assertEquals("stage-1", parentId);
    }

    @Test
    public void theParentWalkReturnsEmptyWhenNoEnclosingBlockIsLabelled() {
        FlowNode node = mock(FlowNode.class);
        doReturn(List.of()).when(node).getEnclosingBlocks();

        assertEquals("", VarroaRunRecorder.walkToRecordedParent(node));
    }

    // --- Status derivation table (5.10) ---

    @Test
    public void anInterruptedErrorReportsAborted() {
        FlowNode start = mock(FlowNode.class);
        FlowNode end = mock(FlowNode.class);
        ErrorAction error = new ErrorAction(new FlowInterruptedException());
        when(end.getAction(ErrorAction.class)).thenReturn(error);

        assertEquals("ABORTED", VarroaRunRecorder.deriveActionStatus(start, end, List.of(start, end)));
    }

    @Test
    public void anyOtherErrorReportsFailure() {
        FlowNode start = mock(FlowNode.class);
        FlowNode end = mock(FlowNode.class);
        ErrorAction error = new ErrorAction(new RuntimeException("boom"));
        when(end.getAction(ErrorAction.class)).thenReturn(error);

        assertEquals("FAILURE", VarroaRunRecorder.deriveActionStatus(start, end, List.of(start, end)));
    }

    @Test
    public void aWarningActionInTheBlockReportsUnstable() {
        FlowNode start = mock(FlowNode.class);
        FlowNode end = mock(FlowNode.class);
        FlowNode inner = mock(FlowNode.class);
        when(inner.getAction(WarningAction.class)).thenReturn(new WarningAction(Result.UNSTABLE));

        assertEquals("UNSTABLE", VarroaRunRecorder.deriveActionStatus(start, end, List.of(start, inner, end)));
    }

    @Test
    public void aNotExecutedStartNodeReportsNotBuilt() {
        FlowNode start = mock(FlowNode.class);
        FlowNode end = mock(FlowNode.class);
        when(start.getAction(NotExecutedNodeAction.class)).thenReturn(new NotExecutedNodeAction());

        assertEquals("NOT_BUILT", VarroaRunRecorder.deriveActionStatus(start, end, List.of(start, end)));
    }

    @Test
    public void noQualifyingActionReportsSuccess() {
        FlowNode start = mock(FlowNode.class);
        FlowNode end = mock(FlowNode.class);

        assertEquals("SUCCESS", VarroaRunRecorder.deriveActionStatus(start, end, List.of(start, end)));
    }

    @Test
    public void aStageThatEndsDuringARunningBuildNeverReportsNotBuilt() {
        // A block-end status must never fall back to NOT_BUILT purely
        // because the overall run is still in progress: with no error,
        // no warning, and an executed start node, the derivation is
        // SUCCESS regardless of the run's own state.
        FlowNode start = mock(FlowNode.class);
        FlowNode end = mock(FlowNode.class);

        String status = VarroaRunRecorder.deriveActionStatus(start, end, List.of(start, end));

        assertNotEquals("NOT_BUILT", status);
        assertEquals("SUCCESS", status);
    }

    // --- The nodesInBlock graph walk ---

    @Test
    public void nodesInBlockWalksFromEndToStartAndNoFurther() {
        FlowNode beforeBlock = mock(FlowNode.class);
        when(beforeBlock.getId()).thenReturn("before");

        FlowNode start = mock(FlowNode.class);
        when(start.getId()).thenReturn("start");
        doReturn(List.of(beforeBlock)).when(start).getParents();

        FlowNode mid = mock(FlowNode.class);
        when(mid.getId()).thenReturn("mid");
        doReturn(List.of(start)).when(mid).getParents();

        FlowNode end = mock(FlowNode.class);
        when(end.getId()).thenReturn("end");
        doReturn(List.of(mid)).when(end).getParents();

        List<FlowNode> nodes = VarroaRunRecorder.nodesInBlock(start, end);

        assertEquals(3, nodes.size());
        assertTrue(nodes.contains(start));
        assertTrue(nodes.contains(mid));
        assertTrue(nodes.contains(end));
        assertFalse("the walk must not cross the block's own start", nodes.contains(beforeBlock));
    }

    // --- computeChunkStatus2 refinement mapping ---

    @Test
    public void genericStatusMapsOntoTheStageStatusTable() {
        assertEquals("SUCCESS", VarroaRunRecorder.mapGenericStatus(GenericStatus.SUCCESS, "FAILURE"));
        assertEquals("UNSTABLE", VarroaRunRecorder.mapGenericStatus(GenericStatus.UNSTABLE, "SUCCESS"));
        assertEquals("FAILURE", VarroaRunRecorder.mapGenericStatus(GenericStatus.FAILURE, "SUCCESS"));
        assertEquals("ABORTED", VarroaRunRecorder.mapGenericStatus(GenericStatus.ABORTED, "SUCCESS"));
        assertEquals("NOT_BUILT", VarroaRunRecorder.mapGenericStatus(GenericStatus.NOT_EXECUTED, "SUCCESS"));
    }

    @Test
    public void anInProgressOrNullGenericStatusKeepsTheActionDerivedValue() {
        assertEquals("SUCCESS", VarroaRunRecorder.mapGenericStatus(GenericStatus.IN_PROGRESS, "SUCCESS"));
        assertEquals("FAILURE", VarroaRunRecorder.mapGenericStatus(GenericStatus.QUEUED, "FAILURE"));
        assertEquals("ABORTED", VarroaRunRecorder.mapGenericStatus(GenericStatus.PAUSED_PENDING_INPUT, "ABORTED"));
        assertEquals("SUCCESS", VarroaRunRecorder.mapGenericStatus(null, "SUCCESS"));
    }
}
