package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertNull;

import hudson.model.Label;
import hudson.slaves.DumbSlave;
import java.util.concurrent.TimeUnit;
import org.jenkinsci.plugins.workflow.actions.WorkspaceAction;
import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.cps.nodes.StepAtomNode;
import org.jenkinsci.plugins.workflow.graph.FlowNode;
import org.jenkinsci.plugins.workflow.graphanalysis.DepthFirstScanner;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests that {@link VarroaRunRecorder#workspaceOf} names the agent of a
 * body step inside a real Pipeline {@code node} block. The body step
 * carries no workspace action of its own, so the lookup must reach the
 * enclosing block.
 */
public class RunStatePusherNodeNameTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Test
    public void workspaceOfANodeBodyStepNamesTheAgent() throws Exception {
        DumbSlave agent = j.createOnlineSlave(Label.get("l1303"));
        WorkflowJob job = j.createProject(WorkflowJob.class, "node-body");
        job.setDefinition(new CpsFlowDefinition("node('l1303') { echo 'inside' }", true));

        WorkflowRun run = job.scheduleBuild2(0).get(60, TimeUnit.SECONDS);
        j.assertBuildStatus(hudson.model.Result.SUCCESS, run);

        FlowNode echoNode = null;
        for (FlowNode node : new DepthFirstScanner().allNodes(run.getExecution())) {
            if (node instanceof StepAtomNode step
                    && step.getDescriptor() != null
                    && "echo".equals(step.getDescriptor().getFunctionName())) {
                echoNode = node;
                break;
            }
        }
        assertNotNull("the run must contain an echo step", echoNode);

        assertNull("the echo step must carry no workspace action of its own",
                echoNode.getAction(WorkspaceAction.class));
        WorkspaceAction workspace = VarroaRunRecorder.workspaceOf(echoNode);
        assertNotNull("the enclosing node block must supply the workspace action", workspace);
        assertEquals(agent.getNodeName(), workspace.getNode());
    }
}
