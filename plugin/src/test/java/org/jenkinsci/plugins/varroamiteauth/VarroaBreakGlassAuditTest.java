package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertTrue;

import java.util.List;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import org.junit.After;
import org.junit.Before;
import org.junit.Test;

/**
 * Plain-JUnit tests for {@link VarroaBreakGlassAudit} (D8, task 5): the two
 * activity types, the sensitive-path set, the repeat-suppression windows,
 * the bounded maps, and the full-sink drop.
 */
public class VarroaBreakGlassAuditTest {

    @Before
    public void setUp() {
        VarroaBreakGlassAudit.resetForTesting();
        ActivitySink.get().resetForTesting();
    }

    @After
    public void tearDown() {
        VarroaBreakGlassAudit.resetForTesting();
        ActivitySink.get().resetForTesting();
    }

    private static List<ActivityEvent> drainAll() {
        return ActivitySink.get().drain(1024).events;
    }

    /** Task 5.1: one session event for an admin request. */
    @Test
    public void oneSessionEventForAnAdminRequest() {
        VarroaBreakGlassAudit.record("alice", "/job/team-a/", 1_000L);

        List<ActivityEvent> events = drainAll();
        assertEquals(1, events.size());
        assertEquals("controller.breakglass.session", events.get(0).getType());
        assertEquals("alice", events.get(0).getActor());
        assertEquals("/job/team-a/", events.get(0).getUrl());
    }

    /** Task 5.2: one script event per sensitive path. */
    @Test
    public void scriptEventFiresForEachSensitivePath() {
        String[] sensitivePaths = {
            "/script", "/script/", "/scriptText", "/manage", "/manage/",
            "/configureSecurity", "/configureSecurity/", "/pluginManager",
            "/pluginManager/", "/computer/agent-1/script",
        };
        for (String path : sensitivePaths) {
            VarroaBreakGlassAudit.resetForTesting();
            ActivitySink.get().resetForTesting();

            VarroaBreakGlassAudit.record("alice", path, 1_000L);

            List<ActivityEvent> events = drainAll();
            boolean sawScript = false;
            for (ActivityEvent e : events) {
                if ("controller.breakglass.script".equals(e.getType())) {
                    sawScript = true;
                }
            }
            assertTrue("expected a script event for " + path, sawScript);
        }
    }

    /** Task 5.2: a non-sensitive admin path emits no script event. */
    @Test
    public void nonSensitivePathEmitsNoScriptEvent() {
        VarroaBreakGlassAudit.record("alice", "/job/team-a/", 1_000L);

        for (ActivityEvent e : drainAll()) {
            assertTrue("only a session event is expected",
                    "controller.breakglass.session".equals(e.getType()));
        }
    }

    /** Task 5.3: one session event for twenty requests inside 30 minutes. */
    @Test
    public void sessionDedupeSuppressesRepeatsInsideTheWindow() {
        long start = 10_000L;
        for (int i = 0; i < 20; i++) {
            VarroaBreakGlassAudit.record("alice", "/job/team-a/", start + i * 1000L);
        }

        int sessionCount = 0;
        for (ActivityEvent e : drainAll()) {
            if ("controller.breakglass.session".equals(e.getType())) {
                sessionCount++;
            }
        }
        assertEquals(1, sessionCount);
    }

    /** Task 5.3: a second session event fires after the 30 minute window. */
    @Test
    public void sessionDedupeFiresAgainAfterTheWindow() {
        VarroaBreakGlassAudit.record("alice", "/job/team-a/", 0L);
        VarroaBreakGlassAudit.record("alice", "/job/team-a/",
                VarroaBreakGlassAudit.SESSION_WINDOW_MILLIS + 1);

        int sessionCount = 0;
        for (ActivityEvent e : drainAll()) {
            if ("controller.breakglass.session".equals(e.getType())) {
                sessionCount++;
            }
        }
        assertEquals(2, sessionCount);
    }

    /** Task 5.3: the script dedupe window is independent and 60 seconds. */
    @Test
    public void scriptDedupeFiresAgainAfterItsShorterWindow() {
        VarroaBreakGlassAudit.record("alice", "/manage", 0L);
        VarroaBreakGlassAudit.record("alice", "/manage",
                VarroaBreakGlassAudit.SCRIPT_WINDOW_MILLIS + 1);

        int scriptCount = 0;
        for (ActivityEvent e : drainAll()) {
            if ("controller.breakglass.script".equals(e.getType())) {
                scriptCount++;
            }
        }
        assertEquals(2, scriptCount);
    }

    /** Task 5.3: the session map clears rather than growing past its cap. */
    @Test
    public void sessionMapClearsAtItsCap() {
        for (int i = 0; i < VarroaBreakGlassAudit.SESSION_MAP_CAP; i++) {
            VarroaBreakGlassAudit.record("user-" + i, "/job/team-a/", 1_000L);
        }
        assertEquals(VarroaBreakGlassAudit.SESSION_MAP_CAP,
                VarroaBreakGlassAudit.sessionMapSizeForTesting());

        // One more distinct principal pushes the map to its cap: it clears
        // rather than growing to CAP + 1.
        VarroaBreakGlassAudit.record("one-more", "/job/team-a/", 1_000L);
        assertEquals(1, VarroaBreakGlassAudit.sessionMapSizeForTesting());
    }

    /** Task 5.3: the script map clears rather than growing past its cap. */
    @Test
    public void scriptMapClearsAtItsCap() {
        for (int i = 0; i < VarroaBreakGlassAudit.SCRIPT_MAP_CAP; i++) {
            VarroaBreakGlassAudit.record("user-" + i, "/manage", 1_000L);
        }
        assertEquals(VarroaBreakGlassAudit.SCRIPT_MAP_CAP,
                VarroaBreakGlassAudit.scriptMapSizeForTesting());

        VarroaBreakGlassAudit.record("one-more", "/manage", 1_000L);
        assertEquals(1, VarroaBreakGlassAudit.scriptMapSizeForTesting());
    }

    /** Task 5.4: a full sink drops the event and never blocks the caller. */
    @Test
    public void fullSinkDropsTheEventWithoutBlocking() {
        ActivitySink sink = ActivitySink.get();
        while (sink.remainingCapacity() > 0) {
            sink.offer(ActivityEvent.builder().type("filler").build());
        }
        long droppedBefore = sink.droppedCount();

        long startNanos = System.nanoTime();
        VarroaBreakGlassAudit.record("alice", "/manage", 1_000L);
        long elapsedMillis = (System.nanoTime() - startNanos) / 1_000_000;

        assertTrue("record() must return promptly on a full sink", elapsedMillis < 2000);
        assertTrue("a full sink must drop the offered event", sink.droppedCount() > droppedBefore);
    }

    /**
     * The session dedupe decision is atomic per principal: N concurrent
     * requests for one principal at the same instant must emit exactly one
     * session event.
     */
    @Test
    public void concurrentRequestsForOnePrincipalEmitOneSessionEvent() throws InterruptedException {
        int threadCount = 32;
        runConcurrently(threadCount, () -> VarroaBreakGlassAudit.record("alice", "/job/team-a/", 5_000L));

        int sessionCount = 0;
        for (ActivityEvent e : drainAll()) {
            if ("controller.breakglass.session".equals(e.getType())) {
                sessionCount++;
            }
        }
        assertEquals("exactly one session event for concurrent requests at the same instant",
                1, sessionCount);
    }

    /**
     * The script dedupe decision is atomic per principal and path: N
     * concurrent requests for one principal and one sensitive path at the
     * same instant must emit exactly one script event.
     */
    @Test
    public void concurrentRequestsForOnePrincipalAndPathEmitOneScriptEvent() throws InterruptedException {
        int threadCount = 32;
        runConcurrently(threadCount, () -> VarroaBreakGlassAudit.record("alice", "/manage", 5_000L));

        int scriptCount = 0;
        for (ActivityEvent e : drainAll()) {
            if ("controller.breakglass.script".equals(e.getType())) {
                scriptCount++;
            }
        }
        assertEquals("exactly one script event for concurrent requests at the same instant",
                1, scriptCount);
    }

    /**
     * Admission at the cap is atomic too: with the session map full, N
     * concurrent requests for one new principal clear the map once and
     * emit exactly one session event.
     */
    @Test
    public void concurrentAdmissionAtCapacityEmitsOneSessionEvent() throws InterruptedException {
        for (int i = 0; i < VarroaBreakGlassAudit.SESSION_MAP_CAP; i++) {
            VarroaBreakGlassAudit.record("filler-" + i, "/job/team-a/", 1_000L);
        }
        drainAll();
        assertEquals(VarroaBreakGlassAudit.SESSION_MAP_CAP, VarroaBreakGlassAudit.sessionMapSizeForTesting());

        runConcurrently(32, () -> VarroaBreakGlassAudit.record("newcomer", "/job/team-a/", 5_000L));

        int sessionCount = 0;
        for (ActivityEvent e : drainAll()) {
            if ("controller.breakglass.session".equals(e.getType())) {
                sessionCount++;
            }
        }
        assertEquals("exactly one session event for concurrent admission at the cap", 1, sessionCount);
        assertEquals("the cleared map holds only the newcomer", 1, VarroaBreakGlassAudit.sessionMapSizeForTesting());
    }

    /** Runs {@code task} on {@code threadCount} threads released by one latch. */
    private static void runConcurrently(int threadCount, Runnable task) throws InterruptedException {
        ExecutorService pool = Executors.newFixedThreadPool(threadCount);
        CountDownLatch ready = new CountDownLatch(threadCount);
        CountDownLatch start = new CountDownLatch(1);
        CountDownLatch done = new CountDownLatch(threadCount);
        AtomicInteger failures = new AtomicInteger(0);
        try {
            for (int i = 0; i < threadCount; i++) {
                pool.submit(() -> {
                    ready.countDown();
                    try {
                        start.await();
                        task.run();
                    } catch (Exception e) {
                        failures.incrementAndGet();
                    } finally {
                        done.countDown();
                    }
                });
            }
            ready.await(5, TimeUnit.SECONDS);
            start.countDown();
            assertTrue("all threads must finish", done.await(10, TimeUnit.SECONDS));
        } finally {
            pool.shutdownNow();
        }
        assertEquals("no task must throw", 0, failures.get());
    }
}
