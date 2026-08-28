package controller

import (
	"testing"
)

func TestMaxConcurrentReconciles(t *testing.T) {
	// Fresh construction via setter on bare Reconciler (no real setup needed).
	t.Run("setter zero becomes default", func(t *testing.T) {
		r := &Reconciler{}
		r.SetMaxConcurrentReconciles(0)
		if r.maxConcurrentReconciles != defaultMaxConcurrentReconciles {
			t.Errorf("SetMaxConcurrentReconciles(0): got %d, want %d", r.maxConcurrentReconciles, defaultMaxConcurrentReconciles)
		}
	})

	t.Run("setter negative becomes default", func(t *testing.T) {
		r := &Reconciler{}
		r.SetMaxConcurrentReconciles(-3)
		if r.maxConcurrentReconciles != defaultMaxConcurrentReconciles {
			t.Errorf("SetMaxConcurrentReconciles(-3): got %d, want %d", r.maxConcurrentReconciles, defaultMaxConcurrentReconciles)
		}
	})

	t.Run("setter positive passes through", func(t *testing.T) {
		r := &Reconciler{}
		r.SetMaxConcurrentReconciles(20)
		if r.maxConcurrentReconciles != 20 {
			t.Errorf("SetMaxConcurrentReconciles(20): got %d, want 20", r.maxConcurrentReconciles)
		}
	})
}
