package pipelinetemplate

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"k8s.io/apimachinery/pkg/util/validation"
)

// schemaResourceSeq gives every compiled schema a unique resource URL, so
// concurrent validation never collides on the compiler's resource cache.
var schemaResourceSeq int64

// fallbackPrinter renders the library's own finding text for an ErrorKind
// findingsFromErrorKind does not recognize.
var fallbackPrinter = message.NewPrinter(language.English)

// literalPatternRegexp wraps a compiled, NormalizePattern-rewritten regular
// expression so String returns the schema author's original pattern text,
// not the rewritten one. The library renders a pattern-keyword finding
// from this String, and the plugin's hand-rolled validator renders the
// same finding from the schema's own, unrewritten pattern value; keeping
// the original text here is what makes the two finding messages match.
type literalPatternRegexp struct {
	*regexp.Regexp
	original string
}

func (r *literalPatternRegexp) String() string {
	return r.original
}

// compileJSONSchema compiles schema, which must already pass
// CheckSchemaSubset, with the shared validator. It uses NormalizePattern for
// the pattern keyword's regex engine, so a value is matched the same way
// the plugin and the browser match it, while keeping the original pattern
// text for finding messages via literalPatternRegexp.
func compileJSONSchema(schema map[string]any) (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseRegexpEngine(func(pattern string) (jsonschema.Regexp, error) {
		compiled, err := regexp.Compile(NormalizePattern(pattern))
		if err != nil {
			return nil, err
		}
		return &literalPatternRegexp{Regexp: compiled, original: pattern}, nil
	})
	id := fmt.Sprintf("pipelinetemplate://schema/%d", atomic.AddInt64(&schemaResourceSeq, 1))
	if err := c.AddResource(id, schema); err != nil {
		return nil, fmt.Errorf("add schema resource: %w", err)
	}
	compiled, err := c.Compile(id)
	if err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}
	return compiled, nil
}

// findingsFromValidationError flattens a jsonschema validation error into
// one Finding per leaf cause. Each Finding's Path is the instance location
// in the dot-and-bracket form the plugin's hand-rolled path builder uses
// (javaStylePath), and each Finding's Message is rendered from the leaf
// cause's typed ErrorKind to read exactly the same as the plugin's own
// hand-rolled text for the same failure (task 5.4). An ErrorKind this
// function does not recognize falls back to the library's own text.
func findingsFromValidationError(err error) []Finding {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []Finding{{Message: err.Error()}}
	}
	out := ve.BasicOutput()
	var findings []Finding
	for _, unit := range out.Errors {
		if unit.Valid || unit.Error == nil {
			continue
		}
		path := javaStylePath(unit.InstanceLocation)
		findings = append(findings, findingsFromErrorKind(path, unit.Error.Kind)...)
	}
	if len(findings) == 0 {
		findings = append(findings, Finding{Message: err.Error()})
	}
	return findings
}

// findingsFromErrorKind renders one leaf validation cause into one or more
// Findings at path. kind.Required and kind.AdditionalProperties each carry
// every offending name in one cause, and the plugin's validator emits one
// finding per name for both, so this returns a slice rather than one
// Finding.
func findingsFromErrorKind(path string, k jsonschema.ErrorKind) []Finding {
	switch v := k.(type) {
	case *kind.Required:
		findings := make([]Finding, len(v.Missing))
		for i, name := range v.Missing {
			findings[i] = Finding{Path: path, Message: fmt.Sprintf(`missing property "%s"`, name)}
		}
		return findings
	case *kind.AdditionalProperties:
		findings := make([]Finding, len(v.Properties))
		for i, name := range v.Properties {
			findings[i] = Finding{Path: path, Message: fmt.Sprintf(`additional property "%s" is not allowed`, name)}
		}
		return findings
	case *kind.Type:
		want := strings.Join(v.Want, " or ")
		return []Finding{{Path: path, Message: fmt.Sprintf("got %s, want %s", v.Got, want)}}
	case *kind.Enum:
		return []Finding{{Path: path, Message: fmt.Sprintf("value must be one of %s", javaListLiteral(v.Want))}}
	case *kind.Const:
		return []Finding{{Path: path, Message: fmt.Sprintf("value violates const %s", javaScalarLiteral(v.Want))}}
	case *kind.Pattern:
		return []Finding{{Path: path, Message: fmt.Sprintf(`value does not match pattern "%s"`, v.Want)}}
	case *kind.MinLength:
		return []Finding{{Path: path, Message: fmt.Sprintf("length is less than the minimum of %d", v.Want)}}
	case *kind.MaxLength:
		return []Finding{{Path: path, Message: fmt.Sprintf("length exceeds the maximum of %d", v.Want)}}
	case *kind.MinItems:
		return []Finding{{Path: path, Message: fmt.Sprintf("holds fewer than the minimum of %d items", v.Want)}}
	case *kind.MaxItems:
		return []Finding{{Path: path, Message: fmt.Sprintf("holds more than the maximum of %d items", v.Want)}}
	case *kind.UniqueItems:
		return []Finding{{Path: path, Message: "items must be unique"}}
	case *kind.Minimum:
		return []Finding{{Path: path, Message: fmt.Sprintf("value violates minimum %s", javaRatLiteral(v.Want))}}
	case *kind.Maximum:
		return []Finding{{Path: path, Message: fmt.Sprintf("value violates maximum %s", javaRatLiteral(v.Want))}}
	case *kind.ExclusiveMinimum:
		return []Finding{{Path: path, Message: fmt.Sprintf("value violates exclusiveMinimum %s", javaRatLiteral(v.Want))}}
	case *kind.ExclusiveMaximum:
		return []Finding{{Path: path, Message: fmt.Sprintf("value violates exclusiveMaximum %s", javaRatLiteral(v.Want))}}
	case *kind.MultipleOf:
		return []Finding{{Path: path, Message: fmt.Sprintf("value violates multipleOf %s", javaRatLiteral(v.Want))}}
	default:
		return []Finding{{Path: path, Message: k.LocalizedString(fallbackPrinter)}}
	}
}

// javaStylePath renders a JSON-Pointer instance location (for example
// "/goVersion" or "/items/0/name") in the dot-and-bracket form the
// plugin's hand-rolled path builder uses ("goVersion", "items[0].name"),
// so a Finding's Path matches on both sides of the corpus. The document
// root ("" or "/") renders as "".
func javaStylePath(instanceLocation string) string {
	trimmed := strings.TrimPrefix(instanceLocation, "/")
	if trimmed == "" {
		return ""
	}
	var out strings.Builder
	for _, tok := range strings.Split(trimmed, "/") {
		tok = unescapeJSONPointerToken(tok)
		if isArrayIndexToken(tok) {
			out.WriteString("[")
			out.WriteString(tok)
			out.WriteString("]")
			continue
		}
		if out.Len() > 0 {
			out.WriteByte('.')
		}
		out.WriteString(tok)
	}
	return out.String()
}

// isArrayIndexToken reports whether tok is a JSON-Pointer array index (all
// decimal digits), which javaStylePath renders as "[tok]" rather than
// ".tok".
func isArrayIndexToken(tok string) bool {
	if tok == "" {
		return false
	}
	for _, r := range tok {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// unescapeJSONPointerToken reverses RFC 6901 escaping on one JSON-Pointer
// token.
func unescapeJSONPointerToken(tok string) string {
	tok = strings.ReplaceAll(tok, "~1", "/")
	tok = strings.ReplaceAll(tok, "~0", "~")
	return tok
}

// javaListLiteral renders items the way Java's AbstractCollection.toString
// renders a List: "[" then each element's own toString, comma-space
// separated, then "]" — the plugin's enum finding embeds the schema's raw
// enum list this way.
func javaListLiteral(items []any) string {
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = javaScalarLiteral(item)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// javaScalarLiteral renders one YAML-decoded scalar the way Java's
// String.valueOf renders the same value decoded by SnakeYAML: a string
// verbatim (no quotes), a boolean as "true"/"false", a whole number with
// no fractional part, and any other number via its shortest decimal form.
func javaScalarLiteral(v any) string {
	switch val := v.(type) {
	case nil:
		return "null"
	case string:
		return val
	case bool:
		if val {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", val)
	}
}

// javaRatLiteral renders a numeric schema bound (minimum, maximum, and
// similar keywords) the way the plugin's checkBound renders the schema's
// own raw numeric value: a whole number with no fractional part, else its
// shortest decimal form.
func javaRatLiteral(r *big.Rat) string {
	if r == nil {
		return ""
	}
	if r.IsInt() {
		return r.Num().String()
	}
	f, _ := r.Float64()
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// ValidateDocument parses raw as a PipelineTemplate document and validates
// its structure, its parameter schema, and its uiSchema. It returns every
// finding and the parsed document. An empty finding list means raw is
// valid. The document is nil when raw does not even decode.
func ValidateDocument(raw []byte) ([]Finding, *Document) {
	var findings []Finding
	if len(raw) > MaxDocumentSize {
		findings = append(findings, Finding{Message: fmt.Sprintf("document exceeds %d bytes", MaxDocumentSize)})
	}
	doc, err := ParseDocument(raw)
	if err != nil {
		return append(findings, Finding{Message: err.Error()}), nil
	}

	if doc.APIVersion != APIVersion {
		findings = append(findings, Finding{Path: "apiVersion", Message: fmt.Sprintf("apiVersion must be %q, got %q", APIVersion, doc.APIVersion)})
	}
	if doc.Kind != Kind {
		findings = append(findings, Finding{Path: "kind", Message: fmt.Sprintf("kind must be %q, got %q", Kind, doc.Kind)})
	}
	if errs := validation.IsDNS1123Label(doc.Metadata.Name); len(errs) > 0 {
		findings = append(findings, Finding{Path: "metadata.name", Message: strings.Join(errs, "; ")})
	}
	if _, err := ParseVersion(doc.Spec.Version); err != nil {
		findings = append(findings, Finding{Path: "spec.version", Message: err.Error()})
	}

	hasJenkinsfile := doc.Spec.Jenkinsfile != ""
	hasSCM := doc.Spec.SCMDefinition != nil
	switch {
	case hasJenkinsfile && hasSCM:
		findings = append(findings, Finding{Path: "spec", Message: "exactly one of spec.jenkinsfile and spec.scmDefinition must be set, both are set"})
	case !hasJenkinsfile && !hasSCM:
		findings = append(findings, Finding{Path: "spec", Message: "exactly one of spec.jenkinsfile and spec.scmDefinition must be set, neither is set"})
	}
	if hasJenkinsfile && len(doc.Spec.Jenkinsfile) > MaxJenkinsfileSize {
		findings = append(findings, Finding{Path: "spec.jenkinsfile", Message: fmt.Sprintf("jenkinsfile exceeds %d bytes", MaxJenkinsfileSize)})
	}
	if hasSCM {
		if doc.Spec.SCMDefinition.Remote == "" {
			findings = append(findings, Finding{Path: "spec.scmDefinition.remote", Message: "remote is required"})
		}
		if doc.Spec.SCMDefinition.ScriptPath == "" {
			findings = append(findings, Finding{Path: "spec.scmDefinition.scriptPath", Message: "scriptPath is required"})
		}
	}

	if doc.Spec.Parameters == nil {
		findings = append(findings, Finding{Path: "spec.parameters", Message: "spec.parameters is required"})
		return findings, doc
	}
	for _, f := range CheckSchema(doc.Spec.Parameters) {
		findings = append(findings, prefixFinding("spec.parameters", f))
	}
	if doc.Spec.UISchema != nil {
		for _, f := range CheckUISchema(doc.Spec.UISchema, doc.Spec.Parameters) {
			findings = append(findings, prefixFinding("spec.uiSchema", f))
		}
	}
	return findings, doc
}

func prefixFinding(prefix string, f Finding) Finding {
	return Finding{Path: joinPath(prefix, f.Path), Message: f.Message}
}

// CheckSchema runs every schema-level check from design D3 against schema:
// the subset walk, and the rule that every "default" satisfies its own
// schema. An empty result means schema is valid.
func CheckSchema(schema map[string]any) []Finding {
	findings := CheckSchemaSubset(schema)
	checkDefaults(schema, "", &findings)
	return findings
}

// checkDefaults recursively validates that every "default" in schema
// satisfies the subschema it appears on, so a template cannot ship a
// default its own consumers cannot use.
func checkDefaults(schema map[string]any, path string, findings *[]Finding) {
	if def, has := schema["default"]; has {
		compiled, err := compileJSONSchema(schema)
		if err == nil {
			if verr := compiled.Validate(def); verr != nil {
				for _, f := range findingsFromValidationError(verr) {
					*findings = append(*findings, Finding{Path: path, Message: fmt.Sprintf("default does not satisfy its own schema: %s", f.Message)})
				}
			}
		}
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		for _, name := range sortedKeys(props) {
			if sub, ok := props[name].(map[string]any); ok {
				checkDefaults(sub, joinPath(path, name), findings)
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		checkDefaults(items, path+"[]", findings)
	}
}
