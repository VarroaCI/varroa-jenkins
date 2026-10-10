package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// seedDeclaredBundle seeds a ComposedBundle whose materialized plugins.yaml is plugins.
func seedDeclaredBundle(client *testClient, name, namespace, plugins string) {
	cmName := name + "-cm"
	crdstore.MustSeed(client.store, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status:     v1alpha1.ComposedBundleStatus{ContentRef: cmName},
	})
	client.configMapData[cmName] = map[string]string{"plugins.yaml": plugins}
}

// TestUpdateCenterDeclaredVersionless covers a bundle plugin with no version
// is declared at latest, in the declared-plugins value and in the gap set.
func TestUpdateCenterDeclaredVersionless(t *testing.T) {
	t.Run("version-less-bundle-plugin-written-as-latest", func(t *testing.T) {
		client := newTestClient()
		client.updateCenter = ucWithPullThrough(true)
		crdstore.MustSeed(client.store, client.updateCenter)
		client.pvc = boundPVC()
		seedDeclaredBundle(client, "b-versionless", "ns", "plugins:\n  - artifactId: foo\n")

		rec := NewUpdateCenterReconciler(client, client.store, "default", "http://unused-uc", testLogger())
		value := rec.buildDeclaredPluginsValue(context.Background(), testLogger())
		if value == nil {
			t.Fatal("declared-plugins value = nil, want a value")
		}
		lines := strings.Split(*value, "\n")
		if !slices.Contains(lines, "foo@latest") {
			t.Errorf("declared-plugins = %q, want a foo@latest line", *value)
		}
		for _, line := range lines {
			if strings.HasSuffix(line, "@") {
				t.Errorf("declared-plugins line %q has an empty version", line)
			}
		}
	})

	t.Run("version-less-and-latest-collapse-to-one-row", func(t *testing.T) {
		client := newTestClient()
		client.updateCenter = ucWithPullThrough(true)
		crdstore.MustSeed(client.store, client.updateCenter)
		client.pvc = boundPVC()

		seedDeclaredBundle(client, "b-versionless-dup", "ns", "plugins:\n  - artifactId: foo\n")
		addProfile(client, "prof-latest-dup", "2.479.1")
		client.configMapData["prof-latest-dup-content"]["plugins.yaml"] =
			"core:\n  - 2.479.1\nplugins:\n  - artifactId: foo\n    version: latest\n"

		ts := gapTestUCServer(t, nil)
		rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
		if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		status := lastUCStatus(t, client)
		if status.GapCount != 1 || len(status.Gaps) != 1 {
			t.Fatalf("gaps = %d %+v, want one foo gap", status.GapCount, status.Gaps)
		}
		if status.Gaps[0].Plugin != "foo" || status.Gaps[0].Version != "latest" {
			t.Errorf("gap = %+v, want foo@latest", status.Gaps[0])
		}
	})
}
