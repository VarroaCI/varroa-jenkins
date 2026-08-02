package api

import (
	"encoding/json"
	"testing"
)

// TestPatchWithinManageScope locks the permission boundary: the "manage"
// permission may only patch spec.powerState / spec.ingressSpec via the generic
// PATCH endpoint. Anything else must require "update".
func TestPatchWithinManageScope(t *testing.T) {
	tests := []struct {
		name  string
		patch string
		want  bool
	}{
		{"power off", `{"spec":{"powerState":"Stopped"}}`, true},
		{"power on", `{"spec":{"powerState":"Running"}}`, true},
		{"ingress", `{"spec":{"ingressSpec":{"host":"x.example.com"}}}`, true},
		{"power and ingress", `{"spec":{"powerState":"Stopped","ingressSpec":{"host":"x"}}}`, true},
		{"identity fields tolerated", `{"apiVersion":"varroa.dev/v1alpha1","kind":"Controller","spec":{"powerState":"Stopped"}}`, true},
		{"empty patch", `{}`, true},

		{"version change rejected", `{"spec":{"version":"2.500"}}`, false},
		{"resources change rejected", `{"spec":{"resources":{"cpu":"4"}}}`, false},
		{"bundle change rejected", `{"spec":{"bundleRef":{"repoURL":"evil"}}}`, false},
		{"power plus forbidden field rejected", `{"spec":{"powerState":"Stopped","version":"2.500"}}`, false},
		{"metadata change rejected", `{"metadata":{"labels":{"x":"y"}}}`, false},
		{"unknown top-level rejected", `{"status":{"phase":"Running"}}`, false},
		{"podOverrides rejected", `{"spec":{"podOverrides":{"env":[{"name":"X","value":"y"}]}}}`, false},
		{"resourceOverlay rejected", `{"spec":{"resourceOverlay":{"statefulSet":"...YAML..."}}}`, false},
		{"ingress annotations rejected", `{"spec":{"ingressSpec":{"annotations":{"nginx.ingress.kubernetes.io/server-snippet":"..."}}}}`, false},
		{"ingress class rejected", `{"spec":{"ingressSpec":{"ingressClassName":"traefik"}}}`, false},
		{"ingress host plus annotations rejected", `{"spec":{"ingressSpec":{"host":"x","annotations":{"a":"b"}}}}`, false},
		{"ingress removal tolerated", `{"spec":{"ingressSpec":null}}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var patch map[string]interface{}
			if err := json.Unmarshal([]byte(tt.patch), &patch); err != nil {
				t.Fatalf("bad test patch: %v", err)
			}
			if got := patchWithinManageScope(patch); got != tt.want {
				t.Errorf("patchWithinManageScope(%s) = %v, want %v", tt.patch, got, tt.want)
			}
		})
	}
}
