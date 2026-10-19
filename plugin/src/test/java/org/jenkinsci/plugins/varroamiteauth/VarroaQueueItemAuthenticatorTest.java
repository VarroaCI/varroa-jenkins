package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

import com.cloudbees.hudson.plugins.folder.Folder;
import com.cloudbees.hudson.plugins.folder.properties.FolderCredentialsProvider;
import com.cloudbees.plugins.credentials.CredentialsProvider;
import com.cloudbees.plugins.credentials.CredentialsScope;
import com.cloudbees.plugins.credentials.common.StandardUsernamePasswordCredentials;
import com.cloudbees.plugins.credentials.domains.Domain;
import com.cloudbees.plugins.credentials.domains.DomainCredentials;
import com.cloudbees.plugins.credentials.impl.UsernamePasswordCredentialsImpl;
import hudson.ExtensionList;
import hudson.model.FreeStyleProject;
import hudson.model.Queue;
import hudson.model.queue.Tasks;
import hudson.security.ACL;
import java.util.List;
import jenkins.branch.OrganizationFolder;
import jenkins.security.QueueItemAuthenticatorProvider;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.jvnet.hudson.test.MockFolder;
import org.springframework.security.core.Authentication;
import org.springframework.security.core.GrantedAuthority;
import org.springframework.security.core.userdetails.UsernameNotFoundException;

/**
 * Tests for {@link VarroaQueueItemAuthenticator} and
 * {@link VarroaQueueItemAuthenticatorProvider} (tasks 10.2, 10.3).
 */
public class VarroaQueueItemAuthenticatorTest {

    static {
        // Fixture-only, as in VarroaTenantIsolationTest: a tenant identity
        // answers the folder-credential lookup through the item permission.
        System.setProperty("com.cloudbees.plugins.credentials.UseItemPermission", "true");
    }

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Test
    public void theVarroaProviderSortsAboveAuthenticationFromBuild() {
        java.util.List<QueueItemAuthenticatorProvider> providers =
            ExtensionList.lookup(QueueItemAuthenticatorProvider.class);

        int varroaIndex = -1;
        int durableTaskIndex = -1;
        for (int i = 0; i < providers.size(); i++) {
            String cls = providers.get(i).getClass().getName();
            if (providers.get(i) instanceof VarroaQueueItemAuthenticatorProvider) {
                varroaIndex = i;
            }
            if (cls.contains("AuthenticationFromBuild")) {
                durableTaskIndex = i;
            }
        }
        assertTrue("the Varroa provider must be registered", varroaIndex >= 0);
        assertTrue("durable-task's AuthenticationFromBuild must be registered", durableTaskIndex >= 0);
        assertTrue("the Varroa provider must sort above AuthenticationFromBuild",
            varroaIndex < durableTaskIndex);
    }

    @Test
    public void aBuildInATenantFolderRunsAsThatTenant() throws Exception {
        VarroaTenantResolverTest.hostTenants(j, "payments");
        MockFolder folder = j.createFolder("payments");
        FreeStyleProject api = folder.createProject(FreeStyleProject.class, "api");

        Authentication auth = Tasks.getAuthenticationOf2(api);
        assertEquals("svc-payments", auth.getName());
        boolean sawTenantRole = false;
        for (GrantedAuthority a : auth.getAuthorities()) {
            if ("ROLE:varroa:tenant:payments".equals(a.getAuthority())) {
                sawTenantRole = true;
            }
        }
        assertTrue("the tenant authority must be granted", sawTenantRole);
    }

    @Test
    public void aBareTopLevelJobGetsNoIdentity() throws Exception {
        FreeStyleProject bare = j.createFreeStyleProject("bare-job");
        assertNull(new VarroaQueueItemAuthenticator().authenticate2((Queue.Task) bare));
    }

    @Test
    public void aTaskWithNoOwningItemGetsNoIdentity() {
        Queue.Task selfOwning = mock(Queue.Task.class);
        when(selfOwning.getOwnerTask()).thenReturn(selfOwning);
        assertNull(new VarroaQueueItemAuthenticator().authenticate2(selfOwning));
    }

    @Test
    public void theItemOverloadDelegatesToTheTaskOverload() throws Exception {
        VarroaTenantResolverTest.hostTenants(j, "payments");
        MockFolder folder = j.createFolder("payments");
        FreeStyleProject api = folder.createProject(FreeStyleProject.class, "api");

        Queue.WaitingItem item = new Queue.WaitingItem(
            java.util.Calendar.getInstance(), api, java.util.List.of());
        Authentication auth = new VarroaQueueItemAuthenticator().authenticate2((Queue.Item) item);
        assertEquals("svc-payments", auth.getName());
    }

    @Test
    public void aFolderComputationInATenantFolderRunsAsSystem() throws Exception {
        VarroaTenantResolverTest.hostTenants(j, "payments");
        Folder folder = j.jenkins.createProject(Folder.class, "payments");
        OrganizationFolder repos = folder.createProject(OrganizationFolder.class, "repos");
        WorkflowMultiBranchProject mb = folder.createProject(WorkflowMultiBranchProject.class, "mb");

        VarroaQueueItemAuthenticator authenticator = new VarroaQueueItemAuthenticator();
        assertNull(authenticator.authenticate2((Queue.Task) repos));
        assertNull(authenticator.authenticate2((Queue.Task) mb));
        assertEquals(ACL.SYSTEM2, Tasks.getAuthenticationOf2(repos));
        assertEquals(ACL.SYSTEM2, Tasks.getAuthenticationOf2(mb));
    }

    @Test
    public void anOrganizationFolderScanReadsItsTenantFolderCredential() throws Exception {
        VarroaTenantResolverTest.hostTenants(j, "payments");
        Folder folder = j.jenkins.createProject(Folder.class, "payments");
        folder.addProperty(new FolderCredentialsProvider.FolderCredentialsProperty(
            new DomainCredentials[]{
                new DomainCredentials(Domain.global(), List.of(
                    new UsernamePasswordCredentialsImpl(
                        CredentialsScope.GLOBAL, "forgejo-ci", "", "ci", "secret")))
            }));
        OrganizationFolder repos = folder.createProject(OrganizationFolder.class, "repos");
        WorkflowJob api = folder.createProject(WorkflowJob.class, "api");

        List<StandardUsernamePasswordCredentials> scanView = CredentialsProvider.lookupCredentialsInItem(
            StandardUsernamePasswordCredentials.class, repos, Tasks.getAuthenticationOf2(repos),
            List.of());
        assertTrue("the scan must see forgejo-ci",
            scanView.stream().anyMatch(c -> "forgejo-ci".equals(c.getId())));

        // The tenant identity has no user record, so the lookup may throw
        // instead of answering an empty list. Either outcome hides forgejo-ci.
        try {
            List<StandardUsernamePasswordCredentials> tenantView =
                CredentialsProvider.lookupCredentialsInItem(
                    StandardUsernamePasswordCredentials.class, repos, Tasks.getAuthenticationOf2(api),
                    List.of());
            assertTrue("the tenant identity must not see forgejo-ci through a scan",
                tenantView.stream().noneMatch(c -> "forgejo-ci".equals(c.getId())));
        } catch (UsernameNotFoundException unresolved) {
            // No credential is visible to the tenant identity.
        }
    }

    @Test
    public void aPipelineBuildInAFolderStillRunsAsTheTenant() throws Exception {
        VarroaTenantResolverTest.hostTenants(j, "payments");
        Folder folder = j.jenkins.createProject(Folder.class, "payments");
        WorkflowJob api = folder.createProject(WorkflowJob.class, "api");
        assertEquals("svc-payments", Tasks.getAuthenticationOf2(api).getName());
    }

    @Test
    public void theDescriptorIsTrivial() {
        VarroaQueueItemAuthenticator.DescriptorImpl descriptor =
            new VarroaQueueItemAuthenticator.DescriptorImpl();
        assertNotNull(descriptor.getDisplayName());
    }
}
