package org.jenkinsci.plugins.varroamiteauth.template;

import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import java.util.List;
import java.util.Map;

import org.junit.Test;
import org.yaml.snakeyaml.Yaml;

/**
 * Direct unit tests for {@link VarroaMarkerValidator} rules that the
 * shared corpus (task 1.8, {@link MarkerValidatorCorpusTest}) does not
 * yet cover: the uiSchema subset (design D4) and the value-type checks on
 * an accepted schema keyword (design D3). The coordinator re-copies the
 * canonical corpus after both the Go and the plugin batch land and
 * re-runs the corpus test; these tests stand on their own until then.
 */
public class VarroaMarkerValidatorTest {

    private static Map<String, Object> yamlMap(String text) {
        Object loaded = new Yaml().load(text);
        assertTrue("fixture must decode to a mapping", loaded instanceof Map);
        @SuppressWarnings("unchecked")
        Map<String, Object> result = (Map<String, Object>) loaded;
        return result;
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

    // ---- uiSchema subset (D4) ----

    private static final String SCHEMA_WITH_TWO_PROPERTIES =
            "type: object\n"
            + "additionalProperties: false\n"
            + "properties:\n"
            + "  goVersion:\n"
            + "    type: string\n"
            + "  publishImage:\n"
            + "    type: boolean\n";

    @Test
    public void aUiSchemaWithinTheSubsetIsAccepted() {
        Map<String, Object> schema = yamlMap(SCHEMA_WITH_TWO_PROPERTIES);
        Map<String, Object> uiSchema = yamlMap(
                "ui:order:\n"
                + "  - goVersion\n"
                + "  - publishImage\n"
                + "goVersion:\n"
                + "  ui:title: Go version\n"
                + "  ui:widget: select\n"
                + "publishImage:\n"
                + "  ui:widget: checkbox\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkUiSchema(uiSchema, schema);

        assertTrue(findingsText(findings), findings.isEmpty());
    }

    @Test
    public void aUiSchemaKeyThatIsNotUiPrefixedMustNameADeclaredProperty() {
        Map<String, Object> schema = yamlMap(SCHEMA_WITH_TWO_PROPERTIES);
        Map<String, Object> uiSchema = yamlMap(
                "notAParameter:\n"
                + "  ui:title: Hidden\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkUiSchema(uiSchema, schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("names no declared property"));
    }

    @Test
    public void uiOrderNamingAnUndeclaredPropertyIsRejected() {
        Map<String, Object> schema = yamlMap(SCHEMA_WITH_TWO_PROPERTIES);
        Map<String, Object> uiSchema = yamlMap(
                "ui:order:\n"
                + "  - goVersion\n"
                + "  - publishImage\n"
                + "  - notAParameter\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkUiSchema(uiSchema, schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("names no declared property"));
    }

    @Test
    public void uiOrderOmittingADeclaredPropertyWithNoWildcardIsRejected() {
        Map<String, Object> schema = yamlMap(SCHEMA_WITH_TWO_PROPERTIES);
        Map<String, Object> uiSchema = yamlMap(
                "ui:order:\n"
                + "  - goVersion\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkUiSchema(uiSchema, schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("omits declared property"));
    }

    @Test
    public void uiOrderWithAWildcardAcceptsAPartialList() {
        Map<String, Object> schema = yamlMap(SCHEMA_WITH_TWO_PROPERTIES);
        Map<String, Object> uiSchema = yamlMap(
                "ui:order:\n"
                + "  - goVersion\n"
                + "  - \"*\"\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkUiSchema(uiSchema, schema);

        assertTrue(findingsText(findings), findings.isEmpty());
    }

    @Test
    public void uiWidgetPasswordIsRejected() {
        Map<String, Object> schema = yamlMap(SCHEMA_WITH_TWO_PROPERTIES);
        Map<String, Object> uiSchema = yamlMap(
                "goVersion:\n"
                + "  ui:widget: password\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkUiSchema(uiSchema, schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("holds no secret"));
    }

    @Test
    public void uiWidgetOutsideTheAllowedSetIsRejected() {
        Map<String, Object> schema = yamlMap(SCHEMA_WITH_TWO_PROPERTIES);
        Map<String, Object> uiSchema = yamlMap(
                "goVersion:\n"
                + "  ui:widget: emoji-picker\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkUiSchema(uiSchema, schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("not in the allowed set"));
    }

    @Test
    public void uiOptionsWithAnUnknownKeyIsRejected() {
        Map<String, Object> schema = yamlMap(SCHEMA_WITH_TWO_PROPERTIES);
        Map<String, Object> uiSchema = yamlMap(
                "goVersion:\n"
                + "  ui:options:\n"
                + "    cols: 5\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkUiSchema(uiSchema, schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("ui:options key \"cols\""));
    }

    @Test
    public void anUnregisteredUiKeyIsRejected() {
        Map<String, Object> schema = yamlMap(SCHEMA_WITH_TWO_PROPERTIES);
        Map<String, Object> uiSchema = yamlMap(
                "goVersion:\n"
                + "  ui:field: CustomField\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkUiSchema(uiSchema, schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("ui:field"));
    }

    // ---- Schema keyword value-type validation (D3) ----

    @Test
    public void aValidSchemaWithEveryKeywordTypeCorrectIsAccepted() {
        Map<String, Object> schema = yamlMap(
                "type: object\n"
                + "additionalProperties: false\n"
                + "properties:\n"
                + "  name:\n"
                + "    type: string\n"
                + "    minLength: 1\n"
                + "    maxLength: 50\n"
                + "  count:\n"
                + "    type: integer\n"
                + "    minimum: 0\n"
                + "    maximum: 10\n"
                + "  tags:\n"
                + "    type: array\n"
                + "    minItems: 0\n"
                + "    maxItems: 5\n"
                + "    uniqueItems: true\n"
                + "    items:\n"
                + "      type: string\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkSchema(schema);

        assertTrue(findingsText(findings), findings.isEmpty());
    }

    @Test
    public void aStringMinLengthOfTheWrongTypeIsRejected() {
        Map<String, Object> schema = yamlMap(
                "type: object\n"
                + "additionalProperties: false\n"
                + "properties:\n"
                + "  name:\n"
                + "    type: string\n"
                + "    minLength: invalid\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkSchema(schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("minLength must be a non-negative integer"));
    }

    @Test
    public void aStringMaxLengthOfTheWrongTypeIsRejected() {
        Map<String, Object> schema = yamlMap(
                "type: object\n"
                + "additionalProperties: false\n"
                + "properties:\n"
                + "  name:\n"
                + "    type: string\n"
                + "    maxLength: \"ten\"\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkSchema(schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("maxLength must be a non-negative integer"));
    }

    @Test
    public void aStringMinLengthThatIsNegativeIsRejected() {
        Map<String, Object> schema = yamlMap(
                "type: object\n"
                + "additionalProperties: false\n"
                + "properties:\n"
                + "  name:\n"
                + "    type: string\n"
                + "    minLength: -1\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkSchema(schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("minLength must be a non-negative integer"));
    }

    @Test
    public void aNumericMinimumOfTheWrongTypeIsRejected() {
        Map<String, Object> schema = yamlMap(
                "type: object\n"
                + "additionalProperties: false\n"
                + "properties:\n"
                + "  count:\n"
                + "    type: integer\n"
                + "    minimum: nope\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkSchema(schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("minimum must be a number"));
    }

    @Test
    public void aNumericMaximumOfTheWrongTypeIsRejected() {
        Map<String, Object> schema = yamlMap(
                "type: object\n"
                + "additionalProperties: false\n"
                + "properties:\n"
                + "  count:\n"
                + "    type: number\n"
                + "    maximum: [1, 2]\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkSchema(schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("maximum must be a number"));
    }

    @Test
    public void anArrayMinItemsOfTheWrongTypeIsRejected() {
        Map<String, Object> schema = yamlMap(
                "type: object\n"
                + "additionalProperties: false\n"
                + "properties:\n"
                + "  tags:\n"
                + "    type: array\n"
                + "    minItems: \"zero\"\n"
                + "    items:\n"
                + "      type: string\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkSchema(schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("minItems must be a non-negative integer"));
    }

    @Test
    public void anArrayMaxItemsOfTheWrongTypeIsRejected() {
        Map<String, Object> schema = yamlMap(
                "type: object\n"
                + "additionalProperties: false\n"
                + "properties:\n"
                + "  tags:\n"
                + "    type: array\n"
                + "    maxItems: true\n"
                + "    items:\n"
                + "      type: string\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkSchema(schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("maxItems must be a non-negative integer"));
    }

    @Test
    public void anArrayUniqueItemsOfTheWrongTypeIsRejected() {
        Map<String, Object> schema = yamlMap(
                "type: object\n"
                + "additionalProperties: false\n"
                + "properties:\n"
                + "  tags:\n"
                + "    type: array\n"
                + "    uniqueItems: \"yes\"\n"
                + "    items:\n"
                + "      type: string\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkSchema(schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("uniqueItems must be a boolean"));
    }

    @Test
    public void anExclusiveMinimumOfTheWrongTypeIsRejected() {
        Map<String, Object> schema = yamlMap(
                "type: object\n"
                + "additionalProperties: false\n"
                + "properties:\n"
                + "  count:\n"
                + "    type: number\n"
                + "    exclusiveMinimum: \"low\"\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkSchema(schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("exclusiveMinimum must be a number"));
    }

    @Test
    public void aNumericMinimumThatIsExplicitlyNullIsRejected() {
        Map<String, Object> schema = yamlMap(
                "type: object\n"
                + "additionalProperties: false\n"
                + "properties:\n"
                + "  count:\n"
                + "    type: number\n"
                + "    minimum: null\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkSchema(schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("minimum must be a number"));
    }

    @Test
    public void aStringMinLengthThatIsExplicitlyNullIsRejected() {
        Map<String, Object> schema = yamlMap(
                "type: object\n"
                + "additionalProperties: false\n"
                + "properties:\n"
                + "  name:\n"
                + "    type: string\n"
                + "    minLength: null\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkSchema(schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("minLength must be a non-negative integer"));
    }

    @Test
    public void anArrayUniqueItemsThatIsExplicitlyNullIsRejected() {
        Map<String, Object> schema = yamlMap(
                "type: object\n"
                + "additionalProperties: false\n"
                + "properties:\n"
                + "  tags:\n"
                + "    type: array\n"
                + "    uniqueItems: null\n"
                + "    items:\n"
                + "      type: string\n");

        List<VarroaMarkerValidator.Finding> findings = VarroaMarkerValidator.checkSchema(schema);

        assertFalse("expected a rejection", findings.isEmpty());
        assertTrue(findingsText(findings), findingsText(findings).contains("uniqueItems must be a boolean"));
    }
}
