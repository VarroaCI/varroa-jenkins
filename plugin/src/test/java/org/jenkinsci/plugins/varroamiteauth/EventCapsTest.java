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
    public void anOverCapLibraryUrlIsDroppedNotTruncated() {
        String overCap = "https://github.com/" + "a".repeat(600);
        BuildLibrary library = BuildLibrary.builder()
                .name("shared-lib")
                .version("1.2.0")
                .repositoryUrl(overCap)
                .repositoryForge("github")
                .build();
        BuildEvent event = BuildEvent.builder()
                .phase(BuildEvent.PHASE_RUN_SOURCE)
                .libraries(List.of(library))
                .build();

        BuildLibrary capped = VarroaRunRecorder.applyCaps(event).getLibraries().get(0);

        assertEquals("", capped.getRepositoryUrl());
        assertEquals("the name and version survive the URL drop", "shared-lib", capped.getName());
        assertEquals("github", capped.getRepositoryForge());
    }

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

    // --- The run-source caps and rungs ---

    @Test
    public void librariesAreCappedAt50KeepingTheNewest() {
        List<BuildLibrary> libraries = new ArrayList<>();
        for (int i = 0; i < 60; i++) {
            libraries.add(BuildLibrary.builder().name("lib-" + i).version("1.0").build());
        }
        BuildEvent event = BuildEvent.builder()
                .phase(BuildEvent.PHASE_RUN_SOURCE)
                .libraries(libraries)
                .build();

        BuildEvent capped = VarroaRunRecorder.applyCaps(event);

        assertEquals(50, capped.getLibraries().size());
        assertEquals("lib-10", capped.getLibraries().get(0).getName());
        assertEquals("lib-59", capped.getLibraries().get(49).getName());
    }

    @Test
    public void ladderDropsPodTemplatesBeforeItTouchesLibraries() {
        List<BuildPodTemplate> podTemplates = new ArrayList<>();
        for (int i = 0; i < 140; i++) {
            podTemplates.add(BuildPodTemplate.builder().name("pod-" + i).label("linux").image(big(12000)).build());
        }
        BuildEvent event = BuildEvent.builder()
                .phase(BuildEvent.PHASE_RUN_SOURCE)
                .definitionPath("ci/Jenkinsfile")
                .podTemplates(podTemplates)
                .libraries(List.of(BuildLibrary.builder().name("shared-lib").version("1.2.0").build()))
                .build();
        assertTrue("fixture must start oversize", serializedBytes(event) > 512 * 1024);

        BuildEvent shrunk = VarroaRunRecorder.applyCaps(event);

        assertTrue(serializedBytes(shrunk) <= 512 * 1024);
        assertTrue("pod_templates were dropped", shrunk.getPodTemplates().size() < 140);
        assertEquals("libraries survive while the pod rung fits", 1, shrunk.getLibraries().size());
    }

    @Test
    public void ladderDropsLibrariesWhenThePodRungIsNotEnough() {
        // The two source rungs sit after the stage rung and the error
        // truncation, so the fixture must stay above 512 KiB with an empty
        // stage list. An identifier holds that size, because the stage rung
        // never drops an identifier.
        List<BuildStage> stages = new ArrayList<>();
        for (int i = 0; i < 1000; i++) {
            stages.add(BuildStage.builder()
                    .stageId("s" + i)
                    .name(big(512))
                    .status("SUCCESS")
                    .error(big(2048))
                    .build());
        }
        List<BuildPodTemplate> podTemplates = new ArrayList<>();
        for (int i = 0; i < 50; i++) {
            podTemplates.add(BuildPodTemplate.builder().name("pod-" + i).label("linux").image(big(512)).build());
        }
        List<BuildLibrary> libraries = new ArrayList<>();
        for (int i = 0; i < 50; i++) {
            libraries.add(BuildLibrary.builder().name("lib-" + i).version(big(512)).build());
        }
        BuildEvent event = BuildEvent.builder()
                .phase(BuildEvent.PHASE_RUN_SOURCE)
                .runId("run-1")
                .intentKey("intent-1")
                .jobPath(big(700 * 1024))
                .buildNumber(7)
                .definitionPath("ci/Jenkinsfile")
                .stages(stages)
                .podTemplates(podTemplates)
                .libraries(libraries)
                .build();
        assertTrue("fixture must start oversize", serializedBytes(event) > 512 * 1024);

        BuildEvent shrunk = VarroaRunRecorder.applyCaps(event);

        assertTrue("the stage rung drains before both source rungs", shrunk.getStages().isEmpty());
        assertTrue("the pod rung drains first", shrunk.getPodTemplates().isEmpty());
        assertTrue("the library rung follows", shrunk.getLibraries().isEmpty());
        assertEquals("only the reduced event survives", "", shrunk.getDefinitionPath());
        assertTrue("the reduced event stays inside the loopback limit",
                serializedBytes(shrunk) <= 1024 * 1024);
    }

    @Test
    public void podTemplatesAreCappedAt50KeepingTheNewest() {
        // The resolver no longer bounds its own list, so a run that
        // allocates 60 nodes reaches the caps with 60 entries.
        List<BuildPodTemplate> podTemplates = new ArrayList<>();
        for (int i = 0; i < 60; i++) {
            podTemplates.add(BuildPodTemplate.builder()
                    .name("pod-" + i)
                    .label("linux")
                    .image("image-" + i)
                    .build());
        }
        BuildEvent event = BuildEvent.builder()
                .phase(BuildEvent.PHASE_RUN_SOURCE)
                .podTemplates(podTemplates)
                .build();

        BuildEvent capped = VarroaRunRecorder.applyCaps(event);

        assertEquals(50, capped.getPodTemplates().size());
        assertEquals("pod-10", capped.getPodTemplates().get(0).getName());
        assertEquals("pod-59", capped.getPodTemplates().get(49).getName());
        assertEquals("image-59", capped.getPodTemplates().get(49).getImage());
    }

    // --- The pending-input rung (D2) ---

    private static BuildInputParameter wideParameter(int choices, int choiceChars) {
        List<String> values = new ArrayList<>();
        for (int i = 0; i < choices; i++) {
            values.add(big(choiceChars));
        }
        return BuildInputParameter.builder()
                .name("ENV")
                .type("ChoiceParameterDefinition")
                .description(big(64))
                .defaultValue(big(64))
                .choices(values)
                .build();
    }

    private static BuildInput wideInput(String id, int parameters) {
        List<BuildInputParameter> defs = new ArrayList<>();
        for (int i = 0; i < parameters; i++) {
            defs.add(wideParameter(20, 400));
        }
        return BuildInput.builder()
                .id(id)
                .message("Deploy?")
                .ok("Ship it")
                .submitter("varroa-payments")
                .parameters(defs)
                .build();
    }

    private static BuildInput narrowInput(String id) {
        return BuildInput.builder()
                .id(id)
                .message("Deploy?")
                .ok("Ship it")
                .parameters(List.of(wideParameter(1, 8)))
                .build();
    }

    @Test
    public void anOversizeRunInputEventLosesItsOldestParameterList() {
        // Four wide inputs hold the event above the size limit, and clearing
        // the oldest parameter list alone brings it under. The rung therefore
        // stops after the oldest one and leaves the later lists alone.
        List<BuildInput> inputs = new ArrayList<>();
        for (int i = 0; i < 4; i++) {
            inputs.add(wideInput("in-" + i, 20));
        }
        for (int i = 4; i < 20; i++) {
            inputs.add(narrowInput("in-" + i));
        }
        BuildEvent event = BuildEvent.builder()
                .phase(BuildEvent.PHASE_RUN_INPUT)
                .runId("run-1")
                .intentKey("intent-1")
                .jobPath("job-a")
                .buildNumber(7)
                .seq(3)
                .inputs(inputs)
                .build();
        assertTrue("fixture must start oversize", serializedBytes(event) > 512 * 1024);

        BuildEvent shrunk = VarroaRunRecorder.applyCaps(event);

        assertEquals("the rung drops no input entry", 20, shrunk.getInputs().size());
        assertTrue("the oldest parameter list goes first", shrunk.getInputs().get(0).getParameters().isEmpty());
        assertFalse("a later parameter list stays", shrunk.getInputs().get(19).getParameters().isEmpty());
        assertEquals("in-19", shrunk.getInputs().get(19).getId());
        assertTrue("the shrunk event fits the size limit", serializedBytes(shrunk) <= 512 * 1024);
    }

    @Test
    public void anOversizeRunInputEventKeepsEveryInput() {
        List<BuildInput> inputs = new ArrayList<>();
        for (int i = 0; i < 20; i++) {
            inputs.add(wideInput("in-" + i, 20));
        }
        BuildEvent event = BuildEvent.builder()
                .phase(BuildEvent.PHASE_RUN_INPUT)
                .runId("run-1")
                .buildNumber(7)
                .inputs(inputs)
                .build();
        assertTrue("fixture must start oversize", serializedBytes(event) > 512 * 1024);

        BuildEvent shrunk = VarroaRunRecorder.applyCaps(event);

        assertEquals("every input survives the ladder", 20, shrunk.getInputs().size());
        for (int i = 0; i < 20; i++) {
            assertEquals("in-" + i, shrunk.getInputs().get(i).getId());
        }
        assertTrue("the oldest parameter list is empty", shrunk.getInputs().get(0).getParameters().isEmpty());
        assertTrue("the shrunk event fits the size limit", serializedBytes(shrunk) <= 512 * 1024);
        assertEquals("the identity fields survive", "run-1", shrunk.getRunId());
    }

    @Test
    public void inputsAreCappedAt20KeepingTheOldest() {
        List<BuildInput> inputs = new ArrayList<>();
        for (int i = 0; i < 25; i++) {
            inputs.add(BuildInput.builder().id("in-" + i).message("q" + i).build());
        }
        BuildEvent event = BuildEvent.builder()
                .phase(BuildEvent.PHASE_RUN_INPUT)
                .inputs(inputs)
                .build();

        BuildEvent capped = VarroaRunRecorder.applyCaps(event);

        assertEquals(20, capped.getInputs().size());
        assertEquals("in-0", capped.getInputs().get(0).getId());
        assertEquals("in-19", capped.getInputs().get(19).getId());
    }
}
