package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;

import hudson.model.AbstractItem;
import hudson.model.Queue;
import java.io.ByteArrayInputStream;
import java.nio.charset.StandardCharsets;
import javax.xml.transform.stream.StreamSource;
import jenkins.branch.OrganizationFolder;
import org.jenkinsci.plugins.workflow.job.WorkflowJob;
import org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests that creating or saving a folder through config.xml starts its first computation.
 * The items engine uses that path, and the Jenkins API that skips the submit step does not.
 */
public class VarroaComputedFolderFirstScanTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private static String configXml(AbstractItem item) throws Exception {
        return item.getConfigFile().asString();
    }

    private void createFromXml(String name, String xml) throws Exception {
        j.jenkins.createProjectFromXML(name, new ByteArrayInputStream(xml.getBytes(StandardCharsets.UTF_8)));
    }

    @Test
    public void createFromXmlSchedulesOrganizationFolderScan() throws Exception {
        OrganizationFolder template = j.jenkins.createProject(OrganizationFolder.class, "template");
        ScannableOrganizationFolders.makeScannable(template);
        String xml = configXml(template);

        createFromXml("created-org", xml);
        j.waitUntilNoActivity();

        OrganizationFolder created = (OrganizationFolder) j.jenkins.getItem("created-org");
        assertTrue("the first computation ran", ScannableOrganizationFolders.computed(created));
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
    public void updateByXmlSchedulesScan() throws Exception {
        OrganizationFolder folder = j.jenkins.createProject(OrganizationFolder.class, "to-update");
        ScannableOrganizationFolders.makeScannable(folder);
        String xml = configXml(folder);

        folder.updateByXml(new StreamSource(new ByteArrayInputStream(xml.getBytes(StandardCharsets.UTF_8))));
        j.waitUntilNoActivity();

        assertTrue("the first computation ran", ScannableOrganizationFolders.computed(folder));
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
