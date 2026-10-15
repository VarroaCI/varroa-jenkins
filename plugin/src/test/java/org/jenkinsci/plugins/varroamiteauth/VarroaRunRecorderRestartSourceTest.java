package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import hudson.model.Result;

import java.io.ByteArrayOutputStream;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Base64;
import java.util.List;
import java.util.Set;
import java.util.zip.GZIPInputStream;

import net.sf.json.JSONObject;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests the restart copy of a real finalized run. The copy is scheduled
 * the way {@link VarroaRunRecorder.Runs#onFinalized} schedules it, with the
 * executor synchronous and the sender recorded.
 */
public class VarroaRunRecorderRestartSourceTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private static final String TWO_STAGES = "pipeline {\n  agent any\n  stages {\n"
            + "    stage('one') { steps { echo 'ONE' } }\n"
            + "    stage('two') { steps { echo 'TWO' } }\n  }\n}\n";

    private final List<JSONObject> records = new ArrayList<>();

    @Before
    public void setUp() {
        VarroaRunRecorder.setGateOverrideForTesting("1");
        RunStatePusher.setActivationUrlForTesting("http://127.0.0.1:9");
        RestartSourcePusher.setCellOverrideForTesting(true);
        RestartSourcePusher.setExecutorForTesting(Runnable::run);
        RestartSourcePusher.setSleeperForTesting(millis -> { });
        RestartSourcePusher.setSenderForTesting((url, record) -> {
            records.add(record);
            return RunStatePusher.RunStateSender.Answer.accepted();
        });
    }

    @After
    public void tearDown() {
        VarroaRunRecorder.setGateOverrideForTesting(null);
        VarroaTemplateDefinitions.setClassNamesForTesting(null);
        RestartSourcePusher.resetForTesting();
        RunStatePusher.resetForTesting();
    }

    @Test
    public void declarativeRunPushesFinalBuildXml() throws Exception {
        WorkflowRun run = run("declared", TWO_STAGES, Result.SUCCESS);

        RestartSourcePusher.scheduleFor(run, "run-declared");

        String buildXml = null;
        boolean hasWorkflowCompleted = false;
        for (JSONObject record : records) {
            String relpath = record.getString("relpath");
            if (relpath.endsWith("/builds/" + run.getNumber() + "/build.xml")) {
                buildXml = gunzipText(record.getString("gzip_base64"));
            }
            if (relpath.contains("/workflow-completed/")) {
                hasWorkflowCompleted = true;
            }
        }
        assertTrue("the copy carries build.xml", buildXml != null);
        assertTrue("build.xml is the final record, got: " + buildXml,
                buildXml.contains("<result>SUCCESS</result>"));
        assertTrue("the copy carries workflow-completed", hasWorkflowCompleted);
    }

    @Test
    public void scriptedRunPushesNothing() throws Exception {
        WorkflowRun run = run("scripted", "echo 'SCRIPTED'\n", Result.SUCCESS);

        RestartSourcePusher.scheduleFor(run, "run-scripted");

        assertTrue("a scripted run keeps no copy, got " + records.size(), records.isEmpty());
    }

    @Test
    public void templateDefinedRunPushesNothing() throws Exception {
        WorkflowRun run = run("templated", TWO_STAGES, Result.SUCCESS);
        VarroaTemplateDefinitions.setClassNamesForTesting(Set.of(CpsFlowDefinition.class.getName()));

        RestartSourcePusher.scheduleFor(run, "run-templated");

        assertFalse("a template-defined run keeps no copy", records.size() > 0);
    }

    private WorkflowRun run(String name, String script, Result expected) throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, name);
        job.setDefinition(new CpsFlowDefinition(script, true));
        return j.assertBuildStatus(expected, job.scheduleBuild2(0));
    }

    private static String gunzipText(String base64) throws Exception {
        byte[] gz = Base64.getDecoder().decode(base64);
        try (GZIPInputStream in = new GZIPInputStream(new java.io.ByteArrayInputStream(gz))) {
            ByteArrayOutputStream out = new ByteArrayOutputStream();
            in.transferTo(out);
            return new String(out.toByteArray(), StandardCharsets.UTF_8);
        }
    }
}
