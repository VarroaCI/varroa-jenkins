package v1alpha1

import "testing"

func TestProvisioningDefaultsDeepCopyOwnsImagePullSecrets(t *testing.T) {
	in := &ProvisioningDefaults{Spec: ProvisioningDefaultsSpec{
		ImagePullSecrets: []string{"registry-a", "registry-b"},
	}}
	out := in.DeepCopy()
	out.Spec.ImagePullSecrets[0] = "changed"

	if in.Spec.ImagePullSecrets[0] != "registry-a" {
		t.Fatalf("copy shares ImagePullSecrets backing array: original[0] = %q", in.Spec.ImagePullSecrets[0])
	}
}
