package controller

import (
	"context"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
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
