package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

const versionProfileSeedTestNamespace = "varroa-system"

func newVersionProfileSeedTestReconciler() (*VersionProfileSeedReconciler, *crdstore.Fake, *fake.Clientset) {
	store := crdstore.NewFake()
	fc := fake.NewSimpleClientset()
	client := &ClientsetClient{clientset: fc}
	return NewVersionProfileSeedReconciler(client, store, versionProfileSeedTestNamespace, nil), store, fc
}

// TestVersionProfileSeed_SeedsOnFreshCluster verifies Reconcile creates the
// JenkinsVersionProfile CR and its pluginset ConfigMap, correctly linked, for
// every entry pluginlock.Seeds() embeds.
func TestVersionProfileSeed_SeedsOnFreshCluster(t *testing.T) {
	ctx := context.Background()
	r, store, fc := newVersionProfileSeedTestReconciler()

	r.Reconcile(ctx)

	seeds := pluginlock.Seeds()
	if len(seeds) == 0 {
		t.Fatal("pluginlock.Seeds() returned no entries; nothing to verify")
	}
	for _, seed := range seeds {
		name := pluginlock.ProfileName(seed.Version)

		profile, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](ctx, store, name, "")
		if err != nil {
			t.Fatalf("profile %s not seeded: %v", name, err)
		}
		if !versionProfileSeedOwned(profile.Labels) {
			t.Errorf("profile %s missing ownership label: %v", name, profile.Labels)
		}
		if profile.Spec.Version != seed.Spec.Version {
			t.Errorf("profile %s spec.version = %q, want %q", name, profile.Spec.Version, seed.Spec.Version)
		}
		if profile.Spec.PluginSetRef == nil || profile.Spec.PluginSetRef.Name != seed.Spec.PluginSetRef.Name {
			t.Errorf("profile %s pluginSetRef = %+v, want name %q", name, profile.Spec.PluginSetRef, seed.Spec.PluginSetRef.Name)
		}

		cm, err := fc.CoreV1().ConfigMaps(versionProfileSeedTestNamespace).Get(ctx, seed.Spec.PluginSetRef.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("pluginset configmap for %s not seeded: %v", name, err)
		}
		if cm.Data["plugins.yaml"] != seed.Plugins {
			t.Errorf("pluginset configmap for %s data mismatch", name)
		}
		if !versionProfileSeedOwned(cm.Labels) {
			t.Errorf("pluginset configmap for %s missing ownership label: %v", name, cm.Labels)
		}
	}
}

// TestVersionProfileSeed_SkipsForeignProfile verifies a live profile CR
// without the ownership label is left untouched, and its pluginset ConfigMap
// is never written either (the ownership preflight happens before any
// ConfigMap write).
func TestVersionProfileSeed_SkipsForeignProfile(t *testing.T) {
	ctx := context.Background()
	r, store, fc := newVersionProfileSeedTestReconciler()

	seeds := pluginlock.Seeds()
	if len(seeds) == 0 {
		t.Fatal("pluginlock.Seeds() returned no entries; nothing to verify")
	}
	target := seeds[0]
	name := pluginlock.ProfileName(target.Version)

	foreign := &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name}, // no ownership label: hand-authored
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: target.Version, Channel: "hand-authored"},
	}
	crdstore.MustSeed(store, foreign)

	r.Reconcile(ctx)

	live, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](ctx, store, name, "")
	if err != nil {
		t.Fatalf("get profile %s: %v", name, err)
	}
	if live.Spec.Channel != "hand-authored" {
		t.Errorf("foreign profile %s was overwritten: spec.channel = %q", name, live.Spec.Channel)
	}

	if _, err := fc.CoreV1().ConfigMaps(versionProfileSeedTestNamespace).Get(ctx, target.Spec.PluginSetRef.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("pluginset configmap for foreign profile %s: err = %v, want NotFound (must not be created)", name, err)
	}
}

// TestVersionProfileSeed_SkipsForeignConfigMap verifies a live pluginset
// ConfigMap without the ownership label is left untouched, and the paired
// profile CR is never created either (only written after the ConfigMap write
// succeeds).
func TestVersionProfileSeed_SkipsForeignConfigMap(t *testing.T) {
	ctx := context.Background()
	r, store, fc := newVersionProfileSeedTestReconciler()

	seeds := pluginlock.Seeds()
	if len(seeds) == 0 {
		t.Fatal("pluginlock.Seeds() returned no entries; nothing to verify")
	}
	target := seeds[0]
	name := pluginlock.ProfileName(target.Version)
	cmName := target.Spec.PluginSetRef.Name

	foreign := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: versionProfileSeedTestNamespace}, // no ownership label
		Data:       map[string]string{"plugins.yaml": "hand-authored"},
	}
	if _, err := fc.CoreV1().ConfigMaps(versionProfileSeedTestNamespace).Create(ctx, foreign, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed foreign configmap: %v", err)
	}

	r.Reconcile(ctx)

	cm, err := fc.CoreV1().ConfigMaps(versionProfileSeedTestNamespace).Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap %s: %v", cmName, err)
	}
	if cm.Data["plugins.yaml"] != "hand-authored" {
		t.Errorf("foreign configmap %s was overwritten: data = %v", cmName, cm.Data)
	}

	if _, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](ctx, store, name, ""); !apierrors.IsNotFound(err) {
		t.Errorf("profile %s: err = %v, want NotFound (must not be created over a foreign configmap)", name, err)
	}
}

// TestVersionProfileSeed_NoWritesWhenUnchanged verifies re-running Reconcile
// against already-owned, up-to-date objects performs no writes: it injects
// hard failures on every subsequent create/update against both the
// JenkinsVersionProfile GVR and the configmaps resource, then asserts a
// second reconcileOne call for an already-converged entry returns no error
// (proving neither write path was invoked).
func TestVersionProfileSeed_NoWritesWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	r, store, fc := newVersionProfileSeedTestReconciler()

	seeds := pluginlock.Seeds()
	if len(seeds) == 0 {
		t.Fatal("pluginlock.Seeds() returned no entries; nothing to verify")
	}
	target := seeds[0]

	r.Reconcile(ctx) // first pass: creates everything

	gvr, err := crdstore.GVRFor[v1alpha1.JenkinsVersionProfile]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	failErr := errors.New("must not be called: object already converged")
	store.FailAlways("create", gvr, failErr)
	store.FailAlways("update", gvr, failErr)

	cmWriteAttempts := 0
	fc.PrependReactor("create", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		cmWriteAttempts++
		return true, nil, fmt.Errorf("must not be called: configmap already converged")
	})
	fc.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		cmWriteAttempts++
		return true, nil, fmt.Errorf("must not be called: configmap already converged")
	})

	if err := r.reconcileOne(ctx, target); err != nil {
		t.Fatalf("reconcileOne on unchanged, already-owned entry performed a write: %v", err)
	}
	if cmWriteAttempts != 0 {
		t.Errorf("configmap write attempts = %d, want 0", cmWriteAttempts)
	}
}

// TestVersionProfileSeed_OneFailureDoesNotBlockOthers verifies one profile's
// failure is isolated: a hard failure seeding the first entry does not
// prevent the second (or later) entries from being seeded in the same
// Reconcile call.
func TestVersionProfileSeed_OneFailureDoesNotBlockOthers(t *testing.T) {
	ctx := context.Background()
	r, store, fc := newVersionProfileSeedTestReconciler()

	seeds := pluginlock.Seeds()
	if len(seeds) < 2 {
		t.Skip("need at least 2 seed entries to verify per-profile isolation")
	}
	failing := seeds[0]
	ok := seeds[1]

	gvr, err := crdstore.GVRFor[v1alpha1.JenkinsVersionProfile]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	store.FailNext("create", gvr, errors.New("injected create failure"))

	r.Reconcile(ctx)

	if _, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](ctx, store, pluginlock.ProfileName(failing.Version), ""); !apierrors.IsNotFound(err) {
		t.Errorf("failing profile %s: err = %v, want NotFound (create was made to fail)", failing.Version, err)
	}

	okName := pluginlock.ProfileName(ok.Version)
	if _, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](ctx, store, okName, ""); err != nil {
		t.Errorf("profile %s was not seeded despite an earlier entry's failure: %v", okName, err)
	}
	if _, err := fc.CoreV1().ConfigMaps(versionProfileSeedTestNamespace).Get(ctx, ok.Spec.PluginSetRef.Name, metav1.GetOptions{}); err != nil {
		t.Errorf("pluginset configmap for %s was not seeded despite an earlier entry's failure: %v", okName, err)
	}
}
