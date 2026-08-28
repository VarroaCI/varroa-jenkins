package api

import (
	"os"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// controllerSpecAllowlist maps a ControllerSpec top-level property name to the
// reason it is permitted to differ between the generated CRD schema and the
// BFF's hand-maintained OpenAPI schema. Seeded with the known intentional
// differences. Add an entry here only when a deliberate divergence is
// introduced — never to silence a real drift.
var controllerSpecAllowlist = map[string]string{
	"resources": "the BFF deliberately hand-maintains a SUBSET of corev1.ResourceRequirements (limits/requests only, type: string); it does not mirror the CRD's claims field or int-or-string anyOf",
}

// controllerSpecDrift compares the top-level properties of the generated CRD's
// ControllerSpec schema against the BFF's OpenAPI ControllerSpec schema and
// returns the offending property names (empty when the two agree).
//
// The comparison is narrow by construction so it survives known intentional
// differences:
//   - top-level property *presence* only (never deep shape),
//   - `x-kubernetes-*` extensions are normalised away before comparing,
//   - an explicit allowlist (controllerSpecAllowlist) carries intentional
//     differences such as `resources`,
//   - a property where the CRD declares an `anyOf` int-or-string and the BFF
//     declares `type: string` is automatically allowed — that shape is
//     deliberately not mirrored.
//
// A failure names the offending property so it is actionable.
func controllerSpecDrift(crdProps, bffProps map[string]any) []string {
	crd := normalizeSchemaProps(crdProps)
	bff := normalizeSchemaProps(bffProps)

	var offending []string
	for name, crdNode := range crd {
		if controllerSpecPropertyAllowed(name, crdNode, bff[name]) {
			continue
		}
		if _, ok := bff[name]; !ok {
			offending = append(offending, name)
		}
	}
	for name, bffNode := range bff {
		if controllerSpecPropertyAllowed(name, crd[name], bffNode) {
			continue
		}
		if _, ok := crd[name]; !ok {
			offending = append(offending, name)
		}
	}
	sort.Strings(offending)
	return offending
}

// controllerSpecPropertyAllowed reports whether property name is permitted to
// differ between the two schemas: either it is on the explicit allowlist, or
// the CRD declares an anyOf int-or-string where the BFF declares type: string.
func controllerSpecPropertyAllowed(name string, crdNode, bffNode any) bool {
	if _, ok := controllerSpecAllowlist[name]; ok {
		return true
	}
	crdMap, crdOK := crdNode.(map[string]any)
	bffMap, bffOK := bffNode.(map[string]any)
	if crdOK && bffOK && isIntOrStringAnyOf(crdMap) && bffMap["type"] == "string" {
		return true
	}
	return false
}

// isIntOrStringAnyOf reports whether node's anyOf is the Kubernetes int-or-
// string shape (a non-empty list whose entries are only type integer/string).
func isIntOrStringAnyOf(node map[string]any) bool {
	raw, ok := node["anyOf"]
	if !ok {
		return false
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		return false
	}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return false
		}
		typ, _ := m["type"].(string)
		if typ != "integer" && typ != "string" {
			return false
		}
	}
	return true
}

// normalizeSchemaProps deep-copies a schema-properties map with every
// `x-kubernetes-*` extension key removed, so extension-only differences never
// trip the presence comparison.
func normalizeSchemaProps(props map[string]any) map[string]any {
	out := make(map[string]any, len(props))
	for name, node := range props {
		out[name] = stripKubernetesExtensions(node)
	}
	return out
}

func stripKubernetesExtensions(v any) any {
	switch n := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, val := range n {
			if strings.HasPrefix(k, "x-kubernetes-") {
				continue
			}
			out[k] = stripKubernetesExtensions(val)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, val := range n {
			out[i] = stripKubernetesExtensions(val)
		}
		return out
	default:
		return v
	}
}

// loadCRDControllerSpecProperties reads the generated Controller CRD and
// returns its spec's top-level property schemas.
func loadCRDControllerSpecProperties(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../../charts/varroa/crds/varroa.dev_controllers.yaml")
	if err != nil {
		t.Fatalf("read CRD: %v", err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("parse CRD: %v", err)
	}
	spec, ok := crd["spec"].(map[string]any)
	if !ok {
		t.Fatal("CRD missing spec")
	}
	versions, ok := spec["versions"].([]any)
	if !ok || len(versions) == 0 {
		t.Fatal("CRD missing versions")
	}
	var schemaProps map[string]any
	for _, v := range versions {
		ver, _ := v.(map[string]any)
		schemaNode, _ := ver["schema"].(map[string]any)
		openAPI, _ := schemaNode["openAPIV3Schema"].(map[string]any)
		rootProps, _ := openAPI["properties"].(map[string]any)
		specSchema, _ := rootProps["spec"].(map[string]any)
		props, _ := specSchema["properties"].(map[string]any)
		if props != nil {
			schemaProps = props
			break
		}
	}
	if schemaProps == nil {
		t.Fatal("CRD spec schema has no properties")
	}
	return schemaProps
}

// loadBFFControllerSpecProperties reads the BFF's hand-maintained OpenAPI
// components and returns ControllerSpec's top-level property schemas.
func loadBFFControllerSpecProperties(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../../api/openapi/components/schemas.yaml")
	if err != nil {
		t.Fatalf("read schemas: %v", err)
	}
	var schemas map[string]any
	if err := yaml.Unmarshal(data, &schemas); err != nil {
		t.Fatalf("parse schemas: %v", err)
	}
	cs, ok := schemas["ControllerSpec"].(map[string]any)
	if !ok {
		t.Fatal("BFF schemas missing ControllerSpec")
	}
	props, _ := cs["properties"].(map[string]any)
	if props == nil {
		t.Fatal("BFF ControllerSpec has no properties")
	}
	return props
}

// TestControllerSpecDriftGuard is the real-file guard: the generated CRD's
// ControllerSpec top-level properties must match the BFF's (modulo the
// allowlist). If this fails, a CRD field was not mirrored in the BFF schema —
// name the field and add it (or allowlist a deliberate divergence).
func TestControllerSpecDriftGuard(t *testing.T) {
	crd := loadCRDControllerSpecProperties(t)
	bff := loadBFFControllerSpecProperties(t)
	// Sanity: the loaders must have actually extracted the schemas, or a
	// silently-empty parse would make the guard pass vacuously.
	if len(crd) == 0 || len(bff) == 0 {
		t.Fatalf("schema loaders returned empty property sets (crd=%d, bff=%d)", len(crd), len(bff))
	}
	offending := controllerSpecDrift(crd, bff)
	if len(offending) > 0 {
		t.Errorf("ControllerSpec drifted between the generated CRD and the BFF OpenAPI schema.\n"+
			"Mirror the field(s) in api/openapi/components/schemas.yaml#/ControllerSpec, or add a reasoned\n"+
			"entry to controllerSpecAllowlist if the difference is intentional.\n  offending: %v", offending)
	}
}

func TestControllerSpecDriftGuard_NamesNewProperty(t *testing.T) {
	// A new CRD property with no BFF counterpart must fail, naming it.
	crd := map[string]any{
		"endpoint": map[string]any{"type": "string"},
		"version":  map[string]any{"type": "string"},
	}
	bff := map[string]any{
		"endpoint": map[string]any{"type": "string"},
	}
	offending := controllerSpecDrift(crd, bff)
	if len(offending) != 1 || offending[0] != "version" {
		t.Fatalf("want offending=[version], got %v", offending)
	}
}

func TestControllerSpecDriftGuard_AllowlistedDifferencePasses(t *testing.T) {
	// resources is allowlisted (BFF hand-maintained subset of
	// ResourceRequirements): a presence difference on it must not fail.
	crd := map[string]any{
		"endpoint":  map[string]any{"type": "string"},
		"resources": map[string]any{"type": "object"},
	}
	bff := map[string]any{
		"endpoint": map[string]any{"type": "string"},
	}
	if offending := controllerSpecDrift(crd, bff); len(offending) != 0 {
		t.Fatalf("allowlisted resources difference must pass, got %v", offending)
	}
}

func TestControllerSpecDriftGuard_IntOrStringVsStringPasses(t *testing.T) {
	// CRD declares an anyOf int-or-string where the BFF declares type: string —
	// deliberately not mirrored, so the difference must pass.
	crd := map[string]any{
		"quantity": map[string]any{
			"anyOf": []any{
				map[string]any{"type": "integer"},
				map[string]any{"type": "string", "pattern": "^[0-9]+$"},
			},
			"x-kubernetes-int-or-string": true,
		},
	}
	bff := map[string]any{
		"quantity": map[string]any{"type": "string"},
	}
	if offending := controllerSpecDrift(crd, bff); len(offending) != 0 {
		t.Fatalf("anyOf int-or-string vs type: string must pass, got %v", offending)
	}
}

func TestControllerSpecDriftGuard_NotAllowlistedStillFails(t *testing.T) {
	// A plain type: object property missing from the BFF is NOT the
	// int-or-string case and NOT allowlisted — it must fail.
	crd := map[string]any{
		"endpoint":  map[string]any{"type": "string"},
		"newObject": map[string]any{"type": "object"},
	}
	bff := map[string]any{
		"endpoint": map[string]any{"type": "string"},
	}
	offending := controllerSpecDrift(crd, bff)
	if len(offending) != 1 || offending[0] != "newObject" {
		t.Fatalf("want offending=[newObject], got %v", offending)
	}
}
