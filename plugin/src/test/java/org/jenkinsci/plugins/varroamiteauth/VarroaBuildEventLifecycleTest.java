package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import hudson.ExtensionList;

import java.nio.file.Path;
import java.util.List;

import org.junit.After;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.jvnet.hudson.test.JenkinsRule;

/**
 * Tests for {@link VarroaBuildEventLifecycle}: the gate check on start,
 * the startup spool replay, and the terminator stopping the dispatcher,
 * the heartbeat timer, and the claim queue's executor (ruling r).
 */
public class VarroaBuildEventLifecycleTest {

    @Rule
    public JenkinsRule j = new JenkinsRule();

    @Rule
    public TemporaryFolder tmp = new TemporaryFolder();

    @After
    public void cleanup() {
        VarroaBuildEventLifecycle.stop();
        VarroaRunRecorder.resetStateForTesting();
    }

    private static BuildEvent event(String runId, long seq, String phase) {
        return BuildEvent.builder().runId(runId).seq(seq).phase(phase).build();
    }

    @Test
    public void startsTheDispatcherOnlyWithTheGateOn() {
        VarroaBuildEventLifecycle.start("0");
        assertNull("the gate is off, so nothing starts", VarroaBuildEventLifecycle.current());

        VarroaBuildEventLifecycle.start("1");
        BuildEventDispatcher dispatcher = VarroaBuildEventLifecycle.current();
        assertNotNull("the gate is on, so the dispatcher starts", dispatcher);
        assertFalse(dispatcher.isStoppedForTesting());
    }

    @Test
    public void replaySpoolOntoAnnouncesEveryExistingRunOldestFirst() throws Exception {
        Path base = tmp.getRoot().toPath();
        RunSpool spool = new RunSpool(base, RunSpool.DEFAULT_CAP_BYTES);
        spool.append("01950000-run-a", event("01950000-run-a", 0, BuildEvent.PHASE_STARTED));
        spool.append("01960000-run-b", event("01960000-run-b", 0, BuildEvent.PHASE_STARTED));
        assertEquals(List.of("01950000-run-a", "01960000-run-b"), spool.listRuns());

        BuildEventSink sink = new BuildEventSink(16);
        BuildEventDispatcher dispatcher = new BuildEventDispatcher(
                sink, spool, new MiteLoopbackClient(), millis -> { });

        VarroaBuildEventLifecycle.replaySpoolOnto(dispatcher, spool);

        // Both runs are announced, older run first; BuildEventDispatcherTest
        // covers the posting itself against a stub server.
        assertEquals(List.of("01950000-run-a", "01960000-run-b"), dispatcher.announcedForTesting());
    }

    @Test
    public void theTerminatorStopsTheDispatcherTheHeartbeatAndTheClaimQueue() {
        VarroaBuildEventLifecycle.start("1");
        BuildEventDispatcher dispatcher = VarroaBuildEventLifecycle.current();
        assertNotNull(dispatcher);
        VarroaRunRecorder.ensureHeartbeatStartedForTesting();
        assertTrue(VarroaRunRecorder.heartbeatStartedForTesting());
        VarroaClaimDispatcher claimDispatcher = ExtensionList.lookupSingleton(VarroaClaimDispatcher.class);
        assertFalse(claimDispatcher.claimQueueForTesting().isShutdownForTesting());

        VarroaBuildEventLifecycle.stop();

        assertTrue("the dispatcher must be stopped", dispatcher.isStoppedForTesting());
        assertNull("the tracked dispatcher must be cleared", VarroaBuildEventLifecycle.current());
        assertFalse("the heartbeat timer must be stopped", VarroaRunRecorder.heartbeatStartedForTesting());
        assertTrue("the claim queue's executor must be stopped",
                claimDispatcher.claimQueueForTesting().isShutdownForTesting());
    }
}
