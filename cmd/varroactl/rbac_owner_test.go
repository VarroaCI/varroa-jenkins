package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// vrbItem returns a VarroaRoleBinding API object with the given metadata. The
// maps are JSON objects, so their values are any, as a decoded response holds them.
func vrbItem(name string, labels, annotations map[string]string) map[string]any {
	meta := map[string]any{"name": name}
	if labels != nil {
		meta["labels"] = anyMap(labels)
	}
	if annotations != nil {
		meta["annotations"] = anyMap(annotations)
	}
	return map[string]any{
		"metadata": meta,
		"spec": map[string]any{
			"roleRef":  "developer",
			"subjects": []any{map[string]any{"kind": "User", "name": "agent:ns-a/tenant-a/agent-a"}},
		},
	}
}

func anyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func tenantAgentLabels() map[string]string {
	return map[string]string{
		v1alpha1.LabelTenantAgent:          "true",
		v1alpha1.LabelTenantAgentNamespace: "ns-a",
		v1alpha1.LabelTenantAgentName:      "agent-a",
	}
}

func broodScheduleAnnotations() map[string]string {
	return map[string]string{
		v1alpha1.AnnotationBroodScheduleNamespace: "ns-a",
		v1alpha1.AnnotationBroodScheduleName:      "nightly",
	}
}

func TestRoleBindingColumns_Owner(t *testing.T) {
	taName := v1alpha1.TenantAgentBindingName("ns-a", "agent-a")
	bsName := v1alpha1.BroodScheduleBindingName("ns-a", "nightly")
	cases := []struct {
		name string
		item map[string]any
		want string
	}{
		{"owned TenantAgent", vrbItem(taName, tenantAgentLabels(), nil), "TenantAgent/ns-a/agent-a"},
		{"owned BroodSchedule", vrbItem(bsName, nil, broodScheduleAnnotations()), "BroodSchedule/ns-a/nightly"},
		{"renamed copy keeps TenantAgent labels", vrbItem("copy-of-agent", tenantAgentLabels(), nil), "-"},
		{"renamed copy keeps BroodSchedule annotations", vrbItem("copy-of-schedule", nil, broodScheduleAnnotations()), "-"},
		{"hand-made without metadata", vrbItem("vrb-plain", nil, nil), "-"},
		{"derived name without labels", vrbItem(taName, nil, nil), "-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cols := roleBindingColumns(tc.item)
			if len(cols) != 5 {
				t.Fatalf("columns = %v, want 5", cols)
			}
			if got := cols[4]; got != tc.want {
				t.Errorf("OWNER column = %q, want %q (row %v)", got, tc.want, cols)
			}
		})
	}
}

func TestRBAC_RoleBindings_OwnerColumn(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				vrbItem(v1alpha1.TenantAgentBindingName("ns-a", "agent-a"), tenantAgentLabels(), nil),
				vrbItem(v1alpha1.BroodScheduleBindingName("ns-a", "nightly"), nil, broodScheduleAnnotations()),
				vrbItem("vrb-plain", nil, nil),
			},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "rolebindings"})
	var runErr error
	out := captureStdout(t, func() { runErr = root.Execute() })
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("want header plus three rows, got %d lines:\n%s", len(lines), out)
	}
	if header := strings.Fields(lines[0]); strings.Join(header, " ") != "NAME ROLE SUBJECTS SCOPE OWNER" {
		t.Errorf("header = %v, want NAME ROLE SUBJECTS SCOPE OWNER", header)
	}
	if last := lastField(lines[1]); last != "TenantAgent/ns-a/agent-a" {
		t.Errorf("TenantAgent row OWNER = %q, want TenantAgent/ns-a/agent-a\n%s", last, out)
	}
	if last := lastField(lines[2]); last != "BroodSchedule/ns-a/nightly" {
		t.Errorf("BroodSchedule row OWNER = %q, want BroodSchedule/ns-a/nightly\n%s", last, out)
	}
	if last := lastField(lines[3]); last != "-" {
		t.Errorf("hand-made row OWNER = %q, want -\n%s", last, out)
	}
}

func TestRBAC_RoleBinding_GetSingleShowsOwner(t *testing.T) {
	testSetup(t)
	name := v1alpha1.TenantAgentBindingName("ns-a", "agent-a")
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(vrbItem(name, tenantAgentLabels(), nil))
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "rolebinding", name})
	var runErr error
	out := captureStdout(t, func() { runErr = root.Execute() })
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Fatalf("want header and a row, got:\n%s", out)
	}
	if last := lastField(lines[len(lines)-1]); last != "TenantAgent/ns-a/agent-a" {
		t.Errorf("row OWNER = %q, want TenantAgent/ns-a/agent-a\n%s", last, out)
	}
}

func TestRBAC_RoleBindings_JSONIsAPIObject(t *testing.T) {
	testSetup(t)
	name := v1alpha1.TenantAgentBindingName("ns-a", "agent-a")
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(vrbItem(name, tenantAgentLabels(), nil))
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "rolebinding", name, "-o", "json"})
	var runErr error
	out := captureStdout(t, func() { runErr = root.Execute() })
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	if strings.Contains(out, `"owner"`) {
		t.Errorf("-o json must print the API object with no owner key:\n%s", out)
	}
	if !strings.Contains(out, v1alpha1.LabelTenantAgentName) {
		t.Errorf("-o json must keep the labels:\n%s", out)
	}
}
