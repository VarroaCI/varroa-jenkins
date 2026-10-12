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
    public void buildLibraryWritesTheProtoJsonNames() {
        JSONObject o = library("shared-lib", "1.2.0", "abc123").toJson();

        assertEquals("shared-lib", o.getString("name"));
        assertEquals("1.2.0", o.getString("version"));
        assertEquals("abc123", o.getString("revision"));
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
}
