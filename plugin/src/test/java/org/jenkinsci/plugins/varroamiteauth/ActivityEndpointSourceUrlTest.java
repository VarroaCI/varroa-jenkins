package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;

import java.util.List;
import jenkins.branch.BranchSource;
import jenkins.branch.OrganizationFolder;
import net.sf.json.JSONObject;
import org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/** Tests the sourceUrl field of the multibranch rows of the branch-inventory envelope. */
public class ActivityEndpointSourceUrlTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Test
    public void singleSourceProjectExportsItsSourceUrl() throws Exception {
        WorkflowMultiBranchProject project = j.jenkins.createProject(WorkflowMultiBranchProject.class, "mb");
        project.setSourcesList(List.of(new BranchSource(
                new jenkins.plugins.git.GitSCMSource("https://git.example.org/o/r.git"))));

        JSONObject row = ActivityEndpoint.buildBranchProjectRow(project);

        assertEquals("https://git.example.org/o/r.git", row.getString("sourceUrl"));
    }

    @Test
    public void projectWithTwoSourcesExportsNoUrl() throws Exception {
        WorkflowMultiBranchProject project = j.jenkins.createProject(WorkflowMultiBranchProject.class, "two");
        project.setSourcesList(List.of(
                new BranchSource(new jenkins.plugins.git.GitSCMSource("https://git.example.org/o/a.git")),
                new BranchSource(new jenkins.plugins.git.GitSCMSource("https://git.example.org/o/b.git"))));

        JSONObject row = ActivityEndpoint.buildBranchProjectRow(project);

        assertEquals("", row.getString("sourceUrl"));
    }

    @Test
    public void organizationFolderExportsNoUrl() throws Exception {
        OrganizationFolder folder = j.jenkins.createProject(OrganizationFolder.class, "org");

        JSONObject row = ActivityEndpoint.buildBranchProjectRow(folder);

        assertEquals("", row.getString("sourceUrl"));
    }
}
