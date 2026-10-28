package org.jenkinsci.plugins.varroamiteauth;

import static org.junit.Assert.*;

import java.util.List;
import net.sf.json.JSONArray;
import net.sf.json.JSONObject;
import org.junit.Test;

/**
 * Tests for the proto JSON keys of the three run-source fields on
 * {@link BuildEvent} — plain JUnit, no JenkinsRule.
 */
public class BuildEventJsonTest {

    private static BuildLibrary library(String name, String version, String revision) {
        return BuildLibrary.builder().name(name).version(version).revision(revision).build();
    }

    private static BuildPodTemplate podTemplate(String name, String label, String image) {
        return BuildPodTemplate.builder().name(name).label(label).image(image).build();
    }

    @Test
    public void buildEventWritesTheScmForgeJsonName() {
        JSONObject o = BuildEvent.builder().scmUrl("https://gitea.example.com/o/r").scmForge("gitea").build().toJson();

        assertEquals("gitea", o.getString("scm_forge"));
    }

    @Test
    public void buildLibraryWritesTheProtoJsonNames() {
        JSONObject o = library("shared-lib", "1.2.0", "abc123").toJson();

        assertEquals("shared-lib", o.getString("name"));
        assertEquals("1.2.0", o.getString("version"));
        assertEquals("abc123", o.getString("revision"));
    }

    @Test
    public void buildLibraryWritesTheRepositoryJsonNames() {
        JSONObject o = BuildLibrary.builder()
                .name("shared-lib")
                .repositoryUrl("https://github.com/example/shared-lib")
                .repositoryForge("github")
                .build()
                .toJson();

        assertEquals("https://github.com/example/shared-lib", o.getString("repository_url"));
        assertEquals("github", o.getString("repository_forge"));
    }

    @Test
    public void buildPodTemplateWritesTheProtoJsonNames() {
        JSONObject o = podTemplate("maven", "linux", "maven:3.9-eclipse-temurin-21").toJson();

        assertEquals("maven", o.getString("name"));
        assertEquals("linux", o.getString("label"));
        assertEquals("maven:3.9-eclipse-temurin-21", o.getString("image"));
    }

    @Test
    public void toJsonWritesTheThreeKeysUnderTheProtoJsonNames() {
        BuildEvent event = BuildEvent.builder()
                .phase(BuildEvent.PHASE_RUN_SOURCE)
                .libraries(List.of(library("shared-lib", "1.2.0", "abc123")))
                .definitionPath("ci/Jenkinsfile")
                .podTemplates(List.of(podTemplate("maven", "linux", "maven:3.9")))
                .build();

        JSONObject o = event.toJson();

        JSONArray libraries = o.getJSONArray("libraries");
        assertEquals(1, libraries.size());
        assertEquals("shared-lib", libraries.getJSONObject(0).getString("name"));
        assertEquals("1.2.0", libraries.getJSONObject(0).getString("version"));
        assertEquals("abc123", libraries.getJSONObject(0).getString("revision"));

        assertEquals("ci/Jenkinsfile", o.getString("definition_path"));

        JSONArray podTemplates = o.getJSONArray("pod_templates");
        assertEquals(1, podTemplates.size());
        assertEquals("maven", podTemplates.getJSONObject(0).getString("name"));
        assertEquals("linux", podTemplates.getJSONObject(0).getString("label"));
        assertEquals("maven:3.9", podTemplates.getJSONObject(0).getString("image"));

        assertFalse("the camelCase name never appears", o.containsKey("definitionPath"));
        assertFalse("the camelCase name never appears", o.containsKey("podTemplates"));
    }

    @Test
    public void anEmptyListAndAnEmptyPathWriteNoKey() {
        BuildEvent event = BuildEvent.builder()
                .phase(BuildEvent.PHASE_RUN_SOURCE)
                .runId("run-1")
                .build();

        JSONObject o = event.toJson();

        assertFalse(o.containsKey("libraries"));
        assertFalse(o.containsKey("definition_path"));
        assertFalse(o.containsKey("pod_templates"));
        assertEquals("run-1", o.getString("run_id"));
    }

    @Test
    public void toBuilderCopiesTheThreeFields() {
        BuildEvent event = BuildEvent.builder()
                .phase(BuildEvent.PHASE_RUN_SOURCE)
                .libraries(List.of(library("shared-lib", "1.2.0", "abc123")))
                .definitionPath("ci/Jenkinsfile")
                .podTemplates(List.of(podTemplate("maven", "linux", "maven:3.9")))
                .build();

        BuildEvent copy = event.toBuilder().build();

        assertEquals(1, copy.getLibraries().size());
        assertEquals("shared-lib", copy.getLibraries().get(0).getName());
        assertEquals("ci/Jenkinsfile", copy.getDefinitionPath());
        assertEquals(1, copy.getPodTemplates().size());
        assertEquals("maven", copy.getPodTemplates().get(0).getName());
    }

    @Test
    public void rejectedSignatureWritesItsProtoKeyOnlyWhenSet() {
        JSONObject with = BuildEvent.builder().phase(BuildEvent.PHASE_COMPLETED)
                .rejectedSignature("method a b").build().toJson();
        assertEquals("method a b", with.getString("rejected_signature"));

        JSONObject without = BuildEvent.builder().phase(BuildEvent.PHASE_COMPLETED).build().toJson();
        assertFalse(without.has("rejected_signature"));
    }

    @Test
    public void testSuitesSerializeWithProtoNames() {
        BuildTestSuite suite = BuildTestSuite.builder()
                .name("unit").total(3).failed(1).skipped(1).durationMs(42L).build();
        JSONObject json = BuildEvent.builder()
                .phase(BuildEvent.PHASE_COMPLETED)
                .testSuites(List.of(suite))
                .build()
                .toJson();

        JSONObject row = json.getJSONArray("test_suites").getJSONObject(0);
        assertEquals("unit", row.getString("name"));
        assertEquals(3, row.getInt("total"));
        assertEquals(1, row.getInt("failed"));
        assertEquals(1, row.getInt("skipped"));
        assertEquals(42L, row.getLong("duration_ms"));
    }

    @Test
    public void emptyTestSuitesWriteNoKey() {
        JSONObject json = BuildEvent.builder().phase(BuildEvent.PHASE_COMPLETED).build().toJson();
        assertFalse(json.has("test_suites"));
    }

    @Test
    public void qualityReportsSerializeWithProtoNames() {
        BuildCoverageCounter line = BuildCoverageCounter.builder().metric("LINE").covered(4).total(5).build();
        BuildCoverageReport coverage = BuildCoverageReport.builder()
                .id("jacoco").name("Coverage").line(line).gateStatus("PASSED").build();
        BuildWarningsReport warnings = BuildWarningsReport.builder()
                .id("checkstyle").name("CheckStyle Warnings").total(3).error(1).high(0).normal(1).low(1)
                .hasReference(true).newTotal(2).fixedTotal(1).gateStatus("NOTE").build();
        JSONObject json = BuildEvent.builder()
                .phase(BuildEvent.PHASE_COMPLETED)
                .coverageReports(List.of(coverage))
                .warningsReports(List.of(warnings))
                .build()
                .toJson();

        JSONObject c = json.getJSONArray("coverage_reports").getJSONObject(0);
        assertEquals("jacoco", c.getString("id"));
        assertEquals(4, c.getJSONObject("line").getInt("covered"));
        assertEquals("PASSED", c.getString("gate_status"));
        JSONObject w = json.getJSONArray("warnings_reports").getJSONObject(0);
        assertEquals("checkstyle", w.getString("id"));
        assertEquals(3, w.getInt("total"));
        assertEquals(2, w.getInt("new_total"));
        assertEquals(1, w.getInt("fixed_total"));
        assertEquals("NOTE", w.getString("gate_status"));
    }

    @Test
    public void emptyQualityListsWriteNoKey() {
        JSONObject json = BuildEvent.builder().phase(BuildEvent.PHASE_COMPLETED).build().toJson();
        assertFalse(json.has("coverage_reports"));
        assertFalse(json.has("warnings_reports"));
    }

    @Test
    public void toBuilderCopiesTheQualityReports() {
        BuildWarningsReport warnings = BuildWarningsReport.builder().id("checkstyle").name("CS").total(1).build();
        BuildCoverageReport coverage = BuildCoverageReport.builder().id("jacoco").name("Coverage").build();
        BuildEvent original = BuildEvent.builder()
                .phase(BuildEvent.PHASE_COMPLETED)
                .coverageReports(List.of(coverage))
                .warningsReports(List.of(warnings))
                .build();

        BuildEvent copy = original.toBuilder().build();

        assertEquals(1, copy.getCoverageReports().size());
        assertEquals("jacoco", copy.getCoverageReports().get(0).getId());
        assertEquals(1, copy.getWarningsReports().size());
        assertEquals("checkstyle", copy.getWarningsReports().get(0).getId());
    }

}
