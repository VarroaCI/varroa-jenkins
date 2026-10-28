package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;

import hudson.FilePath;
import hudson.model.Result;
import java.util.concurrent.TimeUnit;
import net.sf.json.JSONArray;
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
 * Tests for the quality export on the completed event. The pipelines run the
 * real coverage and warnings-ng steps against the fixtures in
 * {@code quality/}, which the test copies into the job workspace first.
 */
public class VarroaRunQualityTest {

    private static final String JACOCO = "quality/jacoco.xml";
    private static final String CHECKSTYLE = "quality/checkstyle-result.xml";

    @Rule
    public JenkinsRule j = new JenkinsRule();

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
    public void aPipelineWithCoverageAndCheckStyleExportsBothReports() throws Exception {
        WorkflowJob p = j.jenkins.createProject(WorkflowJob.class, "quality-" + System.nanoTime());
        FilePath ws = j.jenkins.getWorkspaceFor(p);
        ws.child("target/site/jacoco/jacoco.xml").copyFrom(VarroaRunQualityTest.class.getResource(JACOCO));
        ws.child("target/checkstyle-result.xml").copyFrom(VarroaRunQualityTest.class.getResource(CHECKSTYLE));
        p.setDefinition(new CpsFlowDefinition(
                "node {\n"
                        + "  recordCoverage(tools: [[parser: 'JACOCO', pattern: '**/jacoco.xml']], id: 'jacoco',"
                        + " name: 'Coverage',\n"
                        + "    qualityGates: [[threshold: 60.0, metric: 'LINE', baseline: 'PROJECT',"
                        + " criticality: 'UNSTABLE']])\n"
                        + "  recordIssues(tools: [checkStyle(pattern: '**/checkstyle-result.xml')],\n"
                        + "    qualityGates: [[threshold: 1, type: 'TOTAL', criticality: 'NOTE']])\n"
                        + "}", true));
        String runId = Uuid7.next();
        WorkflowRun run = p.scheduleBuild2(0,
                new VarroaIntentAction(runId, "", null, System.currentTimeMillis())).get(60, TimeUnit.SECONDS);
        j.assertBuildStatusSuccess(run);

        JSONObject json = findCompleted(runId).toJson();

        JSONArray coverage = json.getJSONArray("coverage_reports");
        assertEquals(1, coverage.size());
        JSONObject c = coverage.getJSONObject(0);
        assertEquals("jacoco", c.getString("id"));
        assertEquals("Coverage", c.getString("name"));
        assertEquals("LINE", c.getJSONObject("line").getString("metric"));
        assertEquals(4, c.getJSONObject("line").getInt("covered"));
        assertEquals(5, c.getJSONObject("line").getInt("total"));
        assertEquals("BRANCH", c.getJSONObject("branch").getString("metric"));
        assertEquals(3, c.getJSONObject("branch").getInt("covered"));
        assertEquals(4, c.getJSONObject("branch").getInt("total"));
        assertFalse(c.has("modified_lines"));
        JSONArray metrics = c.getJSONArray("metrics");
        String[] expected = {"MODULE", "PACKAGE", "FILE", "CLASS", "METHOD", "INSTRUCTION"};
        assertEquals(expected.length, metrics.size());
        for (int i = 0; i < expected.length; i++) {
            assertEquals(expected[i], metrics.getJSONObject(i).getString("metric"));
        }
        JSONObject instruction = metrics.getJSONObject(expected.length - 1);
        assertEquals(8, instruction.getInt("covered"));
        assertEquals(10, instruction.getInt("total"));
        assertEquals("PASSED", c.getString("gate_status"));

        JSONArray warnings = json.getJSONArray("warnings_reports");
        assertEquals(1, warnings.size());
        JSONObject w = warnings.getJSONObject(0);
        assertEquals("checkstyle", w.getString("id"));
        assertEquals("CheckStyle Warnings", w.getString("name"));
        assertEquals(3, w.getInt("total"));
        assertEquals(1, w.getInt("error"));
        assertEquals(0, w.getInt("high"));
        assertEquals(1, w.getInt("normal"));
        assertEquals(1, w.getInt("low"));
        assertFalse(w.getBoolean("has_reference"));
        assertFalse(w.has("new_total"));
        assertEquals("NOTE", w.getString("gate_status"));
    }

    @Test
    public void aRunWithNoQualityStepWritesNoQualityKeys() throws Exception {
        WorkflowJob p = j.jenkins.createProject(WorkflowJob.class, "no-quality-" + System.nanoTime());
        p.setDefinition(new CpsFlowDefinition("echo 'none'\n", true));
        String runId = Uuid7.next();
        WorkflowRun run = p.scheduleBuild2(0,
                new VarroaIntentAction(runId, "", null, System.currentTimeMillis())).get(60, TimeUnit.SECONDS);
        j.assertBuildStatus(Result.SUCCESS, run);

        JSONObject json = findCompleted(runId).toJson();

        assertFalse(json.has("coverage_reports"));
        assertFalse(json.has("warnings_reports"));
    }

    private BuildEvent findCompleted(String runId) throws InterruptedException {
        long deadline = System.currentTimeMillis() + 10_000;
        while (System.currentTimeMillis() < deadline) {
            BuildEventSink.DrainResult result = BuildEventSink.get().drain(256);
            for (BuildEvent e : result.events) {
                if (runId.equals(e.getRunId()) && e.isCompletedPhase()) {
                    return e;
                }
            }
            Thread.sleep(50);
        }
        throw new AssertionError("the completed event arrives for run " + runId);
    }
}
