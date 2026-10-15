package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertTrue;

import java.nio.charset.StandardCharsets;
import java.util.List;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests for {@link VarroaSummaryStep} and {@link VarroaSummaryAction}: the
 * step attaches the text as an action, a later call replaces the earlier
 * text, and the byte cut keeps a whole character boundary.
 */
public class VarroaSummaryStepTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private int jobCounter;

    /** Runs {@code script} to a successful completion and returns its run. */
    private WorkflowRun build(String script) throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "summary-" + ++jobCounter);
        job.setDefinition(new CpsFlowDefinition(script, true));
        WorkflowRun run = (WorkflowRun) job.scheduleBuild2(0).waitForStart();
        j.assertBuildStatusSuccess(j.waitForCompletion(run));
        return run;
    }

    @Test
    public void theStepAttachesTheTextAsAnAction() throws Exception {
        WorkflowRun run = build("varroa.summary('deployed canary to 10%')\n");

        VarroaSummaryAction action = run.getAction(VarroaSummaryAction.class);
        assertNotNull("the step attaches one summary action", action);
        assertEquals("deployed canary to 10%", action.getSummary());
    }

    @Test
    public void aLaterCallReplacesTheEarlierText() throws Exception {
        WorkflowRun run = build("varroa.summary('first')\nvarroa.summary('second')\n");

        List<VarroaSummaryAction> actions = run.getActions(VarroaSummaryAction.class);
        assertEquals("the run carries one summary action", 1, actions.size());
        assertEquals("second", actions.get(0).getSummary());
    }

    @Test
    public void anEmptyTextKeepsTheLastTextThatWasNotEmpty() throws Exception {
        WorkflowRun run = build("varroa.summary('kept')\nvarroa.summary('')\n");

        List<VarroaSummaryAction> actions = run.getActions(VarroaSummaryAction.class);
        assertEquals("the run carries one summary action", 1, actions.size());
        assertEquals("kept", actions.get(0).getSummary());
    }

    @Test
    public void anEmptyTextAttachesNoAction() throws Exception {
        WorkflowRun run = build("varroa.summary('')\n");

        List<VarroaSummaryAction> actions = run.getActions(VarroaSummaryAction.class);
        assertEquals("the run carries no summary action", 0, actions.size());
    }

    @Test
    public void theCutStopsAtAWholeCharacterBoundaryUnderTheLimit() {
        // 200 snowmen, three UTF-8 bytes each: 600 bytes, above the limit.
        StringBuilder text = new StringBuilder();
        for (int i = 0; i < 200; i++) {
            text.append('\u2603');
        }
        String full = text.toString();

        String cut = VarroaRunRecorder.truncateUtf8Bytes(full, 512);

        assertTrue("the cut keeps a whole prefix", full.startsWith(cut));
        assertEquals("the cut holds 170 whole characters", 170, cut.length());
        assertEquals("the cut holds 510 UTF-8 bytes", 510, cut.getBytes(StandardCharsets.UTF_8).length);
    }

    @Test
    public void aTextUnderTheLimitIsUnchanged() {
        assertEquals("short text", VarroaRunRecorder.truncateUtf8Bytes("short text", 512));
        assertEquals("", VarroaRunRecorder.truncateUtf8Bytes(null, 512));
    }
}
