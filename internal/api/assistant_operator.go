package api

import (
	"context"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// AssistantOperator reads and writes the AssistantConfig object of the local
// cluster. The BFF never applies the object itself: a write goes to the
// operator, which validates it and applies it with a resourceVersion fence.
type AssistantOperator interface {
	// GetAssistantConfig reads the object from the local store. It answers
	// (nil, nil) when the object does not exist.
	GetAssistantConfig(ctx context.Context, cluster string) (*v1alpha1.AssistantConfig, error)
	// UpdateAssistantConfig sends the object to the operator of cluster. An
	// object with a resourceVersion updates only that version. An object
	// without one only creates it.
	UpdateAssistantConfig(ctx context.Context, cluster string, obj *v1alpha1.AssistantConfig) (*v1alpha1.AssistantConfig, error)
	// SetAssistantKey sends a provider key to the operator of cluster, which
	// writes it into the key Secret. The key is never returned.
	SetAssistantKey(ctx context.Context, cluster, provider string, key []byte) error
	// ClearAssistantKey removes the key of a provider from the key Secret.
	ClearAssistantKey(ctx context.Context, cluster, provider string) error
}
