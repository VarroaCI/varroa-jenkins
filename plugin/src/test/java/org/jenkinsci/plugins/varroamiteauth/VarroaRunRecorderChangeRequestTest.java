package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNull;

import hudson.model.Action;
import java.util.Collections;
import java.util.List;
import jenkins.branch.Branch;
import jenkins.scm.api.SCMHead;
import jenkins.scm.api.metadata.ObjectMetadataAction;
import jenkins.scm.api.mixin.ChangeRequestCheckoutStrategy;
import jenkins.scm.api.mixin.ChangeRequestSCMHead2;
import org.junit.Test;

/**
 * Proves {@link VarroaRunRecorder#changeRequestOf(Branch)} reads the change
 * request of a branch head. The branch is hand-built, so the test needs no
 * Jenkins instance.
 */
public class VarroaRunRecorderChangeRequestTest {

    /** A pull-request head: an id, a target branch, and an origin branch. */
    static final class PullRequestHead extends SCMHead implements ChangeRequestSCMHead2 {
        private static final long serialVersionUID = 1L;
        private final String id;
        private final SCMHead target;
        private final String origin;

        PullRequestHead(String name, String id, SCMHead target, String origin) {
            super(name);
            this.id = id;
            this.target = target;
            this.origin = origin;
        }

        @Override
        public String getId() {
            return id;
        }

        @Override
        public SCMHead getTarget() {
            return target;
        }

        @Override
        public ChangeRequestCheckoutStrategy getCheckoutStrategy() {
            return ChangeRequestCheckoutStrategy.HEAD;
        }

        @Override
        public String getOriginName() {
            return origin;
        }
    }

    /** A branch whose metadata action is supplied by the test. */
    private static Branch branchWith(SCMHead head, ObjectMetadataAction metadata) {
        List<Action> actions = metadata == null ? Collections.emptyList() : List.of(metadata);
        return new Branch("source", head, null, Collections.emptyList()) {
            private static final long serialVersionUID = 1L;

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
    public void pullRequestHeadCarriesEveryField() {
        SCMHead target = new SCMHead("main");
        PullRequestHead head = new PullRequestHead("PR-12", "12", target, "fix/cache");
        ObjectMetadataAction metadata = new ObjectMetadataAction("Fix the cache", "", "https://forge.example/pr/12");

        BuildEvent.ChangeRequestInfo cr = VarroaRunRecorder.changeRequestOf(branchWith(head, metadata));

        assertEquals("12", cr.getId());
        assertEquals("Fix the cache", cr.getTitle());
        assertEquals("fix/cache", cr.getSourceBranch());
        assertEquals("main", cr.getTargetBranch());
        assertEquals("https://forge.example/pr/12", cr.getUrl());
    }

    @Test
    public void branchHeadYieldsNoChangeRequest() {
        assertNull(VarroaRunRecorder.changeRequestOf(branchWith(new SCMHead("main"), null)));
    }

    @Test
    public void nullBranchYieldsNoChangeRequest() {
        assertNull(VarroaRunRecorder.changeRequestOf(null));
    }
}
