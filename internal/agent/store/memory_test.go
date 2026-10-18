package store

import "testing"

// TestMemoryContract runs the shared contract against the in-memory store.
func TestMemoryContract(t *testing.T) {
	t.Parallel()
	runContract(t, func(t *testing.T, o Options) Store { return NewMemory(o) })
}
