package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertTrue;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

import hudson.model.FreeStyleProject;
import hudson.model.Item;
import hudson.security.ACL;
import hudson.security.ACLContext;
import java.io.BufferedReader;
import java.io.StringReader;
import java.io.StringWriter;
import java.util.List;
import jenkins.model.Jenkins;
import org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject;
import net.sf.json.JSONNull;
import net.sf.json.JSONObject;
import org.junit.Rule;
import org.junit.Test;
import org.springframework.security.authentication.UsernamePasswordAuthenticationToken;
import org.springframework.security.core.authority.SimpleGrantedAuthority;
import org.kohsuke.stapler.StaplerRequest2;
import org.kohsuke.stapler.StaplerResponse2;
import org.jvnet.hudson.test.JenkinsRule;
import org.jvnet.hudson.test.MockAuthorizationStrategy;

/** Tests for the {@code POST /varroa-run/scan} route of {@link VarroaRunTriggerAction}. */
public class VarroaRunTriggerActionScanTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    private static final String MITE_AUTHORITY = "ROLE:varroa:system-mite";

    private static StaplerRequest2 scanRequest(String json) throws Exception {
        StaplerRequest2 req = mock(StaplerRequest2.class);
        when(req.getMethod()).thenReturn("POST");
        when(req.getReader()).thenReturn(new BufferedReader(new StringReader(json)));
        return req;
    }

    private static ACLContext asMite() {
        return ACL.as2(new UsernamePasswordAuthenticationToken(
                "mite", "", List.of(new SimpleGrantedAuthority(MITE_AUTHORITY))));
    }

    private static String body(String scanId, String jobPath, String actor) {
        return "{\"scanId\":\"" + scanId + "\",\"jobPath\":\"" + jobPath
                + "\",\"actingUser\":{\"name\":\"" + actor + "\"}}";
    }

    @Test
    public void unknownJobAnswers404() throws Exception {
        VarroaRunTriggerAction action = new VarroaRunTriggerAction();
        StringWriter out = new StringWriter();
        StaplerResponse2 rsp = RefusalAssert.capturing(out);

        try (ACLContext ignored = asMite()) {
            action.doScan(scanRequest(body("scan-404", "no-such-job", "ada")), rsp);
        }

        RefusalAssert.assertRefused(rsp, 404, "Not Found: no item named no-such-job");
    }

    @Test
    public void nonMultibranchItemAnswers409() throws Exception {
        j.jenkins.setNumExecutors(0);
        FreeStyleProject p = j.createFreeStyleProject("scan-freestyle");
        VarroaRunTriggerAction action = new VarroaRunTriggerAction();
        StringWriter out = new StringWriter();
        StaplerResponse2 rsp = RefusalAssert.capturing(out);

        try (ACLContext ignored = asMite()) {
            action.doScan(scanRequest(body("scan-409", p.getFullName(), "ada")), rsp);
        }

        RefusalAssert.assertRefused(rsp, 409, "Conflict: " + p.getFullName() + " is not a multibranch project");
    }

    @Test
    public void actingUserWithoutBuildPermissionAnswers403AndRecordsRefusal() throws Exception {
        j.jenkins.setNumExecutors(0);
        // Every identity reads every item, and no identity holds Item.BUILD.
        j.jenkins.setSecurityRealm(j.createDummySecurityRealm());
        j.jenkins.setAuthorizationStrategy(new MockAuthorizationStrategy()
                .grant(Jenkins.READ, Item.READ).everywhere().toEveryone());
        WorkflowMultiBranchProject mb = j.jenkins.createProject(WorkflowMultiBranchProject.class, "scan-denied");

        VarroaRunTriggerAction action = new VarroaRunTriggerAction();
        StringWriter out = new StringWriter();
        StaplerResponse2 rsp = RefusalAssert.capturing(out);

        try (ACLContext ignored = asMite()) {
            action.doScan(scanRequest(body("scan-403", mb.getFullName(), "bob")), rsp);
        }

        RefusalAssert.assertRefused(rsp, 403, "Forbidden: the acting user may not scan this job");
        JSONObject row = (JSONObject) VarroaBranchScans.lastScanJson(mb.getFullName());
        assertEquals("refused", row.getString("state"));
        assertEquals("scan-403", row.getString("scanId"));
    }

    @Test
    public void aCallerWithoutTheMiteAuthorityAnswers403AndRecordsNoScan() throws Exception {
        j.jenkins.setNumExecutors(0);
        WorkflowMultiBranchProject mb = j.jenkins.createProject(WorkflowMultiBranchProject.class, "scan-no-mite");
        VarroaRunTriggerAction action = new VarroaRunTriggerAction();
        StringWriter out = new StringWriter();
        StaplerResponse2 rsp = RefusalAssert.capturing(out);

        try (ACLContext ignored = ACL.as2(new UsernamePasswordAuthenticationToken(
                "bob", "", List.of(new SimpleGrantedAuthority("ROLE:varroa:developer"))))) {
            action.doScan(scanRequest(body("scan-no-mite", mb.getFullName(), "ada")), rsp);
        }

        RefusalAssert.assertRefused(rsp, 403, "Forbidden: requires ROLE:varroa:system-mite authority");
        // lastScanJson answers JSONNull, not Java null, when no scan is recorded.
        assertTrue(VarroaBranchScans.lastScanJson(mb.getFullName()) instanceof JSONNull);
    }

    @Test
    public void queueRefusalAnswers409AndRecordsTheRefusal() throws Exception {
        // A project with no SCM source gives the queue no computation to
        // accept, so the route takes its refusal branch. The 202 branch
        // needs a sourced project and is not covered here.
        j.jenkins.setNumExecutors(0);
        WorkflowMultiBranchProject mb = j.jenkins.createProject(WorkflowMultiBranchProject.class, "scan-refused");
        VarroaRunTriggerAction action = new VarroaRunTriggerAction();
        StringWriter out = new StringWriter();
        StaplerResponse2 rsp = RefusalAssert.capturing(out);

        try (ACLContext ignored = asMite()) {
            action.doScan(scanRequest(body("scan-409", mb.getFullName(), "ada")), rsp);
        }

        RefusalAssert.assertRefused(rsp, 409, "Conflict: the queue refused to scan " + mb.getFullName());
        JSONObject row = (JSONObject) VarroaBranchScans.lastScanJson(mb.getFullName());
        assertEquals("refused", row.getString("state"));
        assertEquals("scan-409", row.getString("scanId"));
    }
}
