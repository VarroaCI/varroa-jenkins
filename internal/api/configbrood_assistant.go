package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// GetAssistantConfig reads the AssistantConfig of the local cluster from the
// BFF store. The assistant is a core feature, so a member cluster is refused.
func (f *busConfigBrood) GetAssistantConfig(ctx context.Context, cluster string) (*v1alpha1.AssistantConfig, error) {
	if !f.isLocal(cluster) {
		return nil, fmt.Errorf("assistantconfig is served only by the local cluster %q", f.localCluster)
	}
	cr, err := crdstore.Get[v1alpha1.AssistantConfig](ctx, f.store, v1alpha1.AssistantConfigName, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return cr, nil
}

// UpdateAssistantConfig sends the object to the operator of cluster, which
// applies it. The reply holds the stored object.
func (f *busConfigBrood) UpdateAssistantConfig(ctx context.Context, cluster string, obj *v1alpha1.AssistantConfig) (*v1alpha1.AssistantConfig, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal assistantconfig: %w", err)
	}
	req := bus.ConfigApplyRequest{Name: v1alpha1.AssistantConfigName, Object: raw}
	respData, err := f.requestTo(bus.OperatorAssistantConfigSubject(cluster, "update"), req, configCreateUpdateTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigApplyResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("update assistantconfig decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	var out v1alpha1.AssistantConfig
	if err := json.Unmarshal(resp.Item, &out); err != nil {
		return nil, errors.New("update assistantconfig: decode reply: " + err.Error())
	}
	return &out, nil
}

// SetAssistantKey sends a provider key as a core NATS request. The operator
// writes it into the key Secret. The reply carries no key.
func (f *busConfigBrood) SetAssistantKey(ctx context.Context, cluster, provider string, key []byte) error {
	return f.assistantKeyRequest(cluster, "set", bus.AssistantKeyRequest{Provider: provider, APIKey: key})
}

// ClearAssistantKey removes a provider key through the operator of cluster.
func (f *busConfigBrood) ClearAssistantKey(ctx context.Context, cluster, provider string) error {
	return f.assistantKeyRequest(cluster, "clear", bus.AssistantKeyRequest{Provider: provider})
}

// assistantKeyRequest sends one key request to the operator of cluster and maps
// a refusal to an error. The error text never holds the key.
func (f *busConfigBrood) assistantKeyRequest(cluster, verb string, req bus.AssistantKeyRequest) error {
	if !f.isLocal(cluster) {
		return fmt.Errorf("assistant keys are served only by the local cluster %q", f.localCluster)
	}
	respData, err := f.requestTo(bus.OperatorAssistantKeysSubject(cluster, verb), req, configCreateUpdateTimeout)
	if err != nil {
		return err
	}
	var resp bus.AssistantKeyResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return errors.New("assistant key " + verb + ": decode reply")
	}
	if resp.Code != "" {
		return f.codeToErr(resp.Code, resp.Error)
	}
	return nil
}
