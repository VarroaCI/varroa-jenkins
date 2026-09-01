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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func ownedTestLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by":    "varroa-operator",
		"varroa.dev/version-profile-seed": "true",
	}
}

// TestCreateOrUpdateOwnedConfigMap_CreatesNew verifies a missing ConfigMap is
// created with the caller's labels and data.
func TestCreateOrUpdateOwnedConfigMap_CreatesNew(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	ctx := context.Background()

	if err := c.CreateOrUpdateOwnedConfigMap(ctx, "cm1", "ns1", map[string]string{"plugins.yaml": "v1"}, ownedTestLabels()); err != nil {
		t.Fatalf("CreateOrUpdateOwnedConfigMap: %v", err)
	}

	got, err := c.clientset.CoreV1().ConfigMaps("ns1").Get(ctx, "cm1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	if got.Data["plugins.yaml"] != "v1" {
		t.Errorf("data = %v, want plugins.yaml=v1", got.Data)
	}
	for k, v := range ownedTestLabels() {
		if got.Labels[k] != v {
			t.Errorf("label %q = %q, want %q", k, got.Labels[k], v)
		}
	}
}

// TestCreateOrUpdateOwnedConfigMap_UpdatesOwned verifies a live ConfigMap
// already carrying the ownership labels is updated in place.
func TestCreateOrUpdateOwnedConfigMap_UpdatesOwned(t *testing.T) {
	ctx := context.Background()
	pre := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cm1", Namespace: "ns1", Labels: ownedTestLabels()},
		Data:       map[string]string{"plugins.yaml": "v1"},
	}
	c := &ClientsetClient{clientset: fake.NewSimpleClientset(pre)}

	if err := c.CreateOrUpdateOwnedConfigMap(ctx, "cm1", "ns1", map[string]string{"plugins.yaml": "v2"}, ownedTestLabels()); err != nil {
		t.Fatalf("CreateOrUpdateOwnedConfigMap: %v", err)
	}

	got, err := c.clientset.CoreV1().ConfigMaps("ns1").Get(ctx, "cm1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	if got.Data["plugins.yaml"] != "v2" {
		t.Errorf("data = %v, want plugins.yaml=v2", got.Data)
	}
}

// TestCreateOrUpdateOwnedConfigMap_SkipsOnMissingLabel verifies a live
// ConfigMap without the ownership labels is left untouched and
// ErrConfigMapNotOwned is returned.
func TestCreateOrUpdateOwnedConfigMap_SkipsOnMissingLabel(t *testing.T) {
	ctx := context.Background()
	pre := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cm1", Namespace: "ns1"}, // no labels: foreign/hand-authored
		Data:       map[string]string{"plugins.yaml": "original"},
	}
	fc := fake.NewSimpleClientset(pre)
	c := &ClientsetClient{clientset: fc}

	updateAttempts := 0
	fc.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		updateAttempts++
		return false, nil, nil
	})

	err := c.CreateOrUpdateOwnedConfigMap(ctx, "cm1", "ns1", map[string]string{"plugins.yaml": "new"}, ownedTestLabels())
	if !errors.Is(err, ErrConfigMapNotOwned) {
		t.Fatalf("err = %v, want ErrConfigMapNotOwned", err)
	}
	if updateAttempts != 0 {
		t.Errorf("update attempts = %d, want 0 (must not write a foreign object)", updateAttempts)
	}

	got, err := c.clientset.CoreV1().ConfigMaps("ns1").Get(ctx, "cm1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	if got.Data["plugins.yaml"] != "original" {
		t.Errorf("data was modified: %v", got.Data)
	}
}

// TestCreateOrUpdateOwnedConfigMap_ConflictThenRetrySucceeds verifies a
// Conflict on the first Update triggers a bounded retry that re-Gets,
// re-confirms ownership, and succeeds.
func TestCreateOrUpdateOwnedConfigMap_ConflictThenRetrySucceeds(t *testing.T) {
	ctx := context.Background()
	pre := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cm1", Namespace: "ns1", Labels: ownedTestLabels()},
		Data:       map[string]string{"plugins.yaml": "v1"},
	}
	fc := fake.NewSimpleClientset(pre)
	c := &ClientsetClient{clientset: fc}

	updateAttempts := 0
	fc.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		updateAttempts++
		if updateAttempts == 1 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "cm1", fmt.Errorf("conflicting write"))
		}
		return false, nil, nil // let the fake clientset perform the real update
	})

	if err := c.CreateOrUpdateOwnedConfigMap(ctx, "cm1", "ns1", map[string]string{"plugins.yaml": "v2"}, ownedTestLabels()); err != nil {
		t.Fatalf("CreateOrUpdateOwnedConfigMap: %v", err)
	}
	if updateAttempts != 2 {
		t.Errorf("update attempts = %d, want 2 (one conflict, one retry)", updateAttempts)
	}

	got, err := c.clientset.CoreV1().ConfigMaps("ns1").Get(ctx, "cm1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	if got.Data["plugins.yaml"] != "v2" {
		t.Errorf("data = %v, want plugins.yaml=v2 after retry", got.Data)
	}
}

// TestCreateOrUpdateOwnedConfigMap_ConflictThenRetryFindsForeign verifies
// that when the retry's re-Get discovers the object went foreign (e.g. a
// concurrent C4 promotion cleared the ownership label), the reconciler backs
// off with ErrConfigMapNotOwned instead of retrying the write.
func TestCreateOrUpdateOwnedConfigMap_ConflictThenRetryFindsForeign(t *testing.T) {
	ctx := context.Background()
	pre := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cm1", Namespace: "ns1", Labels: ownedTestLabels()},
		Data:       map[string]string{"plugins.yaml": "v1"},
	}
	fc := fake.NewSimpleClientset(pre)
	c := &ClientsetClient{clientset: fc}

	getAttempts := 0
	fc.PrependReactor("get", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		getAttempts++
		if getAttempts == 1 {
			return false, nil, nil // pass through: still owned on the first read
		}
		// Simulate a concurrent promotion clearing the ownership label between
		// the failed Update and this reconciler's retry Get.
		foreign := pre.DeepCopy()
		foreign.Labels = map[string]string{}
		return true, foreign, nil
	})
	updateAttempts := 0
	fc.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		updateAttempts++
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "cm1", fmt.Errorf("conflicting write"))
	})

	err := c.CreateOrUpdateOwnedConfigMap(ctx, "cm1", "ns1", map[string]string{"plugins.yaml": "v2"}, ownedTestLabels())
	if !errors.Is(err, ErrConfigMapNotOwned) {
		t.Fatalf("err = %v, want ErrConfigMapNotOwned", err)
	}
	if updateAttempts != 1 {
		t.Errorf("update attempts = %d, want 1 (retry must not re-attempt the write against a foreign object)", updateAttempts)
	}
}
