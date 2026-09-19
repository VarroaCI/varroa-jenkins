package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import net.sf.json.JSONObject;

import org.junit.After;
import org.junit.Test;

/**
 * Tests for {@link VarroaRunRecorder}'s per-run state bound and its
 * heartbeat tick (D1, D13) — plain JUnit, no JenkinsRule. Drives the tick
 * directly instead of waiting on the real timer interval.
 */
public class RunHeartbeatTest {

    @After
    public void resetState() {
        VarroaRunRecorder.resetStateForTesting();
        BuildEventSink.get().resetForTesting();
    }

    @Test
    public void oneHeartbeatPerRunningRunPerTick() {
        VarroaRunRecorder.registerRunForTesting("job-a", 1, "run-a", "intent-a");
        VarroaRunRecorder.registerRunForTesting("job-b", 1, "run-b", "intent-b");
        VarroaRunRecorder.registerRunForTesting("job-c", 1, "run-c", "intent-c");

        VarroaRunRecorder.heartbeatTick();

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        assertEquals(3, result.events.size());
        for (BuildEvent e : result.events) {
            assertEquals(BuildEvent.PHASE_HEARTBEAT, e.getPhase());
        }
    }

    @Test
    public void heartbeatCarriesIdentityFieldsOnly() {
        VarroaRunRecorder.registerRunForTesting("job-a", 3, "run-a", "intent-a");

        VarroaRunRecorder.heartbeatTick();

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        assertEquals(1, result.events.size());
        BuildEvent e = result.events.get(0);
        assertEquals("run-a", e.getRunId());
        assertEquals("intent-a", e.getIntentKey());
        assertEquals("job-a", e.getJobPath());
        assertEquals(3, e.getBuildNumber());
        assertEquals("", e.getResult());
        assertTrue(e.getStages().isEmpty());
        assertTrue(e.getParameters().isEmpty());
        assertTrue(e.getChanges().isEmpty());
        assertEquals(0, e.getDurationMillis());
    }

    @Test
    public void noEventAfterTheRunEnds() {
        VarroaRunRecorder.registerRunForTesting("job-a", 1, "run-a", "intent-a");
        VarroaRunRecorder.removeRunForTesting("job-a", 1);

        VarroaRunRecorder.heartbeatTick();

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        assertEquals(0, result.events.size());
    }

    @Test
    public void sequenceNumbersAdvanceAcrossTicks() {
        VarroaRunRecorder.registerRunForTesting("job-a", 1, "run-a", "intent-a");

        VarroaRunRecorder.heartbeatTick();
        VarroaRunRecorder.heartbeatTick();

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        assertEquals(2, result.events.size());
        assertTrue(result.events.get(1).getSeq() > result.events.get(0).getSeq());
    }

    @Test
    public void thePerRunStateMapIsBoundedAt512Entries() {
        for (int i = 0; i < 520; i++) {
            VarroaRunRecorder.registerRunForTesting("job-" + i, 1, "run-" + i, "intent-" + i);
        }
        assertEquals(512, VarroaRunRecorder.stateSizeForTesting());
    }

    @Test
    public void heartbeatCarriesNoneOfTheSourceFields() {
        VarroaRunRecorder.registerRunForTesting("job-a", 1, "run-a", "intent-a");

        VarroaRunRecorder.heartbeatTick();

        BuildEventSink.DrainResult result = BuildEventSink.get().drain(100);
        assertEquals(1, result.events.size());
        BuildEvent e = result.events.get(0);
        assertEquals(BuildEvent.PHASE_HEARTBEAT, e.getPhase());
        assertTrue(e.getLibraries().isEmpty());
        assertEquals("", e.getDefinitionPath());
        assertTrue(e.getPodTemplates().isEmpty());
        JSONObject json = e.toJson();
        assertFalse(json.containsKey("libraries"));
        assertFalse(json.containsKey("definition_path"));
        assertFalse(json.containsKey("pod_templates"));
    }
}
