package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayDeque;
import java.util.Deque;
import java.util.HashSet;
import java.util.List;
import java.util.Set;
import java.util.concurrent.BrokenBarrierException;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.CyclicBarrier;
import java.util.function.BooleanSupplier;
import java.util.logging.Handler;
import java.util.logging.Level;
import java.util.logging.LogRecord;
import java.util.logging.Logger;

import org.jenkinsci.plugins.workflow.flow.FlowDurabilityHint;
import org.jenkinsci.plugins.workflow.flow.FlowExecution;
import org.jenkinsci.plugins.workflow.graph.FlowNode;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;

import net.sf.json.JSONObject;

/**
 * Tests for {@link RunStatePusher} (task 9.1): the 2-second coalescing
 * window, the change test on a workflow file, the durability
 * precondition, the refusal table, and the failure isolation.
 *
 * <p>Plain JUnit with a fake {@link RunStatePusher.RunStateSender} and a
 * {@link RunSpool} over a temporary directory. No {@code JenkinsRule}:
 * the class under test needs no live Jenkins instance for these cases.
 */
public class RunStatePusherTest {

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    private static final String RUN_ID = "run-1";
    private static final String JOB_PATH = "alpha/job1";
    private static final long BUILD_NUMBER = 7L;

    private RunStatePusher newPusher(Path rootDir, RunStatePusher.RunStateSender sender, RunSpool spool) {
        return new RunStatePusher(RUN_ID, JOB_PATH, BUILD_NUMBER, rootDir,
                "http://127.0.0.1:0", sender, spool);
    }

    private RunSpool newSpool(String name) throws Exception {
        return new RunSpool(tmp.newFolder(name).toPath(), RunSpool.DEFAULT_CAP_BYTES);
    }

    // ---- 9.1 ----

    @Test
    public void manySchedulesInsideOneWindowMakeOnePush() throws Exception {
        Path rootDir = tmp.newFolder("run-a").toPath();
        Files.writeString(rootDir.resolve(RunStatePusher.BUILD_XML), "<build/>");
        Files.writeString(rootDir.resolve(RunStatePusher.PROGRAM_DAT), "program-bytes");
        Path workflow = Files.createDirectories(rootDir.resolve("workflow"));
        Files.writeString(workflow.resolve("3.xml"), "<flow/>");

        FakeSender sender = new FakeSender();
        RunStatePusher pusher = newPusher(rootDir, sender, newSpool("spool-a"));

        for (int i = 0; i < 6; i++) {
            pusher.schedule(null);
            Thread.sleep(50);
        }

        waitUntil(() -> sender.postCount() >= 3, 5000);
        // A window that wrongly re-armed itself would fire a second push
        // here; give it time to appear before asserting it never does.
        Thread.sleep(RunStatePusher.WINDOW_MILLIS + 500);

        assertEquals("many schedules inside one window must push exactly one file set",
                3, sender.postCount());
        assertEquals("every posted record must belong to the same push generation",
                1, sender.distinctStateGens().size());
    }

    @Test
    public void anUnchangedWorkflowFileIsNotReposted() throws Exception {
        Path rootDir = tmp.newFolder("run-b").toPath();
        Path workflow = Files.createDirectories(rootDir.resolve("workflow"));
        Files.writeString(workflow.resolve("1.xml"), "<flow/>");

        FakeSender sender = new FakeSender();
        RunStatePusher pusher = newPusher(rootDir, sender, newSpool("spool-b"));

        pusher.runWindow();
        assertEquals(1, sender.postCount());
        assertTrue(sender.posts().get(0).getString("relpath").endsWith("workflow/1.xml"));

        sender.clear();
        pusher.runWindow();
        assertEquals("an unchanged workflow file must not be reposted", 0, sender.postCount());
    }

    @Test
    public void aBuildBelowMaxSurvivabilityPushesNothingAndLogsOnce() throws Exception {
        Path rootDir = tmp.newFolder("run-c").toPath();
        Files.writeString(rootDir.resolve(RunStatePusher.BUILD_XML), "<build/>");

        FakeSender sender = new FakeSender();
        RunStatePusher pusher = newPusher(rootDir, sender, newSpool("spool-c"));

        FlowExecution execution = mock(FlowExecution.class);
        when(execution.getDurabilityHint()).thenReturn(FlowDurabilityHint.PERFORMANCE_OPTIMIZED);
        FlowNode node = mock(FlowNode.class);
        when(node.getExecution()).thenReturn(execution);

        Logger logger = Logger.getLogger(RunStatePusher.class.getName());
        Level originalLevel = logger.getLevel();
        TestLogHandler handler = new TestLogHandler();
        handler.setLevel(Level.ALL);
        logger.setLevel(Level.ALL);
        logger.addHandler(handler);
        try {
            pusher.schedule(node);
            pusher.schedule(node);
            // Past the window: a build below MAX_SURVIVABILITY never
            // arms one, so no delayed push should appear either.
            Thread.sleep(RunStatePusher.WINDOW_MILLIS + 500);
        } finally {
            logger.removeHandler(handler);
            logger.setLevel(originalLevel);
        }

        assertEquals("a build below MAX_SURVIVABILITY must push nothing", 0, sender.postCount());
        long warnings = handler.records.stream().filter(r -> r.getLevel() == Level.WARNING).count();
        assertEquals("the refusal must log exactly once, even across two schedule calls", 1, warnings);
    }

    @Test
    public void aStateCapAnswerStopsThePushes() throws Exception {
        Path rootDir = tmp.newFolder("run-d").toPath();
        Files.writeString(rootDir.resolve(RunStatePusher.BUILD_XML), "<build/>");

        FakeSender sender = new FakeSender();
        sender.alwaysAnswer(RunStatePusher.RunStateSender.Answer.refused(RunStatePusher.REASON_STATE_CAP, 0));
        RunStatePusher pusher = newPusher(rootDir, sender, newSpool("spool-d"));

        pusher.runWindow();

        assertTrue("a state-cap refusal must stop the run's pushes", pusher.stoppedForTesting());
        int postsAfterFirstWindow = sender.postCount();

        pusher.runWindow();
        assertEquals("a stopped pusher must post nothing more",
                postsAfterFirstWindow, sender.postCount());
    }

    @Test
    public void aGatewayBudgetFullAnswerDoesNotStopThePushes() throws Exception {
        Path rootDir = tmp.newFolder("run-e").toPath();
        Files.writeString(rootDir.resolve(RunStatePusher.PROGRAM_DAT), "program-bytes");

        FakeSender sender = new FakeSender();
        sender.answerOnce(RunStatePusher.RunStateSender.Answer.refused(RunStatePusher.REASON_BUDGET_FULL, 5));
        RunSpool spool = newSpool("spool-e");
        RunStatePusher pusher = newPusher(rootDir, sender, spool);

        pusher.runWindow();

        assertFalse("a gateway-budget-full answer must not stop the run's pushes",
                pusher.stoppedForTesting());
        assertEquals(1, sender.postCount());
        assertEquals("the unacknowledged record stays spooled for a retry",
                1, spool.readRunStateOrdered(RUN_ID).size());

        sender.clear();
        pusher.runWindow();

        assertFalse(pusher.stoppedForTesting());
        // The window drains the spool first, then pushes the current
        // set: the kept record and the file's fresh copy are two posts.
        assertEquals("a later window still gets the record through", 2, sender.postCount());
        assertTrue("the replay posts the kept record first",
                sender.posts().get(0).getString("relpath").endsWith(RunStatePusher.PROGRAM_DAT));
        assertTrue("the record is acknowledged once it is accepted",
                spool.readRunStateOrdered(RUN_ID).isEmpty());
    }

    @Test
    public void aPostThatThrowsLeavesTheBuildRunningAndEmitsNoBuildEvent() throws Exception {
        Path rootDir = tmp.newFolder("run-f").toPath();
        Files.writeString(rootDir.resolve(RunStatePusher.PROGRAM_DAT), "program-bytes");

        FakeSender sender = new FakeSender();
        sender.throwOnEveryCall(new RuntimeException("loopback unreachable"));
        RunSpool spool = newSpool("spool-f");
        RunStatePusher pusher = newPusher(rootDir, sender, spool);

        long pushedBefore = PluginCounters.snapshot().get(PluginCounters.RUN_STATE_PUSHED);
        long refusedBefore = PluginCounters.snapshot().get(PluginCounters.RUN_STATE_REFUSED);

        // A push failure of any kind must stay inside runWindow(): it
        // must not throw out to the caller, and it must not stop the run.
        pusher.runWindow();

        assertFalse("a post failure must not stop the run's pushes", pusher.stoppedForTesting());
        assertEquals("a thrown post must not count as an acceptance",
                pushedBefore, (long) PluginCounters.snapshot().get(PluginCounters.RUN_STATE_PUSHED));
        assertEquals("a thrown post must not count as a refusal either; it never reached an answer",
                refusedBefore, (long) PluginCounters.snapshot().get(PluginCounters.RUN_STATE_REFUSED));
        assertEquals("the unacknowledged record stays spooled for a retry",
                1, spool.readRunStateOrdered(RUN_ID).size());
    }

    // ---- Pre-flight round 2 ----

    @Test
    public void aRecordKeptByATransportFailureIsRepostedInTheNextWindow() throws Exception {
        Path rootDir = tmp.newFolder("run-g").toPath();
        Files.writeString(rootDir.resolve(RunStatePusher.PROGRAM_DAT), "program-bytes");

        FakeSender sender = new FakeSender();
        RunSpool spool = newSpool("spool-g");
        RunStatePusher pusher = newPusher(rootDir, sender, spool);

        // The first window's post never reaches an answer.
        sender.alwaysAnswer(RunStatePusher.RunStateSender.Answer.refused("transport", 0));
        pusher.runWindow();
        assertEquals("the transport failure keeps its record", 1, spool.readRunStateOrdered(RUN_ID).size());

        // The next window drains the spool first and posts that record
        // again. A generation counter that moved is not supersession.
        sender.clear();
        sender.alwaysAnswer(RunStatePusher.RunStateSender.Answer.accepted());
        pusher.runWindow();

        assertTrue("the kept record must be posted again", sender.postCount() >= 1);
        assertTrue("the replayed record must carry the flow program",
                sender.posts().get(0).getString("relpath").endsWith(RunStatePusher.PROGRAM_DAT));
        assertEquals("the drained record leaves the spool", 0, spool.readRunStateOrdered(RUN_ID).size());
    }

    @Test
    public void aKeptRecordIsNotDroppedWhenTheSourceFileIsGone() throws Exception {
        Path rootDir = tmp.newFolder("run-h").toPath();
        Path workflow = Files.createDirectories(rootDir.resolve("workflow"));
        Path node = workflow.resolve("4.xml");
        Files.writeString(node, "<flow/>");

        FakeSender sender = new FakeSender();
        RunSpool spool = newSpool("spool-h");
        RunStatePusher pusher = newPusher(rootDir, sender, spool);

        sender.alwaysAnswer(RunStatePusher.RunStateSender.Answer.refused("transport", 0));
        pusher.runWindow();
        assertEquals(1, spool.readRunStateOrdered(RUN_ID).size());

        // The source file is gone by the next window, so the spooled
        // record is the last durable copy of it.
        Files.delete(node);

        sender.clear();
        sender.alwaysAnswer(RunStatePusher.RunStateSender.Answer.accepted());
        pusher.runWindow();

        assertEquals("the last durable copy must still be posted", 1, sender.postCount());
        assertTrue(sender.posts().get(0).getString("relpath").endsWith("workflow/4.xml"));
    }

    @Test
    public void removeStopsAWindowThatIsAlreadyOnTheTimer() throws Exception {
        Path rootDir = tmp.newFolder("run-i").toPath();
        Files.writeString(rootDir.resolve(RunStatePusher.PROGRAM_DAT), "program-bytes");

        FakeSender sender = new FakeSender();
        RunStatePusher pusher = newPusher(rootDir, sender, newSpool("spool-i"));
        RunStatePusher.putForTesting(RUN_ID, pusher);

        // The run completes. A window already on the timer must post
        // nothing when it fires.
        RunStatePusher.remove(RUN_ID);
        assertTrue("remove must stop the pusher", pusher.stoppedForTesting());

        pusher.runWindow();
        assertEquals("a stopped pusher posts nothing", 0, sender.postCount());
    }

    @Test
    public void theRetryDelayDoublesAndStopsAtItsCap() throws Exception {
        Path rootDir = tmp.newFolder("run-j").toPath();
        Files.writeString(rootDir.resolve(RunStatePusher.PROGRAM_DAT), "program-bytes");

        FakeSender sender = new FakeSender();
        sender.alwaysAnswer(RunStatePusher.RunStateSender.Answer.refused("transport", 0));
        RunStatePusher pusher = newPusher(rootDir, sender, newSpool("spool-j"));

        long previous = 0;
        for (int i = 0; i < 12; i++) {
            pusher.runWindow();
            long delay = pusher.retryDelayForTesting();
            assertTrue("the retry delay must never exceed its cap",
                    delay <= RunStatePusher.RETRY_DELAY_MAX_MILLIS);
            assertTrue("the retry delay must not shrink while the spool stays undrained",
                    delay >= previous);
            previous = delay;
            // The window armed a timer task of its own; stop it from
            // racing this loop.
            pusher.stoppedForTesting();
        }
        assertEquals("a sustained outage must settle at the cap",
                RunStatePusher.RETRY_DELAY_MAX_MILLIS, pusher.retryDelayForTesting());
    }

    @Test
    public void removeClearsTheRunStateRecordsAndKeepsTheBuildEvents() throws Exception {
        Path rootDir = tmp.newFolder("run-k").toPath();
        Files.writeString(rootDir.resolve(RunStatePusher.PROGRAM_DAT), "program-bytes");

        FakeSender sender = new FakeSender();
        sender.alwaysAnswer(RunStatePusher.RunStateSender.Answer.refused("transport", 0));
        RunSpool spool = newSpool("spool-k");
        RunStatePusher pusher = newPusher(rootDir, sender, spool);
        RunStatePusher.putForTesting(RUN_ID, pusher);

        // A build event of the same run shares the run's directory.
        spool.append(RUN_ID, BuildEvent.builder()
                .phase(BuildEvent.PHASE_COMPLETED)
                .seq(1)
                .runId(RUN_ID)
                .jobPath(JOB_PATH)
                .buildNumber(BUILD_NUMBER)
                .build());

        pusher.runWindow();
        assertEquals("the transport failure keeps its record", 1, spool.readRunStateOrdered(RUN_ID).size());

        RunStatePusher.remove(RUN_ID);

        assertTrue("a finished run keeps no run-state record",
                spool.readRunStateOrdered(RUN_ID).isEmpty());
        assertFalse("the run's own build event must survive",
                spool.readOrdered(RUN_ID).isEmpty());
    }

    // ---- pre-flight round 8: the refresh window ----

    @Test
    public void anIdleBuildPushesAgainWithNoNewFlowNode() throws Exception {
        Path rootDir = tmp.newFolder("run-n").toPath();
        Files.writeString(rootDir.resolve(RunStatePusher.PROGRAM_DAT), "program-bytes");
        Files.writeString(rootDir.resolve(RunStatePusher.BUILD_XML), "<build/>");

        FakeSender sender = new FakeSender();
        RunStatePusher pusher = newPusher(rootDir, sender, newSpool("spool-n"));

        pusher.runWindow();
        int first = sender.postCount();
        assertTrue("the window posts the build record and the flow program", first >= 2);
        assertTrue("a successful window arms a refresh", pusher.refreshArmedForTesting());

        // The refresh fires. No flow node ran, so nothing else would
        // push, and a stale copy would live as long as the step does.
        pusher.runRefresh();
        waitUntil(() -> sender.postCount() > first, 10_000);
    }

    @Test
    public void aStoppedPusherArmsNoRefresh() throws Exception {
        Path rootDir = tmp.newFolder("run-o").toPath();
        Files.writeString(rootDir.resolve(RunStatePusher.PROGRAM_DAT), "program-bytes");

        FakeSender sender = new FakeSender();
        RunStatePusher pusher = newPusher(rootDir, sender, newSpool("spool-o"));
        RunStatePusher.putForTesting(RUN_ID, pusher);

        pusher.runWindow();
        RunStatePusher.remove(RUN_ID);
        pusher.runRefresh();

        assertFalse("a finished run arms no refresh", pusher.refreshArmedForTesting());
    }

    // ---- a build this process resumed ----

    @Test
    public void aResumedBuildPushesWithNoFlowNode() throws Exception {
        Path rootDir = tmp.newFolder("run-r").toPath();
        Files.writeString(rootDir.resolve(RunStatePusher.PROGRAM_DAT), "program-bytes");
        Files.writeString(rootDir.resolve(RunStatePusher.BUILD_XML), "<build/>");

        FakeSender sender = new FakeSender();
        RunStatePusher pusher = newPusher(rootDir, sender, newSpool("spool-r"));

        FlowExecution execution = mock(FlowExecution.class);
        when(execution.getDurabilityHint()).thenReturn(FlowDurabilityHint.MAX_SURVIVABILITY);

        // The resumed build runs inside one long step, so no flow node
        // arms a window. Its own process must replicate the run.
        pusher.scheduleResumed(execution);

        waitUntil(() -> sender.postCount() >= 2, 10_000);
        assertTrue("a successful window arms the refresh that follows it",
                pusher.refreshArmedForTesting());
    }

    @Test
    public void aResumedBuildBelowMaxSurvivabilityPushesNothing() throws Exception {
        Path rootDir = tmp.newFolder("run-s").toPath();
        Files.writeString(rootDir.resolve(RunStatePusher.BUILD_XML), "<build/>");

        FakeSender sender = new FakeSender();
        RunStatePusher pusher = newPusher(rootDir, sender, newSpool("spool-s"));

        FlowExecution execution = mock(FlowExecution.class);
        when(execution.getDurabilityHint()).thenReturn(FlowDurabilityHint.PERFORMANCE_OPTIMIZED);

        pusher.scheduleResumed(execution);
        Thread.sleep(RunStatePusher.WINDOW_MILLIS + 500);

        assertEquals("a resumed build below MAX_SURVIVABILITY must push nothing", 0, sender.postCount());
    }

    @Test
    public void aResumeThatNamesNoExecutionPushesNothing() throws Exception {
        Path rootDir = tmp.newFolder("run-t").toPath();
        Files.writeString(rootDir.resolve(RunStatePusher.BUILD_XML), "<build/>");

        FakeSender sender = new FakeSender();
        RunStatePusher pusher = newPusher(rootDir, sender, newSpool("spool-t"));

        // An execution that is gone names no durability, so the gate
        // fails closed.
        pusher.scheduleResumed(null);
        Thread.sleep(RunStatePusher.WINDOW_MILLIS + 500);

        assertEquals("a resume with no execution must push nothing", 0, sender.postCount());
    }

    // ---- pre-flight round 9: one window at a time ----

    @Test
    public void aRefreshDuringARunningWindowStartsNoSecondWindow() throws Exception {
        Path rootDir = tmp.newFolder("run-p").toPath();
        Files.writeString(rootDir.resolve(RunStatePusher.PROGRAM_DAT), "program-bytes");
        Files.writeString(rootDir.resolve(RunStatePusher.BUILD_XML), "<build/>");

        BlockingSender sender = new BlockingSender();
        RunStatePusher pusher = newPusher(rootDir, sender, newSpool("spool-p"));

        Thread window = new Thread(pusher::runWindow, "window-p");
        window.start();
        sender.awaitFirstPost();

        // The refresh arms a window while the first one still posts. A
        // second window would push the same files under a second
        // generation at the same time.
        pusher.runRefresh();
        Thread.sleep(RunStatePusher.WINDOW_MILLIS + 500);
        assertEquals("a second window posts nothing while the first runs", 1, sender.postCount());
        assertEquals("one window used one generation", 1, sender.distinctStateGens().size());

        sender.release();
        window.join(10_000);

        // The blocked attempt asked for one more window, and exactly one
        // follows.
        waitUntil(() -> sender.distinctStateGens().size() == 2, 15_000);
        Thread.sleep(RunStatePusher.WINDOW_MILLIS + 500);
        assertEquals("one follow-up window, not two", 2, sender.distinctStateGens().size());
    }

    @Test
    public void nodesScheduledDuringAWindowYieldOneFollowUp() throws Exception {
        Path rootDir = tmp.newFolder("run-q").toPath();
        Files.writeString(rootDir.resolve(RunStatePusher.PROGRAM_DAT), "program-bytes");

        BlockingSender sender = new BlockingSender();
        RunStatePusher pusher = newPusher(rootDir, sender, newSpool("spool-q"));

        Thread window = new Thread(pusher::runWindow, "window-q");
        window.start();
        sender.awaitFirstPost();

        for (int i = 0; i < 5; i++) {
            pusher.schedule(null);
        }
        Thread.sleep(RunStatePusher.WINDOW_MILLIS + 500);
        assertEquals("a schedule posts nothing of its own", 1, sender.postCount());
        assertEquals("the running window keeps its own generation", 1, sender.distinctStateGens().size());

        sender.release();
        window.join(10_000);

        waitUntil(() -> sender.distinctStateGens().size() == 2, 15_000);
        Thread.sleep(RunStatePusher.WINDOW_MILLIS + 500);
        assertEquals("five schedules make one follow-up window", 2, sender.distinctStateGens().size());
    }

    // ---- pre-flight round 6: the append and the stop ----

    @Test
    public void aWindowCannotAppendWhileTheStopHoldsTheSpoolLock() throws Exception {
        Path rootDir = tmp.newFolder("run-l").toPath();
        Files.writeString(rootDir.resolve(RunStatePusher.PROGRAM_DAT), "program-bytes");

        FakeSender sender = new FakeSender();
        sender.alwaysAnswer(RunStatePusher.RunStateSender.Answer.refused("transport", 0));
        RunSpool spool = newSpool("spool-l");
        RunStatePusher pusher = newPusher(rootDir, sender, spool);

        Thread window;
        synchronized (pusher.spoolLockForTesting()) {
            window = new Thread(pusher::runWindow, "window-l");
            window.start();
            // The window reaches its append and waits, because the stop
            // holds the same monitor. A record written here would
            // outlive a stop that has already deleted the run's records.
            Thread.sleep(300);
            assertTrue("the append must wait on the monitor", spool.readRunStateOrdered(RUN_ID).isEmpty());
        }
        window.join(5000);
        assertTrue("the window must append once the monitor is free",
                !spool.readRunStateOrdered(RUN_ID).isEmpty());
    }

    @Test
    public void aWindowThatRacesTheStopLeavesNoRecord() throws Exception {
        for (int i = 0; i < 20; i++) {
            Path rootDir = tmp.newFolder("run-m" + i).toPath();
            Files.writeString(rootDir.resolve(RunStatePusher.PROGRAM_DAT), "program-bytes");

            FakeSender sender = new FakeSender();
            sender.alwaysAnswer(RunStatePusher.RunStateSender.Answer.refused("transport", 0));
            RunSpool spool = newSpool("spool-m" + i);
            RunStatePusher pusher = newPusher(rootDir, sender, spool);
            RunStatePusher.putForTesting(RUN_ID, pusher);

            CyclicBarrier start = new CyclicBarrier(2);
            Thread window = new Thread(() -> {
                await(start);
                pusher.runWindow();
            }, "window-m" + i);
            Thread stop = new Thread(() -> {
                await(start);
                RunStatePusher.remove(RUN_ID);
            }, "stop-m" + i);
            window.start();
            stop.start();
            window.join(5000);
            stop.join(5000);

            assertTrue("a finished run keeps no run-state record",
                    spool.readRunStateOrdered(RUN_ID).isEmpty());
        }
    }

    /** Waits on a barrier and fails the calling thread on interruption. */
    private static void await(CyclicBarrier barrier) {
        try {
            barrier.await();
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new IllegalStateException(e);
        } catch (BrokenBarrierException e) {
            throw new IllegalStateException(e);
        }
    }

    // ---- helpers ----

    private static void waitUntil(BooleanSupplier condition, long timeoutMillis) throws InterruptedException {
        long deadline = System.currentTimeMillis() + timeoutMillis;
        while (!condition.getAsBoolean() && System.currentTimeMillis() < deadline) {
            Thread.sleep(50);
        }
        assertTrue("condition not met within " + timeoutMillis + "ms", condition.getAsBoolean());
    }

    /**
     * A sender whose first post waits for the test. It holds a window
     * inside its own push, so the test can fire a second one.
     */
    private static final class BlockingSender implements RunStatePusher.RunStateSender {

        private final List<JSONObject> posts = new CopyOnWriteArrayList<>();
        private final CountDownLatch entered = new CountDownLatch(1);
        private final CountDownLatch released = new CountDownLatch(1);

        void awaitFirstPost() throws InterruptedException {
            assertTrue("the window never reached its first post", entered.await(10, TimeUnit.SECONDS));
        }

        void release() {
            released.countDown();
        }

        int postCount() {
            return posts.size();
        }

        Set<Long> distinctStateGens() {
            Set<Long> gens = new HashSet<>();
            for (JSONObject r : posts) {
                gens.add(r.getLong("state_gen"));
            }
            return gens;
        }

        @Override
        public RunStatePusher.RunStateSender.Answer post(String url, JSONObject record) {
            posts.add(record);
            if (entered.getCount() > 0) {
                entered.countDown();
                try {
                    if (!released.await(20, TimeUnit.SECONDS)) {
                        throw new IllegalStateException("the test never released the first post");
                    }
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                    throw new IllegalStateException(e);
                }
            }
            return RunStatePusher.RunStateSender.Answer.accepted();
        }
    }

    /** Collects every log record published while it is attached. */
    private static final class TestLogHandler extends Handler {
        final List<LogRecord> records = new CopyOnWriteArrayList<>();

        @Override
        public void publish(LogRecord record) {
            records.add(record);
        }

        @Override
        public void flush() {
        }

        @Override
        public void close() {
        }
    }

    /** A {@link RunStatePusher.RunStateSender} double with a scripted answer sequence. */
    private static final class FakeSender implements RunStatePusher.RunStateSender {

        private final List<JSONObject> posts = new CopyOnWriteArrayList<>();
        private final Deque<RunStatePusher.RunStateSender.Answer> queuedAnswers = new ArrayDeque<>();
        private volatile RunStatePusher.RunStateSender.Answer defaultAnswer =
                RunStatePusher.RunStateSender.Answer.accepted();
        private volatile RuntimeException toThrow;

        synchronized void answerOnce(RunStatePusher.RunStateSender.Answer answer) {
            queuedAnswers.add(answer);
        }

        void alwaysAnswer(RunStatePusher.RunStateSender.Answer answer) {
            defaultAnswer = answer;
        }

        void throwOnEveryCall(RuntimeException e) {
            toThrow = e;
        }

        int postCount() {
            return posts.size();
        }

        List<JSONObject> posts() {
            return posts;
        }

        void clear() {
            posts.clear();
        }

        Set<Long> distinctStateGens() {
            Set<Long> gens = new HashSet<>();
            for (JSONObject r : posts) {
                gens.add(r.getLong("state_gen"));
            }
            return gens;
        }

        @Override
        public synchronized RunStatePusher.RunStateSender.Answer post(String url, JSONObject record) {
            posts.add(record);
            if (toThrow != null) {
                throw toThrow;
            }
            if (!queuedAnswers.isEmpty()) {
                return queuedAnswers.poll();
            }
            return defaultAnswer;
        }
    }
}
