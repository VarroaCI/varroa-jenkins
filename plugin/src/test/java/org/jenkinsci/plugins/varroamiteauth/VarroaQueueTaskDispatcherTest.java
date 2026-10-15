package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertTrue;

import hudson.EnvVars;
import hudson.model.FreeStyleProject;
import hudson.model.Label;
import hudson.model.Queue;
import hudson.model.Result;
import hudson.model.queue.CauseOfBlockage;
import hudson.slaves.DumbSlave;
import java.util.concurrent.TimeUnit;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.jvnet.hudson.test.MockFolder;

/**
 * Tests for {@link VarroaQueueTaskDispatcher} (task 10.4): the reserved
 * {@code varroa-tenant-<team>} node label is the only signal the fence
 * reads.
 */
public class VarroaQueueTaskDispatcherTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private CauseOfBlockage waitForBlockage(Queue.Item item) throws InterruptedException {
        long deadline = System.currentTimeMillis() + 30_000;
        CauseOfBlockage cob = null;
        while (System.currentTimeMillis() < deadline) {
            Queue.Item current = j.jenkins.getQueue().getItem(item.getId());
            if (current != null) {
                cob = current.getCauseOfBlockage();
                if (cob != null && cob.getShortDescription() != null
                        && cob.getShortDescription().startsWith("Varroa:")) {
                    return cob;
                }
            }
            Thread.sleep(200);
        }
        return cob;
    }

    @Test
    public void aForeignTenantAgentBlocksTheBuildAndNamesBothTenants() throws Exception {
        VarroaTenantResolverTest.hostTenants(j, "payments", "search");
        DumbSlave search = j.createSlave("search-agent", "shared-pool varroa-tenant-search", new EnvVars());
        j.waitOnline(search);

        MockFolder folder = j.createFolder("payments");
        FreeStyleProject api = folder.createProject(FreeStyleProject.class, "api");
        api.setAssignedLabel(Label.get("shared-pool"));

        Queue.WaitingItem waiting = j.jenkins.getQueue().schedule(api, 0);
        assertNotNull("the build must be queued", waiting);
        CauseOfBlockage cob = waitForBlockage(waiting);
        assertNotNull("the build must stay blocked", cob);
        assertTrue(cob.getShortDescription().contains("payments"));
        assertTrue(cob.getShortDescription().contains("search"));

        j.jenkins.getQueue().cancel(waiting);
    }

    @Test
    public void anUnlabelledNodeBlocksTheBuild() throws Exception {
        VarroaTenantResolverTest.hostTenants(j, "payments");
        DumbSlave plain = j.createSlave("plain-agent", "shared-pool", new EnvVars());
        j.waitOnline(plain);

        MockFolder folder = j.createFolder("payments");
        FreeStyleProject api = folder.createProject(FreeStyleProject.class, "api");
        api.setAssignedLabel(Label.get("shared-pool"));

        Queue.WaitingItem waiting = j.jenkins.getQueue().schedule(api, 0);
        assertNotNull("the build must be queued", waiting);
        CauseOfBlockage cob = waitForBlockage(waiting);
        assertNotNull("an unlabelled node has no proven owner, so the build must stay blocked", cob);

        j.jenkins.getQueue().cancel(waiting);
    }

    @Test
    public void anAmbiguousUserPrefixDoesNotGrantAForeignNode() throws Exception {
        VarroaTenantResolverTest.hostTenants(j, "pay", "pay-prod");
        DumbSlave prod = j.createSlave(
            "pay-prod-agent", "pay-prod-linux varroa-tenant-pay-prod", new EnvVars());
        j.waitOnline(prod);

        MockFolder folder = j.createFolder("pay");
        FreeStyleProject api = folder.createProject(FreeStyleProject.class, "api");
        api.setAssignedLabel(Label.get("pay-prod-linux"));

        Queue.WaitingItem waiting = j.jenkins.getQueue().schedule(api, 0);
        assertNotNull("the build must be queued", waiting);
        CauseOfBlockage cob = waitForBlockage(waiting);
        assertNotNull("the reserved label names pay-prod, not pay, so the build must stay blocked", cob);
        assertTrue(cob.getShortDescription().contains("pay"));
        assertTrue(cob.getShortDescription().contains("pay-prod"));

        j.jenkins.getQueue().cancel(waiting);
    }

    @Test
    public void aNodeLabelledForTwoTenantsBlocksBothTenants() throws Exception {
        VarroaTenantResolverTest.hostTenants(j, "payments", "search");
        DumbSlave ambiguous = j.createSlave(
            "ambiguous-agent",
            "shared-pool varroa-tenant-payments varroa-tenant-search",
            new EnvVars());
        j.waitOnline(ambiguous);

        MockFolder paymentsFolder = j.createFolder("payments");
        FreeStyleProject paymentsApi = paymentsFolder.createProject(FreeStyleProject.class, "api");
        paymentsApi.setAssignedLabel(Label.get("shared-pool"));

        Queue.WaitingItem paymentsWaiting = j.jenkins.getQueue().schedule(paymentsApi, 0);
        assertNotNull("the payments build must be queued", paymentsWaiting);
        CauseOfBlockage paymentsCob = waitForBlockage(paymentsWaiting);
        assertNotNull(
            "an ambiguous node has no proven owner, so the payments build must stay blocked",
            paymentsCob);
        j.jenkins.getQueue().cancel(paymentsWaiting);

        MockFolder searchFolder = j.createFolder("search");
        FreeStyleProject searchApi = searchFolder.createProject(FreeStyleProject.class, "api");
        searchApi.setAssignedLabel(Label.get("shared-pool"));

        Queue.WaitingItem searchWaiting = j.jenkins.getQueue().schedule(searchApi, 0);
        assertNotNull("the search build must be queued", searchWaiting);
        CauseOfBlockage searchCob = waitForBlockage(searchWaiting);
        assertNotNull(
            "an ambiguous node has no proven owner, so the search build must stay blocked",
            searchCob);
        j.jenkins.getQueue().cancel(searchWaiting);
    }

    @Test
    public void aNonTenantBuildIsNotFenced() throws Exception {
        FreeStyleProject top = j.createFreeStyleProject("top-level-job");
        var run = top.scheduleBuild2(0).get(60, TimeUnit.SECONDS);
        j.assertBuildStatus(Result.SUCCESS, run);
    }

    /**
     * Task 4.5: the reserved token decides the owner by exact whole-token
     * match, not by a {@code <team>-} prefix test. A node carrying
     * {@code varroa-tenant-pay-prod} grants the {@code pay-prod} build and
     * still refuses the {@code pay} build, even though {@code pay} is a
     * prefix of {@code pay-prod}.
     */
    @Test
    public void aReservedTokenNamingPayProdResolvesPayProdOnly() throws Exception {
        VarroaTenantResolverTest.hostTenants(j, "pay", "pay-prod");
        DumbSlave prod = j.createSlave(
            "pay-prod-agent", "pay-prod-linux varroa-tenant-pay-prod", new EnvVars());
        j.waitOnline(prod);

        MockFolder prodFolder = j.createFolder("pay-prod");
        FreeStyleProject prodApi = prodFolder.createProject(FreeStyleProject.class, "api");
        prodApi.setAssignedLabel(Label.get("pay-prod-linux"));
        var prodRun = prodApi.scheduleBuild2(0).get(60, TimeUnit.SECONDS);
        j.assertBuildStatus(Result.SUCCESS, prodRun);

        MockFolder payFolder = j.createFolder("pay");
        FreeStyleProject payApi = payFolder.createProject(FreeStyleProject.class, "api");
        payApi.setAssignedLabel(Label.get("pay-prod-linux"));
        Queue.WaitingItem payWaiting = j.jenkins.getQueue().schedule(payApi, 0);
        assertNotNull("the pay build must be queued", payWaiting);
        CauseOfBlockage payCob = waitForBlockage(payWaiting);
        assertNotNull("the token names pay-prod, not pay, so the pay build must stay blocked", payCob);
        assertTrue(payCob.getShortDescription().contains("pay-prod"));
        j.jenkins.getQueue().cancel(payWaiting);
    }

    /**
     * Task 4.5: a node label that merely embeds a team name is not the
     * reserved token. {@code search-linux} carries no
     * {@code varroa-tenant-} token, so the node resolves no owner and the
     * fence blocks the build, even though the team is named {@code search}.
     */
    @Test
    public void aNodeCarryingOnlyANonReservedLabelResolvesNoOwner() throws Exception {
        VarroaTenantResolverTest.hostTenants(j, "search");
        DumbSlave plain = j.createSlave("search-linux-agent", "search-linux", new EnvVars());
        j.waitOnline(plain);

        MockFolder folder = j.createFolder("search");
        FreeStyleProject api = folder.createProject(FreeStyleProject.class, "api");
        api.setAssignedLabel(Label.get("search-linux"));

        Queue.WaitingItem waiting = j.jenkins.getQueue().schedule(api, 0);
        assertNotNull("the build must be queued", waiting);
        CauseOfBlockage cob = waitForBlockage(waiting);
        assertNotNull(
            "search-linux carries no varroa-tenant- token, so the node has no proven owner",
            cob);

        j.jenkins.getQueue().cancel(waiting);
    }

    /**
     * Task 4.5: the fence never tests the token's remainder against the
     * set of tenants the cell hosts. A node reserved for an unhosted
     * tenant still blocks every other tenant's build, and the blockage
     * names the tenant the token carries.
     */
    @Test
    public void aTokenNamingAnUnhostedTenantStillBlocksEveryOtherTenant() throws Exception {
        VarroaTenantResolverTest.hostTenants(j, "payments");
        DumbSlave orphaned = j.createSlave(
            "orphaned-agent", "shared-pool varroa-tenant-orphan", new EnvVars());
        j.waitOnline(orphaned);

        MockFolder folder = j.createFolder("payments");
        FreeStyleProject api = folder.createProject(FreeStyleProject.class, "api");
        api.setAssignedLabel(Label.get("shared-pool"));

        Queue.WaitingItem waiting = j.jenkins.getQueue().schedule(api, 0);
        assertNotNull("the build must be queued", waiting);
        CauseOfBlockage cob = waitForBlockage(waiting);
        assertNotNull(
            "the token names a tenant the cell does not host, so the fence still blocks the "
                + "payments build",
            cob);
        assertTrue(cob.getShortDescription().contains("payments"));
        assertTrue(cob.getShortDescription().contains("orphan"));

        j.jenkins.getQueue().cancel(waiting);
    }

    /**
     * Task 4.6: a node rendered from a {@code Runner} for team alpha
     * carries the labels D3 renders — the reserved token first, then the
     * mandatory {@code <team>-runner} label, then a prefixed declared
     * label. That node takes only alpha's builds and refuses bravo's,
     * naming both tenants in the blockage. A node with no reserved token
     * refuses both teams.
     */
    @Test
    public void aRunnerRenderedNodeFencesByTeamAndAnUnstampedNodeBlocksBoth() throws Exception {
        VarroaTenantResolverTest.hostTenants(j, "alpha", "bravo");

        DumbSlave runnerA = j.createSlave(
            "runner-a", "varroa-tenant-alpha alpha-runner alpha-linux", new EnvVars());
        j.waitOnline(runnerA);

        MockFolder alphaFolder = j.createFolder("alpha");
        FreeStyleProject alphaApi = alphaFolder.createProject(FreeStyleProject.class, "api");
        alphaApi.setAssignedLabel(Label.get("alpha-runner"));
        var alphaRun = alphaApi.scheduleBuild2(0).get(60, TimeUnit.SECONDS);
        j.assertBuildStatus(Result.SUCCESS, alphaRun);

        MockFolder bravoFolder = j.createFolder("bravo");
        FreeStyleProject bravoApi = bravoFolder.createProject(FreeStyleProject.class, "api");
        bravoApi.setAssignedLabel(Label.get("alpha-runner"));
        Queue.WaitingItem bravoWaiting = j.jenkins.getQueue().schedule(bravoApi, 0);
        assertNotNull("the bravo build must be queued", bravoWaiting);
        CauseOfBlockage bravoCob = waitForBlockage(bravoWaiting);
        assertNotNull("team alpha's runner must refuse team bravo's build", bravoCob);
        assertTrue(bravoCob.getShortDescription().contains("alpha"));
        assertTrue(bravoCob.getShortDescription().contains("bravo"));
        j.jenkins.getQueue().cancel(bravoWaiting);

        DumbSlave unstamped = j.createSlave("unstamped-agent", "shared-pool", new EnvVars());
        j.waitOnline(unstamped);

        FreeStyleProject alphaShared = alphaFolder.createProject(FreeStyleProject.class, "shared");
        alphaShared.setAssignedLabel(Label.get("shared-pool"));
        Queue.WaitingItem alphaSharedWaiting = j.jenkins.getQueue().schedule(alphaShared, 0);
        assertNotNull("the alpha build must be queued", alphaSharedWaiting);
        CauseOfBlockage alphaSharedCob = waitForBlockage(alphaSharedWaiting);
        assertNotNull(
            "an unstamped node has no proven owner, so team alpha must stay blocked too",
            alphaSharedCob);
        j.jenkins.getQueue().cancel(alphaSharedWaiting);

        FreeStyleProject bravoShared = bravoFolder.createProject(FreeStyleProject.class, "shared");
        bravoShared.setAssignedLabel(Label.get("shared-pool"));
        Queue.WaitingItem bravoSharedWaiting = j.jenkins.getQueue().schedule(bravoShared, 0);
        assertNotNull("the bravo build must be queued", bravoSharedWaiting);
        CauseOfBlockage bravoSharedCob = waitForBlockage(bravoSharedWaiting);
        assertNotNull(
            "an unstamped node has no proven owner, so team bravo must stay blocked too",
            bravoSharedCob);
        j.jenkins.getQueue().cancel(bravoSharedWaiting);
    }
}
