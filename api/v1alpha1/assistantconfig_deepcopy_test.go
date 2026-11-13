package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAssistantConfigDeepCopyIsIndependent(t *testing.T) {
	in := &AssistantConfig{Spec: AssistantConfigSpec{
		Providers: []AssistantProvider{{
			Name:   "lab",
			Models: []string{"model-a"},
			CIDRs:  []string{"192.0.2.0/24"},
		}},
	}, Status: AssistantConfigStatus{
		Providers:  []AssistantProviderStatus{{Name: "lab", KeySet: true}},
		Conditions: []metav1.Condition{{Type: AssistantConfigCondEgressReady, Status: metav1.ConditionTrue}},
	}}
	out := in.DeepCopy()
	out.Spec.Providers[0].Models[0] = "changed"
	out.Spec.Providers[0].CIDRs[0] = "changed"
	out.Spec.Providers[0].Name = "changed"
	out.Status.Providers[0].KeySet = false
	out.Status.Conditions[0].Status = metav1.ConditionFalse

	if in.Spec.Providers[0].Models[0] != "model-a" || in.Spec.Providers[0].CIDRs[0] != "192.0.2.0/24" {
		t.Fatalf("copy shares provider slices with the original: %+v", in.Spec.Providers[0])
	}
	if in.Spec.Providers[0].Name != "lab" {
		t.Fatalf("copy shares the provider struct with the original")
	}
	if !in.Status.Providers[0].KeySet || in.Status.Conditions[0].Status != metav1.ConditionTrue {
		t.Fatalf("copy shares status with the original: %+v", in.Status)
	}
}
