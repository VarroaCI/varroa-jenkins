package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertTrue;

import hudson.model.Result;
import java.util.ArrayList;
import java.util.List;
import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * A sandboxed script that calls an unapproved method fails on a refused
 * signature. The COMPLETED event names that signature, and the pending view
 * lists it with the job that requested it.
 */
public class VarroaRunRecorderRejectedSignatureTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private int jobCounter;

    @Before
    public void setUp() {
        VarroaRunRecorder.resetStateForTesting();
        BuildEventSink.get().resetForTesting();
        VarroaRunRecorder.setGateOverrideForTesting("1");
    }

    @After
    public void tearDown() {
        VarroaRunRecorder.setGateOverrideForTesting(null);
        VarroaRunRecorder.resetStateForTesting();
        BuildEventSink.get().resetForTesting();
    }

    @Test
    public void completedEventCarriesRejectedSignature() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "rejected-" + ++jobCounter);
        job.setDefinition(new CpsFlowDefinition("System.exit(1)\n", true));
        WorkflowRun run = (WorkflowRun) job.scheduleBuild2(0,
                new VarroaIntentAction(Uuid7.next(), "", null, System.currentTimeMillis())).waitForStart();
        j.assertBuildStatus(Result.FAILURE, j.waitForCompletion(run));

        List<BuildEvent> completed = new ArrayList<>();
        for (BuildEvent e : BuildEventSink.get().drain(100).events) {
            if (BuildEvent.PHASE_COMPLETED.equals(e.getPhase())) {
                completed.add(e);
            }
        }
        assertEquals(1, completed.size());
        String signature = completed.get(0).getRejectedSignature();
        assertTrue("the completion names the refused signature: " + signature,
                signature.startsWith("staticMethod java.lang.System exit"));

        List<ScriptRejections.Requester> requesters = null;
        for (ScriptRejections.Entry entry : ScriptRejections.entries()) {
            if (entry.getSignature().equals(signature)) {
                requesters = entry.getRequesters();
            }
        }
        assertTrue("the refusal map holds the signature", requesters != null);
        assertEquals(job.getFullName(), requesters.get(0).getJobPath());

        JSONListed listed = JSONListed.of(ActivityEndpoint.scriptApprovalEnvelope(ActivityEndpoint.pendingRows()));
        assertTrue("the pending view lists the refused signature", listed.contains(signature, job.getFullName()));
    }

    /** Reads the pending view's JSON for one signature and job path. */
    private static final class JSONListed {
        private final net.sf.json.JSONArray pending;

        private JSONListed(net.sf.json.JSONArray pending) {
            this.pending = pending;
        }

        static JSONListed of(net.sf.json.JSONObject envelope) {
            return new JSONListed(envelope.getJSONArray("pending"));
        }

        boolean contains(String signature, String jobPath) {
            for (int i = 0; i < pending.size(); i++) {
                net.sf.json.JSONObject item = pending.getJSONObject(i);
                if (!signature.equals(item.getString("signature"))) {
                    continue;
                }
                net.sf.json.JSONArray requesters = item.getJSONArray("requesters");
                for (int k = 0; k < requesters.size(); k++) {
                    if (jobPath.equals(requesters.getJSONObject(k).getString("jobPath"))) {
                        return true;
                    }
                }
            }
            return false;
        }
    }
}
