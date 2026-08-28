package overlay

import (
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// CompilePodOverrides renders the typed PodOverrides into a StatefulSet-shaped
// strategic-merge patch (YAML bytes) targeting the container named
// jenkinsContainerName and the pod template. Returns (nil, nil) for a nil input.
//
// jvmOpts is NOT emitted (it is handled directly in the StatefulSet builder).
// The compiled patch has empty apiVersion/kind stripped as a defensive measure
// (see spike findings §3 — typed structs with omitempty already omit them, but
// this guards against future changes to TypeMeta).
func CompilePodOverrides(po *v1alpha1.PodOverrides, jenkinsContainerName string) ([]byte, error) {
	if po == nil {
		return nil, nil
	}

	// Determine what's populated (skip jvmOpts — handled by the builder;
	// probes moved to ProbesSpec, compiled separately).
	hasContainerFields := len(po.Env) > 0 || len(po.EnvFrom) > 0 ||
		len(po.VolumeMounts) > 0
	hasPodSpecFields := len(po.Volumes) > 0 || len(po.NodeSelector) > 0 ||
		len(po.Tolerations) > 0 || po.Affinity != nil || po.SecurityContext != nil
	hasPodMetaFields := len(po.PodLabels) > 0 || len(po.PodAnnotations) > 0
	hasStsMetaFields := len(po.Labels) > 0 || len(po.Annotations) > 0

	if !hasContainerFields && !hasPodSpecFields && !hasPodMetaFields && !hasStsMetaFields {
		return nil, nil
	}

	// Build a minimal StatefulSet patch with only the populated fields.
	sts := &appsv1.StatefulSet{}

	// StatefulSet metadata-level overrides.
	if hasStsMetaFields {
		sts.ObjectMeta = metav1.ObjectMeta{
			Labels:      po.Labels,
			Annotations: po.Annotations,
		}
	}

	// Always set Spec.Template when any pod-level field is present.
	if hasContainerFields || hasPodSpecFields || hasPodMetaFields {
		sts.Spec.Template = corev1.PodTemplateSpec{}
	}

	// Pod template metadata.
	if hasPodMetaFields {
		sts.Spec.Template.ObjectMeta = metav1.ObjectMeta{
			Labels:      po.PodLabels,
			Annotations: po.PodAnnotations,
		}
	}

	// Pod spec-level fields.
	podSpec := &sts.Spec.Template.Spec

	if len(po.Volumes) > 0 {
		podSpec.Volumes = po.Volumes
	}
	if len(po.NodeSelector) > 0 {
		podSpec.NodeSelector = po.NodeSelector
	}
	if len(po.Tolerations) > 0 {
		podSpec.Tolerations = po.Tolerations
	}
	if po.Affinity != nil {
		podSpec.Affinity = po.Affinity
	}
	if po.SecurityContext != nil {
		podSpec.SecurityContext = po.SecurityContext
	}

	// Container-scoped fields (targeted by name so strategic merge matches the
	// existing jenkins container).
	if hasContainerFields {
		container := corev1.Container{Name: jenkinsContainerName}
		if len(po.Env) > 0 {
			container.Env = po.Env
		}
		if len(po.EnvFrom) > 0 {
			container.EnvFrom = po.EnvFrom
		}
		if len(po.VolumeMounts) > 0 {
			container.VolumeMounts = po.VolumeMounts
		}
		podSpec.Containers = []corev1.Container{container}
	}

	// Marshal to JSON, strip apiVersion/kind, convert to YAML.
	jsonBytes, err := json.Marshal(sts)
	if err != nil {
		return nil, fmt.Errorf("marshal podOverrides patch: %w", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &m); err != nil {
		return nil, fmt.Errorf("unmarshal podOverrides patch: %w", err)
	}

	// Strip apiVersion and kind (defensive — they'd be empty/zero from a bare
	// struct but could be present from a derived struct).
	delete(m, "apiVersion")
	delete(m, "kind")

	// Clean up empty/null sub-objects that come from zero-value struct fields.
	// This keeps the patch minimal and avoids blanking base fields with nulls.
	stripEmptyNils(m)

	if len(m) == 0 {
		return nil, nil
	}

	cleanedJSON, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal cleaned podOverrides patch: %w", err)
	}

	yamlBytes, err := yaml.JSONToYAML(cleanedJSON)
	if err != nil {
		return nil, fmt.Errorf("json-to-yaml: %w", err)
	}

	return yamlBytes, nil
}

// stripEmptyNils recursively removes map entries whose values are nil, empty
// maps, empty slices, empty strings, zero numbers, or false booleans. It also
// recurses into slice elements (maps inside arrays) to clean nested content.
// Used to clean up artifacts from marshaling a partially-populated typed struct.
func stripEmptyNils(m map[string]interface{}) {
	for k, v := range m {
		switch val := v.(type) {
		case nil:
			delete(m, k)
		case map[string]interface{}:
			stripEmptyNils(val)
			if len(val) == 0 {
				delete(m, k)
			}
		case []interface{}:
			// Recurse into map elements inside slices.
			for i, item := range val {
				if itemMap, ok := item.(map[string]interface{}); ok {
					stripEmptyNils(itemMap)
					val[i] = itemMap
				}
			}
			if len(val) == 0 {
				delete(m, k)
			}
		case string:
			if val == "" {
				delete(m, k)
			}
		case float64:
			if val == 0 {
				delete(m, k)
			}
		case bool:
			if !val {
				delete(m, k)
			}
		}
	}
}
