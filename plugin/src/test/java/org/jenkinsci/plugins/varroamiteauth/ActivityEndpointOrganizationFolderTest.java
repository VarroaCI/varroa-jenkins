package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotNull;

import jenkins.branch.OrganizationFolder;
import net.sf.json.JSONArray;
import net.sf.json.JSONObject;
import org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject;
import org.junit.Rule;
import org.junit.Test;
import org.jvnet.hudson.test.JenkinsRule;

/** Tests the organization-folder rows of the branch-inventory envelope. */
public class ActivityEndpointOrganizationFolderTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Test
    public void envelopeIncludesOrganizationFolderRow() throws Exception {
        OrganizationFolder folder = j.jenkins.createProject(OrganizationFolder.class, "org");
        WorkflowMultiBranchProject repo = new WorkflowMultiBranchProject(folder, "repo");
        folder.addLoadedChild(repo, "repo");

        JSONObject envelope = ActivityEndpoint.buildBranchEnvelope(j.jenkins);

        JSONObject row = findRow(envelope.getJSONArray("projects"), "org");
        assertNotNull(row);
        JSONArray heads = row.getJSONArray("heads");
        assertEquals(1, heads.size());
        JSONObject repository = heads.getJSONObject(0);
        assertEquals("repository", repository.getString("kind"));
        assertEquals("repo", repository.getString("name"));
        assertEquals("org/repo", repository.getString("jobPath"));
        assertEquals("", repository.getString("revision"));
        assertFalse(repository.getBoolean("orphaned"));
    }

    private static JSONObject findRow(JSONArray projects, String jobPath) {
        for (int i = 0; i < projects.size(); i++) {
            JSONObject row = projects.getJSONObject(i);
            if (jobPath.equals(row.getString("jobPath"))) {
                return row;
            }
        }
        return null;
    }
}
