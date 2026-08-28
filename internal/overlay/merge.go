// Package overlay implements strategic-merge-patch-based resource overlay for
// Varroa's managed Kubernetes resources (StatefulSet, Service, Ingress).
//
// Spike findings (task 0.1 — confirmed 2026-06):
//
//  1. LIST-MERGE BY KEY WORKS
//     StrategicMergePatch against appsv1.StatefulSet{} correctly merges lists
//     by their patchMergeKey (e.g. containers by name, env by name, volumes by
//     name, ports by name). A patch adding one env entry to the "jenkins"
//     container preserves all existing env entries and appends the new one.
//     Same for volumes and Service ports. No field failed list-merge.
//
//  2. ROUND-TRIP FIDELITY
//     Values survive the JSON → StrategicMergePatch → JSON round-trip.
//     json.Unmarshal into map[string]interface{} yields float64 for JSON
//     numbers (standard Go stdlib behavior), but re-marshaling via json.Marshal
//     correctly serialises them back (no .0 suffix for integer values). No
//     int/string/Quantity coercion loss was observed.
//
//  3. TYPEMETA HAZARD (nuanced)
//     A zero-value appsv1.StatefulSet{} marshals WITHOUT apiVersion or kind
//     because metav1.TypeMeta's json tags carry omitempty. Therefore applying
//     such a patch does NOT blank the base's apiVersion/kind — the fields are
//     simply absent from the patch JSON. HOWEVER, a raw user overlay YAML that
//     explicitly includes apiVersion: "" or kind: "" WOULD blank those fields.
//     Design decision stands: CompilePodOverrides MUST strip empty apiVersion
//     and kind from the patch JSON as a defensive measure, and Merge should do
//     the same for raw overlays (or let the strategic merge patch handle it
//     since the fields are typically absent, not empty).
//
//  4. SERVICE PORTS
//     corev1.Service{} schema correctly list-merges ports by name. Adding one
//     annotation via ObjectMeta does not disturb existing ports or selector.
package overlay

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"sigs.k8s.io/yaml"
)

// Merge applies a strategic-merge patch (patchYAML, YAML or JSON bytes) onto
// base, using schema as the strategic-merge directive source (e.g.
// appsv1.StatefulSet{}). Returns a NEW unstructured; base is not mutated.
// A nil/empty patch returns base unchanged.
func Merge(base *unstructured.Unstructured, patchYAML []byte, schema any) (*unstructured.Unstructured, error) {
	if len(patchYAML) == 0 {
		// Return base itself (caller should not mutate it).
		return base, nil
	}

	patchJSON, err := yaml.YAMLToJSON(patchYAML)
	if err != nil {
		return nil, fmt.Errorf("yaml-to-json: %w", err)
	}

	baseJSON, err := json.Marshal(base.Object)
	if err != nil {
		return nil, fmt.Errorf("marshal base: %w", err)
	}

	mergedJSON, err := strategicpatch.StrategicMergePatch(baseJSON, patchJSON, schema)
	if err != nil {
		return nil, fmt.Errorf("strategic-merge: %w", err)
	}

	var merged map[string]interface{}
	if err := json.Unmarshal(mergedJSON, &merged); err != nil {
		return nil, fmt.Errorf("unmarshal merged: %w", err)
	}

	return &unstructured.Unstructured{Object: merged}, nil
}

// ParsePatch validates that patchYAML is well-formed YAML and can be
// structurally applied as a strategic-merge patch against schema, WITHOUT
// applying it to any base. A nil/empty patch is valid. Returns a typed error
// on malformed YAML or structural errors detected by the strategic merge
// library (e.g. $setElementOrder on a non-list field).
func ParsePatch(patchYAML []byte, schema any) error {
	if len(patchYAML) == 0 {
		return nil
	}

	patchJSON, err := yaml.YAMLToJSON(patchYAML)
	if err != nil {
		return fmt.Errorf("malformed YAML: %w", err)
	}

	// Apply against an empty object — a patch that can't apply to "{}" under
	// the schema is structurally invalid.
	if _, err := strategicpatch.StrategicMergePatch([]byte("{}"), patchJSON, schema); err != nil {
		return fmt.Errorf("invalid patch: %w", err)
	}

	return nil
}
