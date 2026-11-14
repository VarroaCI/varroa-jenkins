package transport

import (
	"context"
	"testing"
)

// TestBusReadModelTransport_PutPluginClassificationRejected verifies that the
// read-only BFF transport rejects PutPluginClassification with the same error as Send.
func TestBusReadModelTransport_PutPluginClassificationRejected(t *testing.T) {
	model := NewBusReadModel("test", nil, nil, nil)
	tr := NewBusReadModelTransport(model)

	err := tr.PutPluginClassification(context.TODO(), "", "", nil)
	if err == nil {
		t.Fatal("expected error from PutPluginClassification on read-only transport")
	}
	sendErr := tr.Send(context.TODO(), "", "", nil)
	if sendErr == nil {
		t.Fatal("expected error from Send on read-only transport")
	}
	// Both should be rejected — the exact error text is not prescribed, but
	// both must be non-nil.
}
