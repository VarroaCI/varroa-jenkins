package v1alpha1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestPodOverridesDeepCopyIsolatesCorev1SubObjects implements the controller-resource-overrides
// "DeepCopy isolates embedded corev1 sub-objects" scenario: a populated PodOverrides (env + a
// non-nil affinity) is deep-copied, the COPY is mutated, and the ORIGINAL must be unchanged.
func TestPodOverridesDeepCopyIsolatesCorev1SubObjects(t *testing.T) {
	orig := ControllerSpec{
		PodOverrides: &PodOverrides{
			Env: []corev1.EnvVar{{Name: "FOO", Value: "bar"}},
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{
							{MatchExpressions: []corev1.NodeSelectorRequirement{
								{Key: "disktype", Operator: corev1.NodeSelectorOpIn, Values: []string{"ssd"}},
							}},
						},
					},
				},
			},
		},
	}

	cp := new(ControllerSpec)
	orig.DeepCopyInto(cp)

	// Mutate the copy's embedded corev1 sub-objects.
	cp.PodOverrides.Env[0].Name = "MUTATED"
	cp.PodOverrides.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.
		NodeSelectorTerms[0].MatchExpressions[0].Key = "mutated"

	if got := orig.PodOverrides.Env[0].Name; got != "FOO" {
		t.Fatalf("env shallow-copied: original Env[0].Name = %q, want FOO", got)
	}
	if got := orig.PodOverrides.Affinity.NodeAffinity.
		RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].
		MatchExpressions[0].Key; got != "disktype" {
		t.Fatalf("affinity shallow-copied: original key = %q, want disktype", got)
	}
}
