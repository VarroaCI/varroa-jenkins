package overlay

import (
	"encoding/json"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// Exported canonical name sets — single source of truth.
// The operator imports these rather than defining its own literals.
// ---------------------------------------------------------------------------

const (
	// JenkinsContainerName is the canonical name of the Jenkins container
	// within the StatefulSet pod template.
	JenkinsContainerName = "jenkins"
	// MiteContainerName is the canonical name of the mite sidecar container.
	MiteContainerName = "mite"
)

var (
	// ManagedVolumeNames lists the operator-managed pod volumes that overlays
	// should not modify (warn-but-allow).
	ManagedVolumeNames = []string{
		"jenkins-home",
		"init-scripts",
		"casc-config",
		"casc-bundle",
		"bootstrap",
		"varroa-run",
		"plugins",
	}

	// ManagedInitContainers lists the operator-managed init containers that
	// overlays should not modify (warn-but-allow).
	ManagedInitContainers = []string{
		"plugins-init",
		"init-groovy",
		"casc-seed",
	}
)

// protectedEnvPrefixes and protectedEnvExact are the env-var name patterns
// that overlays should not modify.
var (
	protectedEnvPrefixes = []string{"VARROA_"}
	protectedEnvExact    = map[string]bool{
		"CASC_JENKINS_CONFIG": true,
		"JENKINS_OPTS":        true,
		"JAVA_OPTS":           true,
	}
)

// managedPortNames lists the operator-managed Service port names.
var managedPortNames = map[string]bool{
	"http":  true,
	"agent": true,
}

// ---------------------------------------------------------------------------
// Warning
// ---------------------------------------------------------------------------

// Warning records a warn-but-allow guardrail hit where an overlay or
// podOverrides edit touched an operator-managed path.
type Warning struct {
	Resource string `json:"resource"`
	Path     string `json:"path"`
	Message  string `json:"message"`
}

// ---------------------------------------------------------------------------
// ScanProtected
// ---------------------------------------------------------------------------

// ScanProtected inspects the pre-merge patch maps for edits touching
// operator-managed paths and returns warnings. It NEVER errors and NEVER
// blocks (warn-but-allow). It inspects the PATCH, not the merged result.
//
// statefulSetPatch is the UNION of the compiled podOverrides patch and the
// raw statefulSet overlay — the caller deep-merges those two patch maps
// (overlay over podOverrides) before passing them in. service/ingress patches
// are the raw overlays parsed to maps.
func ScanProtected(statefulSetPatch, servicePatch, ingressPatch map[string]interface{}) []Warning {
	sts := scanStatefulSetPatch(statefulSetPatch)
	svc := scanServicePatch(servicePatch)
	ing := scanIngressPatch(ingressPatch)

	warnings := make([]Warning, 0, len(sts)+len(svc)+len(ing))
	warnings = append(warnings, sts...)
	warnings = append(warnings, svc...)
	warnings = append(warnings, ing...)

	return warnings
}

// ---------------------------------------------------------------------------
// StatefulSet scans
// ---------------------------------------------------------------------------

func scanStatefulSetPatch(patch map[string]interface{}) []Warning {
	if patch == nil {
		return nil
	}
	var warns []Warning

	// metadata.ownerReferences
	if meta, _ := patch["metadata"].(map[string]interface{}); meta != nil {
		if _, ok := meta["ownerReferences"]; ok {
			warns = append(warns, Warning{
				Resource: "statefulSet",
				Path:     "metadata.ownerReferences",
				Message:  "overrides ownerReferences (GC/ownership)",
			})
		}
	}

	// spec.selector — immutable
	if spec, _ := patch["spec"].(map[string]interface{}); spec != nil {
		if _, ok := spec["selector"]; ok {
			warns = append(warns, Warning{
				Resource: "statefulSet",
				Path:     "spec.selector",
				Message:  "modifies the StatefulSet selector (immutable)",
			})
		}

		// spec.replicas — managed by powerState
		if _, ok := spec["replicas"]; ok {
			warns = append(warns, Warning{
				Resource: "statefulSet",
				Path:     "spec.replicas",
				Message:  "overrides replicas (managed by powerState)",
			})
		}
	}

	// spec.template.spec.containers — check mite, env, container names
	warns = append(warns, scanContainersInPatch(patch)...)

	// spec.template.spec.initContainers — check managed names
	warns = append(warns, scanInitContainersInPatch(patch)...)

	// spec.template.spec.volumes — check managed names
	warns = append(warns, scanVolumesInPatch(patch)...)

	return warns
}

func scanContainersInPatch(patch map[string]interface{}) []Warning {
	spec, _ := patch["spec"].(map[string]interface{})
	if spec == nil {
		return nil
	}
	tmpl, _ := spec["template"].(map[string]interface{})
	if tmpl == nil {
		return nil
	}
	podSpec, _ := tmpl["spec"].(map[string]interface{})
	if podSpec == nil {
		return nil
	}

	containers, _ := podSpec["containers"].([]interface{})
	if len(containers) == 0 {
		return nil
	}

	var warns []Warning

	for i, c := range containers {
		cm, _ := c.(map[string]interface{})
		if cm == nil {
			continue
		}
		name, _ := cm["name"].(string)

		// Mite sidecar edit check.
		if name == MiteContainerName {
			warns = append(warns, Warning{
				Resource: "statefulSet",
				Path:     fmt.Sprintf("spec.template.spec.containers[%d]", i),
				Message:  "overrides the operator-managed mite sidecar",
			})
		}

		// Container rename check for index 0/1 (jenkins/mite).
		if i == 0 && name != JenkinsContainerName {
			warns = append(warns, Warning{
				Resource: "statefulSet",
				Path:     fmt.Sprintf("spec.template.spec.containers[%d].name", i),
				Message:  fmt.Sprintf("renames the jenkins container (name=%q)", name),
			})
		}
		if i == 1 && name != MiteContainerName {
			warns = append(warns, Warning{
				Resource: "statefulSet",
				Path:     fmt.Sprintf("spec.template.spec.containers[%d].name", i),
				Message:  fmt.Sprintf("renames the mite container (name=%q)", name),
			})
		}

		// Protected env vars check (only on the jenkins container).
		if name == JenkinsContainerName {
			warns = append(warns, scanProtectedEnv(cm)...)
		}
	}

	return warns
}

func scanProtectedEnv(container map[string]interface{}) []Warning {
	envList, _ := container["env"].([]interface{})
	if len(envList) == 0 {
		return nil
	}
	var warns []Warning
	for _, e := range envList {
		em, _ := e.(map[string]interface{})
		if em == nil {
			continue
		}
		name, _ := em["name"].(string)
		if name == "" {
			continue
		}
		if protectedEnvExact[name] {
			warns = append(warns, Warning{
				Resource: "statefulSet",
				Path:     fmt.Sprintf("spec.template.spec.containers[name=jenkins].env[%s]", name),
				Message:  fmt.Sprintf("overrides operator-managed env var %s", name),
			})
			continue
		}
		for _, prefix := range protectedEnvPrefixes {
			if strings.HasPrefix(name, prefix) {
				warns = append(warns, Warning{
					Resource: "statefulSet",
					Path:     fmt.Sprintf("spec.template.spec.containers[name=jenkins].env[%s]", name),
					Message:  fmt.Sprintf("overrides operator-managed env var %s", name),
				})
				break
			}
		}
	}
	return warns
}

func scanInitContainersInPatch(patch map[string]interface{}) []Warning {
	spec, _ := patch["spec"].(map[string]interface{})
	if spec == nil {
		return nil
	}
	tmpl, _ := spec["template"].(map[string]interface{})
	if tmpl == nil {
		return nil
	}
	podSpec, _ := tmpl["spec"].(map[string]interface{})
	if podSpec == nil {
		return nil
	}

	initContainers, _ := podSpec["initContainers"].([]interface{})
	if len(initContainers) == 0 {
		return nil
	}

	managedSet := make(map[string]bool, len(ManagedInitContainers))
	for _, n := range ManagedInitContainers {
		managedSet[n] = true
	}

	var warns []Warning
	for _, c := range initContainers {
		cm, _ := c.(map[string]interface{})
		if cm == nil {
			continue
		}
		name, _ := cm["name"].(string)
		if managedSet[name] {
			warns = append(warns, Warning{
				Resource: "statefulSet",
				Path:     fmt.Sprintf("spec.template.spec.initContainers[name=%s]", name),
				Message:  fmt.Sprintf("overrides the operator-managed init container %s", name),
			})
		}
	}
	return warns
}

func scanVolumesInPatch(patch map[string]interface{}) []Warning {
	spec, _ := patch["spec"].(map[string]interface{})
	if spec == nil {
		return nil
	}
	tmpl, _ := spec["template"].(map[string]interface{})
	if tmpl == nil {
		return nil
	}
	podSpec, _ := tmpl["spec"].(map[string]interface{})
	if podSpec == nil {
		return nil
	}

	volumes, _ := podSpec["volumes"].([]interface{})
	if len(volumes) == 0 {
		return nil
	}

	managedSet := make(map[string]bool, len(ManagedVolumeNames))
	for _, n := range ManagedVolumeNames {
		managedSet[n] = true
	}

	var warns []Warning
	for _, v := range volumes {
		vm, _ := v.(map[string]interface{})
		if vm == nil {
			continue
		}
		name, _ := vm["name"].(string)
		if managedSet[name] {
			warns = append(warns, Warning{
				Resource: "statefulSet",
				Path:     fmt.Sprintf("spec.template.spec.volumes[name=%s]", name),
				Message:  fmt.Sprintf("overrides operator-managed volume %s", name),
			})
		}
	}
	return warns
}

// ---------------------------------------------------------------------------
// Service scans
// ---------------------------------------------------------------------------

func scanServicePatch(patch map[string]interface{}) []Warning {
	if patch == nil {
		return nil
	}
	var warns []Warning

	// metadata.ownerReferences
	if meta, _ := patch["metadata"].(map[string]interface{}); meta != nil {
		if _, ok := meta["ownerReferences"]; ok {
			warns = append(warns, Warning{
				Resource: "service",
				Path:     "metadata.ownerReferences",
				Message:  "overrides ownerReferences",
			})
		}
	}

	// spec.selector
	if spec, _ := patch["spec"].(map[string]interface{}); spec != nil {
		if _, ok := spec["selector"]; ok {
			warns = append(warns, Warning{
				Resource: "service",
				Path:     "spec.selector",
				Message:  "overrides the operator-managed Service selector",
			})
		}

		// Managed ports (by name).
		if ports, _ := spec["ports"].([]interface{}); len(ports) > 0 {
			for _, p := range ports {
				pm, _ := p.(map[string]interface{})
				if pm == nil {
					continue
				}
				name, _ := pm["name"].(string)
				if managedPortNames[name] {
					warns = append(warns, Warning{
						Resource: "service",
						Path:     fmt.Sprintf("spec.ports[name=%s]", name),
						Message:  "overrides an operator-managed Service port",
					})
				}
			}
		}
	}

	return warns
}

// ---------------------------------------------------------------------------
// Ingress scans
// ---------------------------------------------------------------------------

func scanIngressPatch(patch map[string]interface{}) []Warning {
	if patch == nil {
		return nil
	}
	var warns []Warning

	// metadata.ownerReferences
	if meta, _ := patch["metadata"].(map[string]interface{}); meta != nil {
		if _, ok := meta["ownerReferences"]; ok {
			warns = append(warns, Warning{
				Resource: "ingress",
				Path:     "metadata.ownerReferences",
				Message:  "overrides ownerReferences",
			})
		}
	}

	// Managed rule (host/path) — best-effort: we check the first rule's host
	// and path for any change. A precise IngressSpec-derived match is deferred
	// to the operator wiring (scanning for any rule in the patch).
	if spec, _ := patch["spec"].(map[string]interface{}); spec != nil {
		if rules, _ := spec["rules"].([]interface{}); len(rules) > 0 {
			// Any rule present in the patch could modify the managed rule.
			warns = append(warns, Warning{
				Resource: "ingress",
				Path:     "spec.rules",
				Message:  "overrides the operator-managed Ingress routing rule",
			})
		}
		if _, ok := spec["tls"]; ok {
			warns = append(warns, Warning{
				Resource: "ingress",
				Path:     "spec.tls",
				Message:  "overrides the operator-managed Ingress TLS",
			})
		}
	}

	return warns
}

// ---------------------------------------------------------------------------
// ScanOverrides — convenience for the operator
// ---------------------------------------------------------------------------

// ScanOverrides compiles and scans a Controller's podOverrides + resourceOverlay
// for protected-path edits, returning any warnings detected. It never blocks
// (warn-but-allow). Malformed YAML in an overlay returns an error.
//
// A nil/nil input pair returns nil warnings with no error.
func ScanOverrides(po *v1alpha1.PodOverrides, ro *v1alpha1.ResourceOverlay) ([]Warning, error) {
	// Compile podOverrides to a map.
	var stsPatch map[string]interface{}

	if po != nil {
		yamlBytes, err := CompilePodOverrides(po, JenkinsContainerName)
		if err != nil {
			return nil, fmt.Errorf("compile podOverrides: %w", err)
		}
		if len(yamlBytes) > 0 {
			jsonBytes, err := yaml.YAMLToJSON(yamlBytes)
			if err != nil {
				return nil, fmt.Errorf("podOverrides yaml-to-json: %w", err)
			}
			if err := json.Unmarshal(jsonBytes, &stsPatch); err != nil {
				return nil, fmt.Errorf("podOverrides unmarshal: %w", err)
			}
		}
	}

	// Parse resourceOverlay YAML strings to maps.
	var svcMap, ingMap map[string]interface{}

	if ro != nil {
		if ro.StatefulSet != "" {
			m, err := yamlToMap(ro.StatefulSet)
			if err != nil {
				return nil, fmt.Errorf("resourceOverlay.statefulSet: %w", err)
			}
			stsPatch = deepMergeMaps(stsPatch, m)
		}
		if ro.Service != "" {
			m, err := yamlToMap(ro.Service)
			if err != nil {
				return nil, fmt.Errorf("resourceOverlay.service: %w", err)
			}
			svcMap = m
		}
		if ro.Ingress != "" {
			m, err := yamlToMap(ro.Ingress)
			if err != nil {
				return nil, fmt.Errorf("resourceOverlay.ingress: %w", err)
			}
			ingMap = m
		}
	}

	warns := ScanProtected(stsPatch, svcMap, ingMap)
	return warns, nil
}

// yamlToMap converts a YAML string to a map[string]interface{}.
func yamlToMap(yamlStr string) (map[string]interface{}, error) {
	jsonBytes, err := yaml.YAMLToJSON([]byte(yamlStr))
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// deepMergeMaps merges src into dst for top-level keys only (shallow merge is
// sufficient for patch maps — the strategic-merge patch library handles deeper
// list merging). src keys overwrite dst keys.
func deepMergeMaps(dst, src map[string]interface{}) map[string]interface{} {
	if dst == nil {
		return src
	}
	if src == nil {
		return dst
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
