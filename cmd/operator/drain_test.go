package main

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// stubCommandCRUDClient implements controller.CommandCRUDClient for tests.
// The drain runner now reads/writes controllers directly through crdstore,
// so this stub is only used for its ListControllerCRDs and DeleteControllerCRD
// methods in tests that still exercise the old code paths.
type stubCommandCRUDClient struct {
	controllers []*v1alpha1.Controller
	deleteCalls []struct{ Name, Namespace string }
	listErr     error
	deleteErr   error
}

func (s *stubCommandCRUDClient) ListControllerCRDs(_ context.Context, _ string) ([]*v1alpha1.Controller, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.controllers, nil
}

func (s *stubCommandCRUDClient) DeleteControllerCRD(_ context.Context, name, namespace string) error {
	s.deleteCalls = append(s.deleteCalls, struct{ Name, Namespace string }{name, namespace})
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return nil
}

// Unused methods of CommandCRUDClient.
func (s *stubCommandCRUDClient) GetControllerCRD(_ context.Context, _, _ string) (*v1alpha1.Controller, error) {
	return nil, errors.New("unimplemented")
}
func (s *stubCommandCRUDClient) ApplyControllerCRD(_ context.Context, _ *v1alpha1.Controller) error {
	return errors.New("unimplemented")
}
func (s *stubCommandCRUDClient) ApplyControllerSpecSSA(_ context.Context, _, _ string, _ map[string]any, _ string, _ bool) (*v1alpha1.Controller, error) {
	return nil, errors.New("unimplemented")
}
func (s *stubCommandCRUDClient) DeleteControllerPod(_ context.Context, _, _ string) error {
	return errors.New("unimplemented")
}
func (s *stubCommandCRUDClient) GetComposedBundleCRD(_ context.Context, _, _ string) (*v1alpha1.ComposedBundle, error) {
	return nil, errors.New("unimplemented")
}
func (s *stubCommandCRUDClient) ApplyComposedBundleCRD(_ context.Context, _ *v1alpha1.ComposedBundle) error {
	return errors.New("unimplemented")
}
func (s *stubCommandCRUDClient) DeleteComposedBundleCRD(_ context.Context, _, _ string) error {
	return errors.New("unimplemented")
}
func (s *stubCommandCRUDClient) GetProvisioningDefaultsCRD(_ context.Context, _ string) (*v1alpha1.ProvisioningDefaults, error) {
	return nil, errors.New("unimplemented")
}

// seedDrainStore creates a crdstore.Fake seeded with controllers for drain tests.
func seedDrainStore(ctrls []*v1alpha1.Controller) *crdstore.Fake {
	s := crdstore.NewFake()
	objs := make([]any, len(ctrls))
	for i, c := range ctrls {
		objs[i] = c
	}
	crdstore.MustSeed(s, objs...)
	return s
}

// countStoreControllers returns the number of controllers in the store.
func countStoreControllers(s *crdstore.Fake) int {
	ctrls, _ := crdstore.List[v1alpha1.Controller](context.Background(), s, "", "")
	return len(ctrls)
}

func TestClusterDrainRunner(t *testing.T) {
	t.Run("state active => no calls", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		store := controller.NewLifecycleStore(client, "varroa-system", nil)
		stub := &stubCommandCRUDClient{}
		beatCalled := false
		crdStore := crdstore.NewFake()
		runner := &clusterDrainRunner{
			lifecycle: store,
			client:    stub,
			store:     crdStore,
			beatNow:   func() { beatCalled = true },
		}
		// Load to set cached state to active
		store.Load(context.Background())
		runner.tick(context.Background())
		if countStoreControllers(crdStore) != 0 {
			t.Error("expected no controllers in store")
		}
		if beatCalled {
			t.Error("beatNow should not be called for active state")
		}
	})

	t.Run("draining with 2 CRs issues 2 deletes", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		store := controller.NewLifecycleStore(client, "varroa-system", nil)
		store.SetDraining(context.Background(), "test")

		ctrls := []*v1alpha1.Controller{
			{ObjectMeta: metav1.ObjectMeta{Name: "ctrl1", Namespace: "ns1"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "ctrl2", Namespace: "ns2"}},
		}
		stub := &stubCommandCRUDClient{controllers: ctrls}
		crdStore := seedDrainStore(ctrls)
		beatCalled := false
		runner := &clusterDrainRunner{
			lifecycle: store,
			client:    stub,
			store:     crdStore,
			beatNow:   func() { beatCalled = true },
		}
		runner.tick(context.Background())
		// Verify both controllers were deleted from the store.
		if countStoreControllers(crdStore) != 0 {
			t.Fatalf("expected 0 controllers remaining in store, got %d", countStoreControllers(crdStore))
		}
		if beatCalled {
			t.Error("beatNow should not be called when CRs remain")
		}
	})

	t.Run("already-terminating CR skipped", func(t *testing.T) {
		now := metav1.Now()
		client := fake.NewSimpleClientset()
		store := controller.NewLifecycleStore(client, "varroa-system", nil)
		store.SetDraining(context.Background(), "test")

		ctrls := []*v1alpha1.Controller{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "terminating",
					Namespace:         "ns1",
					DeletionTimestamp: &now,
				},
			},
			{ObjectMeta: metav1.ObjectMeta{Name: "alive", Namespace: "ns2"}},
		}
		stub := &stubCommandCRUDClient{controllers: ctrls}
		crdStore := seedDrainStore(ctrls)
		runner := &clusterDrainRunner{
			lifecycle: store,
			client:    stub,
			store:     crdStore,
		}
		runner.tick(context.Background())
		// terminating CR should still be in store, alive CR should be deleted.
		remaining, _ := crdstore.List[v1alpha1.Controller](context.Background(), crdStore, "", "")
		if len(remaining) != 1 {
			t.Fatalf("expected 1 remaining controller (terminating), got %d", len(remaining))
		}
		if remaining[0].Name != "terminating" {
			t.Errorf("expected 'terminating' to remain, got %q", remaining[0].Name)
		}
	})

	t.Run("one delete error does not stop the pass", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		store := controller.NewLifecycleStore(client, "varroa-system", nil)
		store.SetDraining(context.Background(), "test")

		ctrls := []*v1alpha1.Controller{
			{ObjectMeta: metav1.ObjectMeta{Name: "ok", Namespace: "ns1"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "ok2", Namespace: "ns2"}},
		}
		stub := &stubCommandCRUDClient{controllers: ctrls}
		crdStore := seedDrainStore(ctrls)
		// Inject a delete error on the first controller's delete.
		ctrlGVR, _ := crdstore.GVRFor[v1alpha1.Controller]()
		crdStore.FailNext("delete", ctrlGVR, errors.New("some error"))
		runner := &clusterDrainRunner{
			lifecycle: store,
			client:    stub,
			store:     crdStore,
		}
		runner.tick(context.Background())
		// The first controller delete should have failed, but the second should
		// have succeeded. So one controller remains.
		if countStoreControllers(crdStore) != 1 {
			t.Fatalf("expected 1 remaining controller after one delete error, got %d", countStoreControllers(crdStore))
		}
	})

	t.Run("zero CRs flips to drained", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		store := controller.NewLifecycleStore(client, "varroa-system", nil)
		store.SetDraining(context.Background(), "test")

		stub := &stubCommandCRUDClient{}
		beatCalled := false
		crdStore := crdstore.NewFake()
		runner := &clusterDrainRunner{
			lifecycle: store,
			client:    stub,
			store:     crdStore,
			beatNow:   func() { beatCalled = true },
		}
		runner.tick(context.Background())

		st := store.State()
		if st.State != bus.ClusterStateDrained {
			t.Errorf("state after zero CRs = %q, want %q", st.State, bus.ClusterStateDrained)
		}
		if !beatCalled {
			t.Error("beatNow should be called after draining completes")
		}
	})

	t.Run("list error logged, no panic", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		store := controller.NewLifecycleStore(client, "varroa-system", nil)
		store.SetDraining(context.Background(), "test")

		stub := &stubCommandCRUDClient{}
		crdStore := crdstore.NewFake()
		// Inject a list error into the store.
		ctrlGVR, _ := crdstore.GVRFor[v1alpha1.Controller]()
		crdStore.FailNext("list", ctrlGVR, errors.New("list failed"))
		runner := &clusterDrainRunner{
			lifecycle: store,
			client:    stub,
			store:     crdStore,
		}
		// Should not panic, just log and return
		runner.tick(context.Background())
		// State should remain draining
		st := store.State()
		if st.State != bus.ClusterStateDraining {
			t.Errorf("state = %q, want %q", st.State, bus.ClusterStateDraining)
		}
	})

	t.Run("drained does not flip again", func(t *testing.T) {
		// If we start already drained, tick should be a no-op for CR deletion
		// (state is not draining, so the early return skips everything).
		client := fake.NewSimpleClientset()
		store := controller.NewLifecycleStore(client, "varroa-system", nil)
		store.SetDraining(context.Background(), "test")
		store.SetDrained(context.Background())

		stub := &stubCommandCRUDClient{}
		beatCalled := false
		crdStore := crdstore.NewFake()
		runner := &clusterDrainRunner{
			lifecycle: store,
			client:    stub,
			store:     crdStore,
			beatNow:   func() { beatCalled = true },
		}
		runner.tick(context.Background())
		if beatCalled {
			t.Error("beatNow should not be called when already drained")
		}
		if countStoreControllers(crdStore) != 0 {
			t.Errorf("expected no controllers in store when drained, got %d", countStoreControllers(crdStore))
		}
	})
}
