package pipelinetemplate

import "fmt"

// ApplyDefaults returns a copy of values with every schema-declared default
// filled in for a property values omits. It descends into a nested object
// property's own defaults. It does not validate values.
func ApplyDefaults(schema map[string]any, values map[string]any) map[string]any {
	out := make(map[string]any, len(values))
	for k, v := range values {
		out[k] = v
	}
	props, _ := schema["properties"].(map[string]any)
	for name, raw := range props {
		sub, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, present := out[name]; !present {
			if def, hasDefault := sub["default"]; hasDefault {
				out[name] = def
			}
		}
		if nested, ok := out[name].(map[string]any); ok {
			if t, _ := sub["type"].(string); t == "object" {
				out[name] = ApplyDefaults(sub, nested)
			}
		}
	}
	return out
}

// ValidateValues applies schema's defaults to values, then validates the
// defaulted map against schema. It returns the defaulted values and every
// finding; an empty finding list means values is valid.
func ValidateValues(schema map[string]any, values map[string]any) (map[string]any, []Finding, error) {
	defaulted := ApplyDefaults(schema, values)
	compiled, err := compileJSONSchema(schema)
	if err != nil {
		return defaulted, nil, fmt.Errorf("compile schema: %w", err)
	}
	if verr := compiled.Validate(any(defaulted)); verr != nil {
		return defaulted, findingsFromValidationError(verr), nil
	}
	return defaulted, nil, nil
}
