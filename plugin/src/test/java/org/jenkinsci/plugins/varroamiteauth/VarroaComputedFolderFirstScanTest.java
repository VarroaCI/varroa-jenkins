package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;

import com.cloudbees.hudson.plugins.folder.computed.ComputedFolder;
import hudson.Extension;
import hudson.model.AbstractItem;
import hudson.model.Item;
import hudson.model.Queue;
import hudson.model.queue.QueueListener;
import java.io.ByteArrayInputStream;
import java.net.URL;
import java.nio.charset.StandardCharsets;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicInteger;
import jenkins.branch.OrganizationFolder;
import org.htmlunit.HttpMethod;
import org.htmlunit.WebRequest;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests when the listener starts a first folder computation. The items engine creates
 * an item over HTTP and replaces a configuration with a config.xml POST. A plain save of
 * the folder starts no computation.
 */
public class VarroaComputedFolderFirstScanTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Before
    public void clearCounts() {
        ComputationCounter.COUNTS.clear();
    }

    private static String configXml(AbstractItem item) throws Exception {
        return item.getConfigFile().asString();
    }

    private void createFromXml(String name, String xml) throws Exception {
        j.jenkins.createProjectFromXML(name, new ByteArrayInputStream(xml.getBytes(StandardCharsets.UTF_8)));
    }

    private static int computations(String fullName) {
        AtomicInteger count = ComputationCounter.COUNTS.get(fullName);
        return count == null ? 0 : count.get();
    }

    /** Counts the computations that leave the queue without being cancelled. */
    @Extension
    public static class ComputationCounter extends QueueListener {

        static final ConcurrentHashMap<String, AtomicInteger> COUNTS = new ConcurrentHashMap<>();

        @Override
        public void onLeft(Queue.LeftItem li) {
            if (!li.isCancelled() && li.task instanceof ComputedFolder) {
                COUNTS.computeIfAbsent(((Item) li.task).getFullName(), k -> new AtomicInteger()).incrementAndGet();
            }
        }
    }

    @Test
    public void createFromXmlStartsOneOrganizationFolderScan() throws Exception {
        OrganizationFolder template = j.jenkins.createProject(OrganizationFolder.class, "template");
        ScannableOrganizationFolders.makeScannable(template);
        String xml = configXml(template);

        createFromXml("created-org", xml);
        j.waitUntilNoActivity();

        OrganizationFolder created = (OrganizationFolder) j.jenkins.getItem("created-org");
        assertTrue("the first computation ran", ScannableOrganizationFolders.computed(created));
        assertEquals(1, computations("created-org"));
    }

    @Test
    public void createItemOverHttpStartsOneScan() throws Exception {
        OrganizationFolder template = j.jenkins.createProject(OrganizationFolder.class, "template-http");
        ScannableOrganizationFolders.makeScannable(template);
        String xml = configXml(template);

        j.jenkins.setCrumbIssuer(null);
        JenkinsRule.WebClient wc = j.createWebClient();
        WebRequest request = new WebRequest(new URL(j.getURL(), "createItem?name=http-org"), HttpMethod.POST);
        request.setAdditionalHeader("Content-Type", "application/xml");
        request.setRequestBody(xml);
        wc.getPage(request);
        j.waitUntilNoActivity();

        OrganizationFolder created = (OrganizationFolder) j.jenkins.getItem("http-org");
        assertNotNull("the folder was created", created);
        assertTrue("the first computation ran", ScannableOrganizationFolders.computed(created));
        assertEquals(1, computations("http-org"));
    }

    @Test
    public void createFromXmlSchedulesMultibranchScan() throws Exception {
        WorkflowMultiBranchProject template = j.jenkins.createProject(WorkflowMultiBranchProject.class, "template-mb");
        String xml = configXml(template);

        createFromXml("created-mb", xml);
        j.waitUntilNoActivity();

        WorkflowMultiBranchProject created = (WorkflowMultiBranchProject) j.jenkins.getItem("created-mb");
        assertTrue("the first computation ran", ScannableOrganizationFolders.computed(created));
    }

    @Test
    public void plainPipelineIsNotScheduled() throws Exception {
        j.jenkins.setNumExecutors(0);
        WorkflowJob template = j.jenkins.createProject(WorkflowJob.class, "template-job");
        String xml = configXml(template);

        createFromXml("created-job", xml);

        assertNotNull(j.jenkins.getItem("created-job"));
        assertNull(queuedItem("created-job"));
    }

    @Test
    public void configXmlPostSchedulesOneScan() throws Exception {
        OrganizationFolder folder = j.jenkins.createProject(OrganizationFolder.class, "to-update");
        ScannableOrganizationFolders.makeScannable(folder);
        j.waitUntilNoActivity();
        ComputationCounter.COUNTS.clear();
        String xml = configXml(folder);

        j.jenkins.setCrumbIssuer(null);
        JenkinsRule.WebClient wc = j.createWebClient();
        WebRequest request = new WebRequest(new URL(j.getURL(), "job/to-update/config.xml"), HttpMethod.POST);
        request.setAdditionalHeader("Content-Type", "application/xml");
        request.setRequestBody(xml);
        wc.getPage(request);
        j.waitUntilNoActivity();

        assertTrue("the first computation ran", ScannableOrganizationFolders.computed(folder));
        assertEquals(1, computations("to-update"));
    }

    @Test
    public void saveDoesNotScheduleScan() throws Exception {
        OrganizationFolder folder = j.jenkins.createProject(OrganizationFolder.class, "saved-folder");
        j.waitUntilNoActivity();
        ComputationCounter.COUNTS.clear();

        folder.setDisplayName("saved folder");
        folder.save();
        j.waitUntilNoActivity();

        assertEquals(0, computations("saved-folder"));
    }

    private static Queue.Item queuedItem(String fullName) {
        for (Queue.Item item : Queue.getInstance().getItems()) {
            if (item.task.getFullDisplayName().contains(fullName)) {
                return item;
            }
        }
        return null;
    }

}
