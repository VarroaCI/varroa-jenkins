package bundle

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
)

// OCIAuth holds OCI registry credentials for bundle materialization.
type OCIAuth struct {
	Username string
	Password string
	Registry string
}

// dockerConfigJSON is the standard Docker config.json shape.
type dockerConfigJSON struct {
	Auths map[string]dockerAuthEntry `json:"auths"`
}

type dockerAuthEntry struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Auth     string `json:"auth,omitempty"`
}

// WriteDockerConfigJSON writes a temporary Docker config.json from the given
// OCIAuth and returns the file path. The caller must remove the file (or the
// parent temp dir) after use.
func WriteDockerConfigJSON(auth *OCIAuth) (string, error) {
	if auth == nil {
		return "", nil
	}

	registry := auth.Registry
	if registry == "" {
		return "", fmt.Errorf("OCIAuth: registry is required")
	}

	// Build the auth value: base64(username:password)
	authValue := base64.StdEncoding.EncodeToString([]byte(auth.Username + ":" + auth.Password))

	cfg := dockerConfigJSON{
		Auths: map[string]dockerAuthEntry{
			registry: {
				Username: auth.Username,
				Password: auth.Password,
				Auth:     authValue,
			},
		},
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal docker config: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "varroa-oci-auth-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir for docker config: %w", err)
	}

	configPath := tmpDir + "/config.json"
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", fmt.Errorf("write docker config.json: %w", err)
	}

	return configPath, nil
}

// dockerConfigJSONFromBytes parses a raw dockerconfigjson byte payload
// (the standard k8s .dockerconfigjson secret shape) and returns the
// registry, username, and password from the first auth entry found.
type dockerConfigJSONSecret struct {
	Auths map[string]dockerAuthEntry `json:"auths"`
}

// OCIAuthFromSecret reads a Kubernetes Secret (as raw byte map) and returns
// an OCIAuth. It sniffs .dockerconfigjson FIRST (standard k8s dockerconfigjson
// shape: {"auths":{"<reg>":{"username","password","auth"}}}), then falls back
// to username/password keys, else errors. Mirrors GitAuthFromSecret's
// key-sniffing structure.
func OCIAuthFromSecret(data map[string][]byte) (*OCIAuth, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("secret is empty")
	}

	// Sniff .dockerconfigjson FIRST (standard k8s docker config secret key).
	if raw, ok := data[".dockerconfigjson"]; ok && len(raw) > 0 {
		var parsed dockerConfigJSONSecret
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, fmt.Errorf("parse .dockerconfigjson: %w", err)
		}
		if len(parsed.Auths) == 0 {
			return nil, fmt.Errorf(".dockerconfigjson has no auth entries")
		}
		// A map has no defined iteration order, so more than one entry makes the
		// selected registry non-deterministic. Reject rather than guess.
		if len(parsed.Auths) > 1 {
			return nil, fmt.Errorf("secret has %d auth entries; exactly one registry is supported", len(parsed.Auths))
		}
		for registry, entry := range parsed.Auths {
			username := entry.Username
			password := entry.Password
			if username == "" && password == "" && entry.Auth != "" {
				// Decode base64-encoded "user:pass" from the auth field.
				decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
				if err != nil {
					return nil, fmt.Errorf("decode .dockerconfigjson auth for %q: %w", registry, err)
				}
				username, password = split2(string(decoded), ":")
			}
			return &OCIAuth{Username: username, Password: password, Registry: registry}, nil
		}
	}

	// Fallback: username/password keys.
	username := string(data["username"])
	password := string(data["password"])
	if password == "" {
		password = string(data["token"])
	}
	registry := string(data["registry"])
	if registry == "" {
		registry = string(data["server"])
	}
	if username != "" {
		return &OCIAuth{Username: username, Password: password, Registry: registry}, nil
	}

	return nil, fmt.Errorf("unsupported OCI credential secret shape: expected .dockerconfigjson or username key")
}

// split2 splits a string at the first occurrence of sep, returning both parts.
// If sep is not found, the second part is empty (matching Python/JavaScript semantics).
func split2(s, sep string) (string, string) {
	i := indexOf(s, sep)
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+len(sep):]
}

// indexOf returns the index of the first occurrence of sep in s, or -1.
func indexOf(s, sep string) int {
	for i := 0; i <= len(s)-len(sep); i++ {
		if s[i:i+len(sep)] == sep {
			return i
		}
	}
	return -1
}
