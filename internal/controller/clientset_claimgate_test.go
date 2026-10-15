package controller

import (
	"testing"
)

// TestBuildStatefulSetClaimGateEnv asserts VARROA_CLAIM_GATE on the Jenkins
// container in both states, and that an empty spec value stamps "0" (task
// 6.2, design D12).
func TestBuildStatefulSetClaimGateEnv(t *testing.T) {
	cases := []struct {
		name      string
		claimGate string
		want      string
	}{
		{name: "enabled", claimGate: "1", want: "1"},
		{name: "disabled", claimGate: "0", want: "0"},
		{name: "unset defaults to disabled", claimGate: "", want: "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sts := buildStatefulSet(StatefulSetSpec{
				Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci",
				ClaimGate: tc.claimGate,
			})
			got := builtContainerEnv(t, sts, "jenkins")["VARROA_CLAIM_GATE"]
			if got != tc.want {
				t.Errorf("VARROA_CLAIM_GATE = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuildStatefulSetClaimTimeoutEnv asserts the timeout-split rule of
// design D6 / task 6.2: unset stamps neither variable, 5000 stamps 5000 on
// Jenkins and 4500 on the mite, and a value below the 5000ms floor (4000)
// stamps neither variable.
func TestBuildStatefulSetClaimTimeoutEnv(t *testing.T) {
	cases := []struct {
		name           string
		claimTimeoutMS int
		wantJenkins    string
		wantJenkinsSet bool
		wantMite       string
		wantMiteSet    bool
	}{
		{name: "unset stamps neither", claimTimeoutMS: 0, wantJenkinsSet: false, wantMiteSet: false},
		{name: "5000 splits to 5000/4500", claimTimeoutMS: 5000, wantJenkins: "5000", wantJenkinsSet: true, wantMite: "4500", wantMiteSet: true},
		{name: "below floor stamps neither", claimTimeoutMS: 4000, wantJenkinsSet: false, wantMiteSet: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sts := buildStatefulSet(StatefulSetSpec{
				Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci",
				ClaimTimeoutMS: tc.claimTimeoutMS,
			})
			jenkinsEnv := builtContainerEnv(t, sts, "jenkins")
			gotJenkins, jenkinsSet := jenkinsEnv["VARROA_CLAIM_TIMEOUT_MS"]
			if jenkinsSet != tc.wantJenkinsSet {
				t.Errorf("jenkins VARROA_CLAIM_TIMEOUT_MS set = %v, want %v (value %q)", jenkinsSet, tc.wantJenkinsSet, gotJenkins)
			}
			if tc.wantJenkinsSet && gotJenkins != tc.wantJenkins {
				t.Errorf("jenkins VARROA_CLAIM_TIMEOUT_MS = %q, want %q", gotJenkins, tc.wantJenkins)
			}

			miteEnv := builtContainerEnv(t, sts, "mite")
			gotMite, miteSet := miteEnv["MITE_CLAIM_TIMEOUT_MS"]
			if miteSet != tc.wantMiteSet {
				t.Errorf("mite MITE_CLAIM_TIMEOUT_MS set = %v, want %v (value %q)", miteSet, tc.wantMiteSet, gotMite)
			}
			if tc.wantMiteSet && gotMite != tc.wantMite {
				t.Errorf("mite MITE_CLAIM_TIMEOUT_MS = %q, want %q", gotMite, tc.wantMite)
			}
		})
	}
}

// TestBuildStatefulSetPodUIDFieldRef asserts VARROA_POD_UID on the mite
// container only, sourced from the downward API field metadata.uid (task
// 6.2a).
func TestBuildStatefulSetPodUIDFieldRef(t *testing.T) {
	sts := buildStatefulSet(StatefulSetSpec{Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci"})

	podSpec := sts.Object["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
	containers := podSpec["containers"].([]map[string]interface{})

	for _, cm := range containers {
		name := cm["name"]
		entries, _ := cm["env"].([]map[string]interface{})
		var fieldRefEntry map[string]interface{}
		for _, e := range entries {
			if e["name"] == "VARROA_POD_UID" {
				fieldRefEntry = e
				break
			}
		}
		if name == "mite" {
			if fieldRefEntry == nil {
				t.Fatalf("mite container missing VARROA_POD_UID env entry")
			}
			valueFrom, _ := fieldRefEntry["valueFrom"].(map[string]interface{})
			fieldRef, _ := valueFrom["fieldRef"].(map[string]interface{})
			if fieldRef["fieldPath"] != "metadata.uid" {
				t.Errorf("mite VARROA_POD_UID fieldRef.fieldPath = %v, want metadata.uid", fieldRef["fieldPath"])
			}
		} else if fieldRefEntry != nil {
			t.Errorf("container %v must not carry VARROA_POD_UID, only mite does", name)
		}
	}
}

// TestBuildStatefulSetLoopbackVolume asserts the shared varroa-loopback
// emptyDir volume and its mount at /var/run/varroa in both the Jenkins and
// mite containers (task 6.3).
func TestBuildStatefulSetLoopbackVolume(t *testing.T) {
	sts := buildStatefulSet(StatefulSetSpec{Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci"})

	vols := builtVolumes(t, sts)
	var loopbackVols int
	for _, v := range vols {
		if v["name"] == "varroa-loopback" {
			loopbackVols++
			if _, ok := v["emptyDir"]; !ok {
				t.Errorf("varroa-loopback volume must be emptyDir, got %v", v)
			}
		}
	}
	if loopbackVols != 1 {
		t.Fatalf("expected exactly one varroa-loopback volume, got %d (volumes: %v)", loopbackVols, vols)
	}

	for _, cn := range []string{"jenkins", "mite"} {
		mounts := builtContainerVolumeMounts(t, sts, cn)
		found := 0
		for _, m := range mounts {
			if m["name"] == "varroa-loopback" {
				found++
				if m["mountPath"] != "/var/run/varroa" {
					t.Errorf("container %q varroa-loopback mountPath = %v, want /var/run/varroa", cn, m["mountPath"])
				}
			}
		}
		if found != 1 {
			t.Errorf("container %q must mount varroa-loopback exactly once, found %d (mounts: %v)", cn, found, mounts)
		}
	}
}
