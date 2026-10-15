package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// approvedSignaturesOf reads security.scriptApproval.approvedSignatures from
// rendered jenkins.yaml. It fails the test when the key is absent.
func approvedSignaturesOf(t *testing.T, jenkinsYAML string) []string {
	t.Helper()
	var doc struct {
		Security struct {
			ScriptApproval struct {
				ApprovedSignatures *[]string `yaml:"approvedSignatures"`
			} `yaml:"scriptApproval"`
		} `yaml:"security"`
	}
	if err := yaml.Unmarshal([]byte(jenkinsYAML), &doc); err != nil {
		t.Fatalf("parse rendered jcasc: %v", err)
	}
	if doc.Security.ScriptApproval.ApprovedSignatures == nil {
		t.Fatalf("approvedSignatures key missing in:\n%s", jenkinsYAML)
	}
	return *doc.Security.ScriptApproval.ApprovedSignatures
}

func TestInjectPoolSignatures_EmptyWritesEmptyList(t *testing.T) {
	out, err := injectPoolSignatures("jenkins:\n  systemMessage: hi\n", nil)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if got := approvedSignaturesOf(t, out); len(got) != 0 {
		t.Fatalf("empty pool must render an empty list, got %v", got)
	}
}

func TestInjectPoolSignatures_UnionSortedDeduped(t *testing.T) {
	base := `security:
  scriptApproval:
    approvedSignatures:
    - "staticMethod org.example.Util run java.lang.String"
    - "method hudson.Util getHost"
`
	pool := []string{"new java.util.Date", "method hudson.Util getHost", "field java.lang.System out"}
	out, err := injectPoolSignatures(base, pool)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	got := approvedSignaturesOf(t, out)
	want := []string{
		"field java.lang.System out",
		"method hudson.Util getHost",
		"new java.util.Date",
		"staticMethod org.example.Util run java.lang.String",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("signatures = %v, want %v", got, want)
	}
}

func TestInjectPoolSignatures_KeepsOtherSecurityKeys(t *testing.T) {
	base := `security:
  globalJobDslSecurityConfiguration:
    useScriptSecurity: true
  scriptApproval:
    forceSandbox: false
`
	out, err := injectPoolSignatures(base, []string{"method hudson.Util getHost"})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if !strings.Contains(out, "useScriptSecurity: true") || !strings.Contains(out, "forceSandbox: false") {
		t.Fatalf("other security keys dropped:\n%s", out)
	}
}

func TestInjectPoolSignatures_SkipsVarToken(t *testing.T) {
	out, err := injectPoolSignatures("", []string{"method hudson.Util ${token}", "method hudson.Util getHost"})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	got := approvedSignaturesOf(t, out)
	if len(got) != 1 || got[0] != "method hudson.Util getHost" {
		t.Fatalf("signatures = %v, want only the plain entry", got)
	}
}

// TestApplyPoolSignatures_NonCellNoop checks that a controller that is not
// a cell leaves the bundle unchanged.
func TestApplyPoolSignatures_NonCellNoop(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := testController("plain", "ns1", v1alpha1.ControllerPhaseConnected)
	cr.Labels = map[string]string{cells.LabelCellPool: "pool-a"}
	mat := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}
	before := mat.JenkinsYAML

	if err := rec.applyPoolSignatures(context.Background(), cr, mat); err != nil {
		t.Fatalf("applyPoolSignatures: %v", err)
	}
	if mat.JenkinsYAML != before {
		t.Fatalf("a non-cell controller must not change the bundle: %q", mat.JenkinsYAML)
	}
}

// TestApplyPoolSignatures_RendersPoolList checks that a cell renders the
// pool's signatures, and that a pool which no longer approves any clears
// them on the next pass.
func TestApplyPoolSignatures_RendersPoolList(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellController()
	crdstore.MustSeed(client.store, &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec: v1alpha1.CellPoolSpec{
			ProfileRef:         "lts-2-555",
			ApprovedSignatures: []string{"method hudson.Util getHost"},
		},
	})
	cr.Labels = map[string]string{cells.LabelCellPool: "pool-a"}

	mat := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}
	if err := rec.applyPoolSignatures(context.Background(), cr, mat); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if got := approvedSignaturesOf(t, mat.JenkinsYAML); len(got) != 1 || got[0] != "method hudson.Util getHost" {
		t.Fatalf("pass 1 signatures = %v", got)
	}

	// The pool is gone, so the cell renders an empty list and the old
	// signature is revoked.
	poolGVR := schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "cellpools"}
	if err := client.store.DeleteObject(context.Background(), poolGVR, "", "pool-a"); err != nil {
		t.Fatalf("delete pool: %v", err)
	}
	mat = &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}
	if err := rec.applyPoolSignatures(context.Background(), cr, mat); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if got := approvedSignaturesOf(t, mat.JenkinsYAML); len(got) != 0 {
		t.Fatalf("pass 2 signatures = %v, want empty", got)
	}
}

// TestApplyPoolSignatures_PoolReadErrorFailsResolve checks that a read
// failure is an error and never reads as a pool that approves nothing.
func TestApplyPoolSignatures_PoolReadErrorFailsResolve(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellController()
	cr.Labels = map[string]string{cells.LabelCellPool: "pool-a"}
	gvr := schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "cellpools"}
	client.store.FailAlways("get", gvr, errors.New("apiserver unavailable"))

	mat := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}
	if err := rec.applyPoolSignatures(context.Background(), cr, mat); err == nil {
		t.Fatal("a pool read failure must fail the resolve")
	}
}
