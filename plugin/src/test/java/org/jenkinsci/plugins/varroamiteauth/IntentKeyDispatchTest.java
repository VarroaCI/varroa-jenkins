package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

import hudson.model.Cause;
import hudson.model.Job;
import hudson.model.Queue;

import java.util.List;
import org.junit.Test;

/**
 * Tests for {@link IntentKey}'s revision wait: blocks until the factory
 * revision differs from the admission revision, resolves at once when it
 * already differs, falls back after 30 s, and is skipped for a non-branch
 * cause (D5).
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

    /** A factory whose revision can change between polls. */
    static class MutableFactory {
        volatile String revision;

        MutableFactory(String revision) {
            this.revision = revision;
        }

        public String getRevision(Object project) {
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

    /** Shared fake clock and sleeper: a sleep advances the clock by the same amount. */
    static final class FakeTime implements IntentKey.Clock, IntentKey.Sleeper {
        long now;
        int sleepCount;

        FakeTime(long start) {
            this.now = start;
        }

        @Override
        public long nowMillis() {
            return now;
        }

        @Override
        public void sleep(long millis) {
            now += millis;
            sleepCount++;
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

    @Test
    public void aRevisionEqualToAdmissionBlocksUntilItChanges() {
        MutableFactory factory = new MutableFactory("rev-old");
        MultiBranchProject parent = new MultiBranchProject(factory);
        Job<?, ?> job = job("folder/job", null);
        Queue.Item item = itemWithCauses(List.of(new BranchEventCause()));
        VarroaIntentAction action = new VarroaIntentAction("run-1", "", "rev-old", 0L);
        FakeTime time = new FakeTime(1_000_000L);

        // The factory reports the same (admission) revision until the
        // fake sleeper has been called twice, then a new one.
        IntentKey.Sleeper sleeper = millis -> {
            time.sleep(millis);
            if (time.sleepCount == 2) {
                factory.revision = "rev-new";
            }
        };

        IntentKey.Result result = IntentKey.derive(item, job, action, time, sleeper, () -> parent);
        assertEquals(IntentKey.ORIGIN_SCM, result.origin);
        assertEquals("scm:folder/job:rev-new", result.intentKey);
        assertTrue("expected at least 2 sleeps before the revision changed", time.sleepCount >= 2);
    }

    @Test
    public void aChangedRevisionResolvesAtOnce() {
        MutableFactory factory = new MutableFactory("rev-new");
        MultiBranchProject parent = new MultiBranchProject(factory);
        Job<?, ?> job = job("folder/job", null);
        Queue.Item item = itemWithCauses(List.of(new BranchIndexingCause()));
        VarroaIntentAction action = new VarroaIntentAction("run-1", "", "rev-old", 0L);
        FakeTime time = new FakeTime(1_000_000L);

        IntentKey.Result result = IntentKey.derive(item, job, action, time, time, () -> parent);
        assertEquals("scm:folder/job:rev-new", result.intentKey);
        assertEquals("no wait needed when the first read already differs", 0, time.sleepCount);
    }

    @Test
    public void a30SecondDeadlineFallsBackToApiUuid() {
        MutableFactory factory = new MutableFactory("rev-stuck");
        MultiBranchProject parent = new MultiBranchProject(factory);
        Job<?, ?> job = job("folder/job", null);
        Queue.Item item = itemWithCauses(List.of(new BranchEventCause()));
        VarroaIntentAction action = new VarroaIntentAction("run-1", "", "rev-stuck", 0L);
        FakeTime time = new FakeTime(1_000_000L);

        IntentKey.Result result = IntentKey.derive(item, job, action, time, time, () -> parent);
        assertEquals(IntentKey.ORIGIN_CELL_LOCAL, result.origin);
        assertTrue(result.intentKey.startsWith("api:"));
        assertEquals(15, time.sleepCount);
    }

    @Test
    public void aNonBranchCauseSkipsTheWaitEntirely() {
        MultiBranchProject parent = new MultiBranchProject(new PoisonFactory());
        Job<?, ?> job = job("folder/job", null);
        Queue.Item item = itemWithCauses(List.of(new Cause.UserIdCause()));
        FakeTime time = new FakeTime(1_000_000L);

        IntentKey.Result result = IntentKey.derive(item, job, null, time, time, () -> parent);
        // Falls straight to the api:<uuid> fallback; the poison factory,
        // if touched, would have thrown.
        assertEquals(IntentKey.ORIGIN_CELL_LOCAL, result.origin);
        assertEquals(0, time.sleepCount);
    }
}
