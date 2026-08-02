package controller

import (
	"context"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

func TestLifecycleStoreAbsentConfigMap(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := NewLifecycleStore(client, "varroa-system", slog.Default())

	st, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.State != bus.ClusterStateActive {
		t.Errorf("State = %q, want %q", st.State, bus.ClusterStateActive)
	}
	if st.DrainStartedAt != nil {
		t.Errorf("DrainStartedAt = %v, want nil", st.DrainStartedAt)
	}
}

func TestLifecycleStoreFullRoundTrip(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := NewLifecycleStore(client, "varroa-system", slog.Default())

	ctx := context.Background()

	// 1. Load absent → active
	st, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.State != bus.ClusterStateActive {
		t.Errorf("initial State = %q, want %q", st.State, bus.ClusterStateActive)
	}

	// 2. SetDraining
	if err := store.SetDraining(ctx, "admin"); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}
	st = store.State()
	if st.State != bus.ClusterStateDraining {
		t.Errorf("after SetDraining State = %q, want %q", st.State, bus.ClusterStateDraining)
	}
	if st.RequestedBy != "admin" {
		t.Errorf("RequestedBy = %q, want %q", st.RequestedBy, "admin")
	}
	if st.DrainStartedAt == nil {
		t.Fatal("DrainStartedAt should be set")
	}

	// Verify via API
	cm, err := client.CoreV1().ConfigMaps("varroa-system").Get(ctx, "varroa-cluster-lifecycle", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get ConfigMap: %v", err)
	}
	if cm.Data["state"] != bus.ClusterStateDraining {
		t.Errorf("ConfigMap state = %q, want %q", cm.Data["state"], bus.ClusterStateDraining)
	}

	// 3. SetDrained
	if err := store.SetDrained(ctx); err != nil {
		t.Fatalf("SetDrained: %v", err)
	}
	st = store.State()
	if st.State != bus.ClusterStateDrained {
		t.Errorf("after SetDrained State = %q, want %q", st.State, bus.ClusterStateDrained)
	}

	// 4. Reload from API
	st2, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load after SetDrained: %v", err)
	}
	if st2.State != bus.ClusterStateDrained {
		t.Errorf("reloaded State = %q, want %q", st2.State, bus.ClusterStateDrained)
	}

	// 5. SetActive (cancel)
	if err := store.SetActive(ctx); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	st = store.State()
	if st.State != bus.ClusterStateActive {
		t.Errorf("after SetActive State = %q, want %q", st.State, bus.ClusterStateActive)
	}
	if st.DrainStartedAt != nil {
		t.Errorf("DrainStartedAfter cancel = %v, want nil", st.DrainStartedAt)
	}

	// Verify via API
	cm, err = client.CoreV1().ConfigMaps("varroa-system").Get(ctx, "varroa-cluster-lifecycle", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get ConfigMap after cancel: %v", err)
	}
	if cm.Data["state"] != bus.ClusterStateActive {
		t.Errorf("ConfigMap state after cancel = %q, want %q", cm.Data["state"], bus.ClusterStateActive)
	}
	if _, ok := cm.Data["drainStartedAt"]; ok {
		t.Error("drainStartedAt should be removed after cancel")
	}
}

func TestLifecycleStoreMalformedDrainStartedAt(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := NewLifecycleStore(client, "varroa-system", slog.Default())

	// Pre-create a ConfigMap with malformed drainStartedAt
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "varroa-cluster-lifecycle",
			Namespace: "varroa-system",
		},
		Data: map[string]string{
			"state":          "draining",
			"drainStartedAt": "not-a-timestamp",
			"requestedBy":    "admin",
		},
	}
	if _, err := client.CoreV1().ConfigMaps("varroa-system").Create(context.Background(), cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create ConfigMap: %v", err)
	}

	st, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Malformed drainStartedAt should be treated as active
	if st.State != bus.ClusterStateActive {
		t.Errorf("State = %q, want %q (active after malformed timestamp)", st.State, bus.ClusterStateActive)
	}
}

func TestLifecycleStoreStateNoAPICall(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := NewLifecycleStore(client, "varroa-system", slog.Default())

	// State() on a fresh store should return zero-value (active).
	st := store.State()
	if st.State != "" {
		t.Errorf("State() on fresh store = %q, want empty string", st.State)
	}

	// After a Load, State() should return the loaded value without API calls.
	if _, err := store.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	st = store.State()
	if st.State != bus.ClusterStateActive {
		t.Errorf("State() after Load = %q, want %q", st.State, bus.ClusterStateActive)
	}

	// After SetDraining, State() should reflect the write.
	if err := store.SetDraining(context.Background(), "test"); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}
	st = store.State()
	if st.State != bus.ClusterStateDraining {
		t.Errorf("State() after SetDraining = %q, want %q", st.State, bus.ClusterStateDraining)
	}
}

func TestLifecycleStoreDrainingTimestamps(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := NewLifecycleStore(client, "varroa-system", slog.Default())

	ctx := context.Background()

	// SetDraining and verify drainStartedAt is approximately now.
	before := time.Now().UTC()
	if err := store.SetDraining(ctx, "admin"); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}
	after := time.Now().UTC()

	st := store.State()
	if st.DrainStartedAt == nil {
		t.Fatal("DrainStartedAt should be set")
	}
	if st.DrainStartedAt.Before(before.Add(-time.Second)) || st.DrainStartedAt.After(after.Add(time.Second)) {
		t.Errorf("DrainStartedAt = %v, should be between %v and %v", st.DrainStartedAt, before, after)
	}
}
