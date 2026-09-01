package bundle

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ValidateResult holds the result of bundle validation.
type ValidateResult struct {
	Valid    bool
	Errors   []string
	Warnings []string
}

// PluginPin is one entry of a bundle's plugins.yaml.
type PluginPin struct {
	ArtifactID string `yaml:"artifactId"`
	Version    string `yaml:"version"`
}

// ParsePluginPins unmarshals a bundle's plugins.yaml content into its list of
// plugin pins. It performs no validation of the entries themselves.
func ParsePluginPins(pluginsYAML string) ([]PluginPin, error) {
	var pc struct {
		Plugins []PluginPin `yaml:"plugins"`
	}
	if err := yaml.Unmarshal([]byte(pluginsYAML), &pc); err != nil {
		return nil, err
	}
	return pc.Plugins, nil
}

// varPattern matches ${VAR_NAME} placeholders.
var varPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// isVarroaVar reports whether a variable name belongs to the varroa_* family
// (reserved for per-controller injection).
func isVarroaVar(name string) bool {
	return strings.HasPrefix(name, "varroa_")
}

// jcascSecretSourcePrefixes are the built-in configuration-as-code-plugin
// secret-source prefixes. A ${...} placeholder whose contents start with one
// of these is resolved by JCasC itself at Jenkins startup, not by Varroa —
// it must reach Jenkins byte-for-byte. Unlike the "^${var}" literal-escape
// convention (which makes JCasC treat the value as inert literal text and
// would break these), this list only whitelists the check: content and
// resolution semantics are left untouched.
// The set matches casc's FixedInterpolatorStringLookup dispatch table, and
// matching is case-insensitive because commons-text lowercases the prefix
// before lookup (${ReadFile:...} works in Jenkins).
var jcascSecretSourcePrefixes = []string{
	"file:", "readfile:",
	"filebase64:", "readfilebase64:",
	"base64:", "decodebase64:",
	"json:", "sysprop:", "trim:",
}

// IsJCascSecretSourceRef reports whether name (the content of a ${...}
// placeholder) is a JCasC secret-source reference reserved for Jenkins'
// own configuration-as-code interpolation.
func IsJCascSecretSourceRef(name string) bool {
	i := strings.IndexByte(name, ':')
	if i < 0 {
		return false
	}
	prefix := strings.ToLower(name[:i+1])
	for _, p := range jcascSecretSourcePrefixes {
		if prefix == p {
			return true
		}
	}
	return false
}

// Validator validates a CasC bundle against the CloudBees bundle specification.
type Validator struct{}

// NewValidator creates a new Validator.
func NewValidator() *Validator {
	return &Validator{}
}

// Validate checks a bundle directory for correctness. The bundle must contain
// a valid bundle.yaml manifest that references all required files.
func (v *Validator) Validate(bundleDir string) *ValidateResult {
	result := &ValidateResult{Valid: true}

	// bundle.yaml is required
	bundleYAMLPath := filepath.Join(bundleDir, "bundle.yaml")
	if _, err := os.Stat(bundleYAMLPath); os.IsNotExist(err) {
		result.Valid = false
		result.Errors = append(result.Errors, "bundle.yaml is required but missing")
		return result
	}

	manifest, err := ParseManifest(bundleDir)
	if err != nil {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("invalid bundle.yaml: %s", err.Error()))
		return result
	}

	// All jcasc files must exist and be non-empty
	for _, p := range manifest.Jcasc {
		if err := validateFileContent(bundleDir, p); err != nil {
			result.Valid = false
			result.Errors = append(result.Errors, err.Error())
		}
	}

	// All referenced optional files must exist if declared
	for _, p := range manifest.Plugins {
		if err := v.checkFile(bundleDir, p); err != nil {
			result.Valid = false
			result.Errors = append(result.Errors, err.Error())
		}
	}
	for _, p := range manifest.Items {
		if err := v.checkFile(bundleDir, p); err != nil {
			result.Valid = false
			result.Errors = append(result.Errors, err.Error())
		}
	}
	for _, p := range manifest.Rbac {
		if err := v.checkFile(bundleDir, p); err != nil {
			result.Valid = false
			result.Errors = append(result.Errors, err.Error())
		}
	}
	for _, p := range manifest.Variables {
		if err := v.checkFile(bundleDir, p); err != nil {
			result.Valid = false
			result.Errors = append(result.Errors, err.Error())
		}
	}

	// Warn if no variables files are declared
	if len(manifest.Variables) == 0 {
		result.Warnings = append(result.Warnings, "no variables.yaml referenced in bundle.yaml — using defaults only")
	}

	return result
}

func (v *Validator) checkFile(bundleDir, name string) error {
	path := filepath.Join(bundleDir, name)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("referenced file %q is missing", name)
	}
	return nil
}

func validateFileContent(bundleDir, path string) error {
	fullPath := filepath.Join(bundleDir, path)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return fmt.Errorf("required file %q is missing", path)
	}
	if len(data) == 0 {
		return fmt.Errorf("required file %q is empty", path)
	}
	return nil
}

// ValidateContent runs the materialize-time validation floor on merged bundle
// content. It checks: YAML parseability of each section, plugin entry
// well-formedness, and unresolved ${var} placeholders (excluding the varroa_*
// family). Returns structured errors and warnings.
func ValidateContent(jenkinsYAML, pluginsYAML, itemsYAML, rbacYAML string, definedVars Variables) *ValidateResult {
	result := &ValidateResult{Valid: true}

	// Build set of defined variable names (from variable files and spec).
	definedNames := make(map[string]bool)
	for k := range definedVars {
		definedNames[k] = true
	}

	// Check JCasC YAML parseability.
	if strings.TrimSpace(jenkinsYAML) != "" {
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(jenkinsYAML), &doc); err != nil {
			result.Valid = false
			result.Errors = append(result.Errors, fmt.Sprintf("jcasc: invalid YAML: %v", err))
		}
	}

	// Check plugins well-formedness.
	if strings.TrimSpace(pluginsYAML) != "" {
		pins, err := ParsePluginPins(pluginsYAML)
		if err != nil {
			result.Valid = false
			result.Errors = append(result.Errors, fmt.Sprintf("plugins: invalid YAML: %v", err))
		} else {
			for i, p := range pins {
				if p.ArtifactID == "" {
					result.Valid = false
					result.Errors = append(result.Errors, fmt.Sprintf("plugins[%d]: artifactId is required", i))
				}
				switch p.Version {
				case "":
					result.Valid = false
					result.Errors = append(result.Errors, fmt.Sprintf("plugins[%d] (%s): version is required (pin an exact version)", i, p.ArtifactID))
				case "latest":
					result.Warnings = append(result.Warnings, fmt.Sprintf("plugins[%d] (%s): version \"latest\" is non-deterministic; pin an exact version", i, p.ArtifactID))
				}
			}
		}
	}

	// Check items YAML parseability.
	if strings.TrimSpace(itemsYAML) != "" {
		var ic struct {
			Items []map[string]any `yaml:"items"`
		}
		if err := yaml.Unmarshal([]byte(itemsYAML), &ic); err != nil {
			result.Valid = false
			result.Errors = append(result.Errors, fmt.Sprintf("items: invalid YAML: %v", err))
		}
	}

	// Check RBAC YAML parseability.
	if strings.TrimSpace(rbacYAML) != "" {
		var rc struct {
			Roles map[string]any `yaml:"roles"`
		}
		if err := yaml.Unmarshal([]byte(rbacYAML), &rc); err != nil {
			result.Valid = false
			result.Errors = append(result.Errors, fmt.Sprintf("rbac: invalid YAML: %v", err))
		}
	}

	// Detect unresolved ${var} placeholders.
	// Exclude varroa_* family (reserved for per-controller injection) and
	// JCasC literal escapes (^${var}), which Jenkins resolves/escapes itself.
	seen := make(map[string]bool)
	scan := func(content string, allowJcascSecretSources bool) {
		for _, loc := range varPattern.FindAllStringSubmatchIndex(content, -1) {
			start := loc[0]
			varName := content[loc[2]:loc[3]]
			if start > 0 && content[start-1] == '^' {
				continue // JCasC literal escape: ^${var} is not ours to resolve
			}
			if isVarroaVar(varName) {
				continue // allowed at materialize time
			}
			if allowJcascSecretSources && IsJCascSecretSourceRef(varName) {
				continue // JCasC secret source (e.g. readFile:/base64:); Jenkins resolves it, not us
			}
			if definedNames[varName] {
				continue // defined by bundle variables
			}
			if seen[varName] {
				continue
			}
			seen[varName] = true
			result.Valid = false
			result.Errors = append(result.Errors, fmt.Sprintf("unresolved variable: ${%s}", varName))
		}
	}
	// jenkins.yaml and rbac.yaml are applied through configuration-as-code,
	// where SecretSourceResolver interpolates ${readFile:...}-style refs at
	// Jenkins startup. plugins.yaml and items.yaml never see JCasC
	// interpolation, so a secret-source ref there is a genuine unresolved
	// variable and still fails validation.
	scan(strings.Join([]string{jenkinsYAML, rbacYAML}, "\n"), true)
	scan(strings.Join([]string{pluginsYAML, itemsYAML}, "\n"), false)

	return result
}
