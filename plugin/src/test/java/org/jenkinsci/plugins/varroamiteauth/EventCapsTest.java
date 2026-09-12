package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import org.junit.Test;

/**
 * Tests for {@link VarroaRunRecorder}'s D11 size caps, the oversize
 * ladder, and the identifier pass-through rule — plain JUnit, no
 * JenkinsRule.
 */
public class EventCapsTest {

    private static BuildStage stage(String id) {
        return BuildStage.builder().stageId(id).name("stage-" + id).status("SUCCESS").build();
    }

    private static BuildChange change(String commitId, String message) {
        return BuildChange.builder().commitId(commitId).message(message).build();
    }

    private static String big(int chars) {
        StringBuilder sb = new StringBuilder(chars);
        for (int i = 0; i < chars; i++) {
            sb.append('x');
        }
        return sb.toString();
    }

    private static int serializedBytes(BuildEvent event) {
        return event.toJson().toString().getBytes(StandardCharsets.UTF_8).length;
    }

    // --- Hard caps (5.11) ---

    @Test
    public void stagesAreCappedAt1000KeepingTheNewest() {
        List<BuildStage> stages = new ArrayList<>();
        for (int i = 0; i < 1005; i++) {
            stages.add(stage("s" + i));
        }
        BuildEvent event = BuildEvent.builder().phase(BuildEvent.PHASE_COMPLETED).stages(stages).build();

        BuildEvent capped = VarroaRunRecorder.applyCaps(event);

        assertEquals(1000, capped.getStages().size());
        assertEquals("s5", capped.getStages().get(0).getStageId());
        assertEquals("s1004", capped.getStages().get(999).getStageId());
    }

    @Test
    public void changesAreCappedAt200KeepingTheNewest() {
        List<BuildChange> changes = new ArrayList<>();
        for (int i = 0; i < 205; i++) {
            changes.add(change("c" + i, "msg"));
        }
        BuildEvent event = BuildEvent.builder().phase(BuildEvent.PHASE_COMPLETED).changes(changes).build();

        BuildEvent capped = VarroaRunRecorder.applyCaps(event);

        assertEquals(200, capped.getChanges().size());
        assertEquals("c5", capped.getChanges().get(0).getCommitId());
        assertEquals("c204", capped.getChanges().get(199).getCommitId());
    }

    @Test
    public void parametersAreCappedAt200KeepingTheNewest() {
        Map<String, String> params = new LinkedHashMap<>();
        for (int i = 0; i < 205; i++) {
            params.put("p" + i, "v" + i);
        }
        BuildEvent event = BuildEvent.builder().phase(BuildEvent.PHASE_COMPLETED).parameters(params).build();

        BuildEvent capped = VarroaRunRecorder.applyCaps(event);

        assertEquals(200, capped.getParameters().size());
        assertFalse(capped.getParameters().containsKey("p0"));
        assertTrue(capped.getParameters().containsKey("p204"));
    }

    // --- Oversize ladder (5.11a) ---

    @Test
    public void ladderDropsStagesBeforeTouchingChanges() {
        List<BuildStage> stages = new ArrayList<>();
        for (int i = 0; i < 1000; i++) {
            stages.add(BuildStage.builder().stageId("s" + i).name(big(1000)).status("SUCCESS").build());
        }
        List<BuildChange> changes = new ArrayList<>();
        for (int i = 0; i < 10; i++) {
            changes.add(change("c" + i, "small message"));
        }
        BuildEvent event = BuildEvent.builder()
                .phase(BuildEvent.PHASE_COMPLETED)
                .stages(stages)
                .changes(changes)
                .build();
        assertTrue("fixture must start oversize", serializedBytes(event) > 512 * 1024);

        BuildEvent shrunk = VarroaRunRecorder.applyCaps(event);

        assertTrue(serializedBytes(shrunk) <= 512 * 1024);
        assertEquals("changes are untouched while dropping stages already fits",
                10, shrunk.getChanges().size());
        assertTrue("some stages were dropped", shrunk.getStages().size() < 1000);
    }

    @Test
    public void ladderDropsChangesWhenStagesAloneDoNotFit() {
        List<BuildChange> changes = new ArrayList<>();
        for (int i = 0; i < 200; i++) {
            changes.add(change("c" + i, big(4000)));
        }
        BuildEvent event = BuildEvent.builder().phase(BuildEvent.PHASE_COMPLETED).changes(changes).build();
        assertTrue("fixture must start oversize", serializedBytes(event) > 512 * 1024);

        BuildEvent shrunk = VarroaRunRecorder.applyCaps(event);

        assertTrue(serializedBytes(shrunk) <= 512 * 1024);
        assertTrue("changes were dropped", shrunk.getChanges().size() < 200);
    }

    @Test
    public void aReducedEventStillAboveTheLoopbackLimitIsDroppedAndCounted() {
        VarroaRunRecorder.resetOversizedDroppedForTesting();
        BuildEvent event = BuildEvent.builder()
                .phase(BuildEvent.PHASE_COMPLETED)
                .runId("run-1")
                .intentKey("intent-1")
                .jobPath(big(2 * 1024 * 1024))
                .buildNumber(7)
                .result("SUCCESS")
                .stages(List.of(stage("s0")))
                .changes(List.of(change("c0", "msg")))
                .build();

        BuildEvent result = VarroaRunRecorder.applyCaps(event);

        // The ladder cannot shrink an identifier, so even the reduced,
        // identity-only shape still carries the 2 MiB job_path and stays
        // above the loopback's 1 MiB body limit. The plugin never posts
        // that body: it drops the event and counts the drop.
        assertNull(result);
        assertEquals(1, VarroaRunRecorder.oversizedDroppedCount());
    }

    @Test
    public void aReducedEventThatFitsPostsNormally() {
        // Large stage content forces the ladder all the way through
        // stages, changes, and parameters, but with ordinary, short
        // identifiers the event is small long before it needs the
        // reduced, identity-only fallback.
        List<BuildStage> stages = new ArrayList<>();
        for (int i = 0; i < 1000; i++) {
            stages.add(BuildStage.builder().stageId("s" + i).name(big(1000)).status("SUCCESS").build());
        }
        BuildEvent event = BuildEvent.builder()
                .phase(BuildEvent.PHASE_COMPLETED)
                .runId("run-1")
                .intentKey("intent-1")
                .jobPath("normal/job/path")
                .buildNumber(7)
                .result("SUCCESS")
                .stages(stages)
                .build();
        assertTrue("fixture must start oversize", serializedBytes(event) > 512 * 1024);

        BuildEvent result = VarroaRunRecorder.applyCaps(event);

        assertNotNull("a normal-sized event must never be dropped", result);
        assertTrue(serializedBytes(result) <= 512 * 1024);
        assertEquals("run-1", result.getRunId());
        assertEquals("normal/job/path", result.getJobPath());
    }

    @Test
    public void aReducedEventAtTheFramedLoopbackLimitPostsButOneByteOverIsDropped() {
        VarroaRunRecorder.resetOversizedDroppedForTesting();
        // The posted body frames a solo event as "[" + json + "]", two
        // bytes the raw event size does not carry. overhead is that
        // framing-free size with an empty jobPath, so overhead + N gives
        // the exact raw size for an N-character jobPath.
        int overhead = serializedBytes(reducedFixture(0));
        int fitLength = (1024 * 1024) - 2 - overhead;
        int dropLength = fitLength + 1;

        BuildEvent fits = VarroaRunRecorder.applyCaps(reducedFixture(fitLength));
        assertNotNull("a body whose framed size lands exactly at the 1 MiB limit must post", fits);
        assertEquals(1024 * 1024, serializedBytes(fits) + 2);

        BuildEvent drops = VarroaRunRecorder.applyCaps(reducedFixture(dropLength));
        assertNull("a body one byte over the framed 1 MiB limit must be dropped", drops);
        assertEquals(1, VarroaRunRecorder.oversizedDroppedCount());
    }

    /** A reduced, identity-only event shape — no stages, changes, or parameters. */
    private static BuildEvent reducedFixture(int jobPathLength) {
        return BuildEvent.builder()
                .phase(BuildEvent.PHASE_COMPLETED)
                .runId("run-1")
                .intentKey("intent-1")
                .jobPath(big(jobPathLength))
                .buildNumber(7)
                .result("SUCCESS")
                .build();
    }

    // --- Identifier pass-through (5.11) ---

    @Test
    public void identifiersAreNeverTruncated() {
        String longValue = big(4000);
        BuildEvent event = BuildEvent.builder()
                .phase(BuildEvent.PHASE_COMPLETED)
                .runId(longValue)
                .intentKey(longValue)
                .jobPath(longValue)
                .actor(longValue)
                .result(longValue)
                .upstreamJobPath(longValue)
                .build();

        BuildEvent capped = VarroaRunRecorder.applyCaps(event);

        assertEquals(4000, capped.getRunId().length());
        assertEquals(4000, capped.getIntentKey().length());
        assertEquals(4000, capped.getJobPath().length());
        assertEquals(4000, capped.getActor().length());
        assertEquals(4000, capped.getResult().length());
        assertEquals(4000, capped.getUpstreamJobPath().length());
    }

    @Test
    public void stageIdentifiersAreNeverTruncated() {
        String longValue = big(4000);
        BuildStage longStage = BuildStage.builder()
                .stageId(longValue)
                .parentId(longValue)
                .status("SUCCESS")
                .build();
        BuildEvent event = BuildEvent.builder()
                .phase(BuildEvent.PHASE_STAGE)
                .stages(List.of(longStage))
                .build();

        BuildEvent capped = VarroaRunRecorder.applyCaps(event);

        assertEquals(4000, capped.getStages().get(0).getStageId().length());
        assertEquals(4000, capped.getStages().get(0).getParentId().length());
    }
}
