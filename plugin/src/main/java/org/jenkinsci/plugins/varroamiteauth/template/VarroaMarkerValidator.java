package org.jenkinsci.plugins.varroamiteauth.template;

import java.io.ByteArrayInputStream;
import java.math.BigDecimal;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Collections;
import java.util.Iterator;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeSet;
import java.util.regex.Pattern;
import java.util.regex.PatternSyntaxException;

import org.yaml.snakeyaml.DumperOptions;
import org.yaml.snakeyaml.LoaderOptions;
import org.yaml.snakeyaml.Yaml;
import org.yaml.snakeyaml.constructor.SafeConstructor;
import org.yaml.snakeyaml.representer.Representer;

/**
 * The Java implementation of design D3's JSON Schema subset checker and
 * values validator, and of D6's marker YAML parsing (task 5.13). Mirrors
 * {@code internal/bundle/pipelinetemplate} so the plugin, the operator, and
 * the browser agree on every accepted schema and every accepted value
 * (D15's shared corpus proves this for the schema-subset and values
 * halves).
 *
 * <p>Not an extension point (D8): every method here is static.
 */
public final class VarroaMarkerValidator {

    private VarroaMarkerValidator() {
    }

    // ---- Finding ----

    /** One problem found while checking a schema or a values document. */
    public static final class Finding {
        private final String path;
        private final String message;

        public Finding(String path, String message) {
            this.path = path != null ? path : "";
            this.message = message;
        }

        public String getPath() {
            return path;
        }

        public String getMessage() {
            return message;
        }

        @Override
        public String toString() {
            return path.isEmpty() ? message : path + ": " + message;
        }
    }

    // ---- Marker YAML parsing (D6, task 5.13) ----

    /** The maximum size, in bytes, of a marker file, per {@code pipeline-marker-file}. */
    public static final int MAX_MARKER_BYTES = 64 * 1024;

    /** A marker or a template fragment failed to parse as one YAML mapping document. */
    public static final class MarkerFormatException extends Exception {
        public MarkerFormatException(String message) {
            super(message);
        }
    }

    /**
     * Parses {@code raw} as one YAML mapping document, using SnakeYAML's
     * safe constructor. Rejects a document over {@link #MAX_MARKER_BYTES}
     * bytes before parsing, and rejects more than one YAML document. Does
     * not interpret the document's keys; the caller applies the marker's
     * own shape rules.
     */
    public static Map<String, Object> parseMarkerYaml(byte[] raw) throws MarkerFormatException {
        if (raw.length > MAX_MARKER_BYTES) {
            throw new MarkerFormatException("marker exceeds " + MAX_MARKER_BYTES + " bytes");
        }
        LoaderOptions options = new LoaderOptions();
        options.setCodePointLimit(MAX_MARKER_BYTES);
        DumperOptions dumperOptions = new DumperOptions();
        Yaml yaml = new Yaml(new SafeConstructor(options), new Representer(dumperOptions), dumperOptions, options);
        try {
            Iterator<Object> docs = yaml.loadAll(new ByteArrayInputStream(raw)).iterator();
            if (!docs.hasNext()) {
                return new LinkedHashMap<>();
            }
            Object first = docs.next();
            if (docs.hasNext()) {
                throw new MarkerFormatException("decode marker: more than one YAML document");
            }
            if (first == null) {
                return new LinkedHashMap<>();
            }
            if (!(first instanceof Map)) {
                throw new MarkerFormatException("decode marker: top-level document must be a mapping");
            }
            @SuppressWarnings("unchecked")
            Map<String, Object> result = (Map<String, Object>) first;
            return result;
        } catch (MarkerFormatException e) {
            throw e;
        } catch (RuntimeException e) {
            throw new MarkerFormatException("decode marker: " + e.getMessage());
        }
    }

    /** The required {@code apiVersion} of a marker, per {@code pipeline-marker-file}. */
    public static final String MARKER_API_VERSION = "varroa.dev/v1";

    /** The required {@code kind} of a marker, per {@code pipeline-marker-file}. */
    public static final String MARKER_KIND = "Pipeline";

    private static final Set<String> MARKER_TOP_LEVEL_KEYS =
            Set.of("apiVersion", "kind", "template", "templateVersion", "values");

    /** The parsed content of a {@code .varroa/pipeline.yaml} marker. Mirrors {@code pipelinetemplate.Marker}. */
    public static final class Marker {
        private final String template;
        private final String templateVersion;
        private final Map<String, Object> values;

        Marker(String template, String templateVersion, Map<String, Object> values) {
            this.template = template;
            this.templateVersion = templateVersion;
            this.values = values;
        }

        public String getTemplate() {
            return template;
        }

        public String getTemplateVersion() {
            return templateVersion;
        }

        public Map<String, Object> getValues() {
            return values;
        }
    }

    /**
     * Parses {@code raw} as one marker document (D6, {@code
     * pipeline-marker-file}): {@link #parseMarkerYaml}, then the marker's
     * own shape rules. Rejects any top-level key other than {@code
     * apiVersion}, {@code kind}, {@code template}, {@code templateVersion},
     * and {@code values}; a wrong {@code apiVersion} or {@code kind}; a
     * missing or non-string {@code template}; and a {@code values} that is
     * not a mapping. Mirrors {@code pipelinetemplate.ParseMarker}.
     */
    public static Marker parseMarker(byte[] raw) throws MarkerFormatException {
        Map<String, Object> doc = parseMarkerYaml(raw);
        for (String key : doc.keySet()) {
            if (!MARKER_TOP_LEVEL_KEYS.contains(key)) {
                throw new MarkerFormatException("marker holds an unknown key \"" + key + "\"");
            }
        }
        Object apiVersion = doc.get("apiVersion");
        if (!MARKER_API_VERSION.equals(apiVersion)) {
            throw new MarkerFormatException(
                    "marker apiVersion must be \"" + MARKER_API_VERSION + "\", got " + describe(apiVersion));
        }
        Object kind = doc.get("kind");
        if (!MARKER_KIND.equals(kind)) {
            throw new MarkerFormatException("marker kind must be \"" + MARKER_KIND + "\", got " + describe(kind));
        }
        Object template = doc.get("template");
        if (!(template instanceof String) || ((String) template).isEmpty()) {
            throw new MarkerFormatException("marker template is required");
        }
        Object templateVersion = doc.get("templateVersion");
        if (templateVersion != null && !(templateVersion instanceof String)) {
            throw new MarkerFormatException("marker templateVersion must be a string");
        }
        Object values = doc.get("values");
        if (values != null && !(values instanceof Map)) {
            throw new MarkerFormatException("marker values must be a mapping");
        }
        @SuppressWarnings("unchecked")
        Map<String, Object> valuesMap = values != null ? (Map<String, Object>) values : new LinkedHashMap<>();
        return new Marker((String) template, templateVersion != null ? (String) templateVersion : "", valuesMap);
    }

    private static String describe(Object v) {
        return v == null ? "nothing" : "\"" + v + "\"";
    }

    // ---- Schema subset checking (D3; mirrors schema.go) ----

    private static final int MAX_SCHEMA_DEPTH = 3;
    private static final int MAX_TOTAL_PROPERTIES = 100;
    private static final int MAX_ENUM_ENTRIES = 100;
    private static final int MAX_SCHEMA_SIZE = 64 * 1024;
    private static final String SCHEMA_DIALECT = "https://json-schema.org/draft/2020-12/schema";

    private static final Pattern PROPERTY_NAME_PATTERN = Pattern.compile("^[a-zA-Z][a-zA-Z0-9_]{0,62}$");

    private static final Set<String> OBJECT_KEYS =
            Set.of("type", "title", "description", "properties", "required", "additionalProperties");
    private static final Set<String> STRING_KEYS =
            Set.of("type", "title", "description", "default", "enum", "minLength", "maxLength", "pattern");
    private static final Set<String> NUMERIC_KEYS = Set.of("type", "title", "description", "default", "enum",
            "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum");
    private static final Set<String> BOOLEAN_KEYS = Set.of("type", "title", "description", "default");
    private static final Set<String> ARRAY_KEYS = Set.of("type", "title", "description", "default", "items",
            "minItems", "maxItems", "uniqueItems");

    /**
     * Walks {@code schema} and returns every construct outside the fixed
     * JSON Schema subset from design D3. An empty result means schema is
     * entirely within the subset.
     */
    public static List<Finding> checkSchemaSubset(Map<String, Object> schema) {
        SchemaChecker c = new SchemaChecker();
        try {
            String dumped = new Yaml(new LoaderOptions()).dump(schema);
            if (dumped.getBytes(StandardCharsets.UTF_8).length > MAX_SCHEMA_SIZE) {
                c.addf("", "schema text exceeds " + MAX_SCHEMA_SIZE + " bytes");
            }
        } catch (RuntimeException e) {
            c.addf("", "schema text could not be measured: " + e.getMessage());
        }
        Object rootType = schema.get("type");
        if (!"object".equals(rootType)) {
            c.addf("", "root schema must declare type: object");
        }
        c.checkNode(schema, "", true, 1);
        if (c.propCount > MAX_TOTAL_PROPERTIES) {
            c.addf("", "schema declares more than " + MAX_TOTAL_PROPERTIES + " properties in total");
        }
        return c.findings;
    }

    /**
     * Runs every schema-level check from design D3 against {@code schema}:
     * the subset walk, and the rule that every {@code default} satisfies
     * its own schema. An empty result means {@code schema} is valid.
     */
    public static List<Finding> checkSchema(Map<String, Object> schema) {
        List<Finding> findings = new ArrayList<>(checkSchemaSubset(schema));
        checkDefaults(schema, "", findings);
        return findings;
    }

    private static final class SchemaChecker {
        final List<Finding> findings = new ArrayList<>();
        int propCount;

        void addf(String path, String message) {
            findings.add(new Finding(path, message));
        }

        /**
         * Rejects {@code key} when {@code node} carries it with a
         * non-numeric value, including an explicit {@code null}. Tests
         * presence with {@code containsKey}, not a null check on the
         * value, matching how {@code schema.go} tells "present" apart
         * from "absent".
         */
        void checkNumberKeyword(Map<String, Object> node, String key, String path) {
            if (!node.containsKey(key)) {
                return;
            }
            Object raw = node.get(key);
            if (!(raw instanceof Number)) {
                addf(path, key + " must be a number");
            }
        }

        /**
         * Rejects {@code key} when {@code node} carries it with a value that
         * is not a non-negative integer, matching Ajv's requirement for
         * {@code minLength}, {@code maxLength}, {@code minItems}, and
         * {@code maxItems}. An explicit {@code null} value is rejected the
         * same way, because presence is tested with {@code containsKey}.
         */
        void checkNonNegativeIntegerKeyword(Map<String, Object> node, String key, String path) {
            if (!node.containsKey(key)) {
                return;
            }
            Object raw = node.get(key);
            if (!isNonNegativeInteger(raw)) {
                addf(path, key + " must be a non-negative integer");
            }
        }

        /**
         * Rejects {@code key} when {@code node} carries it with a
         * non-boolean value, including an explicit {@code null}. Tests
         * presence with {@code containsKey}, not a null check on the
         * value.
         */
        void checkBooleanKeyword(Map<String, Object> node, String key, String path) {
            if (!node.containsKey(key)) {
                return;
            }
            Object raw = node.get(key);
            if (!(raw instanceof Boolean)) {
                addf(path, key + " must be a boolean");
            }
        }

        void rejectKeyword(String path, String key) {
            switch (key) {
                case "format":
                    addf(path, "keyword \"format\" is rejected because Ajv ignores an unregistered format "
                            + "while Go and the plugin would reject it");
                    break;
                case "multipleOf":
                    addf(path, "keyword \"multipleOf\" is rejected because Ajv applies multipleOfPrecision, "
                            + "which the other validators do not");
                    break;
                default:
                    addf(path, "keyword \"" + key + "\" is not in the allowed subset");
            }
        }

        void checkNode(Map<String, Object> node, String path, boolean isRoot, int depth) {
            if (!node.containsKey("type")) {
                addf(path, "schema must declare \"type\"");
                return;
            }
            Object rawType = node.get("type");
            if (rawType instanceof String) {
                switch ((String) rawType) {
                    case "object":
                        checkObject(node, path, isRoot, depth);
                        break;
                    case "string":
                        checkString(node, path);
                        break;
                    case "number":
                    case "integer":
                        checkNumeric(node, path, (String) rawType);
                        break;
                    case "boolean":
                        checkBoolean(node, path);
                        break;
                    case "array":
                        checkArray(node, path, depth);
                        break;
                    case "null":
                        addf(path, "type \"null\" is rejected");
                        break;
                    default:
                        addf(path, "unknown type \"" + rawType + "\"");
                }
            } else if (rawType instanceof List) {
                addf(path, "type written as an array is rejected");
            } else {
                addf(path, "\"type\" must be a string");
            }
        }

        void checkObject(Map<String, Object> node, String path, boolean isRoot, int depth) {
            if (depth > MAX_SCHEMA_DEPTH) {
                addf(path, "nesting depth exceeds the limit of " + MAX_SCHEMA_DEPTH);
            }
            for (String key : sortedKeys(node)) {
                if (OBJECT_KEYS.contains(key)) {
                    continue;
                }
                if (isRoot && "$schema".equals(key)) {
                    continue;
                }
                rejectKeyword(path, key);
            }
            if (isRoot && node.containsKey("$schema")) {
                Object raw = node.get("$schema");
                if (!(raw instanceof String) || !SCHEMA_DIALECT.equals(raw)) {
                    addf(path, "$schema must be \"" + SCHEMA_DIALECT + "\"");
                }
            }
            boolean hasAP = node.containsKey("additionalProperties");
            Object ap = node.get("additionalProperties");
            if (isRoot && !hasAP) {
                addf(path, "root schema must set additionalProperties: false");
            } else if (hasAP && (!(ap instanceof Boolean) || (Boolean) ap)) {
                addf(path, "additionalProperties must be false");
            }
            Object rawProps = node.get("properties");
            if (rawProps != null) {
                Map<String, Object> props = asMap(rawProps);
                if (props == null) {
                    addf(path, "properties must be an object");
                } else {
                    for (String name : sortedKeys(props)) {
                        String propPath = joinPath(path, name);
                        if (!PROPERTY_NAME_PATTERN.matcher(name).matches()) {
                            addf(propPath, "property name \"" + name + "\" must match " + PROPERTY_NAME_PATTERN.pattern());
                        }
                        propCount++;
                        Map<String, Object> sub = asMap(props.get(name));
                        if (sub == null) {
                            addf(propPath, "property schema must be an object");
                            continue;
                        }
                        checkNode(sub, propPath, false, depth + 1);
                    }
                }
            }
            if (node.containsKey("required")) {
                Object req = node.get("required");
                if (!(req instanceof List)) {
                    addf(path, "required must be a list of strings");
                } else {
                    for (Object item : (List<?>) req) {
                        if (!(item instanceof String)) {
                            addf(path, "required entries must be strings");
                        }
                    }
                }
            }
        }

        void checkString(Map<String, Object> node, String path) {
            for (String key : sortedKeys(node)) {
                if (!STRING_KEYS.contains(key)) {
                    rejectKeyword(path, key);
                }
            }
            checkNonNegativeIntegerKeyword(node, "minLength", path);
            checkNonNegativeIntegerKeyword(node, "maxLength", path);
            if (node.containsKey("enum")) {
                Object raw = node.get("enum");
                if (!(raw instanceof List)) {
                    addf(path, "enum must be a list");
                } else {
                    List<?> list = (List<?>) raw;
                    if (list.size() > MAX_ENUM_ENTRIES) {
                        addf(path, "enum holds more than " + MAX_ENUM_ENTRIES + " entries");
                    }
                    for (Object v : list) {
                        if (!(v instanceof String)) {
                            addf(path, "enum entries must be strings for a string property");
                        }
                    }
                }
            }
            if (node.containsKey("pattern")) {
                Object raw = node.get("pattern");
                if (!(raw instanceof String)) {
                    addf(path, "pattern must be a string");
                } else {
                    String ps = (String) raw;
                    List<String> msgs = checkPattern(ps);
                    for (String m : msgs) {
                        addf(path, "pattern: " + m);
                    }
                    if (msgs.isEmpty()) {
                        try {
                            Pattern.compile(normalizePattern(ps));
                        } catch (PatternSyntaxException e) {
                            addf(path, "pattern does not compile: " + e.getMessage());
                        }
                    }
                }
            }
        }

        void checkNumeric(Map<String, Object> node, String path, String type) {
            for (String key : sortedKeys(node)) {
                if (!NUMERIC_KEYS.contains(key)) {
                    rejectKeyword(path, key);
                }
            }
            checkNumberKeyword(node, "minimum", path);
            checkNumberKeyword(node, "maximum", path);
            checkNumberKeyword(node, "exclusiveMinimum", path);
            checkNumberKeyword(node, "exclusiveMaximum", path);
            if (node.containsKey("enum")) {
                Object raw = node.get("enum");
                if (!(raw instanceof List)) {
                    addf(path, "enum must be a list");
                } else {
                    List<?> list = (List<?>) raw;
                    if (list.size() > MAX_ENUM_ENTRIES) {
                        addf(path, "enum holds more than " + MAX_ENUM_ENTRIES + " entries");
                    }
                    for (Object v : list) {
                        if (!(v instanceof Number)) {
                            addf(path, "enum entries must be numbers for a " + type + " property");
                        }
                    }
                }
            }
        }

        void checkBoolean(Map<String, Object> node, String path) {
            for (String key : sortedKeys(node)) {
                if (!BOOLEAN_KEYS.contains(key)) {
                    rejectKeyword(path, key);
                }
            }
        }

        void checkArray(Map<String, Object> node, String path, int depth) {
            for (String key : sortedKeys(node)) {
                if (!ARRAY_KEYS.contains(key)) {
                    rejectKeyword(path, key);
                }
            }
            checkNonNegativeIntegerKeyword(node, "minItems", path);
            checkNonNegativeIntegerKeyword(node, "maxItems", path);
            checkBooleanKeyword(node, "uniqueItems", path);
            if (!node.containsKey("items")) {
                return;
            }
            Object itemsRaw = node.get("items");
            if (itemsRaw instanceof List) {
                addf(path, "items must be exactly one schema, not a tuple");
                return;
            }
            Map<String, Object> itemsSchema = asMap(itemsRaw);
            if (itemsSchema == null) {
                addf(path, "items must be a schema object");
                return;
            }
            checkNode(itemsSchema, path + "[]", false, depth + 1);
        }
    }

    /**
     * Recursively validates that every {@code default} in {@code schema}
     * satisfies the subschema it appears on, so a template cannot ship a
     * default its own consumers cannot use.
     */
    private static void checkDefaults(Map<String, Object> schema, String path, List<Finding> findings) {
        if (schema.containsKey("default")) {
            Object def = schema.get("default");
            List<Finding> subFindings = new ArrayList<>();
            validateNode(def, schema, "", subFindings);
            for (Finding f : subFindings) {
                findings.add(new Finding(path, "default does not satisfy its own schema: " + f.getMessage()));
            }
        }
        Map<String, Object> props = asMap(schema.get("properties"));
        if (props != null) {
            for (String name : sortedKeys(props)) {
                Map<String, Object> sub = asMap(props.get(name));
                if (sub != null) {
                    checkDefaults(sub, joinPath(path, name), findings);
                }
            }
        }
        Map<String, Object> items = asMap(schema.get("items"));
        if (items != null) {
            checkDefaults(items, path + "[]", findings);
        }
    }

    // ---- uiSchema subset checking (D4; mirrors uischema.go) ----

    private static final Set<String> ALLOWED_UI_WIDGETS =
            Set.of("textarea", "select", "radio", "checkbox", "updown");

    /**
     * Walks {@code uiSchema} against {@code schema} and returns every
     * construct outside the fixed uiSchema subset from design D4. A key
     * that is not {@code ui:}-prefixed must name a declared property of
     * the corresponding node in {@code schema}, mirroring {@code
     * properties}; every {@code ui:order} entry must likewise name a
     * declared property or be {@code "*"}. An empty result means {@code
     * uiSchema} is entirely within the subset.
     */
    public static List<Finding> checkUiSchema(Map<String, Object> uiSchema, Map<String, Object> schema) {
        List<Finding> findings = new ArrayList<>();
        checkUiNode(uiSchema, schema, "", findings);
        return findings;
    }

    private static void checkUiNode(Map<String, Object> node, Map<String, Object> schemaNode, String path,
            List<Finding> findings) {
        Map<String, Object> props = schemaNode != null ? asMap(schemaNode.get("properties")) : null;
        for (String key : sortedKeys(node)) {
            Object val = node.get(key);
            if ("ui:order".equals(key)) {
                checkUiOrder(val, props, path, findings);
                continue;
            }
            if (key.startsWith("ui:")) {
                checkUiKey(key, val, path, findings);
                continue;
            }
            if (props == null || !props.containsKey(key)) {
                findings.add(new Finding(joinPath(path, key),
                        "uiSchema key \"" + key + "\" names no declared property"));
                continue;
            }
            Map<String, Object> sub = asMap(val);
            if (sub == null) {
                findings.add(new Finding(joinPath(path, key), "a nested uiSchema entry must be an object"));
                continue;
            }
            Map<String, Object> subSchema = asMap(props.get(key));
            checkUiNode(sub, subSchema, joinPath(path, key), findings);
        }
    }

    private static void checkUiKey(String key, Object val, String path, List<Finding> findings) {
        switch (key) {
            case "ui:title":
            case "ui:description":
            case "ui:help":
            case "ui:placeholder":
                if (!(val instanceof String)) {
                    findings.add(new Finding(joinPath(path, key), key + " must be a string"));
                }
                break;
            case "ui:widget":
                checkUiWidget(val, joinPath(path, key), findings);
                break;
            case "ui:options":
                checkUiOptions(val, joinPath(path, key), findings);
                break;
            default:
                findings.add(new Finding(joinPath(path, key), "uiSchema key \"" + key + "\" is not in the allowed subset"));
        }
    }

    private static void checkUiWidget(Object val, String path, List<Finding> findings) {
        if (!(val instanceof String)) {
            findings.add(new Finding(path, "ui:widget must be a string"));
            return;
        }
        String s = (String) val;
        if ("password".equals(s)) {
            findings.add(new Finding(path, "ui:widget: password is rejected because a marker value holds no secret"));
            return;
        }
        if (!ALLOWED_UI_WIDGETS.contains(s)) {
            findings.add(new Finding(path, "ui:widget \"" + s + "\" is not in the allowed set"));
        }
    }

    private static void checkUiOptions(Object val, String path, List<Finding> findings) {
        Map<String, Object> opts = asMap(val);
        if (opts == null) {
            findings.add(new Finding(path, "ui:options must be an object"));
            return;
        }
        for (String k : sortedKeys(opts)) {
            if (!"rows".equals(k)) {
                findings.add(new Finding(path, "ui:options key \"" + k + "\" is not in the allowed subset"));
            }
        }
    }

    private static void checkUiOrder(Object val, Map<String, Object> props, String path, List<Finding> findings) {
        if (!(val instanceof List)) {
            findings.add(new Finding(joinPath(path, "ui:order"), "ui:order must be a list"));
            return;
        }
        Set<String> named = new java.util.HashSet<>();
        boolean wildcard = false;
        for (Object item : (List<?>) val) {
            if (!(item instanceof String)) {
                findings.add(new Finding(joinPath(path, "ui:order"), "ui:order entries must be strings"));
                continue;
            }
            String s = (String) item;
            if ("*".equals(s)) {
                wildcard = true;
                continue;
            }
            if (props == null || !props.containsKey(s)) {
                findings.add(new Finding(joinPath(path, "ui:order"),
                        "ui:order entry \"" + s + "\" names no declared property"));
            }
            named.add(s);
        }
        if (wildcard) {
            return;
        }
        if (props != null) {
            for (String name : sortedKeys(props)) {
                if (!named.contains(name)) {
                    findings.add(new Finding(joinPath(path, "ui:order"),
                            "ui:order omits declared property \"" + name + "\" and carries no \"*\""));
                }
            }
        }
    }

    // ---- Values validation (mirrors values.go) ----

    /** The outcome of {@link #validateValues}: the defaulted values and every finding. */
    public static final class ValuesResult {
        private final Map<String, Object> defaulted;
        private final List<Finding> findings;

        ValuesResult(Map<String, Object> defaulted, List<Finding> findings) {
            this.defaulted = defaulted;
            this.findings = findings;
        }

        public Map<String, Object> getDefaulted() {
            return defaulted;
        }

        public List<Finding> getFindings() {
            return findings;
        }
    }

    /**
     * Returns a copy of {@code values} with every schema-declared default
     * filled in for a property {@code values} omits. Descends into a
     * nested object property's own defaults. Does not validate {@code
     * values}.
     */
    public static Map<String, Object> applyDefaults(Map<String, Object> schema, Map<String, Object> values) {
        Map<String, Object> out = new LinkedHashMap<>(values != null ? values : Collections.emptyMap());
        Map<String, Object> props = asMap(schema.get("properties"));
        if (props == null) {
            return out;
        }
        for (Map.Entry<String, Object> e : props.entrySet()) {
            String name = e.getKey();
            Map<String, Object> sub = asMap(e.getValue());
            if (sub == null) {
                continue;
            }
            if (!out.containsKey(name) && sub.containsKey("default")) {
                out.put(name, sub.get("default"));
            }
            Object cur = out.get(name);
            if (cur instanceof Map && "object".equals(sub.get("type"))) {
                @SuppressWarnings("unchecked")
                Map<String, Object> curMap = (Map<String, Object>) cur;
                out.put(name, applyDefaults(sub, curMap));
            }
        }
        return out;
    }

    /**
     * Applies {@code schema}'s defaults to {@code values}, then validates
     * the defaulted map against {@code schema}. An empty finding list
     * means {@code values} is valid.
     */
    public static ValuesResult validateValues(Map<String, Object> schema, Map<String, Object> values) {
        Map<String, Object> defaulted = applyDefaults(schema, values);
        List<Finding> findings = new ArrayList<>();
        validateNode(defaulted, schema, "", findings);
        return new ValuesResult(defaulted, findings);
    }

    private static void validateNode(Object instance, Map<String, Object> schema, String path, List<Finding> findings) {
        Object rawType = schema.get("type");
        if (!(rawType instanceof String)) {
            return;
        }
        switch ((String) rawType) {
            case "object":
                validateObject(instance, schema, path, findings);
                break;
            case "string":
                validateString(instance, schema, path, findings);
                break;
            case "number":
            case "integer":
                validateNumeric(instance, schema, path, (String) rawType, findings);
                break;
            case "boolean":
                validateBoolean(instance, path, findings);
                break;
            case "array":
                validateArray(instance, schema, path, findings);
                break;
            default:
                // An out-of-subset type never reaches here: CheckSchemaSubset
                // already rejects the document before values are validated.
        }
    }

    private static void validateObject(Object instance, Map<String, Object> schema, String path, List<Finding> findings) {
        if (!(instance instanceof Map)) {
            findings.add(new Finding(path, "got " + kindOf(instance) + ", want object"));
            return;
        }
        @SuppressWarnings("unchecked")
        Map<String, Object> obj = (Map<String, Object>) instance;
        Map<String, Object> props = asMap(schema.get("properties"));
        if (props != null) {
            for (Map.Entry<String, Object> e : props.entrySet()) {
                String name = e.getKey();
                if (obj.containsKey(name)) {
                    Map<String, Object> sub = asMap(e.getValue());
                    if (sub != null) {
                        validateNode(obj.get(name), sub, joinPath(path, name), findings);
                    }
                }
            }
        }
        Object rawRequired = schema.get("required");
        if (rawRequired instanceof List) {
            for (Object r : (List<?>) rawRequired) {
                String name = String.valueOf(r);
                if (!obj.containsKey(name)) {
                    findings.add(new Finding(path, "missing property \"" + name + "\""));
                }
            }
        }
        if (Boolean.FALSE.equals(schema.get("additionalProperties"))) {
            for (Object rawKey : obj.keySet()) {
                String key = String.valueOf(rawKey);
                if (props == null || !props.containsKey(key)) {
                    findings.add(new Finding(path, "additional property \"" + key + "\" is not allowed"));
                }
            }
        }
    }

    private static void validateString(Object instance, Map<String, Object> schema, String path, List<Finding> findings) {
        if (!(instance instanceof String)) {
            findings.add(new Finding(path, "got " + kindOf(instance) + ", want string"));
            return;
        }
        String s = (String) instance;
        Object enumVal = schema.get("enum");
        if (enumVal instanceof List) {
            boolean match = false;
            for (Object v : (List<?>) enumVal) {
                if (s.equals(v)) {
                    match = true;
                    break;
                }
            }
            if (!match) {
                findings.add(new Finding(path, "value must be one of " + enumText((List<?>) enumVal)));
            }
        }
        Object minLen = schema.get("minLength");
        if (minLen instanceof Number && s.codePointCount(0, s.length()) < ((Number) minLen).intValue()) {
            findings.add(new Finding(path, "length is less than the minimum of " + numberText((Number) minLen)));
        }
        Object maxLen = schema.get("maxLength");
        if (maxLen instanceof Number && s.codePointCount(0, s.length()) > ((Number) maxLen).intValue()) {
            findings.add(new Finding(path, "length exceeds the maximum of " + numberText((Number) maxLen)));
        }
        Object pattern = schema.get("pattern");
        if (pattern instanceof String) {
            String normalized = normalizePattern((String) pattern);
            try {
                if (!Pattern.compile(normalized).matcher(s).find()) {
                    findings.add(new Finding(path, "value does not match pattern \"" + pattern + "\""));
                }
            } catch (PatternSyntaxException e) {
                findings.add(new Finding(path, "pattern does not compile: " + e.getMessage()));
            }
        }
    }

    private static void validateNumeric(Object instance, Map<String, Object> schema, String path, String type,
            List<Finding> findings) {
        if (!(instance instanceof Number)) {
            findings.add(new Finding(path, "got " + kindOf(instance) + ", want " + type));
            return;
        }
        double d = ((Number) instance).doubleValue();
        if ("integer".equals(type) && d != Math.floor(d)) {
            findings.add(new Finding(path, "got number, want integer"));
            return;
        }
        Object enumVal = schema.get("enum");
        if (enumVal instanceof List) {
            boolean match = false;
            for (Object v : (List<?>) enumVal) {
                if (v instanceof Number && ((Number) v).doubleValue() == d) {
                    match = true;
                    break;
                }
            }
            if (!match) {
                findings.add(new Finding(path, "value must be one of " + enumText((List<?>) enumVal)));
            }
        }
        checkBound(schema, "minimum", d, path, findings, false, true);
        checkBound(schema, "maximum", d, path, findings, true, true);
        checkBound(schema, "exclusiveMinimum", d, path, findings, false, false);
        checkBound(schema, "exclusiveMaximum", d, path, findings, true, false);
    }

    private static void checkBound(Map<String, Object> schema, String key, double d, String path,
            List<Finding> findings, boolean isUpper, boolean inclusive) {
        Object raw = schema.get(key);
        if (!(raw instanceof Number)) {
            return;
        }
        double bound = ((Number) raw).doubleValue();
        boolean ok = isUpper
                ? (inclusive ? d <= bound : d < bound)
                : (inclusive ? d >= bound : d > bound);
        if (!ok) {
            findings.add(new Finding(path, "value violates " + key + " " + numberText((Number) raw)));
        }
    }

    private static void validateBoolean(Object instance, String path, List<Finding> findings) {
        if (!(instance instanceof Boolean)) {
            findings.add(new Finding(path, "got " + kindOf(instance) + ", want boolean"));
        }
    }

    private static void validateArray(Object instance, Map<String, Object> schema, String path, List<Finding> findings) {
        if (!(instance instanceof List)) {
            findings.add(new Finding(path, "got " + kindOf(instance) + ", want array"));
            return;
        }
        List<?> list = (List<?>) instance;
        Object minItems = schema.get("minItems");
        if (minItems instanceof Number && list.size() < ((Number) minItems).intValue()) {
            findings.add(new Finding(path, "holds fewer than the minimum of " + numberText((Number) minItems) + " items"));
        }
        Object maxItems = schema.get("maxItems");
        if (maxItems instanceof Number && list.size() > ((Number) maxItems).intValue()) {
            findings.add(new Finding(path, "holds more than the maximum of " + numberText((Number) maxItems) + " items"));
        }
        if (Boolean.TRUE.equals(schema.get("uniqueItems"))) {
            Set<Object> seen = new java.util.HashSet<>();
            for (Object item : list) {
                if (!seen.add(item)) {
                    findings.add(new Finding(path, "items must be unique"));
                    break;
                }
            }
        }
        Map<String, Object> itemsSchema = asMap(schema.get("items"));
        if (itemsSchema != null) {
            for (int i = 0; i < list.size(); i++) {
                validateNode(list.get(i), itemsSchema, path + "[" + i + "]", findings);
            }
        }
    }

    private static String kindOf(Object v) {
        if (v == null) {
            return "null";
        }
        if (v instanceof String) {
            return "string";
        }
        if (v instanceof Boolean) {
            return "boolean";
        }
        if (v instanceof Number) {
            return "number";
        }
        if (v instanceof List) {
            return "array";
        }
        if (v instanceof Map) {
            return "object";
        }
        return v.getClass().getSimpleName();
    }

    // ---- Pattern checking (D3; mirrors pattern.go) ----

    private static final int MAX_PATTERN_LENGTH = 200;
    private static final int MAX_REPEAT_BOUND = 1000;
    private static final Pattern REPEAT_BODY_PATTERN = Pattern.compile("^[0-9]+(,[0-9]*)?$");

    /**
     * Returns every reason {@code pattern} falls outside the portable
     * construct whitelist from design D3. An empty result means {@code
     * pattern} is accepted.
     */
    public static List<String> checkPattern(String pattern) {
        List<String> findings = new ArrayList<>();
        if (pattern.codePointCount(0, pattern.length()) > MAX_PATTERN_LENGTH) {
            findings.add("pattern exceeds the " + MAX_PATTERN_LENGTH + " character limit");
        }
        findings.addAll(scanPattern(pattern).findings);
        return findings;
    }

    /**
     * Rewrites every unescaped {@code $} outside a character class to
     * {@code \z}, so the pattern means the end of the input in every
     * engine. The caller must have already accepted {@code pattern} with
     * {@link #checkPattern}.
     */
    public static String normalizePattern(String pattern) {
        return scanPattern(pattern).normalized;
    }

    private static final class ScanResult {
        final String normalized;
        final List<String> findings;

        ScanResult(String normalized, List<String> findings) {
            this.normalized = normalized;
            this.findings = findings;
        }
    }

    private static boolean isClassEscape(int esc) {
        switch (esc) {
            case 'd':
            case 'D':
            case 'w':
            case 'W':
            case '.':
            case '^':
            case '$':
            case '|':
            case '?':
            case '*':
            case '+':
            case '(':
            case ')':
            case '[':
            case ']':
            case '{':
            case '}':
            case '/':
            case '\\':
            case '-':
                return true;
            default:
                return false;
        }
    }

    private static void checkQuantifierContext(String lastAtom, List<String> findings) {
        switch (lastAtom) {
            case "":
                findings.add("quantifier has nothing to quantify");
                break;
            case "group":
                findings.add("a quantifier may not follow a closing parenthesis");
                break;
            case "quantified":
                findings.add("a quantifier may not follow another quantifier");
                break;
            default:
                // "atom": an ordinary atom may be quantified.
        }
    }

    private static void checkRepeatBody(String body, List<String> findings) {
        if (!REPEAT_BODY_PATTERN.matcher(body).matches()) {
            findings.add("repetition {" + body + "} is not m, m,, or m,n");
            return;
        }
        String[] parts = body.split(",", 2);
        int m;
        try {
            m = Integer.parseInt(parts[0]);
        } catch (NumberFormatException e) {
            findings.add("repetition {" + body + "} does not parse");
            return;
        }
        if (m > MAX_REPEAT_BOUND) {
            findings.add("repetition exceeds the " + MAX_REPEAT_BOUND + " limit");
        }
        if (parts.length == 2 && !parts[1].isEmpty()) {
            int n;
            try {
                n = Integer.parseInt(parts[1]);
            } catch (NumberFormatException e) {
                findings.add("repetition {" + body + "} does not parse");
                return;
            }
            if (n > MAX_REPEAT_BOUND) {
                findings.add("repetition exceeds the " + MAX_REPEAT_BOUND + " limit");
            }
            if (n < m) {
                findings.add("repetition upper bound is less than the lower bound in {" + body + "}");
            }
        }
    }

    /**
     * Walks {@code pattern} left to right once, in lockstep producing the
     * normalized form (every unescaped, unclassed {@code $} rewritten to
     * {@code \z}) and every reason {@code pattern} falls outside the
     * whitelist.
     */
    private static ScanResult scanPattern(String pattern) {
        List<String> findings = new ArrayList<>();
        StringBuilder out = new StringBuilder();
        int[] runes = pattern.codePoints().toArray();
        int n = runes.length;
        int i = 0;
        boolean inClass = false;
        int groupDepth = 0;
        String lastAtom = "";

        while (i < n) {
            int ch = runes[i];
            if (inClass) {
                if (ch == '\\') {
                    if (i + 1 >= n) {
                        findings.add("trailing backslash has nothing to escape");
                        i++;
                        continue;
                    }
                    int esc = runes[i + 1];
                    if (!isClassEscape(esc)) {
                        findings.add("escape \\" + new String(Character.toChars(esc)) + " is not in the allowed set");
                    }
                    out.appendCodePoint(ch);
                    out.appendCodePoint(esc);
                    i += 2;
                } else if (ch == ']') {
                    inClass = false;
                    out.appendCodePoint(ch);
                    i++;
                    lastAtom = "atom";
                } else {
                    out.appendCodePoint(ch);
                    i++;
                }
                continue;
            }
            switch (ch) {
                case '\\': {
                    if (i + 1 >= n) {
                        findings.add("trailing backslash has nothing to escape");
                        i++;
                        break;
                    }
                    int esc = runes[i + 1];
                    switch (esc) {
                        case 's':
                        case 'S':
                            findings.add("escape \\" + (char) esc
                                    + " is rejected because RE2, java.util.regex, and ECMA-262 disagree on it");
                            break;
                        case 'b':
                            findings.add("escape \\b is not in the allowed set");
                            break;
                        case '-':
                            findings.add("escape \\- is allowed only inside a character class");
                            break;
                        case 'd':
                        case 'D':
                        case 'w':
                        case 'W':
                        case '.':
                        case '^':
                        case '$':
                        case '|':
                        case '?':
                        case '*':
                        case '+':
                        case '(':
                        case ')':
                        case '[':
                        case ']':
                        case '{':
                        case '}':
                        case '/':
                        case '\\':
                            break;
                        default:
                            findings.add("escape \\" + new String(Character.toChars(esc)) + " is not in the allowed set");
                    }
                    out.appendCodePoint(ch);
                    out.appendCodePoint(esc);
                    i += 2;
                    lastAtom = "atom";
                    break;
                }
                case '[':
                    inClass = true;
                    out.appendCodePoint(ch);
                    i++;
                    if (i < n && runes[i] == '^') {
                        out.appendCodePoint('^');
                        i++;
                    }
                    break;
                case ']':
                    findings.add("unmatched \"]\" outside a character class");
                    out.appendCodePoint(ch);
                    i++;
                    break;
                case '(': {
                    groupDepth++;
                    out.appendCodePoint(ch);
                    i++;
                    if (i + 1 < n && runes[i] == '?' && runes[i + 1] == ':') {
                        out.appendCodePoint('?');
                        out.appendCodePoint(':');
                        i += 2;
                    } else if (i < n && runes[i] == '?') {
                        int next = (i + 1 < n) ? runes[i + 1] : 0;
                        if (next == '=' || next == '!') {
                            findings.add("lookahead is not in the allowed set");
                        } else if (next == '<') {
                            findings.add("lookbehind or a named group is not in the allowed set");
                        } else {
                            findings.add("only a non-capturing group \"(?:...)\" may start with \"?\"");
                        }
                        out.appendCodePoint('?');
                        i++;
                    }
                    lastAtom = "";
                    break;
                }
                case ')':
                    if (groupDepth == 0) {
                        findings.add("unmatched \")\"");
                    } else {
                        groupDepth--;
                    }
                    out.appendCodePoint(ch);
                    i++;
                    lastAtom = "group";
                    break;
                case '|':
                    out.appendCodePoint(ch);
                    i++;
                    lastAtom = "";
                    break;
                case '^':
                    out.appendCodePoint(ch);
                    i++;
                    lastAtom = "atom";
                    break;
                case '$':
                    out.append("\\z");
                    i++;
                    lastAtom = "atom";
                    break;
                case '*':
                case '+':
                case '?':
                    checkQuantifierContext(lastAtom, findings);
                    out.appendCodePoint(ch);
                    i++;
                    lastAtom = "quantified";
                    break;
                case '{': {
                    int j = i + 1;
                    while (j < n && runes[j] != '}') {
                        j++;
                    }
                    if (j >= n) {
                        findings.add("unterminated repetition");
                        out.appendCodePoint(ch);
                        i++;
                        break;
                    }
                    StringBuilder bodyBuilder = new StringBuilder();
                    for (int k = i + 1; k < j; k++) {
                        bodyBuilder.appendCodePoint(runes[k]);
                    }
                    String body = bodyBuilder.toString();
                    checkRepeatBody(body, findings);
                    checkQuantifierContext(lastAtom, findings);
                    out.append('{').append(body).append('}');
                    i = j + 1;
                    lastAtom = "quantified";
                    break;
                }
                default:
                    out.appendCodePoint(ch);
                    i++;
                    lastAtom = "atom";
            }
        }
        if (inClass) {
            findings.add("unterminated character class");
        }
        if (groupDepth != 0) {
            findings.add("unbalanced parentheses");
        }
        return new ScanResult(out.toString(), findings);
    }

    // ---- shared helpers ----

    @SuppressWarnings("unchecked")
    private static Map<String, Object> asMap(Object o) {
        return (o instanceof Map) ? (Map<String, Object>) o : null;
    }

    /**
     * Renders a list the way {@code java.util.AbstractCollection#toString}
     * renders it, but replaces each {@code Number} element's own
     * {@code toString} with {@link #numberText(Number)} so an integral
     * value never carries a trailing {@code .0}. Mirrors
     * {@code javaListLiteral} in validate.go, which the enum finding uses
     * on the Go side.
     */
    private static String enumText(List<?> items) {
        StringBuilder out = new StringBuilder("[");
        for (int i = 0; i < items.size(); i++) {
            if (i > 0) {
                out.append(", ");
            }
            Object v = items.get(i);
            out.append(v instanceof Number ? numberText((Number) v) : String.valueOf(v));
        }
        return out.append("]").toString();
    }

    /**
     * Renders a schema-declared number the way {@code javaRatLiteral} in
     * validate.go renders the same value: a whole number with no
     * fractional part, else its shortest decimal form, never in
     * scientific notation. Every values message that embeds a keyword
     * value ({@code minLength}, {@code maxLength}, {@code minItems},
     * {@code maxItems}, {@code minimum}, {@code maximum},
     * {@code exclusiveMinimum}, {@code exclusiveMaximum}) goes through
     * this one helper.
     */
    private static String numberText(Number n) {
        if (n instanceof Integer || n instanceof Long) {
            return n.toString();
        }
        double d = n.doubleValue();
        if (Double.isInfinite(d) || Double.isNaN(d)) {
            return Double.toString(d);
        }
        // BigDecimal keeps every digit of an integral value past the long
        // range, where a long cast would saturate.
        return new BigDecimal(Double.toString(d)).stripTrailingZeros().toPlainString();
    }

    /** Reports whether {@code v}, decoded from YAML, holds an integer value that is zero or greater. */
    private static boolean isNonNegativeInteger(Object v) {
        if (v instanceof Integer) {
            return (Integer) v >= 0;
        }
        if (v instanceof Long) {
            return (Long) v >= 0;
        }
        if (v instanceof Double) {
            double d = (Double) v;
            return !Double.isInfinite(d) && d >= 0 && d == Math.floor(d);
        }
        return false;
    }

    private static String joinPath(String base, String seg) {
        return base.isEmpty() ? seg : base + "." + seg;
    }

    private static List<String> sortedKeys(Map<String, Object> m) {
        return new ArrayList<>(new TreeSet<>(m.keySet()));
    }
}
