package pipelinetemplate

import "fmt"

var allowedUIWidgets = map[string]bool{
	"textarea": true, "select": true, "radio": true, "checkbox": true, "updown": true,
}

// CheckUISchema walks uiSchema and returns every construct outside the
// fixed uiSchema subset from design D4. schema is the corresponding JSON
// Schema, used to check that a ui:order names every declared property. An
// empty result means uiSchema is entirely within the subset.
func CheckUISchema(uiSchema map[string]any, schema map[string]any) []Finding {
	var findings []Finding
	checkUINode(uiSchema, schema, "", &findings)
	return findings
}

func checkUINode(node map[string]any, schemaNode map[string]any, path string, findings *[]Finding) {
	props, _ := schemaNode["properties"].(map[string]any)
	for _, key := range sortedKeys(node) {
		val := node[key]
		if key == "ui:order" {
			checkUIOrder(val, props, path, findings)
			continue
		}
		if len(key) >= 3 && key[:3] == "ui:" {
			checkUIKey(key, val, path, findings)
			continue
		}
		// design D4: a non-"ui:" key mirrors properties, so it must name a
		// declared property of the template's parameters schema at this
		// level.
		subSchema, declared := props[key].(map[string]any)
		if !declared {
			*findings = append(*findings, Finding{Path: joinPath(path, key), Message: fmt.Sprintf("uiSchema key %q names no declared property", key)})
			continue
		}
		sub, ok := val.(map[string]any)
		if !ok {
			*findings = append(*findings, Finding{Path: joinPath(path, key), Message: "a nested uiSchema entry must be an object"})
			continue
		}
		checkUINode(sub, subSchema, joinPath(path, key), findings)
	}
}

func checkUIKey(key string, val any, path string, findings *[]Finding) {
	switch key {
	case "ui:title", "ui:description", "ui:help", "ui:placeholder":
		if _, ok := val.(string); !ok {
			*findings = append(*findings, Finding{Path: joinPath(path, key), Message: fmt.Sprintf("%s must be a string", key)})
		}
	case "ui:widget":
		checkUIWidget(val, joinPath(path, key), findings)
	case "ui:options":
		checkUIOptions(val, joinPath(path, key), findings)
	default:
		*findings = append(*findings, Finding{Path: joinPath(path, key), Message: fmt.Sprintf("uiSchema key %q is not in the allowed subset", key)})
	}
}

func checkUIWidget(val any, path string, findings *[]Finding) {
	s, ok := val.(string)
	if !ok {
		*findings = append(*findings, Finding{Path: path, Message: "ui:widget must be a string"})
		return
	}
	if s == "password" {
		*findings = append(*findings, Finding{Path: path, Message: "ui:widget: password is rejected because a marker value holds no secret"})
		return
	}
	if !allowedUIWidgets[s] {
		*findings = append(*findings, Finding{Path: path, Message: fmt.Sprintf("ui:widget %q is not in the allowed set", s)})
	}
}

func checkUIOptions(val any, path string, findings *[]Finding) {
	opts, ok := val.(map[string]any)
	if !ok {
		*findings = append(*findings, Finding{Path: path, Message: "ui:options must be an object"})
		return
	}
	for _, k := range sortedKeys(opts) {
		if k != "rows" {
			*findings = append(*findings, Finding{Path: path, Message: fmt.Sprintf("ui:options key %q is not in the allowed subset", k)})
		}
	}
}

func checkUIOrder(val any, props map[string]any, path string, findings *[]Finding) {
	list, ok := val.([]any)
	if !ok {
		*findings = append(*findings, Finding{Path: joinPath(path, "ui:order"), Message: "ui:order must be a list"})
		return
	}
	named := map[string]bool{}
	wildcard := false
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			*findings = append(*findings, Finding{Path: joinPath(path, "ui:order"), Message: "ui:order entries must be strings"})
			continue
		}
		if s == "*" {
			wildcard = true
			continue
		}
		if _, declared := props[s]; !declared {
			*findings = append(*findings, Finding{Path: joinPath(path, "ui:order"), Message: fmt.Sprintf("ui:order entry %q names no declared property", s)})
			continue
		}
		named[s] = true
	}
	if wildcard {
		return
	}
	for _, name := range sortedKeys(props) {
		if !named[name] {
			*findings = append(*findings, Finding{Path: joinPath(path, "ui:order"), Message: fmt.Sprintf("ui:order omits declared property %q and carries no \"*\"", name)})
		}
	}
}
