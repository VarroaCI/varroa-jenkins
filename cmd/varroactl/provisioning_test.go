package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Provisioning tests (7.2)
// ---------------------------------------------------------------------------

func TestProvisioning_DefaultsDefaultName(t *testing.T) {
	testSetup(t)
	var path string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"metadata":{"name":"varroa-defaults"},"spec":{"key":"value"}}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "provisioningdefaults"})
	if err := root.Execute(); err != nil {
		t.Fatalf("get provisioningdefaults failed: %v", err)
	}
	if !strings.HasSuffix(path, "/api/v1/clusters/core/provisioningdefaults/varroa-defaults") {
		t.Errorf("expected default URL suffix /api/v1/clusters/core/provisioningdefaults/varroa-defaults, got %s", path)
	}
}

func TestProvisioning_DefaultsExplicitName(t *testing.T) {
	testSetup(t)
	var path string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"metadata":{"name":"my-defaults"},"spec":{"key":"value"}}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "provisioningdefaults", "my-defaults"})
	if err := root.Execute(); err != nil {
		t.Fatalf("get provisioningdefaults my-defaults failed: %v", err)
	}
	if !strings.HasSuffix(path, "/api/v1/clusters/core/provisioningdefaults/my-defaults") {
		t.Errorf("expected URL suffix /api/v1/clusters/core/provisioningdefaults/my-defaults, got %s", path)
	}
}

func TestProvisioning_VersionProfileFilterHit(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/v1/clusters/core/version-profiles") {
			t.Errorf("expected /api/v1/clusters/core/version-profiles, got %s", r.URL.Path)
		}
		_, _ = fmt.Fprint(w, `{"items":[{"name":"v2.0","version":"2.0","channel":"stable","pluginCount":3},{"name":"v1.0","version":"1.0","channel":"stable","pluginCount":1}]}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "versionprofiles", "v2.0"})
	if err := root.Execute(); err != nil {
		t.Fatalf("get versionprofiles v2.0 failed: %v", err)
	}
}

func TestProvisioning_VersionProfileFilterMiss(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"items":[{"name":"v2.0","version":"2.0"}]}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "versionprofiles", "v3.0"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for versionprofile not found")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got %v", err)
	}
}

func TestProvisioning_VersionProfileDescribe(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"items":[{"name":"v2.0","version":"2.0","channel":"stable"}]}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"describe", "versionprofile", "v2.0"})
	if err := root.Execute(); err != nil {
		t.Fatalf("describe versionprofile v2.0 failed: %v", err)
	}
}

func TestProvisioning_SingletonArgRejected(t *testing.T) {
	testSetup(t)
	root := newRootCmd()
	root.SetArgs([]string{"get", "provisioning-config", "extra"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected usage error for singleton with arg")
	}
}

func TestProvisioning_VersionProfileCreateWithCluster(t *testing.T) {
	testSetup(t)
	var method, path string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"name":"custom","version":"2.555","channel":"lts"}`)
	})
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "profile.yaml")
	if err := os.WriteFile(file, []byte(`apiVersion: varroa.dev/v1alpha1
kind: JenkinsVersionProfile
metadata:
  name: custom
spec:
  version: "2.555"
`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	root := newRootCmd()
	root.SetArgs([]string{"create", "versionprofile", "-f", file, "--cluster", "prod"})
	if err := root.Execute(); err != nil {
		t.Fatalf("create versionprofile failed: %v", err)
	}
	if method != http.MethodPost {
		t.Errorf("expected POST, got %s", method)
	}
	if !strings.HasSuffix(path, "/api/v1/clusters/prod/version-profiles") {
		t.Errorf("expected /api/v1/clusters/prod/version-profiles, got %s", path)
	}
}

func TestProvisioning_VersionProfileDeleteWithCluster(t *testing.T) {
	testSetup(t)
	var method, path string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"delete", "versionprofile", "custom", "--cluster", "prod"})
	if err := root.Execute(); err != nil {
		t.Fatalf("delete versionprofile failed: %v", err)
	}
	if method != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", method)
	}
	if !strings.HasSuffix(path, "/api/v1/clusters/prod/version-profiles/custom") {
		t.Errorf("expected /api/v1/clusters/prod/version-profiles/custom, got %s", path)
	}
}

func TestProvisioning_ConfigWithCluster(t *testing.T) {
	testSetup(t)
	var path string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"defaults":{},"versionProfiles":[]}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "provisioning-config", "--cluster", "prod"})
	if err := root.Execute(); err != nil {
		t.Fatalf("get provisioning-config --cluster prod failed: %v", err)
	}
	if !strings.HasSuffix(path, "/api/v1/clusters/prod/provisioning/config") {
		t.Errorf("expected /api/v1/clusters/prod/provisioning/config, got %s", path)
	}
}

func TestProvisioning_DeployableNamespaces(t *testing.T) {
	testSetup(t)
	var path string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"namespaces":["ns1","ns2"],"defaultNamespace":"ns1","allowFreeform":true,"degraded":false}`)
	})
	defer srv.Close()

	// Default (no cluster flag) → core
	root := newRootCmd()
	root.SetArgs([]string{"get", "deployable-namespaces"})
	if err := root.Execute(); err != nil {
		t.Fatalf("get deployable-namespaces failed: %v", err)
	}
	if !strings.HasSuffix(path, "/api/v1/clusters/core/namespaces/deployable") {
		t.Errorf("expected /api/v1/clusters/core/namespaces/deployable, got %s", path)
	}
}

func TestProvisioning_DeployableNamespaces_WithClusterFlag(t *testing.T) {
	testSetup(t)
	var path string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"namespaces":["ns1"],"defaultNamespace":"ns1","allowFreeform":false,"degraded":false}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "deployable-namespaces", "--cluster", "dev-cluster"})
	if err := root.Execute(); err != nil {
		t.Fatalf("get deployable-namespaces --cluster dev-cluster failed: %v", err)
	}
	if !strings.HasSuffix(path, "/api/v1/clusters/dev-cluster/namespaces/deployable") {
		t.Errorf("expected /api/v1/clusters/dev-cluster/namespaces/deployable, got %s", path)
	}
}

func TestProvisioning_BuiltinRoles(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[{"metadata":{"name":"admin"},"spec":{"jenkinsRoleRef":"admin","apiRules":["rule1"],"permissions":["perm1"]}}]}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "builtin-roles"})
	if err := root.Execute(); err != nil {
		t.Fatalf("get builtin-roles failed: %v", err)
	}
}

func TestProvisioning_IdentitySettings(t *testing.T) {
	testSetup(t)
	var path string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"mode":"local","cookieDomain":"example.com"}`)
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "identity-settings"})
	if err := root.Execute(); err != nil {
		t.Fatalf("get identity-settings failed: %v", err)
	}
	if !strings.HasSuffix(path, "/api/v1/identity-settings") {
		t.Errorf("expected /api/v1/identity-settings, got %s", path)
	}
}

// TestProvisioning_VersionProfileColumns pins the flat VersionProfileDetail
// DTO shape (/version-profiles is not a CR list — no spec/status nesting).
func TestProvisioning_VersionProfileColumns(t *testing.T) {
	item := map[string]any{
		"name":        "jenkins-version-2-570",
		"version":     "2.570",
		"channel":     "weekly",
		"recommended": true,
		"pluginCount": float64(71),
		"contentRef":  "jenkins-version-2-570-pluginset-content",
		"eol":         "2027-01-01",
		"conditions": []any{
			map[string]any{"type": "PluginSetReady", "status": "True"},
		},
	}
	got := versionProfileColumns(item)
	want := []string{"jenkins-version-2-570", "2.570", "weekly", "true", "71", "True"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("column %d: got %q, want %q (row %v)", i, got[i], want[i], got)
		}
	}
	wide := versionProfileWideColumns(item)
	if wide[6] != "2027-01-01" || wide[7] != "jenkins-version-2-570-pluginset-content" {
		t.Errorf("wide columns wrong: %v", wide[6:])
	}
}
