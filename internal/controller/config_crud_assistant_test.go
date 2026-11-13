package controller

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// assistantValid returns an object that passes ValidateAssistantConfig.
func assistantValid(rv string) v1alpha1.AssistantConfig {
	return v1alpha1.AssistantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.AssistantConfigName, ResourceVersion: rv},
		Spec: v1alpha1.AssistantConfigSpec{
			Providers: []v1alpha1.AssistantProvider{{
				Name: "lab", Kind: v1alpha1.AssistantProviderKindAnthropic, Trust: v1alpha1.AssistantTrustExternal,
				Models: []string{"model-a"},
			}},
			DefaultProvider: "lab",
		},
	}
}

func applyAssistant(t *testing.T, c *ConfigCRUD, name string, obj v1alpha1.AssistantConfig) bus.ConfigApplyResponse {
	t.Helper()
	req := bus.ConfigApplyRequest{Name: name, Object: mustMarshal(t, obj)}
	var resp bus.ConfigApplyResponse
	if err := json.Unmarshal(c.HandleAssistantConfigUpdate(mustMarshal(t, req)), &resp); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	return resp
}

func storedAssistant(t *testing.T, store crdstore.Backend) *v1alpha1.AssistantConfig {
	t.Helper()
	cr, err := crdstore.Get[v1alpha1.AssistantConfig](context.Background(), store, v1alpha1.AssistantConfigName, "")
	if err != nil {
		t.Fatalf("get stored assistant config: %v", err)
	}
	return cr
}

// TestHandleAssistantConfigUpdateCreatesOnlyWithoutVersion pins that a write
// with no resourceVersion creates the object and never replaces one.
func TestHandleAssistantConfigUpdateCreatesOnlyWithoutVersion(t *testing.T) {
	store := crdstore.NewFake()
	c := &ConfigCRUD{Store: store, Logger: testLogger()}
	if resp := applyAssistant(t, c, v1alpha1.AssistantConfigName, assistantValid("")); resp.Code != "" {
		t.Fatalf("create: code %q: %s", resp.Code, resp.Error)
	}
	resp := applyAssistant(t, c, v1alpha1.AssistantConfigName, assistantValid(""))
	if resp.Code != bus.CodeConflict {
		t.Fatalf("second create: code %q, want conflict", resp.Code)
	}
}

// TestHandleAssistantConfigUpdateFencesOnVersion pins that a stale version is refused.
func TestHandleAssistantConfigUpdateFencesOnVersion(t *testing.T) {
	store := crdstore.NewFake()
	c := &ConfigCRUD{Store: store, Logger: testLogger()}
	created := applyAssistant(t, c, v1alpha1.AssistantConfigName, assistantValid(""))
	if created.Code != "" {
		t.Fatalf("create: code %q: %s", created.Code, created.Error)
	}
	var item v1alpha1.AssistantConfig
	if err := json.Unmarshal(created.Item, &item); err != nil {
		t.Fatal(err)
	}
	stale := item.ResourceVersion

	item.Spec.Providers[0].Models = []string{"model-a", "model-b"}
	if resp := applyAssistant(t, c, v1alpha1.AssistantConfigName, item); resp.Code != "" {
		t.Fatalf("update at the read version: code %q: %s", resp.Code, resp.Error)
	}
	resp := applyAssistant(t, c, v1alpha1.AssistantConfigName, assistantValid(stale))
	if resp.Code != bus.CodeConflict {
		t.Fatalf("update at a stale version: code %q, want conflict", resp.Code)
	}
}

// TestHandleAssistantConfigUpdateForcesName pins that the operator stores the
// singleton name whatever name the request carries.
func TestHandleAssistantConfigUpdateForcesName(t *testing.T) {
	store := crdstore.NewFake()
	c := &ConfigCRUD{Store: store, Logger: testLogger()}
	resp := applyAssistant(t, c, "other", assistantValid(""))
	if resp.Code != "" {
		t.Fatalf("create: code %q: %s", resp.Code, resp.Error)
	}
	if got := storedAssistant(t, store); got.Name != v1alpha1.AssistantConfigName {
		t.Fatalf("stored name %q, want %q", got.Name, v1alpha1.AssistantConfigName)
	}
}

func TestHandleAssistantConfigUpdateRefusesInvalidSpec(t *testing.T) {
	store := crdstore.NewFake()
	c := &ConfigCRUD{Store: store, Logger: testLogger()}
	bad := assistantValid("")
	bad.Spec.DefaultProvider = "ghost"
	resp := applyAssistant(t, c, v1alpha1.AssistantConfigName, bad)
	if resp.Code != bus.CodeInvalid {
		t.Fatalf("invalid spec: code %q, want invalid", resp.Code)
	}
	if _, err := crdstore.Get[v1alpha1.AssistantConfig](context.Background(), store, v1alpha1.AssistantConfigName, ""); err == nil {
		t.Fatal("an invalid spec was stored")
	}
}

func TestHandleAssistantConfigUpdateCallsChangeHook(t *testing.T) {
	store := crdstore.NewFake()
	calls := 0
	c := &ConfigCRUD{Store: store, Logger: testLogger(), OnAssistantConfigChange: func() { calls++ }}
	applyAssistant(t, c, v1alpha1.AssistantConfigName, assistantValid(""))
	if calls != 1 {
		t.Fatalf("hook calls after a stored write: %d, want 1", calls)
	}
	bad := assistantValid("")
	bad.Spec.DefaultProvider = "ghost"
	applyAssistant(t, c, v1alpha1.AssistantConfigName, bad)
	if calls != 1 {
		t.Fatalf("hook calls after a refused write: %d, want 1", calls)
	}
}
