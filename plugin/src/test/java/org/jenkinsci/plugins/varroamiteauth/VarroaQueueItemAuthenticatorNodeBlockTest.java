package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertTrue;

import hudson.model.Label;
import hudson.model.Result;
import java.util.concurrent.TimeUnit;
import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.jvnet.hudson.test.MockFolder;

/**
 * Tests the owner-task resolution step (task 10.3a): a real Pipeline
 * {@code node} block schedules a durable-task {@code PlaceholderTask},
 * which is not a {@code hudson.model.Item}. Only {@link
 * VarroaTenantResolver}'s owner-task step resolves it to the owning job.
 */
public class VarroaQueueItemAuthenticatorNodeBlockTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Test
    public void aNodeBlockAuthenticatesThePlaceholderTaskAsItsTenant() throws Exception {
        VarroaTenantResolverTest.hostTenants(j, "payments");
        // The Pipeline durability layer reloads the run's Authentication
        // through the active SecurityRealm; only VarroaSecurityRealm
        // answers svc-payments (task 10.3b).
        j.jenkins.setSecurityRealm(new VarroaSecurityRealm());
        j.createOnlineSlave(Label.get("varroa-tenant-payments"));

        MockFolder folder = j.createFolder("payments");
        WorkflowJob api = folder.createProject(WorkflowJob.class, "api");
        api.setDefinition(new CpsFlowDefinition(
            "node('varroa-tenant-payments') {\n"
                + "  echo 'node-block-ran'\n"
                + "}",
            true));

        WorkflowRun run = api.scheduleBuild2(0).get(60, TimeUnit.SECONDS);
        j.assertBuildStatus(Result.SUCCESS, run);
        // Core itself prints the resolved run identity to the console
        // before the script runs; the PlaceholderTask's owner-task
        // resolution is what makes that identity svc-payments.
        assertTrue("the node block must run as the tenant service identity",
            j.getLog(run).contains("Running as svc-payments"));
    }
}
