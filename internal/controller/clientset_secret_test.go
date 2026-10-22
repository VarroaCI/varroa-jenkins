package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
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

// countVerb returns how many recorded actions on the fake clientset carry verb.
func countVerb(cs *fake.Clientset, verb string) int {
	n := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == verb {
			n++
		}
	}
	return n
}

// TestCreateOrUpdateSecret_RecreatesWhenUpdateAnswersNotFound covers a delete
// that lands between the Get and the Update. The method creates the Secret again.
func TestCreateOrUpdateSecret_RecreatesWhenUpdateAnswersNotFound(t *testing.T) {
	seed := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Data:       map[string][]byte{"k": []byte("old")},
	}
	cs := fake.NewSimpleClientset(seed)
	gvr := corev1.SchemeGroupVersion.WithResource("secrets")
	fired := false
	cs.PrependReactor("update", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		if fired {
			return false, nil, nil
		}
		fired = true
		if err := cs.Tracker().Delete(gvr, "ns", "s"); err != nil {
			t.Fatalf("delete from tracker: %v", err)
		}
		return true, nil, apierrors.NewNotFound(corev1.Resource("secrets"), "s")
	})
	c := &ClientsetClient{clientset: cs}

	if err := c.CreateOrUpdateSecret(context.Background(), "s", "ns", map[string][]byte{"k": []byte("new")}); err != nil {
		t.Fatalf("CreateOrUpdateSecret: %v", err)
	}
	live, err := cs.CoreV1().Secrets("ns").Get(context.Background(), "s", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get live secret: %v", err)
	}
	if string(live.Data["k"]) != "new" {
		t.Fatalf("live data = %q, want %q", live.Data["k"], "new")
	}
	if n := countVerb(cs, "create"); n != 2 {
		t.Fatalf("create actions = %d, want 2", n)
	}
}

// TestCreateOrUpdateSecret_RecreatesWhenGetAnswersNotFound covers a delete that
// lands between the AlreadyExists and the Get.
func TestCreateOrUpdateSecret_RecreatesWhenGetAnswersNotFound(t *testing.T) {
	seed := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Data:       map[string][]byte{"k": []byte("old")},
	}
	cs := fake.NewSimpleClientset(seed)
	gvr := corev1.SchemeGroupVersion.WithResource("secrets")
	fired := false
	cs.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		if fired {
			return false, nil, nil
		}
		fired = true
		if err := cs.Tracker().Delete(gvr, "ns", "s"); err != nil {
			t.Fatalf("delete from tracker: %v", err)
		}
		return true, nil, apierrors.NewNotFound(corev1.Resource("secrets"), "s")
	})
	c := &ClientsetClient{clientset: cs}

	if err := c.CreateOrUpdateSecret(context.Background(), "s", "ns", map[string][]byte{"k": []byte("new")}); err != nil {
		t.Fatalf("CreateOrUpdateSecret: %v", err)
	}
	live, err := cs.CoreV1().Secrets("ns").Get(context.Background(), "s", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get live secret: %v", err)
	}
	if string(live.Data["k"]) != "new" {
		t.Fatalf("live data = %q, want %q", live.Data["k"], "new")
	}
}

// TestCreateOrUpdateSecret_StopsAfterOneRecreate shows that a Secret which keeps
// vanishing returns NotFound after one recreate.
func TestCreateOrUpdateSecret_StopsAfterOneRecreate(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(corev1.Resource("secrets"), "s")
	})
	cs.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(corev1.Resource("secrets"), "s")
	})
	c := &ClientsetClient{clientset: cs}

	err := c.CreateOrUpdateSecret(context.Background(), "s", "ns", map[string][]byte{"k": []byte("new")})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound after one recreate, got %v", err)
	}
	if n := countVerb(cs, "create"); n != 2 {
		t.Fatalf("create actions = %d, want 2", n)
	}
}

// TestCreateOrUpdateSecret_ConflictNotRetried shows that a Conflict on the Update
// is returned at once and never retried.
func TestCreateOrUpdateSecret_ConflictNotRetried(t *testing.T) {
	seed := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Data:       map[string][]byte{"k": []byte("old")},
	}
	cs := fake.NewSimpleClientset(seed)
	cs.PrependReactor("update", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(corev1.Resource("secrets"), "s", errors.New("stale"))
	})
	c := &ClientsetClient{clientset: cs}

	err := c.CreateOrUpdateSecret(context.Background(), "s", "ns", map[string][]byte{"k": []byte("new")})
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected Conflict, got %v", err)
	}
	if n := countVerb(cs, "create"); n != 1 {
		t.Fatalf("create actions = %d, want 1", n)
	}
	if n := countVerb(cs, "update"); n != 1 {
		t.Fatalf("update actions = %d, want 1", n)
	}
}

// ownedSecretSeed returns the Secret that the owner and labelled tests start from.
func ownedSecretSeed() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Data:       map[string][]byte{"k": []byte("old")},
	}
}

// deleteOnceOnUpdate returns an update reactor that deletes the Secret from the
// tracker and answers NotFound on its first call only.
func deleteOnceOnUpdate(cs *fake.Clientset) clienttesting.ReactionFunc {
	gvr := corev1.SchemeGroupVersion.WithResource("secrets")
	fired := false
	return func(clienttesting.Action) (bool, runtime.Object, error) {
		if fired {
			return false, nil, nil
		}
		fired = true
		_ = cs.Tracker().Delete(gvr, "ns", "s")
		return true, nil, apierrors.NewNotFound(corev1.Resource("secrets"), "s")
	}
}

// TestCreateOrUpdateSecretWithOwner_RecreatesWhenUpdateAnswersNotFound covers a
// delete that lands between the Get and the Update. The method creates the Secret
// again, with the owner reference.
func TestCreateOrUpdateSecretWithOwner_RecreatesWhenUpdateAnswersNotFound(t *testing.T) {
	cs := fake.NewSimpleClientset(ownedSecretSeed())
	cs.PrependReactor("update", "secrets", deleteOnceOnUpdate(cs))
	c := &ClientsetClient{clientset: cs}
	owner := metav1.OwnerReference{APIVersion: "varroa.dev/v1alpha1", Kind: "Controller", Name: "c", UID: "u1"}

	if err := c.CreateOrUpdateSecretWithOwner(context.Background(), "s", "ns", map[string][]byte{"k": []byte("new")}, owner); err != nil {
		t.Fatalf("CreateOrUpdateSecretWithOwner: %v", err)
	}
	live, err := cs.CoreV1().Secrets("ns").Get(context.Background(), "s", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get live secret: %v", err)
	}
	if string(live.Data["k"]) != "new" {
		t.Fatalf("live data = %q, want %q", live.Data["k"], "new")
	}
	if len(live.OwnerReferences) != 1 || live.OwnerReferences[0].UID != "u1" {
		t.Fatalf("live owner references = %v, want UID u1", live.OwnerReferences)
	}
	if n := countVerb(cs, "create"); n != 1 {
		t.Fatalf("create actions = %d, want 1", n)
	}
	if n := countVerb(cs, "update"); n != 1 {
		t.Fatalf("update actions = %d, want 1", n)
	}
}

// TestCreateOrUpdateSecretWithOwner_StopsAfterOneRetry shows that a Secret which
// keeps vanishing returns NotFound after one retry, and never creates it.
func TestCreateOrUpdateSecretWithOwner_StopsAfterOneRetry(t *testing.T) {
	cs := fake.NewSimpleClientset(ownedSecretSeed())
	cs.PrependReactor("update", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(corev1.Resource("secrets"), "s")
	})
	c := &ClientsetClient{clientset: cs}
	owner := metav1.OwnerReference{APIVersion: "varroa.dev/v1alpha1", Kind: "Controller", Name: "c", UID: "u1"}

	err := c.CreateOrUpdateSecretWithOwner(context.Background(), "s", "ns", map[string][]byte{"k": []byte("new")}, owner)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound after one retry, got %v", err)
	}
	if n := countVerb(cs, "update"); n != 2 {
		t.Fatalf("update actions = %d, want 2", n)
	}
	if n := countVerb(cs, "create"); n != 0 {
		t.Fatalf("create actions = %d, want 0", n)
	}
}

// TestCreateOrUpdateSecretWithOwner_ConflictNotRetried shows that a Conflict on
// the Update is returned at once.
func TestCreateOrUpdateSecretWithOwner_ConflictNotRetried(t *testing.T) {
	cs := fake.NewSimpleClientset(ownedSecretSeed())
	cs.PrependReactor("update", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(corev1.Resource("secrets"), "s", errors.New("stale"))
	})
	c := &ClientsetClient{clientset: cs}
	owner := metav1.OwnerReference{APIVersion: "varroa.dev/v1alpha1", Kind: "Controller", Name: "c", UID: "u1"}

	err := c.CreateOrUpdateSecretWithOwner(context.Background(), "s", "ns", map[string][]byte{"k": []byte("new")}, owner)
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected Conflict, got %v", err)
	}
	if n := countVerb(cs, "update"); n != 1 {
		t.Fatalf("update actions = %d, want 1", n)
	}
	if n := countVerb(cs, "create"); n != 0 {
		t.Fatalf("create actions = %d, want 0", n)
	}
	if n := countVerb(cs, "get"); n != 1 {
		t.Fatalf("get actions = %d, want 1", n)
	}
}

// TestCreateOrUpdateSecretWithLabels_UpdateNotFoundIsReturned covers a delete
// that lands between the Get and the Update. The method returns the NotFound and
// creates nothing, because the data can be a partial store.
func TestCreateOrUpdateSecretWithLabels_UpdateNotFoundIsReturned(t *testing.T) {
	cs := fake.NewSimpleClientset(ownedSecretSeed())
	cs.PrependReactor("update", "secrets", deleteOnceOnUpdate(cs))
	c := &ClientsetClient{clientset: cs}

	labels := map[string]string{"varroa.dev/cell-pool": "p"}
	err := c.CreateOrUpdateSecretWithLabels(context.Background(), "s", "ns", labels, map[string][]byte{"k": []byte("new")})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound, got %v", err)
	}
	if n := countVerb(cs, "get"); n != 1 {
		t.Fatalf("get actions = %d, want 1", n)
	}
	if n := countVerb(cs, "update"); n != 1 {
		t.Fatalf("update actions = %d, want 1", n)
	}
	if n := countVerb(cs, "create"); n != 0 {
		t.Fatalf("create actions = %d, want 0", n)
	}
	if _, err := cs.CoreV1().Secrets("ns").Get(context.Background(), "s", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("live secret after the delete: got %v, want NotFound", err)
	}
}

// TestCreateOrUpdateSecretWithLabels_GetNotFoundIsReturned shows that an absent
// Secret answers NotFound from the Get and is never created.
func TestCreateOrUpdateSecretWithLabels_GetNotFoundIsReturned(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := &ClientsetClient{clientset: cs}

	err := c.CreateOrUpdateSecretWithLabels(context.Background(), "s", "ns", map[string]string{"a": "b"}, map[string][]byte{"k": []byte("new")})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound, got %v", err)
	}
	if n := countVerb(cs, "get"); n != 1 {
		t.Fatalf("get actions = %d, want 1", n)
	}
	if n := countVerb(cs, "create"); n != 0 {
		t.Fatalf("create actions = %d, want 0", n)
	}
	if n := countVerb(cs, "update"); n != 0 {
		t.Fatalf("update actions = %d, want 0", n)
	}
}

// TestCreateOrUpdateSecretWithLabels_ConflictNotRetried shows that a Conflict on
// the Update is returned at once.
func TestCreateOrUpdateSecretWithLabels_ConflictNotRetried(t *testing.T) {
	cs := fake.NewSimpleClientset(ownedSecretSeed())
	cs.PrependReactor("update", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(corev1.Resource("secrets"), "s", errors.New("stale"))
	})
	c := &ClientsetClient{clientset: cs}

	err := c.CreateOrUpdateSecretWithLabels(context.Background(), "s", "ns", map[string]string{"a": "b"}, map[string][]byte{"k": []byte("new")})
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected Conflict, got %v", err)
	}
	if n := countVerb(cs, "update"); n != 1 {
		t.Fatalf("update actions = %d, want 1", n)
	}
	if n := countVerb(cs, "create"); n != 0 {
		t.Fatalf("create actions = %d, want 0", n)
	}
}

// notFoundOnceOnGet returns a get reactor that answers NotFound on its first
// call only. The Create that follows then answers AlreadyExists from the tracker.
func notFoundOnceOnGet() clienttesting.ReactionFunc {
	fired := false
	return func(clienttesting.Action) (bool, runtime.Object, error) {
		if fired {
			return false, nil, nil
		}
		fired = true
		return true, nil, apierrors.NewNotFound(corev1.Resource("secrets"), "s")
	}
}

// TestCreateOrUpdateSecretWithOwner_UpdatesWhenCreateAnswersAlreadyExists covers
// a create that lands between the Get and the Create. The method reads the live
// Secret and updates it with the owner reference.
func TestCreateOrUpdateSecretWithOwner_UpdatesWhenCreateAnswersAlreadyExists(t *testing.T) {
	cs := fake.NewSimpleClientset(ownedSecretSeed())
	cs.PrependReactor("get", "secrets", notFoundOnceOnGet())
	c := &ClientsetClient{clientset: cs}
	owner := metav1.OwnerReference{APIVersion: "varroa.dev/v1alpha1", Kind: "Controller", Name: "c", UID: "u1"}

	if err := c.CreateOrUpdateSecretWithOwner(context.Background(), "s", "ns", map[string][]byte{"k": []byte("new")}, owner); err != nil {
		t.Fatalf("CreateOrUpdateSecretWithOwner: %v", err)
	}
	if n := countVerb(cs, "get"); n != 2 {
		t.Fatalf("get actions = %d, want 2", n)
	}
	live, err := cs.CoreV1().Secrets("ns").Get(context.Background(), "s", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get live secret: %v", err)
	}
	if string(live.Data["k"]) != "new" {
		t.Fatalf("live data = %q, want %q", live.Data["k"], "new")
	}
	if len(live.OwnerReferences) != 1 || live.OwnerReferences[0].UID != "u1" {
		t.Fatalf("live owner references = %v, want UID u1", live.OwnerReferences)
	}
	if n := countVerb(cs, "create"); n != 1 {
		t.Fatalf("create actions = %d, want 1", n)
	}
	if n := countVerb(cs, "update"); n != 1 {
		t.Fatalf("update actions = %d, want 1", n)
	}
}

// TestCreateOrUpdateSecretWithOwner_AlreadyExistsThenUpdateNotFound pins the
// bounded loop: AlreadyExists sends the method back to the Get once, and a
// NotFound on that update is returned without a second create.
func TestCreateOrUpdateSecretWithOwner_AlreadyExistsThenUpdateNotFound(t *testing.T) {
	cs := fake.NewSimpleClientset(ownedSecretSeed())
	cs.PrependReactor("get", "secrets", notFoundOnceOnGet())
	cs.PrependReactor("update", "secrets", deleteOnceOnUpdate(cs))
	c := &ClientsetClient{clientset: cs}
	owner := metav1.OwnerReference{APIVersion: "varroa.dev/v1alpha1", Kind: "Controller", Name: "c", UID: "u1"}

	err := c.CreateOrUpdateSecretWithOwner(context.Background(), "s", "ns", map[string][]byte{"k": []byte("new")}, owner)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound, got %v", err)
	}
	if n := countVerb(cs, "create"); n != 1 {
		t.Fatalf("create actions = %d, want 1", n)
	}
	if n := countVerb(cs, "update"); n != 1 {
		t.Fatalf("update actions = %d, want 1", n)
	}
}

// TestCreateOrUpdateSecretWithOwner_UpdatesOwnerWhenDataMatches covers a live
// Secret whose Data matches but which has no owner reference. The first call
// writes the owner. A second call finds the Secret converged and writes nothing.
func TestCreateOrUpdateSecretWithOwner_UpdatesOwnerWhenDataMatches(t *testing.T) {
	seed := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Data:       map[string][]byte{"k": []byte("new")},
	}
	cs := fake.NewSimpleClientset(seed)
	c := &ClientsetClient{clientset: cs}
	owner := metav1.OwnerReference{APIVersion: "varroa.dev/v1alpha1", Kind: "Controller", Name: "c", UID: "u1"}
	data := map[string][]byte{"k": []byte("new")}

	if err := c.CreateOrUpdateSecretWithOwner(context.Background(), "s", "ns", data, owner); err != nil {
		t.Fatalf("first call: %v", err)
	}
	live, err := cs.CoreV1().Secrets("ns").Get(context.Background(), "s", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get live secret: %v", err)
	}
	if len(live.OwnerReferences) != 1 || live.OwnerReferences[0].UID != "u1" {
		t.Fatalf("live owner references = %v, want UID u1", live.OwnerReferences)
	}
	if n := countVerb(cs, "update"); n != 1 {
		t.Fatalf("update actions after first call = %d, want 1", n)
	}

	if err := c.CreateOrUpdateSecretWithOwner(context.Background(), "s", "ns", data, owner); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if n := countVerb(cs, "update"); n != 1 {
		t.Fatalf("update actions after second call = %d, want 1", n)
	}
}

// TestCreateOrUpdateSecretWithOwner_NoWriteWhenOwnerAmongOthers covers a Secret
// that another manager co-owns. The owner is already present, so the method
// writes nothing and leaves the other owner reference in place.
func TestCreateOrUpdateSecretWithOwner_NoWriteWhenOwnerAmongOthers(t *testing.T) {
	owner := metav1.OwnerReference{APIVersion: "varroa.dev/v1alpha1", Kind: "Controller", Name: "c", UID: "u1"}
	other := metav1.OwnerReference{APIVersion: "v1", Kind: "Other", Name: "o", UID: "u2"}
	seed := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns", OwnerReferences: []metav1.OwnerReference{other, owner}},
		Data:       map[string][]byte{"k": []byte("new")},
	}
	cs := fake.NewSimpleClientset(seed)
	c := &ClientsetClient{clientset: cs}

	if err := c.CreateOrUpdateSecretWithOwner(context.Background(), "s", "ns", map[string][]byte{"k": []byte("new")}, owner); err != nil {
		t.Fatalf("CreateOrUpdateSecretWithOwner: %v", err)
	}
	if n := countVerb(cs, "update"); n != 0 {
		t.Fatalf("update actions = %d, want 0", n)
	}
}
