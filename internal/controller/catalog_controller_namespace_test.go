package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func jcascItemNS(name, ns, content string) *v1alpha1.CatalogItem {
	return &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC},
		Status:     v1alpha1.CatalogItemStatus{Content: content, Valid: true},
	}
}

// TestInputSummary_ResolvedNamespaces stamps the resolved namespace for explicit,
// local, and fallback itemRefs, plus an empty namespace for a gitSource, in input order.
func TestInputSummary_ResolvedNamespaces(t *testing.T) {
	// Local bare git repo so the gitSource input resolves cleanly.
	fixture := t.TempDir()
	bare := filepath.Join(fixture, "bare.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, fixture, "init", "--bare", bare)
	work := filepath.Join(fixture, "work")
	gitCmd(t, fixture, "clone", bare, work)
	gitCmd(t, work, "checkout", "-b", "main")
	writeFile(t, filepath.Join(work, "bundle.yaml"), "id: test\nversion: \"1\"\napiVersion: \"1\"\njcasc:\n  - jenkins.yaml\n")
	writeFile(t, filepath.Join(work, "jenkins.yaml"), "jenkins:\n  systemMessage: \"git\"\n")
	gitCmd(t, work, "add", ".")
	gitCmd(t, work, "commit", "-m", "initial")
	gitCmd(t, work, "push", "origin", "main")
	repoURL := "file://" + bare

	tc := newCatalogTestClient()
	// Distinct top-level keys so the three jcasc items merge without conflict.
	tc.seedItem(jcascItemNS("explicit-item", "team-b", "alpha:\n  v: explicit\n"))
	tc.seedItem(jcascItemNS("local-item", "tenant-ns", "beta:\n  v: local\n"))
	tc.seedItem(jcascItemNS("platform-item", "operator-ns", "gamma:\n  v: platform\n"))

	cb := &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "sum-cb", Namespace: "tenant-ns"},
		Spec: v1alpha1.ComposedBundleSpec{
			Inputs: []v1alpha1.ComposedInput{
				{ItemRef: &v1alpha1.ComposedItemRef{Name: "explicit-item", Namespace: "team-b"}},
				{ItemRef: &v1alpha1.ComposedItemRef{Name: "local-item"}},
				{ItemRef: &v1alpha1.ComposedItemRef{Name: "platform-item"}},
				{GitSource: &v1alpha1.GitBundleSource{RepoURL: repoURL, Path: ".", Revision: "main"}},
			},
		},
	}

	rec := newComposedBundleReconcilerWithNS(tc, t, "operator-ns")
	rec.ReconcileComposedBundle(context.Background(), cb)

	if tc.lastStatus() == nil {
		t.Fatal("no status patched")
	}
	if tc.lastStatus().Phase != v1alpha1.ComposedBundleReady {
		t.Fatalf("phase = %q, want Ready (errors: %v)", tc.lastStatus().Phase, tc.lastStatus().Errors)
	}
	sum := tc.lastStatus().InputSummary
	if len(sum) != 4 {
		t.Fatalf("inputSummary len = %d, want 4: %+v", len(sum), sum)
	}
	want := []struct {
		kind string
		ns   string
	}{
		{"itemRef", "team-b"},
		{"itemRef", "tenant-ns"},
		{"itemRef", "operator-ns"},
		{"gitSource", ""},
	}
	for i, w := range want {
		if sum[i].Kind != w.kind {
			t.Errorf("entry[%d].Kind = %q, want %q", i, sum[i].Kind, w.kind)
		}
		if sum[i].Namespace != w.ns {
			t.Errorf("entry[%d].Namespace = %q, want %q", i, sum[i].Namespace, w.ns)
		}
	}
}

// TestInputSummary_UnresolvedEmpty leaves the namespace empty for an itemRef that
// resolves nowhere.
func TestInputSummary_UnresolvedEmpty(t *testing.T) {
	tc := newCatalogTestClient()
	cb := &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "unres-cb", Namespace: "tenant-ns"},
		Spec: v1alpha1.ComposedBundleSpec{
			Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "nowhere"}}},
		},
	}
	// A distinct operator namespace here (value is immaterial — no items exist).
	rec := newComposedBundleReconcilerWithNS(tc, t, "varroa-system")
	rec.ReconcileComposedBundle(context.Background(), cb)

	if tc.lastStatus() == nil {
		t.Fatal("no status patched")
	}
	if len(tc.lastStatus().InputSummary) != 1 {
		t.Fatalf("inputSummary len = %d, want 1", len(tc.lastStatus().InputSummary))
	}
	if tc.lastStatus().InputSummary[0].Namespace != "" {
		t.Errorf("unresolved entry Namespace = %q, want empty", tc.lastStatus().InputSummary[0].Namespace)
	}
}

// TestInputSummary_ShadowWarningReachesStatus confirms a row-4 shadow warning lands
// in status.warnings and status.message while the bundle stays Ready.
func TestInputSummary_ShadowWarningReachesStatus(t *testing.T) {
	tc := newCatalogTestClient()
	tc.seedItem(jcascItemNS("theme", "tenant-ns", "jenkins:\n  systemMessage: \"local\"\n"))
	tc.seedItem(jcascItemNS("theme", "operator-ns", "jenkins:\n  systemMessage: \"platform\"\n"))

	cb := &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "shadow-cb", Namespace: "tenant-ns"},
		Spec: v1alpha1.ComposedBundleSpec{
			Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "theme"}}},
		},
	}
	rec := newComposedBundleReconcilerWithNS(tc, t, "operator-ns")
	rec.ReconcileComposedBundle(context.Background(), cb)

	if tc.lastStatus() == nil {
		t.Fatal("no status patched")
	}
	if tc.lastStatus().Phase != v1alpha1.ComposedBundleReady {
		t.Fatalf("phase = %q, want Ready", tc.lastStatus().Phase)
	}
	warnJoined := strings.Join(tc.lastStatus().Warnings, " ")
	if !strings.Contains(warnJoined, "tenant-ns/theme") || !strings.Contains(warnJoined, "operator-ns/theme") {
		t.Errorf("status.warnings missing shadow warning naming both namespaces: %v", tc.lastStatus().Warnings)
	}
	if !strings.Contains(tc.lastStatus().Message, "shadowed") {
		t.Errorf("status.message missing shadow warning: %q", tc.lastStatus().Message)
	}
	// The local (tenant-ns) item is the one used.
	if tc.lastStatus().InputSummary[0].Namespace != "tenant-ns" {
		t.Errorf("resolved namespace = %q, want tenant-ns", tc.lastStatus().InputSummary[0].Namespace)
	}
}
