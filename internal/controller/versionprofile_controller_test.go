package controller

import (
	"context"
	"log/slog"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// profileReconcilerNamespace is the operator namespace used in tests.
const profileReconcilerNamespace = "varroa-system"

func TestJenkinsVersionProfileReconciler_NoPluginSetRef(t *testing.T) {
	client := newTestClient()
	client.profiles["test-profile"] = &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-profile",
			UID:  "00000000-0000-0000-0000-000000000001",
		},
		Spec: v1alpha1.JenkinsVersionProfileSpec{
			Version: "2.479.3",
			Channel: "lts",
		},
	}
	crdstore.MustSeed(client.store, client.profiles["test-profile"])

	rec := NewJenkinsVersionProfileReconciler(client, client.store, profileReconcilerNamespace, slog.Default())
	result, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-profile"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Error("expected no requeue for metadata-only profile")
	}

	// No -pluginset ConfigMap should be created.
	if _, ok := client.configMapData["test-profile-pluginset-content"]; ok {
		t.Error("expected no -pluginset ConfigMap for profile without pluginSetRef")
	}
}

func TestJenkinsVersionProfileReconciler_ValidSourceCM(t *testing.T) {
	client := newTestClient()
	client.profiles["test-profile"] = &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-profile",
			UID:  "00000000-0000-0000-0000-000000000001",
		},
		Spec: v1alpha1.JenkinsVersionProfileSpec{
			Version: "2.479.3",
			Channel: "lts",
			PluginSetRef: &v1alpha1.ConfigMapRef{
				Name: "source-plugins",
			},
		},
	}
	crdstore.MustSeed(client.store, client.profiles["test-profile"])

	// Set up the source ConfigMap with a valid plugins.yaml.
	client.configMapData["source-plugins"] = map[string]string{
		"plugins.yaml": `core:
  - "2.479.3"
plugins:
  - artifactId: "git"
    version: "5.6.0"
  - artifactId: "workflow-aggregator"
    version: "2.8.0"
`,
	}

	rec := NewJenkinsVersionProfileReconciler(client, client.store, profileReconcilerNamespace, slog.Default())
	_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-profile"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check that the -pluginset ConfigMap was created with owner reference.
	cmName := "test-profile-pluginset-content"
	cmData, ok := client.configMapData[cmName]
	if !ok {
		t.Fatalf("expected ConfigMap %q to be created", cmName)
	}
	if cmData["plugins.yaml"] == "" {
		t.Error("expected plugins.yaml in the owned ConfigMap")
	}
	owners := client.configMapOwners[cmName]
	if len(owners) == 0 {
		t.Error("expected owner references on the ConfigMap")
	} else if owners[0].Name != "test-profile" {
		t.Errorf("expected owner name test-profile, got %s", owners[0].Name)
	}

	if client.cmWrites == 0 {
		t.Error("expected at least one ConfigMap write")
	}
}

func TestJenkinsVersionProfileReconciler_MissingSourceCM(t *testing.T) {
	client := newTestClient()
	client.profiles["test-profile"] = &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-profile",
			UID:  "00000000-0000-0000-0000-000000000001",
		},
		Spec: v1alpha1.JenkinsVersionProfileSpec{
			Version: "2.479.3",
			Channel: "lts",
			PluginSetRef: &v1alpha1.ConfigMapRef{
				Name: "nonexistent-source",
			},
		},
	}
	crdstore.MustSeed(client.store, client.profiles["test-profile"])

	rec := NewJenkinsVersionProfileReconciler(client, client.store, profileReconcilerNamespace, slog.Default())
	_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-profile"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No -pluginset ConfigMap should be created.
	if _, ok := client.configMapData["test-profile-pluginset-content"]; ok {
		t.Error("expected no -pluginset ConfigMap when source CM is missing")
	}
}

func TestJenkinsVersionProfileReconciler_RequiredPluginsMissing(t *testing.T) {
	client := newTestClient()
	client.profiles["test-profile"] = &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-profile",
			UID:  "00000000-0000-0000-0000-000000000001",
		},
		Spec: v1alpha1.JenkinsVersionProfileSpec{
			Version: "2.479.3",
			Channel: "lts",
			PluginSetRef: &v1alpha1.ConfigMapRef{
				Name: "source-plugins",
			},
			JCasC: &v1alpha1.VersionJCasC{
				RequiredPlugins: []string{"missing-plugin", "also-missing"},
			},
		},
	}
	crdstore.MustSeed(client.store, client.profiles["test-profile"])

	// Source CM has only "git" plugin, not the required ones.
	client.configMapData["source-plugins"] = map[string]string{
		"plugins.yaml": `core:
  - "2.479.3"
plugins:
  - artifactId: "git"
    version: "5.6.0"
`,
	}

	rec := NewJenkinsVersionProfileReconciler(client, client.store, profileReconcilerNamespace, slog.Default())
	_, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-profile"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The -pluginset ConfigMap should still be created (materialization succeeded).
	cmName := "test-profile-pluginset-content"
	if _, ok := client.configMapData[cmName]; !ok {
		t.Error("expected -pluginset ConfigMap even with missing required plugins")
	}
}
