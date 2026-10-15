package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertTrue;

import com.cloudbees.hudson.plugins.folder.Folder;
import com.cloudbees.hudson.plugins.folder.properties.FolderCredentialsProvider;
import com.cloudbees.plugins.credentials.CredentialsScope;
import com.cloudbees.plugins.credentials.domains.Domain;
import com.cloudbees.plugins.credentials.domains.DomainCredentials;
import hudson.EnvVars;
import hudson.model.Queue;
import hudson.model.Result;
import hudson.model.queue.CauseOfBlockage;
import hudson.slaves.DumbSlave;
import hudson.util.Secret;
import java.util.List;
import java.util.concurrent.TimeUnit;
import org.jenkinsci.plugins.plaincredentials.impl.StringCredentialsImpl;
import org.jenkinsci.plugins.workflow.cps.CpsFlowDefinition;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.job.WorkflowRun;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * The {@code node}-block isolation tests (task 10.3c): a real {@code node}
 * block of {@code payments/api} on its own tenant's agent can read that
 * tenant's folder credential, and the same block is blocked, naming both
 * tenants, on a foreign tenant's agent.
 */
public class VarroaTenantIsolationTest {

    static {
        // Fixture-only: by default credentials-plugin gates a build's own
        // folder-credential lookup on who TRIGGERED the build, not on the
        // run's own authenticated identity. Enabling this flag makes the
        // lookup honor the run's own Item.CONFIGURE-implied permission
        // instead, which is what a production cell's Jenkins pod must also
        // set for a tenant build to read its own folder's credentials. Set
        // once, at class-load time, before core's CredentialsProvider
        // class initializes (each test class here forks its own JVM).
        System.setProperty("com.cloudbees.plugins.credentials.UseItemPermission", "true");
    }

    private WorkflowJob paymentsApi() throws Exception {
        Folder folder = j.jenkins.createProject(Folder.class, "payments");
        folder.addProperty(new FolderCredentialsProvider.FolderCredentialsProperty(
            new DomainCredentials[]{
                new DomainCredentials(Domain.global(), List.of(
                    new StringCredentialsImpl(
                        CredentialsScope.GLOBAL, "payments-secret", "", Secret.fromString("shh"))))
            }));
        WorkflowJob api = folder.createProject(WorkflowJob.class, "api");
        api.setDefinition(new CpsFlowDefinition(
            "node('ci-pool') {\n"
                + "  withCredentials([string(credentialsId: 'payments-secret', variable: 'S')]) {\n"
                + "    echo 'credential-lookup-succeeded'\n"
                + "  }\n"
                + "}",
            true));
        return api;
    }

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Test
    public void aFolderCredentialResolvesInsideANodeBlockOnItsOwnTenantAgent() throws Exception {
        VarroaTenantResolverTest.hostTenants(j, "payments", "search");
        // The Pipeline durability layer reloads the run's Authentication
        // through the active SecurityRealm; only VarroaSecurityRealm
        // answers svc-payments (task 10.3b).
        j.jenkins.setSecurityRealm(new VarroaSecurityRealm());
        DumbSlave own = j.createSlave("payments-agent", "ci-pool varroa-tenant-payments", new EnvVars());
        j.waitOnline(own);

        WorkflowJob api = paymentsApi();
        WorkflowRun run = api.scheduleBuild2(0).get(60, TimeUnit.SECONDS);
        j.assertBuildStatus(Result.SUCCESS, run);
        assertTrue(j.getLog(run).contains("credential-lookup-succeeded"));
    }

    @Test
    public void aForeignTenantAgentBlocksTheSameBlockAndNamesBothTenants() throws Exception {
        VarroaTenantResolverTest.hostTenants(j, "payments", "search");
        j.jenkins.setSecurityRealm(new VarroaSecurityRealm());
        DumbSlave foreign = j.createSlave("search-agent", "ci-pool varroa-tenant-search", new EnvVars());
        j.waitOnline(foreign);

        WorkflowJob api = paymentsApi();
        api.scheduleBuild2(0);
        // The WorkflowJob's own top-level task is a FlyweightTask and starts
        // immediately on the controller; only the script's node('ci-pool')
        // step queues the real, executor-bound PlaceholderTask this fence
        // blocks. Poll every current queue item, not one tracked id.
        CauseOfBlockage cob = null;
        long deadline = System.currentTimeMillis() + 30_000;
        while (System.currentTimeMillis() < deadline) {
            for (Queue.Item current : j.jenkins.getQueue().getItems()) {
                CauseOfBlockage candidate = current.getCauseOfBlockage();
                if (candidate != null && candidate.getShortDescription() != null
                        && candidate.getShortDescription().startsWith("Varroa:")) {
                    cob = candidate;
                    break;
                }
            }
            if (cob != null) {
                break;
            }
            Thread.sleep(200);
        }
        assertNotNull("the build must stay blocked on the foreign tenant's agent", cob);
        assertTrue(cob.getShortDescription().contains("payments"));
        assertTrue(cob.getShortDescription().contains("search"));

        for (Queue.Item item : j.jenkins.getQueue().getItems()) {
            j.jenkins.getQueue().cancel(item);
        }
    }
}
