package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
)

// cellSecretsTestPool is the pool every test of this file names.
const cellSecretsTestPool = "big"

// cellSecretsTestNamespace is both the operator namespace and the namespace
// of every pool cell, which is the invariant resolveCellSecretsName reads.
const cellSecretsTestNamespace = "varroa-system"

// builtInitContainerVolumeMounts returns the volumeMounts of one init
// container of a StatefulSet rendered by buildStatefulSet.
func builtInitContainerVolumeMounts(t *testing.T, sts *unstructured.Unstructured, container string) []map[string]interface{} {
	t.Helper()
	podSpec := sts.Object["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
	for _, cm := range podSpec["initContainers"].([]map[string]interface{}) {
		if cm["name"] != container {
			continue
		}
		mounts, _ := cm["volumeMounts"].([]map[string]interface{})
		return mounts
	}
	t.Fatalf("init container %q not found", container)
	return nil
}

// builtPodAnnotations returns the pod-template annotations of a rendered
// StatefulSet. The builder stores them as map[string]interface{}, so the
// read goes through NestedStringMap, the way the production code does.
func builtPodAnnotations(t *testing.T, sts *unstructured.Unstructured) map[string]string {
	t.Helper()
	anns, _, err := unstructured.NestedStringMap(sts.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		t.Fatalf("read pod template annotations: %v", err)
	}
	return anns
}

// completePoolStore returns a usable pool store for the test pool.
func completePoolStore(t *testing.T) map[string][]byte {
	t.Helper()
	data, err := cells.MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	return data
}

// poolCellFor returns a pool cell in the operator namespace.
func poolCellFor() *v1alpha1.Controller {
	cr := testController("cell-big-0", cellSecretsTestNamespace, v1alpha1.ControllerPhaseProvisioning)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	cr.Spec.ComposedBundleRef = &v1alpha1.ComposedBundleRef{Name: "test-bundle", Namespace: "ns"}
	cr.Labels = map[string]string{cells.LabelCellPool: cellSecretsTestPool}
	return cr
}

// poolCellReconciler returns a Reconciler whose operator namespace equals
// the namespace of every pool cell this file builds. It also passes the cell
// configuration gate, which returns before the pool store resolution.
func poolCellReconciler(client *testClient, cr *v1alpha1.Controller) *Reconciler {
	client.existingSecrets["abase-"+cellSecretsTestPool] = map[string][]byte{
		"access-key-id": []byte("a"), "secret-access-key": []byte("b"),
	}
	if cr != nil {
		pinnedProfile(client, cr)
	}
	rec := newTestReconciler(client)
	rec.operatorNamespace = cellSecretsTestNamespace
	rec.SetClaimGate("1")
	rec.SetArtifactSecretBase("abase")
	rec.SetBlobstoreURL("s3://my-bucket?region=us-east-1")
	seedClientCRDs(client)
	return rec
}

// TestBuildStatefulSet_CellSecretsMount asserts the volume and the
// plugins-init mount a pool cell carries.
func TestBuildStatefulSet_CellSecretsMount(t *testing.T) {
	sts := buildStatefulSet(StatefulSetSpec{
		Name: "cell-big-0-00000000", Namespace: cellSecretsTestNamespace, ControllerName: "cell-big-0",
		Cell:            true,
		CellSecretsName: cells.PoolSecretName(cellSecretsTestPool),
	})

	found := false
	for _, v := range builtVolumes(t, sts) {
		if v["name"] != cellSecretsVolumeName {
			continue
		}
		found = true
		secret, _ := v["secret"].(map[string]interface{})
		if secret["secretName"] != cells.PoolSecretName(cellSecretsTestPool) {
			t.Errorf("volume secretName = %v, want %v", secret["secretName"], cells.PoolSecretName(cellSecretsTestPool))
		}
	}
	if !found {
		t.Fatal("expected a varroa-cell-secrets volume")
	}

	mounted := false
	for _, m := range builtInitContainerVolumeMounts(t, sts, "plugins-init") {
		if m["name"] != cellSecretsVolumeName {
			continue
		}
		mounted = true
		if m["mountPath"] != cellSecretsMountPath {
			t.Errorf("mountPath = %v, want %v", m["mountPath"], cellSecretsMountPath)
		}
		if m["readOnly"] != true {
			t.Error("expected the pool store mount to be readOnly")
		}
	}
	if !mounted {
		t.Fatal("expected plugins-init to mount varroa-cell-secrets")
	}
}

// TestBuildStatefulSet_NoCellSecrets asserts that a controller with an empty
// CellSecretsName renders no volume and no mount. It covers the non-cell
// controller, whose render stays unchanged.
func TestBuildStatefulSet_NoCellSecrets(t *testing.T) {
	sts := buildStatefulSet(StatefulSetSpec{
		Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci",
	})
	for _, v := range builtVolumes(t, sts) {
		if v["name"] == cellSecretsVolumeName {
			t.Fatal("expected no varroa-cell-secrets volume when CellSecretsName is empty")
		}
	}
	for _, m := range builtInitContainerVolumeMounts(t, sts, "plugins-init") {
		if m["name"] == cellSecretsVolumeName {
			t.Fatal("expected no varroa-cell-secrets mount when CellSecretsName is empty")
		}
	}
}

// TestPluginsInitScript_SeedsTheStore asserts the guarded copy: the script
// follows each symlink, sets 0700 on the directory, and sets 0600 on each
// file.
func TestPluginsInitScript_SeedsTheStore(t *testing.T) {
	for _, want := range []string{
		"if [ -d " + cellSecretsMountPath + " ]; then",
		"chmod 700 /var/jenkins_home/secrets",
		"cp -L " + cellSecretsMountPath + "/* /var/jenkins_home/secrets/",
		"chmod 600 /var/jenkins_home/secrets/*",
	} {
		if !strings.Contains(pluginsInitScript, want) {
			t.Errorf("pluginsInitScript does not carry %q", want)
		}
	}
}

// TestBuildStatefulSet_CellSecretsHasNoContentHash asserts that the pod
// template carries no hash of the pool store, so a hand edit rolls no pod.
// The spec sets the hashes that do reach the pod template, so the absent
// key is proof rather than an empty annotation map.
func TestBuildStatefulSet_CellSecretsHasNoContentHash(t *testing.T) {
	spec := StatefulSetSpec{
		Name: "cell-big-0-00000000", Namespace: cellSecretsTestNamespace, ControllerName: "cell-big-0",
		Cell:            true,
		CellSecretsName: cells.PoolSecretName(cellSecretsTestPool),
		PluginsChecksum: "plugins-checksum",
	}
	anns := builtPodAnnotations(t, buildStatefulSet(spec))
	if len(anns) == 0 {
		t.Fatal("expected the pod template to carry annotations")
	}
	if _, ok := anns["varroa.dev/plugins-checksum"]; !ok {
		t.Fatalf("expected the plugins checksum annotation, got %v", anns)
	}
	for key := range anns {
		if strings.Contains(key, "cell-secrets") {
			t.Errorf("the pod template carries the annotation %q", key)
		}
	}
}

// TestCreateStatefulSet_CellSecretsHasNoContentHash asserts the same rule
// on the write path, where the artifact Secret's own hash annotation is
// stamped. A pool store hash would land beside it.
func TestCreateStatefulSet_CellSecretsHasNoContentHash(t *testing.T) {
	c, dyn := newFakeStsClient()
	spec := baseCascHashSpec("cell-secrets-hash", "casc-hash")
	spec.Cell = true
	spec.CellSecretsName = cells.PoolSecretName(cellSecretsTestPool)
	spec.ArtifactSecretName = "cell-artifacts"
	spec.ArtifactSecretHash = "deadbeef"

	if err := c.CreateStatefulSet(context.Background(), spec, noStatefulSetFence); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := dyn.Resource(stsGVR).Namespace("ns").Get(context.Background(), "cell-secrets-hash", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	anns := builtPodAnnotations(t, got)
	if _, ok := anns[artifactSecretHashAnnotation]; !ok {
		t.Fatalf("expected the artifact secret hash annotation, got %v", anns)
	}
	for key := range anns {
		if strings.Contains(key, "cell-secrets") {
			t.Errorf("the pod template carries the annotation %q", key)
		}
	}
}

// TestResolveCellSecretsName_Resolves asserts the happy path and the three
// ordered tests that answer an empty name with no error.
func TestResolveCellSecretsName_Resolves(t *testing.T) {
	name := cells.PoolSecretName(cellSecretsTestPool)

	t.Run("a pool cell resolves its store", func(t *testing.T) {
		client := newTestClientWithBundle()
		client.existingSecrets[name] = completePoolStore(t)
		cr := poolCellFor()
		rec := poolCellReconciler(client, nil)
		got, err := rec.resolveCellSecretsName(context.Background(), cr)
		if err != nil {
			t.Fatalf("resolveCellSecretsName: %v", err)
		}
		if got != name {
			t.Fatalf("name = %q, want %q", got, name)
		}
	})

	t.Run("a non-cell controller resolves nothing", func(t *testing.T) {
		client := newTestClientWithBundle()
		client.existingSecrets[name] = completePoolStore(t)
		rec := poolCellReconciler(client, nil)
		cr := testController("ci", cellSecretsTestNamespace, v1alpha1.ControllerPhaseProvisioning)
		cr.Labels = map[string]string{cells.LabelCellPool: cellSecretsTestPool}
		got, err := rec.resolveCellSecretsName(context.Background(), cr)
		if err != nil || got != "" {
			t.Fatalf("name = %q, err = %v, want an empty name and no error", got, err)
		}
	})

	t.Run("a cell outside the operator namespace resolves nothing", func(t *testing.T) {
		client := newTestClientWithBundle()
		client.existingSecrets[name] = completePoolStore(t)
		rec := poolCellReconciler(client, nil)
		cr := poolCellFor()
		cr.Namespace = "team-a"
		got, err := rec.resolveCellSecretsName(context.Background(), cr)
		if err != nil || got != "" {
			t.Fatalf("name = %q, err = %v, want an empty name and no error", got, err)
		}
	})

	t.Run("a cell with no pool label resolves nothing", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := poolCellReconciler(client, nil)
		cr := poolCellFor()
		cr.Labels = nil
		got, err := rec.resolveCellSecretsName(context.Background(), cr)
		if err != nil || got != "" {
			t.Fatalf("name = %q, err = %v, want an empty name and no error", got, err)
		}
	})
}

// TestHandleProvisioning_CellSecretsFaultBlocksReconcile asserts that each
// unusable state blocks the reconcile with the reason CellSecretsFailed and
// builds no StatefulSet.
func TestHandleProvisioning_CellSecretsFaultBlocksReconcile(t *testing.T) {
	name := cells.PoolSecretName(cellSecretsTestPool)
	cases := []struct {
		label string
		seed  func(client *testClient)
	}{
		{
			label: "absent",
			seed: func(client *testClient) {
				client.getSecretErrs = map[string]error{
					name: apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name),
				}
			},
		},
		{
			label: "empty data",
			seed: func(client *testClient) {
				client.existingSecrets[name] = map[string][]byte{}
			},
		},
		{
			label: "a missing key",
			seed: func(client *testClient) {
				data := completePoolStore(t)
				delete(data, cells.KeyHudsonUtilSecret)
				client.existingSecrets[name] = data
			},
		},
		{
			label: "a key that does not decrypt",
			seed: func(client *testClient) {
				data := completePoolStore(t)
				value := data[cells.KeyJnlpSecret]
				value[len(value)-1] ^= 0xff
				client.existingSecrets[name] = data
			},
		},
		{
			label: "a key of another store",
			seed: func(client *testClient) {
				data := completePoolStore(t)
				data[cells.KeyInstanceIdentity] = completePoolStore(t)[cells.KeyInstanceIdentity]
				client.existingSecrets[name] = data
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			client := newTestClientWithBundle()
			tc.seed(client)
			cr := poolCellFor()
			rec := poolCellReconciler(client, cr)

			if err := rec.handleProvisioning(context.Background(), cr); err == nil {
				t.Fatal("expected handleProvisioning to fail on an unusable pool store")
			}
			if len(client.stsSpecs) != 0 {
				t.Errorf("expected no StatefulSet, got %d", len(client.stsSpecs))
			}
			cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionReconcileBlocked)
			if cond == nil || cond.Status != metav1.ConditionTrue ||
				cond.Reason != v1alpha1.ReasonReconcileBlockedCellSecretsFailed {
				t.Fatalf("ReconcileBlocked = %+v, want True/%s", cond, v1alpha1.ReasonReconcileBlockedCellSecretsFailed)
			}
			if !strings.Contains(cr.Status.LastReconcileError, name) {
				t.Errorf("lastReconcileError = %q, want it to name %q", cr.Status.LastReconcileError, name)
			}
		})
	}
}

// TestHandleProvisioning_HandEditRollsNoPod asserts that a changed pool
// store leaves the rendered StatefulSet identical, so the operator rolls no
// pod on its own.
func TestHandleProvisioning_HandEditRollsNoPod(t *testing.T) {
	name := cells.PoolSecretName(cellSecretsTestPool)
	client := newTestClientWithBundle()
	client.existingSecrets[name] = completePoolStore(t)
	cr := poolCellFor()
	rec := poolCellReconciler(client, cr)

	if err := rec.handleProvisioning(context.Background(), cr); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	client.existingSecrets[name] = completePoolStore(t)
	if err := rec.handleProvisioning(context.Background(), cr); err != nil {
		t.Fatalf("tick 2: %v", err)
	}

	if len(client.stsSpecs) < 2 {
		t.Fatalf("expected two StatefulSet builds, got %d", len(client.stsSpecs))
	}
	first := client.stsSpecs[0]
	second := client.stsSpecs[len(client.stsSpecs)-1]
	if !reflect.DeepEqual(first, second) {
		t.Error("the hand edit changed the rendered StatefulSet")
	}
}
