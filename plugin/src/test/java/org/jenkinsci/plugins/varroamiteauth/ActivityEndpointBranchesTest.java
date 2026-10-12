package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import hudson.model.Action;
import java.util.Collections;
import java.util.List;
import jenkins.branch.Branch;
import jenkins.scm.api.SCMHead;
import jenkins.scm.api.metadata.ObjectMetadataAction;
import jenkins.scm.api.mixin.TagSCMHead;
import net.sf.json.JSONObject;
import org.junit.Test;

/**
 * Proves the branch-inventory envelope has its empty shape when no Jenkins
 * instance answers, and that the head row reads a branch, a change request,
 * a tag, and an orphaned branch. The branches are hand-built, so the tests
 * need no Jenkins instance and no SCM source.
 */
public class ActivityEndpointBranchesTest {

    /** A tag head: an SCM head that Jenkins reports as a tag. */
    static final class TagHead extends SCMHead implements TagSCMHead {
        private static final long serialVersionUID = 1L;

        TagHead(String name) {
            super(name);
        }

        @Override
        public long getTimestamp() {
            return 0L;
        }
    }

    /** A branch with the given head, optional metadata, and buildable state. */
    private static Branch branchWith(SCMHead head, ObjectMetadataAction metadata, boolean buildable) {
        List<Action> actions = metadata == null ? Collections.emptyList() : List.of(metadata);
        return new Branch("source", head, null, Collections.emptyList()) {
            private static final long serialVersionUID = 1L;

            @Override
            public boolean isBuildable() {
                return buildable;
            }

            @Override
            public <T extends Action> T getAction(Class<T> type) {
                for (Action a : actions) {
                    if (type.isInstance(a)) {
                        return type.cast(a);
                    }
                }
                return null;
            }
        };
    }

    @Test
    public void noJenkinsInstanceAnswersAnEmptyInventory() {
        JSONObject envelope = ActivityEndpoint.buildBranchEnvelope(null);

        assertEquals(0, envelope.getJSONArray("projects").size());
        assertEquals(0, envelope.getInt("total"));
        assertFalse(envelope.getBoolean("truncated"));
    }

    @Test
    public void branchHeadRowCarriesKindRevisionAndNoRequest() {
        Branch branch = branchWith(new SCMHead("feature/login"), null, true);

        JSONObject row = ActivityEndpoint.buildBranchHeadRow("payments/mb/feature/login", "abc123", branch);

        assertEquals("branch", row.getString("kind"));
        assertEquals("payments/mb/feature/login", row.getString("jobPath"));
        assertEquals("abc123", row.getString("revision"));
        assertFalse(row.getBoolean("orphaned"));
        assertFalse(row.has("changeRequest"));
    }

    @Test
    public void changeRequestHeadCarriesItsRequest() {
        VarroaRunRecorderChangeRequestTest.PullRequestHead head =
                new VarroaRunRecorderChangeRequestTest.PullRequestHead(
                        "PR-12", "12", new SCMHead("main"), "fix/cache");
        ObjectMetadataAction metadata =
                new ObjectMetadataAction("Fix the cache", "", "https://forge.example/pr/12");

        JSONObject row = ActivityEndpoint.buildBranchHeadRow(
                "payments/mb/PR-12", "def456", branchWith(head, metadata, true));

        assertEquals("changeRequest", row.getString("kind"));
        JSONObject request = row.getJSONObject("changeRequest");
        assertEquals("12", request.getString("id"));
        assertEquals("Fix the cache", request.getString("title"));
        assertEquals("fix/cache", request.getString("sourceBranch"));
        assertEquals("main", request.getString("targetBranch"));
        assertEquals("https://forge.example/pr/12", request.getString("url"));
    }

    @Test
    public void tagHeadIsReportedAsATag() {
        Branch branch = branchWith(new TagHead("v1.0"), null, true);

        JSONObject row = ActivityEndpoint.buildBranchHeadRow("payments/mb/v1.0", "", branch);

        assertEquals("tag", row.getString("kind"));
        assertFalse(row.has("changeRequest"));
    }

    @Test
    public void unbuildableBranchIsMarkedOrphaned() {
        Branch branch = branchWith(new SCMHead("old-feature"), null, false);

        JSONObject row = ActivityEndpoint.buildBranchHeadRow("payments/mb/old-feature", "9f8e", branch);

        assertTrue(row.getBoolean("orphaned"));
        assertEquals("branch", row.getString("kind"));
    }
}
