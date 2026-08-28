package api

import "encoding/json"

// Sensitive and churn field paths stripped from every API object leaving the
// BFF, whether over REST or MCP.
//
// The secret values are the load-bearing half: status.wakeToken is the sole
// credential on the deliberately-unauthenticated /hibernation/{token}/...
// surface, and the User credential fields are a plaintext password (write-only,
// cleared by the reconciler, but live between write and reconcile) and its
// hash. None may ever appear in a response.
//
// The metadata fields are noise reduction: managedFields in particular can
// dominate a serialized CR, and no consumer of these surfaces performs
// optimistic concurrency — MCP's update_controller applies under a named
// field manager via server-side apply, not a resourceVersion precondition.
var (
	strippedMetadata = []string{"managedFields", "resourceVersion", "uid", "generation"}
)

// SanitizeObject round-trips v through JSON and removes credential and churn
// fields from the resulting object, or from each element when v encodes a
// collection. Scalars and other non-object payloads pass through unchanged.
//
// It is deliberately path-precise rather than a recursive key sweep: a blanket
// "delete any key named password" would also strip legitimate reference fields
// (secretRef, existingSecret, tlsSecretName) that callers need. Only the three
// paths that carry an actual secret value are removed.
//
// Sanitization is idempotent — applying it to an already-sanitized object is a
// no-op — so layering it at more than one point in a response path is safe.
func SanitizeObject(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(b, &decoded); err != nil {
		return nil, err
	}
	return sanitizeValue(decoded), nil
}

// sanitizeValue dispatches on the decoded shape: a collection has each member
// sanitized, an object is sanitized in place, anything else is returned as-is.
func sanitizeValue(v any) any {
	switch t := v.(type) {
	case []any:
		for i, elem := range t {
			t[i] = sanitizeValue(elem)
		}
		return t
	case map[string]any:
		sanitizeObjectMap(t)
		return t
	default:
		return v
	}
}

// sanitizeObjectMap strips the known paths from a single decoded API object.
func sanitizeObjectMap(obj map[string]any) {
	if meta, ok := obj["metadata"].(map[string]any); ok {
		for _, k := range strippedMetadata {
			delete(meta, k)
		}
	}
	if spec, ok := obj["spec"].(map[string]any); ok {
		// UserSpec.Password: write-only, echoed back until the reconciler
		// hashes and clears it.
		delete(spec, "password")
	}
	if status, ok := obj["status"].(map[string]any); ok {
		delete(status, "wakeToken")
		if creds, ok := status["credentials"].(map[string]any); ok {
			delete(creds, "passwordHash")
		}
	}
}
