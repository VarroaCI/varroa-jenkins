package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

import hudson.model.Cause;
import hudson.model.Job;
import hudson.model.Queue;

import java.util.List;
import org.junit.Test;

/**
 * Tests for {@link IntentKey}'s revision read: each call reads the factory
 * once and never waits. It answers "not yet" while the factory revision
 * equals the admission revision before the deadline, resolves at once
 * when it differs, falls back at the deadline, and is skipped for a
 * non-branch cause (D5).
 */
public class IntentKeyDispatchTest {

    /** Test double matched by simple name: a branch event cause. */
    public static class BranchEventCause extends Cause {
        @Override
        public String getShortDescription() {
            return "Branch event";
        }
    }

    /** Test double matched by simple name: a branch indexing cause. */
    public static class BranchIndexingCause extends Cause {
        @Override
        public String getShortDescription() {
            return "Branch indexing";
        }
    }

    /** Test double matched by simple name: the multi-branch parent. */
    static class MultiBranchProject {
        private final Object factory;

        MultiBranchProject(Object factory) {
            this.factory = factory;
        }

        public Object getProjectFactory() {
            return factory;
        }
    }

    /** A factory whose revision can change between polls. It counts each revision read. */
    static class MutableFactory {
        volatile String revision;
        final java.util.concurrent.atomic.AtomicInteger reads = new java.util.concurrent.atomic.AtomicInteger();

        MutableFactory(String revision) {
            this.revision = revision;
        }

        public String getRevision(Object project) {
            reads.incrementAndGet();
            return revision;
        }

        public String getLastSeenRevision(Object project) {
            return revision;
        }
    }

    /** A factory that must never be called — proves the wait was skipped. */
    static class PoisonFactory {
        public String getRevision(Object project) {
            throw new AssertionError("factory must not be consulted with no branch cause");
        }

        public String getLastSeenRevision(Object project) {
            throw new AssertionError("factory must not be consulted with no branch cause");
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

    private static Queue.Item itemWithCauses(List<Cause> causes) {
        Queue.Item item = mock(Queue.Item.class);
        when(item.getActions()).thenReturn(List.of());
        when(item.getCauses()).thenReturn(causes);
        return item;
    }

    private static final long NOW = 1_000_000L;
    private static final long DEADLINE = NOW + IntentKey.WAIT_DEADLINE_MILLIS;

    @Test
    public void aRevisionEqualToAdmissionAnswersNotYet() {
        MutableFactory factory = new MutableFactory("rev-old");
        MultiBranchProject parent = new MultiBranchProject(factory);
        Job<?, ?> job = job("folder/job", null);
        Queue.Item item = itemWithCauses(List.of(new BranchEventCause()));
        VarroaIntentAction action = new VarroaIntentAction("run-1", "", "rev-old", 0L);

        assertNull("an unaccepted revision before the deadline answers not yet",
                IntentKey.derive(item, job, action, NOW, DEADLINE, () -> parent));
        assertEquals("one call reads the factory once", 1, factory.reads.get());

        factory.revision = "rev-new";
        IntentKey.Result result = IntentKey.derive(item, job, action, NOW + 2000L, DEADLINE, () -> parent);
        assertEquals(IntentKey.ORIGIN_SCM, result.origin);
        assertEquals("scm:folder/job:rev-new", result.intentKey);
        assertEquals(2, factory.reads.get());
    }

    @Test
    public void aChangedRevisionResolvesAtOnce() {
        MutableFactory factory = new MutableFactory("rev-new");
        MultiBranchProject parent = new MultiBranchProject(factory);
        Job<?, ?> job = job("folder/job", null);
        Queue.Item item = itemWithCauses(List.of(new BranchIndexingCause()));
        VarroaIntentAction action = new VarroaIntentAction("run-1", "", "rev-old", 0L);

        IntentKey.Result result = IntentKey.derive(item, job, action, NOW, DEADLINE, () -> parent);
        assertEquals("scm:folder/job:rev-new", result.intentKey);
        assertEquals(1, factory.reads.get());
    }

    @Test
    public void anUnchangedRevisionAtTheDeadlineFallsBackToApiUuid() {
        MutableFactory factory = new MutableFactory("rev-stuck");
        MultiBranchProject parent = new MultiBranchProject(factory);
        Job<?, ?> job = job("folder/job", null);
        Queue.Item item = itemWithCauses(List.of(new BranchEventCause()));
        VarroaIntentAction action = new VarroaIntentAction("run-1", "", "rev-stuck", 0L);

        assertNull(IntentKey.derive(item, job, action, DEADLINE - 1, DEADLINE, () -> parent));
        IntentKey.Result result = IntentKey.derive(item, job, action, DEADLINE, DEADLINE, () -> parent);
        assertEquals(IntentKey.ORIGIN_CELL_LOCAL, result.origin);
        assertTrue(result.intentKey.startsWith("api:"));
        assertEquals(2, factory.reads.get());
    }

    @Test
    public void aNonBranchCauseSkipsTheWaitEntirely() {
        MultiBranchProject parent = new MultiBranchProject(new PoisonFactory());
        Job<?, ?> job = job("folder/job", null);
        Queue.Item item = itemWithCauses(List.of(new Cause.UserIdCause()));

        IntentKey.Result result = IntentKey.derive(item, job, null, NOW, DEADLINE, () -> parent);
        // Falls straight to the api:<uuid> fallback; the poison factory,
        // if touched, would have thrown.
        assertEquals(IntentKey.ORIGIN_CELL_LOCAL, result.origin);
    }

    @Test
    public void aNonMultibranchParentFallsBackAtOnce() {
        Job<?, ?> job = job("folder/job", null);
        Queue.Item item = itemWithCauses(List.of(new BranchEventCause()));
        VarroaIntentAction action = new VarroaIntentAction("run-1", "", "rev-old", 0L);

        IntentKey.Result result = IntentKey.derive(item, job, action, NOW, DEADLINE, Object::new);
        assertNotNull("a parent with no factory has no revision to wait for", result);
        assertEquals(IntentKey.ORIGIN_CELL_LOCAL, result.origin);
        assertTrue(result.intentKey.startsWith("api:"));
    }
}
