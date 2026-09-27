package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
)

// TestBuildStatefulSetCellVolumeShape asserts design D1: a cell omits
// volumeClaimTemplates entirely and renders the jenkins-home pod volume as
// an emptyDir instead of a claim. A normal controller keeps its claim
// template and has no explicit jenkins-home pod volume (the StatefulSet
// binds it implicitly from the claim template). Both shapes mount
// jenkins-home at /var/jenkins_home on the jenkins container.
func TestBuildStatefulSetCellVolumeShape(t *testing.T) {
	cases := []struct {
		name string
		cell bool
	}{
		{name: "normal controller", cell: false},
		{name: "cell", cell: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sts := buildStatefulSet(StatefulSetSpec{
				Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci",
				Cell: tc.cell,
			})

			_, hasClaimTemplate := sts.Object["spec"].(map[string]interface{})["volumeClaimTemplates"]
			if hasClaimTemplate == tc.cell {
				t.Errorf("volumeClaimTemplates present = %v, want %v", hasClaimTemplate, !tc.cell)
			}

			mountPath := ""
			for _, m := range builtContainerVolumeMounts(t, sts, "jenkins") {
				if m["name"] == "jenkins-home" {
					mountPath, _ = m["mountPath"].(string)
				}
			}
			if mountPath != "/var/jenkins_home" {
				t.Errorf("jenkins-home mountPath = %q, want /var/jenkins_home", mountPath)
			}

			hasEmptyDirVolume := false
			for _, v := range builtVolumes(t, sts) {
				if v["name"] == "jenkins-home" {
					_, hasEmptyDirVolume = v["emptyDir"]
				}
			}
			if hasEmptyDirVolume != tc.cell {
				t.Errorf("explicit jenkins-home emptyDir pod volume present = %v, want %v", hasEmptyDirVolume, tc.cell)
			}
		})
	}
}

// TestBuildStatefulSetCellUseItemPermission asserts design's apply-phase
// ruling (bb): a cell's jenkins container carries
// -Dcom.cloudbees.plugins.credentials.UseItemPermission=true on JAVA_OPTS, so
// a folder credential stays scoped to its own folder. A normal controller
// carries no such flag.
func TestBuildStatefulSetCellUseItemPermission(t *testing.T) {
	cases := []struct {
		name string
		cell bool
	}{
		{name: "normal controller", cell: false},
		{name: "cell", cell: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sts := buildStatefulSet(StatefulSetSpec{
				Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci",
				Cell: tc.cell,
			})
			javaOpts := builtContainerEnv(t, sts, "jenkins")["JAVA_OPTS"]
			hasFlag := strings.Contains(javaOpts, "-Dcom.cloudbees.plugins.credentials.UseItemPermission=true")
			if hasFlag != tc.cell {
				t.Errorf("JAVA_OPTS = %q, UseItemPermission present = %v, want %v", javaOpts, hasFlag, tc.cell)
			}
		})
	}
}

// TestDeleteCellNetworkPolicyToleratesNotFound guards the Finalize path: a
// cell that never had NetworkPolicy isolation enabled must not fail
// finalization.
func TestDeleteCellNetworkPolicyToleratesNotFound(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	if err := c.DeleteCellNetworkPolicy(context.Background(), "ci-00000000-cell", "team-a"); err != nil {
		t.Fatalf("DeleteCellNetworkPolicy on a missing policy must not error: %v", err)
	}
}

// createdPodLabels runs CreateStatefulSet against a fake dynamic client and
// answers the pod template labels of the created object.
func createdPodLabels(t *testing.T, spec StatefulSetSpec) map[string]string {
	t.Helper()
	scheme := runtime.NewScheme()
	stsGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}
	scheme.AddKnownTypeWithName(stsGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(stsGVK.GroupVersion().WithKind("StatefulSetList"), &unstructured.UnstructuredList{})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{stsGVR: "StatefulSetList"},
	)
	c := &ClientsetClient{dynamic: dyn}
	if err := c.CreateStatefulSet(context.Background(), spec); err != nil {
		t.Fatalf("CreateStatefulSet: %v", err)
	}
	got, err := dyn.Resource(stsGVR).Namespace(spec.Namespace).Get(context.Background(), spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}
	labels, _, err := unstructured.NestedStringMap(got.Object, "spec", "template", "metadata", "labels")
	if err != nil {
		t.Fatalf("read pod labels: %v", err)
	}
	selector, _, _ := unstructured.NestedStringMap(got.Object, "spec", "selector", "matchLabels")
	if _, ok := selector[cells.LabelCellPool]; ok {
		t.Errorf("selector = %v, want no pool label: the selector is immutable", selector)
	}
	return labels
}

func TestCreateStatefulSet_PoolCellPodLabel(t *testing.T) {
	base := func() StatefulSetSpec {
		return StatefulSetSpec{Name: "cell-big-0-00000000", Namespace: "varroa-system", ControllerName: "cell-big-0", Cell: true}
	}

	t.Run("a pool cell carries its pool label", func(t *testing.T) {
		spec := base()
		spec.CellPool = "big"
		if got := createdPodLabels(t, spec)[cells.LabelCellPool]; got != "big" {
			t.Errorf("pod label = %q, want big", got)
		}
	})

	t.Run("no pool means no label", func(t *testing.T) {
		if got, ok := createdPodLabels(t, base())[cells.LabelCellPool]; ok {
			t.Errorf("pod label = %q, want none", got)
		}
	})

	t.Run("the operator value wins over podLabels and the overlay", func(t *testing.T) {
		spec := base()
		spec.CellPool = "big"
		spec.PodOverrides = &v1alpha1.PodOverrides{PodLabels: map[string]string{cells.LabelCellPool: "other", "team": "a"}}
		spec.ResourceOverlay = &v1alpha1.ResourceOverlay{StatefulSet: "spec:\n  template:\n    metadata:\n      labels:\n        varroa.dev/cell-pool: overlay\n"}
		labels := createdPodLabels(t, spec)
		if labels[cells.LabelCellPool] != "big" {
			t.Errorf("pod label = %q, want big", labels[cells.LabelCellPool])
		}
		if labels["team"] != "a" {
			t.Errorf("podLabels entry team = %q, want a", labels["team"])
		}
	})
}

func TestBuildStatefulSetSpec_CellPool(t *testing.T) {
	rec := newTestReconciler(newTestClient())
	in := stsBuildInputs{Name: "ctl", Policy: v1alpha1.ReconciliationPolicy{DrainTimeoutSeconds: 30}}
	cr := func(cell bool, pool string) *v1alpha1.Controller {
		c := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "ctl", Namespace: "varroa-system", Labels: map[string]string{}},
			Spec:       v1alpha1.ControllerSpec{Version: "2.462.3"},
		}
		if cell {
			c.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
		}
		if pool != "" {
			c.Labels[cells.LabelCellPool] = pool
		}
		return c
	}
	cases := []struct {
		name string
		cr   *v1alpha1.Controller
		want string
	}{
		{name: "pool cell", cr: cr(true, "big"), want: "big"},
		{name: "cell with no pool label", cr: cr(true, ""), want: ""},
		{name: "not a cell", cr: cr(false, "big"), want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rec.buildStatefulSetSpec(tc.cr, nil, nil, nil, in).CellPool; got != tc.want {
				t.Errorf("CellPool = %q, want %q", got, tc.want)
			}
		})
	}
}
