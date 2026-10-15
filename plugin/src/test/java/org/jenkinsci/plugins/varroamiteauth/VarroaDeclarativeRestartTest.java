package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;

import java.util.List;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

import hudson.model.Result;

/**
 * Tests for {@link VarroaDeclarativeRestart#inspect}: the restartable stages of
 * a completed declarative run, and the refusal reasons of the other runs.
 */
public class VarroaDeclarativeRestartTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private static final String TWO_STAGES = "pipeline {\n  agent any\n  stages {\n"
            + "    stage('one') { steps { echo 'ONE' } }\n"
            + "    stage('two') { steps { echo 'TWO' } }\n  }\n}\n";

    @Test
    public void aDeclarativeRunAnswersItsStages() throws Exception {
        WorkflowRun run = run("declared", TWO_STAGES, Result.SUCCESS);

        VarroaDeclarativeRestart.Outcome outcome = VarroaDeclarativeRestart.inspect(run);
        assertNull("a declarative run must restart, refusal=" + outcome.getRefusal(), outcome.getRefusal());
        assertEquals(List.of("one", "two"), outcome.getStages());
    }

    @Test
    public void aScriptedRunIsNotDeclarative() throws Exception {
        WorkflowRun run = run("scripted", "echo 'SCRIPTED'\n", Result.SUCCESS);

        VarroaDeclarativeRestart.Outcome outcome = VarroaDeclarativeRestart.inspect(run);
        assertEquals(VarroaDeclarativeRestart.REFUSAL_NOT_DECLARATIVE, outcome.getRefusal());
        assertTrue(outcome.getStages().isEmpty());
    }

    @Test
    public void aStageSkippedForAnEarlierFailureIsExcluded() throws Exception {
        String script = "pipeline {\n  agent any\n  stages {\n"
                + "    stage('one') { steps { error 'boom' } }\n"
                + "    stage('two') { steps { echo 'TWO' } }\n  }\n}\n";
        WorkflowRun run = run("failing", script, Result.FAILURE);

        VarroaDeclarativeRestart.Outcome outcome = VarroaDeclarativeRestart.inspect(run);
        assertFalse("a stage skipped for the failure must not restart, stages=" + outcome.getStages(),
                outcome.getStages().contains("two"));
    }

    @Test
    public void aJobThatDisablesRestartAnswersDisabled() throws Exception {
        String script = "pipeline {\n  agent any\n  options { disableRestartFromStage() }\n  stages {\n"
                + "    stage('one') { steps { echo 'ONE' } }\n  }\n}\n";
        WorkflowRun run = run("disabled", script, Result.SUCCESS);

        VarroaDeclarativeRestart.Outcome outcome = VarroaDeclarativeRestart.inspect(run);
        assertEquals(VarroaDeclarativeRestart.REFUSAL_DISABLED, outcome.getRefusal());
        assertTrue(outcome.getStages().isEmpty());
    }

    private WorkflowRun run(String name, String script, Result expected) throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, name);
        job.setDefinition(new CpsFlowDefinition(script, true));
        return j.assertBuildStatus(expected, job.scheduleBuild2(0));
    }
}
