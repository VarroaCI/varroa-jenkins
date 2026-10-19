package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNull;

import hudson.model.Action;
import java.util.Collections;
import java.util.List;
import jenkins.branch.Branch;
import jenkins.scm.api.SCMHead;
import jenkins.scm.api.SCMRevision;
import jenkins.scm.api.metadata.ObjectMetadataAction;
import jenkins.scm.api.mixin.ChangeRequestCheckoutStrategy;
import jenkins.scm.api.mixin.ChangeRequestSCMHead2;
import jenkins.scm.api.mixin.ChangeRequestSCMRevision;
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

    /** A GitHub-style revision: the head commit and the merge commit are separate accessors. */
    static final class GitHubStyleRevision extends ChangeRequestSCMRevision<PullRequestHead> {
        private static final long serialVersionUID = 1L;
        private final String pullHash;
        private final String mergeHash;

        GitHubStyleRevision(PullRequestHead head, SCMRevision target, String pullHash, String mergeHash) {
            super(head, target);
            this.pullHash = pullHash;
            this.mergeHash = mergeHash;
        }

        public String getPullHash() {
            return pullHash;
        }

        public String getMergeHash() {
            return mergeHash;
        }

        @Override
        public boolean equivalent(ChangeRequestSCMRevision<?> other) {
            return equals(other);
        }

        @Override
        protected int _hashCode() {
            return pullHash.hashCode();
        }

    }

    /** A plain revision answering a hash through its origin revision, as Gitea does. */
    static final class OriginHash {
        private final String hash;

        OriginHash(String hash) {
            this.hash = hash;
        }

        public String getHash() {
            return hash;
        }
    }

    /** A Gitea-style revision: the head commit sits on an origin revision. */
    static final class GiteaStyleRevision extends ChangeRequestSCMRevision<PullRequestHead> {
        private static final long serialVersionUID = 1L;
        private final OriginHash origin;

        GiteaStyleRevision(PullRequestHead head, SCMRevision target, OriginHash origin) {
            super(head, target);
            this.origin = origin;
        }

        public OriginHash getOrigin() {
            return origin;
        }

        @Override
        public boolean equivalent(ChangeRequestSCMRevision<?> other) {
            return equals(other);
        }

        @Override
        protected int _hashCode() {
            return origin.hashCode();
        }

    }

    /** A branch revision: a plain SCM revision with its own hash. */
    static final class BranchStyleRevision extends SCMRevision {
        private static final long serialVersionUID = 1L;
        private final String hash;

        BranchStyleRevision(SCMHead head, String hash) {
            super(head);
            this.hash = hash;
        }

        public String getHash() {
            return hash;
        }

        @Override
        public boolean equals(Object o) {
            return o instanceof BranchStyleRevision && hash.equals(((BranchStyleRevision) o).hash);
        }

        @Override
        public int hashCode() {
            return hash.hashCode();
        }
    }

    /** A revision that has no known hash accessor. */
    static final class OpaqueChangeRequestRevision extends ChangeRequestSCMRevision<PullRequestHead> {
        private static final long serialVersionUID = 1L;

        OpaqueChangeRequestRevision(PullRequestHead head, SCMRevision target) {
            super(head, target);
        }

        @Override
        public boolean equivalent(ChangeRequestSCMRevision<?> other) {
            return other == this;
        }

        @Override
        protected int _hashCode() {
            return 0;
        }

    }

    /** A change-request revision that exposes only a plain {@code getHash}, as some forges do. */
    static final class HashOnlyChangeRequestRevision extends ChangeRequestSCMRevision<PullRequestHead> {
        private static final long serialVersionUID = 1L;
        private final String hash;

        HashOnlyChangeRequestRevision(PullRequestHead head, SCMRevision target, String hash) {
            super(head, target);
            this.hash = hash;
        }

        public String getHash() {
            return hash;
        }

        @Override
        public boolean equivalent(ChangeRequestSCMRevision<?> other) {
            return other == this;
        }

        @Override
        protected int _hashCode() {
            return hash.hashCode();
        }
    }

    @Test
    public void aChangeRequestRevisionWithOnlyAHashAccessorAnswersThatHash() {
        PullRequestHead head = pullRequest();
        SCMRevision target = new BranchStyleRevision(head.getTarget(), "base111");

        assertEquals("hash777", VarroaRunRecorder.revisionHash(new HashOnlyChangeRequestRevision(head, target, "hash777")));
    }

    private static PullRequestHead pullRequest() {
        return new PullRequestHead("PR-12", "12", new SCMHead("main"), "fix/cache");
    }

    @Test
    public void aGitHubPullRequestRevisionAnswersThePullHashNotTheMergeHash() {
        PullRequestHead head = pullRequest();
        SCMRevision target = new BranchStyleRevision(head.getTarget(), "base111");
        GitHubStyleRevision revision = new GitHubStyleRevision(head, target, "head222", "merge333");

        assertEquals("head222", VarroaRunRecorder.revisionHash(revision));
    }

    @Test
    public void aGiteaPullRequestRevisionAnswersTheOriginHash() {
        PullRequestHead head = pullRequest();
        SCMRevision target = new BranchStyleRevision(head.getTarget(), "base111");
        GiteaStyleRevision revision = new GiteaStyleRevision(head, target, new OriginHash("head444"));

        assertEquals("head444", VarroaRunRecorder.revisionHash(revision));
    }

    @Test
    public void aBranchRevisionAnswersItsOwnHash() {
        assertEquals("branch555", VarroaRunRecorder.revisionHash(new BranchStyleRevision(new SCMHead("main"), "branch555")));
    }

    @Test
    public void aChangeRequestRevisionWithNoKnownAccessorAnswersEmpty() {
        PullRequestHead head = pullRequest();
        SCMRevision target = new BranchStyleRevision(head.getTarget(), "base111");

        assertEquals("", VarroaRunRecorder.revisionHash(new OpaqueChangeRequestRevision(head, target)));
    }

    @Test
    public void aNullRevisionAnswersEmpty() {
        assertEquals("", VarroaRunRecorder.revisionHash(null));
    }
}
