package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestCreateSecretExclusiveSurfacesAlreadyExists guards the API-key collision
// path: unlike CreateSecret, the exclusive variant must NOT swallow
// AlreadyExists, so Generate can detect a prefix clash and retry.
func TestCreateSecretExclusiveSurfacesAlreadyExists(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	ctx := context.Background()
	labels := map[string]string{"varroa.dev/apikey": "true"}
	data := map[string][]byte{"hash": []byte("x")}

	if err := c.CreateSecretExclusive(ctx, "apikey-abc", "ns", labels, data); err != nil {
		t.Fatalf("first create: %v", err)
	}
	err := c.CreateSecretExclusive(ctx, "apikey-abc", "ns", labels, data)
	if !apierrors.IsAlreadyExists(err) {
		t.Fatalf("expected AlreadyExists on duplicate, got %v", err)
	}
}

// TestPatchSecretDataPreservesLabels guards the lastUsed flush: patching one
// data key must not drop the key's labels (which list/ownership rely on).
func TestPatchSecretDataPreservesLabels(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	ctx := context.Background()
	labels := map[string]string{
		"varroa.dev/apikey":      "true",
		"varroa.dev/apikey-user": "nathan",
	}
	data := map[string][]byte{"hash": []byte("x"), "lastUsed": []byte("")}
	if err := c.CreateSecretExclusive(ctx, "apikey-abc", "ns", labels, data); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := c.PatchSecretData(ctx, "apikey-abc", "ns", map[string][]byte{"lastUsed": []byte("2026-06-06T00:00:00Z")}); err != nil {
		t.Fatalf("patch: %v", err)
	}

	got, err := c.clientset.CoreV1().Secrets("ns").Get(ctx, "apikey-abc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Labels["varroa.dev/apikey-user"] != "nathan" {
		t.Errorf("label dropped after patch: labels=%v", got.Labels)
	}
	if string(got.Data["lastUsed"]) != "2026-06-06T00:00:00Z" {
		t.Errorf("lastUsed not patched: %q", got.Data["lastUsed"])
	}
	if string(got.Data["hash"]) != "x" {
		t.Errorf("hash data lost after patch: %q", got.Data["hash"])
	}
}

// TestCopyImagePullSecretPreservesTypeAndData guards that a docker-registry
// Secret's Type and Data are preserved when copying to another namespace.
func TestCopyImagePullSecretPreservesTypeAndData(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	ctx := context.Background()

	// Create source secret in operator namespace.
	srcData := map[string][]byte{".dockerconfigjson": []byte(`{"auths":{"ghcr.io":{"auth":"dXNlcjpwYXNz"}}}`)}
	src := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry-creds", Namespace: "varroa"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       srcData,
	}
	if _, err := c.clientset.CoreV1().Secrets("varroa").Create(ctx, src, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create source secret: %v", err)
	}

	if err := c.CopyImagePullSecret(ctx, "varroa", "teams-payments", "registry-creds"); err != nil {
		t.Fatalf("CopyImagePullSecret: %v", err)
	}

	dst, err := c.clientset.CoreV1().Secrets("teams-payments").Get(ctx, "registry-creds", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get destination secret: %v", err)
	}
	if dst.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("expected Type=%q, got %q", corev1.SecretTypeDockerConfigJson, dst.Type)
	}
	if string(dst.Data[".dockerconfigjson"]) != string(srcData[".dockerconfigjson"]) {
		t.Errorf("expected Data to match source")
	}
}

// TestCopyImagePullSecretConvergesOnMutation guards that mutating the source
// Secret triggers an Update on the destination.
func TestCopyImagePullSecretConvergesOnMutation(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	ctx := context.Background()

	oldData := map[string][]byte{".dockerconfigjson": []byte(`{"auths":{"ghcr.io":{"auth":"b2xkOnBhc3M="}}}`)}
	src := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry-creds", Namespace: "varroa"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       oldData,
	}
	if _, err := c.clientset.CoreV1().Secrets("varroa").Create(ctx, src, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create source secret: %v", err)
	}
	// Create destination with same data first.
	dst := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry-creds", Namespace: "teams-payments"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       oldData,
	}
	if _, err := c.clientset.CoreV1().Secrets("teams-payments").Create(ctx, dst, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create dest secret: %v", err)
	}

	// Mutate source.
	newData := map[string][]byte{".dockerconfigjson": []byte(`{"auths":{"ghcr.io":{"auth":"bmV3OnBhc3M="}}}`)}
	src.Data = newData
	if _, err := c.clientset.CoreV1().Secrets("varroa").Update(ctx, src, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update source secret: %v", err)
	}

	if err := c.CopyImagePullSecret(ctx, "varroa", "teams-payments", "registry-creds"); err != nil {
		t.Fatalf("CopyImagePullSecret: %v", err)
	}

	got, err := c.clientset.CoreV1().Secrets("teams-payments").Get(ctx, "registry-creds", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get dest secret: %v", err)
	}
	if string(got.Data[".dockerconfigjson"]) != string(newData[".dockerconfigjson"]) {
		t.Errorf("destination Data not updated: got %q, want %q", got.Data, newData)
	}
}

// TestCopyImagePullSecretSourceAbsentIsNoop guards that a missing source
// Secret is silently ignored.
func TestCopyImagePullSecretSourceAbsentIsNoop(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	ctx := context.Background()

	if err := c.CopyImagePullSecret(ctx, "varroa", "teams-payments", "registry-creds"); err != nil {
		t.Fatalf("expected nil for missing source, got %v", err)
	}
	_, err := c.clientset.CoreV1().Secrets("teams-payments").Get(ctx, "registry-creds", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected destination secret to not exist, got %v", err)
	}
}

// TestCopyImagePullSecretSkipsWriteWhenConverged guards the equality guard:
// when source and destination are already identical, ResourceVersion must not
// bump.
func TestCopyImagePullSecretSkipsWriteWhenConverged(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	ctx := context.Background()

	data := map[string][]byte{".dockerconfigjson": []byte(`{"auths":{"ghcr.io":{"auth":"dXNlcjpwYXNz"}}}`)}
	// Create source.
	src := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry-creds", Namespace: "varroa"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       data,
	}
	if _, err := c.clientset.CoreV1().Secrets("varroa").Create(ctx, src, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create source secret: %v", err)
	}
	// Create identical destination.
	dst := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry-creds", Namespace: "teams-payments"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       data,
	}
	created, err := c.clientset.CoreV1().Secrets("teams-payments").Create(ctx, dst, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create dest secret: %v", err)
	}
	origRV := created.ResourceVersion

	if err := c.CopyImagePullSecret(ctx, "varroa", "teams-payments", "registry-creds"); err != nil {
		t.Fatalf("CopyImagePullSecret: %v", err)
	}

	got, err := c.clientset.CoreV1().Secrets("teams-payments").Get(ctx, "registry-creds", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get dest secret: %v", err)
	}
	if got.ResourceVersion != origRV {
		t.Errorf("ResourceVersion changed from %q to %q — equality guard should have skipped the write", origRV, got.ResourceVersion)
	}
}

// TestCopyImagePullSecretSameNamespaceIsNoop guards that src==dst returns nil
// without any API call.
func TestCopyImagePullSecretSameNamespaceIsNoop(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	ctx := context.Background()

	if err := c.CopyImagePullSecret(ctx, "varroa", "varroa", "registry-creds"); err != nil {
		t.Fatalf("expected nil for same namespace, got %v", err)
	}
}
