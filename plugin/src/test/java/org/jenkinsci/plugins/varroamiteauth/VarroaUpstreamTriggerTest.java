package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertTrue;
import static org.junit.Assert.fail;

import hudson.triggers.Trigger;
import hudson.triggers.TriggerDescriptor;

import java.util.Map;

import jenkins.triggers.TriggeredItem;

import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests for {@link VarroaUpstreamTrigger} (task 10.1): the declarative
 * {@code triggers} block binds one trigger through the {@code
 * varroaUpstream} symbol, and an out-of-range {@code result} is rejected.
 */
public class VarroaUpstreamTriggerTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Test
    public void declarativeTriggersBlockProducesOneTrigger() throws Exception {
        WorkflowJob job = j.jenkins.createProject(WorkflowJob.class, "declarative-upstream-job");
        job.setDefinition(new CpsFlowDefinition(
                "pipeline {\n"
                        + "  agent any\n"
                        + "  triggers { varroaUpstream(tenant: 'payments', job: 'release') }\n"
                        + "  stages {\n"
                        + "    stage('noop') {\n"
                        + "      steps { echo 'noop' }\n"
                        + "    }\n"
                        + "  }\n"
                        + "}",
                true));

        j.buildAndAssertSuccess(job);

        Map<TriggerDescriptor, Trigger<?>> triggers = ((TriggeredItem) job).getTriggers();
        assertEquals("a declarative triggers block must produce exactly one trigger",
                1, triggers.size());

        Trigger<?> trigger = triggers.values().iterator().next();
        assertTrue("the trigger must be a VarroaUpstreamTrigger, got " + trigger.getClass(),
                trigger instanceof VarroaUpstreamTrigger);
        VarroaUpstreamTrigger upstream = (VarroaUpstreamTrigger) trigger;
        assertEquals("payments", upstream.getTenant());
        assertEquals("release", upstream.getJob());
        assertEquals("the result threshold defaults to SUCCESS", "SUCCESS", upstream.getResult());
    }

    @Test
    public void fourthResultValueIsRejected() {
        VarroaUpstreamTrigger trigger = new VarroaUpstreamTrigger("payments", "release");
        try {
            trigger.setResult("ABORTED");
            fail("a result outside SUCCESS, UNSTABLE, and FAILURE must be rejected");
        } catch (IllegalArgumentException e) {
            assertTrue("the message must name every allowed value: " + e.getMessage(),
                    e.getMessage().contains("SUCCESS")
                            && e.getMessage().contains("UNSTABLE")
                            && e.getMessage().contains("FAILURE"));
        }
    }
}
