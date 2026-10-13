package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// seedCoreController seeds a Controller on version that uses the named bundle. An
// empty bundleName leaves composedBundleRef nil, so the controller uses the starter.
func seedCoreController(client *testClient, name, version, bundleName, bundleNamespace string) {
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       v1alpha1.ControllerSpec{Version: version},
	}
	if bundleName != "" {
		cr.Spec.ComposedBundleRef = &v1alpha1.ComposedBundleRef{Name: bundleName, Namespace: bundleNamespace}
	}
	crdstore.MustSeed(client.store, cr)
}

func coreTestClient(t *testing.T) *testClient {
	t.Helper()
	client := newTestClient()
	client.updateCenter = ucWithPullThrough(true)
	crdstore.MustSeed(client.store, client.updateCenter)
	client.pvc = boundPVC()
	return client
}

// TestUpdateCenterLatestPerCore covers a latest pin, which is stored only when a
// stored version runs on the lowest core that requests it.
func TestUpdateCenterLatestPerCore(t *testing.T) {
	t.Run("incompatible-stored-is-gap-for-core", func(t *testing.T) {
		client := coreTestClient(t)
		addProfile(client, "prof-core-gap", "2.555.3")
		client.configMapData["prof-core-gap-content"]["plugins.yaml"] =
			"core:\n  - 2.555.3\nplugins:\n  - artifactId: foo\n    version: latest\n"

		var posted []ucServabilityPlugin
		ts := gapTestUCServerInventory(t, []inventoryEntry{{Name: "foo", Version: "2.0", RequiredCore: "2.570"}}, nil, &posted)
		rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
		if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		status := lastUCStatus(t, client)

		if status.GapCount != 1 || len(status.Gaps) != 1 {
			t.Fatalf("gaps = %d %+v, want one foo gap", status.GapCount, status.Gaps)
		}
		if status.Gaps[0].Core != "2.555.3" {
			t.Errorf("gap core = %q, want 2.555.3", status.Gaps[0].Core)
		}
		if len(posted) != 1 || posted[0] != (ucServabilityPlugin{Name: "foo", Version: "latest", Core: "2.555.3"}) {
			t.Errorf("posted = %+v, want foo latest core 2.555.3", posted)
		}
	})

	t.Run("compatible-stored-no-gap", func(t *testing.T) {
		client := coreTestClient(t)
		addProfile(client, "prof-core-ok", "2.555.3")
		client.configMapData["prof-core-ok-content"]["plugins.yaml"] =
			"core:\n  - 2.555.3\nplugins:\n  - artifactId: foo\n    version: latest\n"

		var posted []ucServabilityPlugin
		ts := gapTestUCServerInventory(t, []inventoryEntry{{Name: "foo", Version: "1.9", RequiredCore: "2.555.3"}}, nil, &posted)
		rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
		if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		status := lastUCStatus(t, client)

		if status.GapCount != 0 {
			t.Errorf("gapCount = %d, want 0", status.GapCount)
		}
		if len(posted) != 0 {
			t.Errorf("posted = %+v, want no post", posted)
		}
	})

	t.Run("bundle-core-is-lowest-controller-core", func(t *testing.T) {
		client := coreTestClient(t)
		seedDeclaredBundle(client, "b-core-low", "ns", "plugins:\n  - artifactId: foo\n    version: latest\n")
		seedCoreController(client, "cr-low", "2.555.3", "b-core-low", "ns")
		seedCoreController(client, "cr-high", "2.570", "b-core-low", "ns")

		var posted []ucServabilityPlugin
		ts := gapTestUCServerInventory(t, []inventoryEntry{}, nil, &posted)
		rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
		if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}

		if len(posted) != 1 || posted[0].Core != "2.555.3" {
			t.Errorf("posted = %+v, want foo latest with core 2.555.3", posted)
		}
	})

	t.Run("bare-line-controller-requests-its-weekly-core", func(t *testing.T) {
		client := coreTestClient(t)
		seedDeclaredBundle(client, "b-bare", "ns", "plugins:\n  - artifactId: foo\n    version: latest\n")
		seedCoreController(client, "cr-bare", "2.479", "b-bare", "ns")

		var posted []ucServabilityPlugin
		ts := gapTestUCServerInventory(t, []inventoryEntry{{Name: "foo", Version: "2.0", RequiredCore: "2.570"}}, nil, &posted)
		rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
		if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		status := lastUCStatus(t, client)

		if status.GapCount != 1 || len(status.Gaps) != 1 {
			t.Fatalf("gaps = %d %+v, want one foo gap", status.GapCount, status.Gaps)
		}
		if status.Gaps[0].Core != "2.479" {
			t.Errorf("gap core = %q, want 2.479", status.Gaps[0].Core)
		}
		if len(posted) != 1 || posted[0] != (ucServabilityPlugin{Name: "foo", Version: "latest", Core: "2.479"}) {
			t.Errorf("posted = %+v, want foo latest core 2.479", posted)
		}
	})

	t.Run("bundle-without-controller-posts-no-core", func(t *testing.T) {
		client := coreTestClient(t)
		seedDeclaredBundle(client, "b-unused", "ns", "plugins:\n  - artifactId: foo\n    version: latest\n  - artifactId: bar\n    version: latest\n")

		// A stored foo of an incompatible core still counts as stored, because no
		// controller requests a core for it. bar is unstored and is posted with no core.
		var posted []ucServabilityPlugin
		ts := gapTestUCServerInventory(t, []inventoryEntry{{Name: "foo", Version: "2.0", RequiredCore: "2.570"}}, nil, &posted)
		rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
		if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		status := lastUCStatus(t, client)

		if status.GapCount != 1 || len(status.Gaps) != 1 || status.Gaps[0].Plugin != "bar" {
			t.Fatalf("gaps = %d %+v, want one bar gap", status.GapCount, status.Gaps)
		}
		if len(posted) != 1 || posted[0] != (ucServabilityPlugin{Name: "bar", Version: "latest"}) {
			t.Errorf("posted = %+v, want bar latest with no core", posted)
		}
	})

	t.Run("starter-bundle-uses-nil-ref-controllers", func(t *testing.T) {
		client := coreTestClient(t)
		// The starter lives in the operator namespace, which the reconciler test uses as "default".
		seedDeclaredBundle(client, v1alpha1.StarterBundleName, "default", "plugins:\n  - artifactId: foo\n    version: latest\n")
		seedCoreController(client, "cr-starter", "2.555.3", "", "")

		var posted []ucServabilityPlugin
		ts := gapTestUCServerInventory(t, []inventoryEntry{}, nil, &posted)
		rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
		if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}

		if len(posted) != 1 || posted[0].Core != "2.555.3" {
			t.Errorf("posted = %+v, want foo latest with core 2.555.3", posted)
		}
	})

	t.Run("unservable-latest-carries-core", func(t *testing.T) {
		client := coreTestClient(t)
		addProfile(client, "prof-core-unservable", "2.555.3")
		client.configMapData["prof-core-unservable-content"]["plugins.yaml"] =
			"core:\n  - 2.555.3\nplugins:\n  - artifactId: foo\n    version: latest\n"

		var posted []ucServabilityPlugin
		ts := gapTestUCServerInventory(t, []inventoryEntry{}, map[string]string{"foo@latest": "not-listed"}, &posted)
		rec := NewUpdateCenterReconciler(client, client.store, "default", ts.URL, testLogger())
		if _, err := rec.Reconcile(context.Background(), reconcileRequest(updateCenterSingletonName)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		status := lastUCStatus(t, client)

		if len(status.Unservable) != 1 {
			t.Fatalf("unservable = %+v, want one entry", status.Unservable)
		}
		if status.Unservable[0].Core != "2.555.3" {
			t.Errorf("unservable core = %q, want 2.555.3", status.Unservable[0].Core)
		}
	})
}

// TestStoredForCore covers the one stored check for a latest pin.
func TestStoredForCore(t *testing.T) {
	byName := inventoryByName([]inventoryEntry{
		{Name: "open", Version: "1.0", RequiredCore: ""},
		{Name: "old", Version: "1.0", RequiredCore: "2.400"},
		{Name: "mixed", Version: "1.0", RequiredCore: "2.570"},
		{Name: "mixed", Version: "2.0", RequiredCore: "2.400"},
	})
	tests := []struct {
		name, plugin, core string
		want               bool
	}{
		{"empty-required-core-is-compatible", "open", "2.555.3", true},
		{"empty-core-accepts-any-stored", "old", "", true},
		{"incompatible-only-is-not-stored", "old", "2.300", false},
		{"compatible-version-is-stored", "old", "2.555.3", true},
		{"mixed-one-compatible-is-stored", "mixed", "2.555.3", true},
		{"mixed-none-compatible-is-not-stored", "mixed", "2.300", false},
		{"absent-plugin-is-not-stored", "missing", "2.555.3", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := storedForCore(byName, tt.plugin, tt.core); got != tt.want {
				t.Errorf("storedForCore(%q, %q) = %v, want %v", tt.plugin, tt.core, got, tt.want)
			}
		})
	}
}

// TestRequestingCore covers the exact core a controller or profile requests.
func TestRequestingCore(t *testing.T) {
	profile := &v1alpha1.JenkinsVersionProfile{Spec: v1alpha1.JenkinsVersionProfileSpec{ResolveVersion: "2.555.3"}}
	tests := []struct {
		name    string
		version string
		profile *v1alpha1.JenkinsVersionProfile
		want    string
	}{
		{"bare-two-segment-is-weekly-core", "2.479", nil, "2.479"},
		{"lts-profile-without-resolve-version-is-weekly-core", "2.479", &v1alpha1.JenkinsVersionProfile{Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.479", Channel: "lts"}}, "2.479"},
		{"suffixed-two-segment-is-exact", "2.570-jdk21", nil, "2.570"},
		{"unparseable-version-requests-no-core", "latest", nil, ""},
		{"empty-version-is-weekly-baseline", "", nil, pluginlock.Baseline()},
		{"weekly-two-segment-is-exact", "2.570", &v1alpha1.JenkinsVersionProfile{Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.570", Channel: "weekly"}}, "2.570"},
		{"build-suffix-is-stripped", "2.492.3-jdk21", nil, "2.492.3"},
		{"profile-resolve-version-wins", "2.400", profile, "2.555.3"},
		{"baseline-is-weekly-exact", "lts", nil, pluginlock.Baseline()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := requestingCore(tt.version, tt.profile); got != tt.want {
				t.Errorf("requestingCore(%q) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}

// TestDeployedCore covers the core a controller deploys, which the update center reads.
func TestDeployedCore(t *testing.T) {
	baseline := pluginlock.Baseline()
	profile := &v1alpha1.JenkinsVersionProfile{Spec: v1alpha1.JenkinsVersionProfileSpec{ResolveVersion: "2.555.3"}}
	tests := []struct {
		name    string
		version string
		profile *v1alpha1.JenkinsVersionProfile
		want    string
	}{
		{"empty-version-is-baseline", "", nil, baseline},
		{"lts-is-baseline", "lts", nil, baseline},
		{"profile-resolve-version-wins", "2.400", profile, "2.555.3"},
		{"trimmed-version", "  2.570  ", nil, "2.570"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deployedCore(tt.version, tt.profile); got != tt.want {
				t.Errorf("deployedCore(%q) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}
