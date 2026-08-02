package controller

import (
	"context"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

const lifecycleConfigMapName = "varroa-cluster-lifecycle"

// LifecycleState is the snapshot of drain state held in the ConfigMap.
type LifecycleState struct {
	State          string     // bus.ClusterStateActive when ConfigMap absent or "active"
	DrainStartedAt *time.Time // parsed RFC3339, nil when active
	RequestedBy    string
}

// LifecycleStore wraps a ConfigMap in the operator namespace for durable
// drain state. All methods are safe for concurrent use.
type LifecycleStore struct {
	client    kubernetes.Interface
	namespace string
	mu        sync.Mutex
	cached    LifecycleState
	logger    *slog.Logger
}

// NewLifecycleStore creates a LifecycleStore.
func NewLifecycleStore(client kubernetes.Interface, namespace string, logger *slog.Logger) *LifecycleStore {
	return &LifecycleStore{
		client:    client,
		namespace: namespace,
		logger:    logger,
	}
}

// Load reads the lifecycle ConfigMap and returns the parsed state.
// When the ConfigMap is absent, returns {State: "active"}.
// On a parse error for drainStartedAt, logs and treats as active.
func (s *LifecycleStore) Load(ctx context.Context) (LifecycleState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(ctx)
}

func (s *LifecycleStore) loadLocked(ctx context.Context) (LifecycleState, error) {
	cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, lifecycleConfigMapName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			s.cached = LifecycleState{State: bus.ClusterStateActive}
			return s.cached, nil
		}
		return s.cached, err
	}

	st := parseConfigMap(cm)
	s.cached = st
	return st, nil
}

// parseConfigMap extracts LifecycleState from a ConfigMap's Data.
func parseConfigMap(cm *corev1.ConfigMap) LifecycleState {
	st := LifecycleState{State: bus.ClusterStateActive}
	if cm.Data == nil {
		return st
	}

	stateVal := cm.Data["state"]
	switch stateVal {
	case bus.ClusterStateDraining, bus.ClusterStateDrained:
		st.State = stateVal
	default:
		// ConfigMap absent or state: active ⇒ active
		return st
	}

	if ds, ok := cm.Data["drainStartedAt"]; ok && ds != "" {
		parsed, err := time.Parse(time.RFC3339, ds)
		if err != nil {
			// Log and treat as active
			return LifecycleState{State: bus.ClusterStateActive}
		}
		st.DrainStartedAt = &parsed
	}
	st.RequestedBy = cm.Data["requestedBy"]
	return st
}

// SetDraining creates or updates the ConfigMap with state=draining,
// drainStartedAt=now RFC3339, and the requestedBy actor.
func (s *LifecycleStore) SetDraining(ctx context.Context, requestedBy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lifecycleConfigMapName,
			Namespace: s.namespace,
		},
		Data: map[string]string{
			"state":          bus.ClusterStateDraining,
			"drainStartedAt": now,
			"requestedBy":    requestedBy,
		},
	}

	existing, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, lifecycleConfigMapName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			_, createErr := s.client.CoreV1().ConfigMaps(s.namespace).Create(ctx, cm, metav1.CreateOptions{})
			if createErr == nil {
				parsed, _ := time.Parse(time.RFC3339, now)
				s.cached = LifecycleState{
					State:          bus.ClusterStateDraining,
					DrainStartedAt: &parsed,
					RequestedBy:    requestedBy,
				}
			}
			return createErr
		}
		return err
	}

	existing.Data = cm.Data
	_, updateErr := s.client.CoreV1().ConfigMaps(s.namespace).Update(ctx, existing, metav1.UpdateOptions{})
	if updateErr == nil {
		parsed, _ := time.Parse(time.RFC3339, now)
		s.cached = LifecycleState{
			State:          bus.ClusterStateDraining,
			DrainStartedAt: &parsed,
			RequestedBy:    requestedBy,
		}
	}
	return updateErr
}

// SetDrained updates the ConfigMap state to drained, keeping the other keys.
func (s *LifecycleStore) SetDrained(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, lifecycleConfigMapName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if existing.Data == nil {
		existing.Data = make(map[string]string)
	}
	existing.Data["state"] = bus.ClusterStateDrained

	_, updateErr := s.client.CoreV1().ConfigMaps(s.namespace).Update(ctx, existing, metav1.UpdateOptions{})
	if updateErr == nil {
		// Preserve existing cached values for drainStartedAt/requestedBy
		s.cached.State = bus.ClusterStateDrained
	}
	return updateErr
}

// SetActive resets the ConfigMap to state=active, removing drainStartedAt
// and requestedBy keys (update in place, no delete).
func (s *LifecycleStore) SetActive(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lifecycleConfigMapName,
			Namespace: s.namespace,
		},
		Data: map[string]string{
			"state": bus.ClusterStateActive,
		},
	}

	existing, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, lifecycleConfigMapName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			_, createErr := s.client.CoreV1().ConfigMaps(s.namespace).Create(ctx, cm, metav1.CreateOptions{})
			if createErr == nil {
				s.cached = LifecycleState{State: bus.ClusterStateActive}
			}
			return createErr
		}
		return err
	}

	// In-place update to exactly state:active, removing the other keys.
	existing.Data = cm.Data
	_, updateErr := s.client.CoreV1().ConfigMaps(s.namespace).Update(ctx, existing, metav1.UpdateOptions{})
	if updateErr == nil {
		s.cached = LifecycleState{State: bus.ClusterStateActive}
	}
	return updateErr
}

// State returns the cached snapshot without an API call.
func (s *LifecycleStore) State() LifecycleState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cached
}
