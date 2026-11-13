package mcp

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// fakeMCPAssistantOperator holds one object. A write with a version that does
// not match answers conflict, and conflictsLeft forces that many extra conflicts.
type fakeMCPAssistantOperator struct {
	stored        *v1alpha1.AssistantConfig
	rv            int
	conflictsLeft int
	puts          []*v1alpha1.AssistantConfig
}

func (f *fakeMCPAssistantOperator) GetAssistantConfig(context.Context, string) (*v1alpha1.AssistantConfig, error) {
	if f.stored == nil {
		return nil, nil
	}
	return f.stored.DeepCopy(), nil
}

func (f *fakeMCPAssistantOperator) UpdateAssistantConfig(_ context.Context, _ string, obj *v1alpha1.AssistantConfig) (*v1alpha1.AssistantConfig, error) {
	f.puts = append(f.puts, obj.DeepCopy())
	if f.conflictsLeft > 0 {
		f.conflictsLeft--
		return nil, &api.BroodError{Code: bus.CodeConflict, Msg: "changed"}
	}
	cur := ""
	if f.stored != nil {
		cur = f.stored.ResourceVersion
	}
	if obj.ResourceVersion != cur {
		return nil, &api.BroodError{Code: bus.CodeConflict, Msg: "changed"}
	}
	f.rv++
	c := obj.DeepCopy()
	c.ResourceVersion = string(rune('0' + f.rv))
	f.stored = c
	return c.DeepCopy(), nil
}

func assistantConfigDeps(op *fakeMCPAssistantOperator) *api.Dependencies {
	return &api.Dependencies{
		Client:     &stubClient{},
		Store:      crdstore.NewFake(),
		Authorizer: adminTestAuthorizer(),
		Assistant:  &api.AssistantRuntime{Operator: op, Cluster: "core"},
	}
}

func storedAssistantObject(rv string) *v1alpha1.AssistantConfig {
	return &v1alpha1.AssistantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.AssistantConfigName, ResourceVersion: rv},
		Spec: v1alpha1.AssistantConfigSpec{
			Providers: []v1alpha1.AssistantProvider{{
				Name: "lab", Kind: v1alpha1.AssistantProviderKindAnthropic, Trust: v1alpha1.AssistantTrustExternal,
				Models: []string{"model-a"},
			}},
			DefaultProvider: "lab",
			Budgets:         v1alpha1.AssistantBudgets{Session: 200000, UserDay: 1000000, TenantDay: 2000000, Steps: 32},
		},
	}
}

func TestGetAssistantConfigToolNeedsAdmin(t *testing.T) {
	deps := assistantConfigDeps(&fakeMCPAssistantOperator{stored: storedAssistantObject("1")})
	tr := callToolAs(t, deps, mcpScopedDevClaims, "get_assistant_config", map[string]interface{}{})
	if !tr.IsError || !strings.Contains(toolText(tr), "assistant:admin") {
		t.Fatalf("non-admin read: isError=%v text=%q, want the assistant:admin refusal", tr.IsError, toolText(tr))
	}
}

func TestUpdateAssistantConfigToolMergesPatch(t *testing.T) {
	op := &fakeMCPAssistantOperator{stored: storedAssistantObject("1"), rv: 1}
	deps := assistantConfigDeps(op)
	tr := callToolAs(t, deps, mcpAdminClaims, "update_assistant_config", map[string]interface{}{
		"object": `{"spec":{"budgets":{"session":300000}}}`,
	})
	if tr.IsError {
		t.Fatalf("update: %s", toolText(tr))
	}
	if len(op.puts) != 1 {
		t.Fatalf("puts: %d, want 1", len(op.puts))
	}
	got := op.puts[0]
	if got.Spec.Budgets.Session != 300000 {
		t.Fatalf("session budget %d, want 300000", got.Spec.Budgets.Session)
	}
	if len(got.Spec.Providers) != 1 || got.Spec.Providers[0].Name != "lab" {
		t.Fatalf("merge dropped the provider list: %+v", got.Spec.Providers)
	}
}

// TestUpdateAssistantConfigToolKeepsReadVersion pins that the write fences on
// the version the tool read, not on a version the patch names.
func TestUpdateAssistantConfigToolKeepsReadVersion(t *testing.T) {
	op := &fakeMCPAssistantOperator{stored: storedAssistantObject("5"), rv: 5}
	deps := assistantConfigDeps(op)
	tr := callToolAs(t, deps, mcpAdminClaims, "update_assistant_config", map[string]interface{}{
		"object": `{"metadata":{"resourceVersion":"99"},"spec":{"approvalExpiry":"12h"}}`,
	})
	if tr.IsError {
		t.Fatalf("update: %s", toolText(tr))
	}
	if len(op.puts) != 1 || op.puts[0].ResourceVersion != "5" {
		t.Fatalf("write carried version %v, want 5 (the patch must not move the fence)", op.puts)
	}
}

func TestUpdateAssistantConfigToolRetriesOnConflict(t *testing.T) {
	op := &fakeMCPAssistantOperator{stored: storedAssistantObject("1"), rv: 1, conflictsLeft: 1}
	deps := assistantConfigDeps(op)
	tr := callToolAs(t, deps, mcpAdminClaims, "update_assistant_config", map[string]interface{}{
		"object": `{"spec":{"approvalExpiry":"12h"}}`,
	})
	if tr.IsError {
		t.Fatalf("update after one conflict: %s", toolText(tr))
	}
	if len(op.puts) != 2 {
		t.Fatalf("puts: %d, want 2 (one conflict, one retry)", len(op.puts))
	}
}
