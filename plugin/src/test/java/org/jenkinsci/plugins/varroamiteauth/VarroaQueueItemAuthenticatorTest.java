package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

import hudson.ExtensionList;
import hudson.model.FreeStyleProject;
import hudson.model.Queue;
import hudson.model.queue.Tasks;
import jenkins.security.QueueItemAuthenticatorProvider;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;
import org.jvnet.hudson.test.MockFolder;
import org.springframework.security.core.Authentication;
import org.springframework.security.core.GrantedAuthority;

/**
 * Tests for {@link VarroaQueueItemAuthenticator} and
 * {@link VarroaQueueItemAuthenticatorProvider} (tasks 10.2, 10.3).
 */
public class VarroaQueueItemAuthenticatorTest {

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
    public void theDescriptorIsTrivial() {
        VarroaQueueItemAuthenticator.DescriptorImpl descriptor =
            new VarroaQueueItemAuthenticator.DescriptorImpl();
        assertNotNull(descriptor.getDisplayName());
    }
}
