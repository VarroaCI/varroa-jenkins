package pipelinetemplate

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

// Finding is one problem found while checking a PipelineTemplate document,
// its parameter schema, or its uiSchema. Path is a dotted, JSON-Pointer-like
// location; it is empty when the problem is about the whole node.
type Finding struct {
	Path    string
	Message string
}

// String renders f for a console or a log line.
func (f Finding) String() string {
	if f.Path == "" {
		return f.Message
	}
	return f.Path + ": " + f.Message
}

const (
	// maxSchemaDepth is the maximum nesting depth of an object schema, root
	// counted as depth 1.
	maxSchemaDepth = 3
	// maxTotalProperties is the maximum number of properties a schema may
	// declare, summed across every nested object.
	maxTotalProperties = 100
	// maxEnumEntries is the maximum number of entries in one enum.
	maxEnumEntries = 100
	// maxSchemaSize is the maximum size, in bytes, of the schema text.
	maxSchemaSize = 64 * 1024
	// schemaDialect is the only accepted value of the root $schema keyword.
	schemaDialect = "https://json-schema.org/draft/2020-12/schema"
)

// propertyNamePattern keeps every property reachable as
// varroaParams.<name> in Groovy and avoids the RJSF error-path defect on a
// key holding ".", "/", or "~".
var propertyNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,62}$`)

var objectKeys = map[string]bool{
	"type": true, "title": true, "description": true,
	"properties": true, "required": true, "additionalProperties": true,
}

var stringKeys = map[string]bool{
	"type": true, "title": true, "description": true, "default": true,
	"enum": true, "minLength": true, "maxLength": true, "pattern": true,
}

var numericKeys = map[string]bool{
	"type": true, "title": true, "description": true, "default": true,
	"enum": true, "minimum": true, "maximum": true,
	"exclusiveMinimum": true, "exclusiveMaximum": true,
}

var booleanKeys = map[string]bool{
	"type": true, "title": true, "description": true, "default": true,
}

var arrayKeys = map[string]bool{
	"type": true, "title": true, "description": true, "default": true,
	"items": true, "minItems": true, "maxItems": true, "uniqueItems": true,
}

// CheckSchemaSubset walks schema and returns every construct outside the
// fixed JSON Schema subset from design D3. An empty result means schema is
// entirely within the subset.
func CheckSchemaSubset(schema map[string]any) []Finding {
	c := &schemaChecker{}
	if b, err := json.Marshal(schema); err == nil && len(b) > maxSchemaSize {
		c.addf("", "schema text exceeds %d bytes", maxSchemaSize)
	}
	if t, _ := schema["type"].(string); t != "object" {
		c.addf("", "root schema must declare type: object")
	}
	c.checkNode(schema, "", true, 1)
	if c.propCount > maxTotalProperties {
		c.addf("", "schema declares more than %d properties in total", maxTotalProperties)
	}
	return c.findings
}

type schemaChecker struct {
	findings  []Finding
	propCount int
}

func (c *schemaChecker) addf(path, format string, args ...any) {
	c.findings = append(c.findings, Finding{Path: path, Message: fmt.Sprintf(format, args...)})
}

// rejectKeyword records that key is not in the allowed subset at path,
// naming the parity reason for the two keywords the three validators
// disagree on.
func (c *schemaChecker) rejectKeyword(path, key string) {
	switch key {
	case "format":
		c.addf(path, "keyword \"format\" is rejected because Ajv ignores an unregistered format while Go and the plugin would reject it")
	case "multipleOf":
		c.addf(path, "keyword \"multipleOf\" is rejected because Ajv applies multipleOfPrecision, which the other validators do not")
	default:
		c.addf(path, "keyword %q is not in the allowed subset", key)
	}
}

func (c *schemaChecker) checkNode(node map[string]any, path string, isRoot bool, depth int) {
	rawType, hasType := node["type"]
	if !hasType {
		c.addf(path, "schema must declare \"type\"")
		return
	}
	switch t := rawType.(type) {
	case string:
		switch t {
		case "object":
			c.checkObject(node, path, isRoot, depth)
		case "string":
			c.checkString(node, path)
		case "number", "integer":
			c.checkNumeric(node, path, t)
		case "boolean":
			c.checkBoolean(node, path)
		case "array":
			c.checkArray(node, path, depth)
		case "null":
			c.addf(path, "type \"null\" is rejected")
		default:
			c.addf(path, "unknown type %q", t)
		}
	case []any:
		c.addf(path, "type written as an array is rejected")
	default:
		c.addf(path, "\"type\" must be a string")
	}
}

func (c *schemaChecker) checkObject(node map[string]any, path string, isRoot bool, depth int) {
	if depth > maxSchemaDepth {
		c.addf(path, "nesting depth exceeds the limit of %d", maxSchemaDepth)
	}
	for _, key := range sortedKeys(node) {
		if objectKeys[key] {
			continue
		}
		if isRoot && key == "$schema" {
			continue
		}
		c.rejectKeyword(path, key)
	}
	if isRoot {
		if raw, present := node["$schema"]; present {
			if s, ok := raw.(string); !ok || s != schemaDialect {
				c.addf(path, "$schema must be %q", schemaDialect)
			}
		}
	}
	ap, hasAP := node["additionalProperties"]
	switch {
	case isRoot && !hasAP:
		c.addf(path, "root schema must set additionalProperties: false")
	case hasAP:
		if b, ok := ap.(bool); !ok || b {
			c.addf(path, "additionalProperties must be false")
		}
	}
	rawProps, hasProps := node["properties"]
	if hasProps {
		props, ok := rawProps.(map[string]any)
		if !ok {
			c.addf(path, "properties must be an object")
		} else {
			for _, name := range sortedKeys(props) {
				propPath := joinPath(path, name)
				if !propertyNamePattern.MatchString(name) {
					c.addf(propPath, "property name %q must match %s", name, propertyNamePattern.String())
				}
				c.propCount++
				sub, ok := props[name].(map[string]any)
				if !ok {
					c.addf(propPath, "property schema must be an object")
					continue
				}
				c.checkNode(sub, propPath, false, depth+1)
			}
		}
	}
	if req, present := node["required"]; present {
		list, ok := req.([]any)
		if !ok {
			c.addf(path, "required must be a list of strings")
		} else {
			for _, item := range list {
				if _, ok := item.(string); !ok {
					c.addf(path, "required entries must be strings")
				}
			}
		}
	}
}

func (c *schemaChecker) checkString(node map[string]any, path string) {
	for _, key := range sortedKeys(node) {
		if !stringKeys[key] {
			c.rejectKeyword(path, key)
		}
	}
	if enumVal, present := node["enum"]; present {
		list, ok := enumVal.([]any)
		if !ok {
			c.addf(path, "enum must be a list")
		} else {
			if len(list) > maxEnumEntries {
				c.addf(path, "enum holds more than %d entries", maxEnumEntries)
			}
			for _, v := range list {
				if _, ok := v.(string); !ok {
					c.addf(path, "enum entries must be strings for a string property")
				}
			}
		}
	}
	if pat, present := node["pattern"]; present {
		ps, ok := pat.(string)
		if !ok {
			c.addf(path, "pattern must be a string")
			return
		}
		msgs := CheckPattern(ps)
		for _, m := range msgs {
			c.addf(path, "pattern: %s", m)
		}
		if len(msgs) == 0 {
			if _, err := regexp.Compile(NormalizePattern(ps)); err != nil {
				c.addf(path, "pattern does not compile: %v", err)
			}
		}
	}
	c.checkNonNegativeInteger(node, path, "minLength")
	c.checkNonNegativeInteger(node, path, "maxLength")
}

func (c *schemaChecker) checkNumeric(node map[string]any, path, typ string) {
	for _, key := range sortedKeys(node) {
		if !numericKeys[key] {
			c.rejectKeyword(path, key)
		}
	}
	if enumVal, present := node["enum"]; present {
		list, ok := enumVal.([]any)
		if !ok {
			c.addf(path, "enum must be a list")
		} else {
			if len(list) > maxEnumEntries {
				c.addf(path, "enum holds more than %d entries", maxEnumEntries)
			}
			for _, v := range list {
				switch v.(type) {
				case int, int64, float64:
				default:
					c.addf(path, "enum entries must be numbers for a %s property", typ)
				}
			}
		}
	}
	c.checkNumber(node, path, "minimum")
	c.checkNumber(node, path, "maximum")
	c.checkNumber(node, path, "exclusiveMinimum")
	c.checkNumber(node, path, "exclusiveMaximum")
}

func (c *schemaChecker) checkBoolean(node map[string]any, path string) {
	for _, key := range sortedKeys(node) {
		if !booleanKeys[key] {
			c.rejectKeyword(path, key)
		}
	}
}

func (c *schemaChecker) checkArray(node map[string]any, path string, depth int) {
	for _, key := range sortedKeys(node) {
		if !arrayKeys[key] {
			c.rejectKeyword(path, key)
		}
	}
	c.checkNonNegativeInteger(node, path, "minItems")
	c.checkNonNegativeInteger(node, path, "maxItems")
	if v, present := node["uniqueItems"]; present {
		if _, ok := v.(bool); !ok {
			c.addf(path, "uniqueItems must be a boolean")
		}
	}
	itemsRaw, present := node["items"]
	if !present {
		return
	}
	if _, isList := itemsRaw.([]any); isList {
		c.addf(path, "items must be exactly one schema, not a tuple")
		return
	}
	itemsSchema, ok := itemsRaw.(map[string]any)
	if !ok {
		c.addf(path, "items must be a schema object")
		return
	}
	c.checkNode(itemsSchema, path+"[]", false, depth+1)
}

// checkNumber records a finding at path when node's key keyword is
// present and is not a JSON number, so a malformed "minimum" or
// "maximum" is rejected at catalog validation instead of reaching a
// validator that disagrees with Ajv about its meaning.
func (c *schemaChecker) checkNumber(node map[string]any, path, keyword string) {
	v, present := node[keyword]
	if !present {
		return
	}
	if !isJSONNumber(v) {
		c.addf(path, "%s must be a number", keyword)
	}
}

// checkNonNegativeInteger records a finding at path when node's key
// keyword is present and is not a non-negative integer, matching Ajv's
// requirement for "minLength", "maxLength", "minItems", and "maxItems".
func (c *schemaChecker) checkNonNegativeInteger(node map[string]any, path, keyword string) {
	v, present := node[keyword]
	if !present {
		return
	}
	if !isNonNegativeInteger(v) {
		c.addf(path, "%s must be a non-negative integer", keyword)
	}
}

// isJSONNumber reports whether v, decoded from YAML, holds a numeric
// value.
func isJSONNumber(v any) bool {
	switch v.(type) {
	case int, int64, uint64, float64:
		return true
	default:
		return false
	}
}

// isNonNegativeInteger reports whether v, decoded from YAML, holds an
// integer value that is zero or greater.
func isNonNegativeInteger(v any) bool {
	switch n := v.(type) {
	case int:
		return n >= 0
	case int64:
		return n >= 0
	case uint64:
		return true
	case float64:
		return n >= 0 && n == float64(int64(n))
	default:
		return false
	}
}

// joinPath appends seg to base, dotted, so a nested finding names its full
// location.
func joinPath(base, seg string) string {
	if base == "" {
		return seg
	}
	return base + "." + seg
}

// sortedKeys returns m's keys in sorted order, so a walk over a map
// produces the same findings in the same order on every run.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
