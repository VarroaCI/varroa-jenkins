package org.jenkinsci.plugins.varroamiteauth.template;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertTrue;
import static org.junit.Assert.fail;

import java.io.InputStream;
import java.util.List;
import java.util.Map;

import org.junit.Test;
import org.yaml.snakeyaml.Yaml;

/**
 * Walks both halves of the shared golden corpus from task 1.8 and asserts
 * {@link VarroaMarkerValidator} reaches the same verdict as the Go side
 * (design D15). The corpus lives at
 * {@code internal/bundle/pipelinetemplate/testdata/schema-corpus/} in the
 * repository; this test reads a mirror under {@code src/test/resources}
 * because the plugin's Maven build runs in a container that does not see
 * the Go module tree.
 */
public class MarkerValidatorCorpusTest {

    private static final String BASE = "/org/jenkinsci/plugins/varroamiteauth/template/schema-corpus/";

    @Test
    public void subsetCorpus() throws Exception {
        Map<String, Object> manifest = loadYamlMap(BASE + "subset/manifest.yaml");
        List<Map<String, Object>> cases = asCaseList(manifest);
        assertFalse("subset manifest holds no cases", cases.isEmpty());
        for (Map<String, Object> c : cases) {
            String id = str(c, "id");
            String file = str(c, "file");
            String verdict = str(c, "verdict");
            String contains = str(c, "contains");
            String message = str(c, "message");

            Map<String, Object> schema = loadYamlMap(BASE + "subset/" + file);
            List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkSchema(schema);
            assertVerdict(id, verdict, contains, message, findings);
        }
    }

    @Test
    public void valuesCorpus() throws Exception {
        Map<String, Object> manifest = loadYamlMap(BASE + "values/manifest.yaml");
        List<Map<String, Object>> cases = asCaseList(manifest);
        assertFalse("values manifest holds no cases", cases.isEmpty());
        for (Map<String, Object> c : cases) {
            String id = str(c, "id");
            String schemaFile = str(c, "schema");
            String valuesFile = str(c, "values");
            String verdict = str(c, "verdict");
            String contains = str(c, "contains");
            String message = str(c, "message");
            @SuppressWarnings("unchecked")
            Map<String, Object> expectDefaults = (Map<String, Object>) c.get("expectDefaults");

            Map<String, Object> schema = loadYamlMap(BASE + "values/" + schemaFile);
            List<VarroaMarkerValidator.Finding> subsetFindings = VarroaMarkerValidator.checkSchema(schema);
            assertTrue(id + ": fixture schema is not itself subset-valid: " + findingsText(subsetFindings),
                    subsetFindings.isEmpty());

            Map<String, Object> values = loadYamlMap(BASE + "values/" + valuesFile);
            VarroaMarkerValidator.ValuesResult result = VarroaMarkerValidator.validateValues(schema, values);
            assertVerdict(id, verdict, contains, message, result.getFindings());

            if (expectDefaults != null) {
                for (Map.Entry<String, Object> e : expectDefaults.entrySet()) {
                    Object got = result.getDefaulted().get(e.getKey());
                    assertNotNull(id + ": expected defaulted value \"" + e.getKey() + "\" to be set", got);
                    assertEquals(id + ": expected defaulted value \"" + e.getKey() + "\"", e.getValue(), got);
                }
            }
        }
    }

    /**
     * Asserts the manifest entry's verdict against {@code findings}. A
     * reject case carrying an exact {@code message} field must match the
     * joined findings text exactly, mirroring the Go corpus runner; a
     * reject case with no {@code message} field keeps the looser {@code
     * contains} substring check.
     */
    private static void assertVerdict(String id, String verdict, String contains, String message,
            List<VarroaMarkerValidator.Finding> findings) {
        String text = findingsText(findings);
        switch (verdict) {
            case "accept":
                assertTrue(id + ": expected accept, got findings: " + text, findings.isEmpty());
                break;
            case "reject":
                assertFalse(id + ": expected reject, got none", findings.isEmpty());
                if (!message.isEmpty()) {
                    assertEquals(id + ": expected findings to read exactly", message, text);
                } else if (!contains.isEmpty()) {
                    assertTrue(id + ": expected findings to mention \"" + contains + "\", got: " + text,
                            text.contains(contains));
                }
                break;
            default:
                fail(id + ": unknown verdict \"" + verdict + "\"");
        }
    }

    private static String findingsText(List<VarroaMarkerValidator.Finding> findings) {
        StringBuilder sb = new StringBuilder();
        for (VarroaMarkerValidator.Finding f : findings) {
            if (sb.length() > 0) {
                sb.append("; ");
            }
            sb.append(f.toString());
        }
        return sb.toString();
    }

    @SuppressWarnings("unchecked")
    private static List<Map<String, Object>> asCaseList(Map<String, Object> manifest) {
        return (List<Map<String, Object>>) (List<?>) manifest.get("cases");
    }

    private static String str(Map<String, Object> m, String key) {
        Object v = m.get(key);
        return v != null ? String.valueOf(v) : "";
    }

    @SuppressWarnings("unchecked")
    private static Map<String, Object> loadYamlMap(String resourcePath) throws Exception {
        try (InputStream in = MarkerValidatorCorpusTest.class.getResourceAsStream(resourcePath)) {
            assertNotNull("missing corpus resource " + resourcePath, in);
            Object loaded = new Yaml().load(in);
            assertTrue(resourcePath + " must decode to a mapping", loaded instanceof Map);
            return (Map<String, Object>) loaded;
        }
    }
}
