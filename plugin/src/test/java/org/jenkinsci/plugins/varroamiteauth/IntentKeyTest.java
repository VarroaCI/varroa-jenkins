package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

import hudson.model.Action;
import hudson.model.Cause;
import hudson.model.InvisibleAction;
import hudson.model.Job;
import hudson.model.Queue;
import jenkins.scm.api.SCMHead;
import jenkins.scm.api.SCMRevision;

import java.util.List;
import org.junit.Test;

/**
 * Tests for {@link IntentKey} — the origin/intent-key table, the two
 * revision sources, revision normalization, and cause description.
 */
public class IntentKeyTest {

    /** Test double: matched by simple name, per D5, with no scm-api dependency. */
    static class SCMRevisionAction extends InvisibleAction {
        private final Object revision;

        SCMRevisionAction(Object revision) {
            this.revision = revision;
        }

        public Object getRevision() {
            return revision;
        }
    }

    /** Test double: a branch project factory whose getRevision answers any object. */
    static class ObjectFactory {
        private final Object revision;

        ObjectFactory(Object revision) {
            this.revision = revision;
        }

        public Object getRevision(Object project) {
            return revision;
        }
    }

    /** Test double: matched by simple name, per D5, with no branch-api dependency. */
    static class MultiBranchProject {
        private final Object factory;

        MultiBranchProject(Object factory) {
            this.factory = factory;
        }

        public Object getProjectFactory() {
            return factory;
        }
    }

    /** A branch project factory double exposing getRevision/getLastSeenRevision. */
    static class FakeFactory {
        private final String revision;
        private final String lastSeenRevision;

        FakeFactory(String revision, String lastSeenRevision) {
            this.revision = revision;
            this.lastSeenRevision = lastSeenRevision;
        }

        public String getRevision(Object project) {
            return revision;
        }

        public String getLastSeenRevision(Object project) {
            return lastSeenRevision;
        }
    }

    /**
     * Mocks a {@link Job} via a name-keyed default {@link
     * org.mockito.stubbing.Answer} rather than {@code when()}/{@code
     * doReturn()}: {@code AbstractItem} carries a synthetic covariant
     * {@code getParent()} bridge (returning {@code Jenkins}) alongside its
     * declared one (returning {@code ItemGroup}), and Mockito's
     * type-checked stubbing picks the wrong one for a raw-typed mock.
     */
    private static Job<?, ?> job(String fullName, Object parent) {
        return mock(Job.class, invocation -> {
            switch (invocation.getMethod().getName()) {
                case "getFullName":
                case "getName":
                    return fullName;
                case "getParent":
                    return parent;
                default:
                    return org.mockito.Mockito.RETURNS_DEFAULTS.answer(invocation);
            }
        });
    }

    private static Queue.Item item(List<Action> actions, List<Cause> causes) {
        Queue.Item item = mock(Queue.Item.class);
        when(item.getActions()).thenReturn(actions);
        when(item.getCauses()).thenReturn(causes);
        return item;
    }

    @Test
    public void existingKeyOnTheActionPassesThrough() {
        Job<?, ?> job = job("folder/job", null);
        Queue.Item item = item(List.of(), List.of());
        VarroaIntentAction action = new VarroaIntentAction("run-1", "", null, 0L);
        action.setOrigin(IntentKey.ORIGIN_VARROA_INTENT);
        action.setIntentKey("manual:11111111-1111-7111-8111-111111111111");
        action.setForgeRevision("head9");

        IntentKey.Result result = IntentKey.derive(item, job, action, 0L, 0L);
        assertEquals(IntentKey.ORIGIN_VARROA_INTENT, result.origin);
        assertEquals("manual:11111111-1111-7111-8111-111111111111", result.intentKey);
        assertEquals("head9", result.forgeRevision);
    }

    @Test
    public void aRevisionBearingActionProducesAScmKey() {
        Job<?, ?> job = job("folder/job", null);
        Queue.Item item = item(List.of(new SCMRevisionAction("abc123")), List.of());

        IntentKey.Result result = IntentKey.derive(item, job, null, 0L, 0L);
        assertEquals(IntentKey.ORIGIN_SCM, result.origin);
        assertEquals("scm:folder/job:abc123", result.intentKey);
        assertEquals("abc123", result.revision);
        assertEquals("", result.forgeRevision);
    }

    private static VarroaRunRecorderChangeRequestTest.PullRequestHead pullRequest() {
        return new VarroaRunRecorderChangeRequestTest.PullRequestHead("PR-12", "12", new SCMHead("main"),
                "fix/cache");
    }

    @Test
    public void aChangeRequestRevisionActionKeysOnTheClaimRevisionAndAnswersTheHeadCommit() {
        VarroaRunRecorderChangeRequestTest.PullRequestHead head = pullRequest();
        SCMRevision target = new VarroaRunRecorderChangeRequestTest.BranchStyleRevision(head.getTarget(), "base111");
        Object revision = new VarroaRunRecorderChangeRequestTest.GitHubStyleRevision(head, target, "head222",
                "merge333");
        Job<?, ?> job = job("folder/job", null);
        Queue.Item item = item(List.of(new SCMRevisionAction(revision)), List.of());

        IntentKey.Result result = IntentKey.derive(item, job, null, 0L, 0L);
        String claimRevision = IntentKey.normalizeRevision(String.valueOf(revision));
        assertEquals("scm:folder/job:" + claimRevision, result.intentKey);
        assertEquals(claimRevision, result.revision);
        assertEquals("head222", result.forgeRevision);
    }

    @Test
    public void aGiteaChangeRequestRevisionAnswersTheOriginHash() {
        VarroaRunRecorderChangeRequestTest.PullRequestHead head = pullRequest();
        SCMRevision target = new VarroaRunRecorderChangeRequestTest.BranchStyleRevision(head.getTarget(), "base111");
        Object revision = new VarroaRunRecorderChangeRequestTest.GiteaStyleRevision(head, target,
                new VarroaRunRecorderChangeRequestTest.OriginHash("head444"));
        Job<?, ?> job = job("folder/job", null);
        Queue.Item item = item(List.of(new SCMRevisionAction(revision)), List.of());

        assertEquals("head444", IntentKey.derive(item, job, null, 0L, 0L).forgeRevision);
    }

    @Test
    public void aBranchRevisionAnswersItsHashAsTheForgeRevision() {
        Object revision = new VarroaRunRecorderChangeRequestTest.BranchStyleRevision(
                new SCMHead("feature"), "branch555");
        Job<?, ?> job = job("folder/job", null);
        Queue.Item item = item(List.of(new SCMRevisionAction(revision)), List.of());

        assertEquals("branch555", IntentKey.derive(item, job, null, 0L, 0L).forgeRevision);
    }

    @Test
    public void theBranchFactoryChangeRequestRevisionAnswersTheHeadCommit() {
        VarroaRunRecorderChangeRequestTest.PullRequestHead head = pullRequest();
        SCMRevision target = new VarroaRunRecorderChangeRequestTest.BranchStyleRevision(head.getTarget(), "base111");
        Object revision = new VarroaRunRecorderChangeRequestTest.GitHubStyleRevision(head, target, "head222",
                "merge333");
        MultiBranchProject parent = new MultiBranchProject(new ObjectFactory(revision));
        Job<?, ?> job = job("folder/job", null);
        Queue.Item item = item(List.of(), List.of(new IntentKeyDispatchTest.BranchEventCause()));

        IntentKey.Result result = IntentKey.derive(item, job, null, 0L, 0L, () -> parent);
        assertEquals("head222", result.forgeRevision);
        assertEquals("scm:folder/job:" + IntentKey.normalizeRevision(String.valueOf(revision)),
                result.intentKey);
    }

    @Test
    public void theBranchFactoryGetRevisionProducesAScmKey() {
        FakeFactory factory = new FakeFactory("rev-from-getRevision", null);
        MultiBranchProject parent = new MultiBranchProject(factory);
        Job<?, ?> job = job("folder/job", null);
        Cause branchCause = new IntentKeyDispatchTest.BranchEventCause();
        Queue.Item item = item(List.of(), List.of(branchCause));

        // The parent travels through the test seam, not the mocked
        // Job.getParent(): see the job() helper's javadoc.
        IntentKey.Result result = IntentKey.derive(item, job, null, 0L, 0L, () -> parent);
        assertEquals(IntentKey.ORIGIN_SCM, result.origin);
        assertEquals("scm:folder/job:rev-from-getRevision", result.intentKey);
    }

    @Test
    public void theBranchFactoryGetLastSeenRevisionProducesAScmKeyWhenGetRevisionIsAbsent() {
        FakeFactory factory = new FakeFactory(null, "rev-from-lastSeen");
        MultiBranchProject parent = new MultiBranchProject(factory);
        Job<?, ?> job = job("folder/job", null);
        Cause branchCause = new IntentKeyDispatchTest.BranchIndexingCause();
        Queue.Item item = item(List.of(), List.of(branchCause));

        IntentKey.Result result = IntentKey.derive(item, job, null, 0L, 0L, () -> parent);
        assertEquals(IntentKey.ORIGIN_SCM, result.origin);
        assertEquals("scm:folder/job:rev-from-lastSeen", result.intentKey);
    }

    @Test
    public void aTimerCauseProducesACronKey() {
        Job<?, ?> job = job("folder/job", null);
        Cause timerCause = new hudson.triggers.TimerTrigger.TimerTriggerCause();
        Queue.Item item = item(List.of(), List.of(timerCause));
        long millis = java.time.Instant.parse("2026-09-10T00:12:34Z").toEpochMilli();
        VarroaIntentAction action = new VarroaIntentAction("run-1", "", null, millis);

        IntentKey.Result result = IntentKey.derive(item, job, action, 0L, 0L);
        assertEquals(IntentKey.ORIGIN_CRON, result.origin);
        assertEquals("cron:folder/job:2026-09-10T00:12:00Z", result.intentKey);
    }

    @Test
    public void noRevisionAndNoCauseFallsBackToApiUuid() {
        Job<?, ?> job = job("folder/job", null);
        Queue.Item item = item(List.of(), List.of());

        IntentKey.Result result = IntentKey.derive(item, job, null, 0L, 0L);
        assertEquals(IntentKey.ORIGIN_CELL_LOCAL, result.origin);
        assertTrue(result.intentKey.startsWith("api:"));
        assertEquals(36 + 4, result.intentKey.length());
    }

    // ---- Revision normalization ----

    @Test
    public void aNormalRevisionPassesThroughUnchanged() {
        assertEquals("abc123", IntentKey.normalizeRevision("abc123"));
    }

    @Test
    public void anEmptyRevisionIsHashed() {
        String normalized = IntentKey.normalizeRevision("");
        assertTrue(normalized.startsWith("sha256:"));
    }

    @Test
    public void aRevisionWithWhitespaceIsHashed() {
        String normalized = IntentKey.normalizeRevision("abc 123");
        assertTrue(normalized.startsWith("sha256:"));
    }

    @Test
    public void aRevisionOver128CharactersIsHashed() {
        String normalized = IntentKey.normalizeRevision("a".repeat(129));
        assertTrue(normalized.startsWith("sha256:"));
    }

    @Test
    public void a128CharacterRevisionPassesThrough() {
        String value = "a".repeat(128);
        assertEquals(value, IntentKey.normalizeRevision(value));
    }

    // ---- Cause description ----

    @Test
    public void causeDescriptionTruncatesTo512Characters() {
        Cause longCause = new Cause() {
            @Override
            public String getShortDescription() {
                return "x".repeat(600);
            }
        };
        String description = IntentKey.causeDescription(List.of(longCause));
        assertEquals(512, description.length());
    }

    @Test
    public void causeDescriptionOfNoCausesIsEmpty() {
        assertEquals("", IntentKey.causeDescription(List.of()));
    }
}
