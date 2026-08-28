package overlay

import (
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// scalarToString normalizes a decoded YAML scalar into its string form for a
// resource-quantity-like field (cpu/memory). sigs.k8s.io/yaml round-trips
// YAML through JSON, so an unquoted numeric value in the overlay (e.g.
// `cpu: 1` or `memory: 2`) decodes into a Go float64, not a string — a plain
// `.(string)` type assertion silently misses it and the field reads as "no
// override" even though the overlay declares one. That false negative lets
// the overlay-declared value get ignored, so the drift check compares the
// live template against the wrong desired value and can loop trying to
// re-apply an override that was already applied. strconv.FormatFloat with
// -1 precision reproduces the YAML source's own digits (1 -> "1", 2.5 ->
// "2.5") without going through resource.Quantity math, so it stays stable
// across reconciles (same input always yields the same output) rather than
// introducing its own formatting drift.
func scalarToString(v interface{}) (s string, ok bool) {
	switch t := v.(type) {
	case string:
		return t, t != ""
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case int:
		return strconv.Itoa(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	default:
		return "", false
	}
}

// findOverlayContainer parses a raw StatefulSet strategic-merge overlay patch
// and returns the container entry named containerName under
// spec.template.spec.containers, or nil (ok=false) if the overlay doesn't
// declare that container. A YAML parse error returns ok=false with the error;
// callers treat that as "no override" (the same YAML hard-fails later in
// CreateStatefulSet's Merge, which owns overlay validity).
func findOverlayContainer(patchYAML []byte, containerName string) (ctr map[string]interface{}, ok bool, err error) {
	if len(patchYAML) == 0 {
		return nil, false, nil
	}
	var root map[string]interface{}
	if err := yaml.Unmarshal(patchYAML, &root); err != nil {
		return nil, false, err
	}
	spec, _ := root["spec"].(map[string]interface{})
	if spec == nil {
		return nil, false, nil
	}
	template, _ := spec["template"].(map[string]interface{})
	if template == nil {
		return nil, false, nil
	}
	podSpec, _ := template["spec"].(map[string]interface{})
	if podSpec == nil {
		return nil, false, nil
	}
	containers, _ := podSpec["containers"].([]interface{})
	if containers == nil {
		return nil, false, nil
	}
	for _, c := range containers {
		m, _ := c.(map[string]interface{})
		if m == nil {
			continue
		}
		if name, _ := m["name"].(string); name == containerName {
			return m, true, nil
		}
	}
	return nil, false, nil
}

// ImageOverride reports the image a raw StatefulSet strategic-merge overlay
// declares for the named container (spec.template.spec.containers[name==X].image),
// or ok=false when the overlay does not set one.
func ImageOverride(patchYAML []byte, containerName string) (image string, ok bool, err error) {
	ctr, found, err := findOverlayContainer(patchYAML, containerName)
	if err != nil || !found {
		return "", false, err
	}
	img, _ := ctr["image"].(string)
	if img == "" {
		return "", false, nil
	}
	return img, true, nil
}

// resourceKeys is the set of ResourceName keys this package recognises for
// per-key parsing (cpu, memory). The dropAllRequests / dropAllLimits booleans
// returned by ResourcesOverride handle whole-map nulls as a true delete-all
// — independent of this list — so keys beyond cpu/memory (e.g.
// ephemeral-storage, nvidia.com/gpu) are also deleted when the overlay
// nulls an entire map.
var resourceKeys = []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory}

// ResourcesOverride reports the CPU and/or memory resource requests and
// limits a raw StatefulSet strategic-merge overlay declares for the named
// container (spec.template.spec.containers[name==X].resources.{requests,limits}
// .{cpu,memory}), plus any resource-name keys or whole sub-maps explicitly set
// to YAML null (which the strategic-merge applies as a DELETE directive — the
// key is removed from the live container). Three map-level nulls are handled:
//   - resources: null      → dropAllRequests=true, dropAllLimits=true
//   - requests: null       → dropAllRequests=true
//   - limits: null         → dropAllLimits=true
//
// dropAllRequests/dropAllLimits signal a TRUE delete-all: every key in the
// corresponding base map must be dropped regardless of name — the desired
// computation must NOT copy any base key into that map (and the live STS
// strategic-merge already deleted them). Per-key nullRequests/nullLimits
// lists handle single-key nulls (e.g. cpu: null) and compose with
// dropAll*: if a whole map is dropped, per-key nulls for that map are
// ignored (their work is already done). A parse failure for one key does
// not fail the whole override — that key is skipped (the caller logs the
// skip) and the rest are kept.
func ResourcesOverride(patchYAML []byte, containerName string) (rr *corev1.ResourceRequirements, nullRequests, nullLimits []corev1.ResourceName, dropAllRequests, dropAllLimits bool, ok bool, err error) {
	ctr, found, err := findOverlayContainer(patchYAML, containerName)
	if err != nil || !found {
		return nil, nil, nil, false, false, false, err
	}

	// resources: null (the key is present and its value is nil) → the
	// strategic-merge deletes the entire resources block for this container.
	if rawResources, hasResources := ctr["resources"]; hasResources && rawResources == nil {
		return nil, nil, nil, true, true, true, nil
	}

	resources, _ := ctr["resources"].(map[string]interface{})
	if resources == nil {
		return nil, nil, nil, false, false, false, nil
	}

	var requests, limits corev1.ResourceList

	// requests: null (key present, value nil) → delete every request key.
	if rawReqs, hasReqs := resources["requests"]; hasReqs && rawReqs == nil {
		dropAllRequests = true
	} else if reqMap, _ := resources["requests"].(map[string]interface{}); reqMap != nil {
		for _, key := range resourceKeys {
			raw, present := reqMap[string(key)]
			if !present {
				continue // key absent — no directive
			}
			if raw == nil { // explicit YAML null — DELETE directive
				nullRequests = append(nullRequests, key)
				continue
			}
			str, sok := scalarToString(raw)
			if !sok || str == "" {
				continue
			}
			q, err := resource.ParseQuantity(str)
			if err != nil {
				continue // per-key parse failure skips only that key
			}
			if requests == nil {
				requests = corev1.ResourceList{}
			}
			requests[key] = q
		}
	}

	// limits: null (key present, value nil) → delete every limit key.
	if rawLims, hasLims := resources["limits"]; hasLims && rawLims == nil {
		dropAllLimits = true
	} else if limMap, _ := resources["limits"].(map[string]interface{}); limMap != nil {
		for _, key := range resourceKeys {
			raw, present := limMap[string(key)]
			if !present {
				continue // key absent — no directive
			}
			if raw == nil { // explicit YAML null — DELETE directive
				nullLimits = append(nullLimits, key)
				continue
			}
			str, sok := scalarToString(raw)
			if !sok || str == "" {
				continue
			}
			q, err := resource.ParseQuantity(str)
			if err != nil {
				continue // per-key parse failure skips only that key
			}
			if limits == nil {
				limits = corev1.ResourceList{}
			}
			limits[key] = q
		}
	}

	hasSet := requests != nil || limits != nil
	hasDirective := hasSet || len(nullRequests) > 0 || len(nullLimits) > 0 || dropAllRequests || dropAllLimits
	if !hasDirective {
		return nil, nil, nil, false, false, false, nil
	}

	var rrVal *corev1.ResourceRequirements
	if hasSet {
		rrVal = &corev1.ResourceRequirements{Requests: requests, Limits: limits}
	}
	return rrVal, nullRequests, nullLimits, dropAllRequests, dropAllLimits, true, nil
}

// PullPolicyOverride reports the imagePullPolicy a raw StatefulSet
// strategic-merge overlay declares for the named container
// (spec.template.spec.containers[name==X].imagePullPolicy), or ok=false when
// the overlay does not set one.
func PullPolicyOverride(patchYAML []byte, containerName string) (pullPolicy string, ok bool, err error) {
	ctr, found, err := findOverlayContainer(patchYAML, containerName)
	if err != nil || !found {
		return "", false, err
	}
	pp, _ := ctr["imagePullPolicy"].(string)
	if pp == "" {
		return "", false, nil
	}
	return pp, true, nil
}
