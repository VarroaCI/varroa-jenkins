package controller

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/overlay"
)

func TestComputePluginRollGate(t *testing.T) {
	const (
		desired = "sha-desired"
		other   = "sha-other"
	)
	auto, manual := v1alpha1.ReconciliationModeAutomatic, v1alpha1.ReconciliationModeManual

	tests := []struct {
		name     string
		applied  string
		approved string
		mode     v1alpha1.ReconciliationMode
		force    bool
		want     pluginRollDecision
	}{
		{
			name: "first provision (no applied) bumps",
			want: pluginRollDecision{Bump: true, STSChecksum: desired, RaisePending: false},
		},
		{
			name: "already converged bumps and resolves pending", applied: desired,
			want: pluginRollDecision{Bump: true, STSChecksum: desired, ResolvePending: true},
		},
		{
			name: "manual mode defers and raises pending", applied: other, mode: manual,
			want: pluginRollDecision{Bump: false, STSChecksum: other, RaisePending: true},
		},
		{
			name: "automatic mode always bumps", applied: other, mode: auto,
			want: pluginRollDecision{Bump: true, STSChecksum: desired},
		},
		{
			name:    "approval for the desired set bumps and is consumed",
			applied: other, approved: desired, mode: manual,
			want: pluginRollDecision{Bump: true, STSChecksum: desired, ClearApproved: true},
		},
		{
			name:    "stale approval clears and still defers",
			applied: other, approved: "sha-old", mode: manual,
			want: pluginRollDecision{Bump: false, STSChecksum: other, ClearApproved: true, RaisePending: true},
		},
		{
			name: "force reprovision bumps regardless of mode", applied: other, mode: manual, force: true,
			want: pluginRollDecision{Bump: true, STSChecksum: desired},
		},
		{
			name:    "force with stale approval clears it too",
			applied: other, approved: "sha-old", mode: manual, force: true,
			want: pluginRollDecision{Bump: true, STSChecksum: desired, ClearApproved: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computePluginRollGate(desired, tt.applied, tt.approved, tt.mode, tt.force)
			if got != tt.want {
				t.Errorf("computePluginRollGate() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestResolveUpdateCenterGate(t *testing.T) {
	readyUC := func(pullThrough bool) *v1alpha1.UpdateCenter {
		return &v1alpha1.UpdateCenter{
			Spec: v1alpha1.UpdateCenterSpec{PullThrough: v1alpha1.UpdateCenterPullThrough{Enabled: pullThrough}},
			Status: v1alpha1.UpdateCenterStatus{Conditions: []v1alpha1.UpdateCenterCondition{
				{Type: "Ready", Status: metav1.ConditionTrue},
			}},
		}
	}
	notReadyUC := func(pullThrough bool) *v1alpha1.UpdateCenter {
		return &v1alpha1.UpdateCenter{
			Spec: v1alpha1.UpdateCenterSpec{PullThrough: v1alpha1.UpdateCenterPullThrough{Enabled: pullThrough}},
		}
	}
	overrideBoth := &v1alpha1.ProvisioningDefaults{Spec: v1alpha1.ProvisioningDefaultsSpec{
		PluginUpdateCenterURL:         "https://example.org/uc.json",
		PluginUpdateCenterDownloadURL: "https://example.org/dl",
	}}
	overrideURLOnly := &v1alpha1.ProvisioningDefaults{Spec: v1alpha1.ProvisioningDefaultsSpec{
		PluginUpdateCenterURL: "https://example.org/uc.json",
	}}

	tests := []struct {
		name      string
		defaults  *v1alpha1.ProvisioningDefaults
		uc        *v1alpha1.UpdateCenter
		ucBaseURL string
		want      ucGateOutcome
	}{
		{name: "uc not configured clears", want: ucGateClear},
		{name: "uc ready clears", uc: readyUC(false), ucBaseURL: "http://uc:8080", want: ucGateClear},
		{name: "uc absent airgap blocks", ucBaseURL: "http://uc:8080", want: ucGateBlockAirgap},
		{name: "uc not ready airgap blocks", uc: notReadyUC(false), ucBaseURL: "http://uc:8080", want: ucGateBlockAirgap},
		{name: "uc not ready pull-through falls back online", uc: notReadyUC(true), ucBaseURL: "http://uc:8080", want: ucGateFallbackOnline},
		{name: "defaults override both tiers noop", defaults: overrideBoth, uc: notReadyUC(false), ucBaseURL: "http://uc:8080", want: ucGateNoop},
		{name: "partial override still gates on the missing tier", defaults: overrideURLOnly, uc: notReadyUC(false), ucBaseURL: "http://uc:8080", want: ucGateBlockAirgap},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveUpdateCenterGate(tt.defaults, tt.uc, tt.ucBaseURL)
			if got.Outcome != tt.want {
				t.Errorf("outcome = %d, want %d", got.Outcome, tt.want)
			}
		})
	}
}

func TestUcBlockAirgapMessage(t *testing.T) {
	tests := []struct {
		name       string
		uc         *v1alpha1.UpdateCenter
		wantSubstr []string
		notSubstr  []string
	}{
		{
			name:       "uc absent points at the missing CR",
			uc:         nil,
			wantSubstr: []string{"varroa-update-center", "was not found"},
			notSubstr:  []string{"seed.refs", "pullThrough"},
		},
		{
			name: "storage not ready points at the CR, not coverage remedies",
			uc: &v1alpha1.UpdateCenter{Status: v1alpha1.UpdateCenterStatus{
				Conditions: []v1alpha1.UpdateCenterCondition{
					{Type: condTypeReady, Status: metav1.ConditionFalse, Reason: reasonStorageUnavailable},
				},
			}},
			wantSubstr: []string{"varroa-update-center", "Inspect"},
			notSubstr:  []string{"seed.refs", "pullThrough"},
		},
		{
			name: "coverage gaps list the actual remedies",
			uc: &v1alpha1.UpdateCenter{
				Status: v1alpha1.UpdateCenterStatus{
					Conditions: []v1alpha1.UpdateCenterCondition{
						{Type: condTypeReady, Status: metav1.ConditionFalse, Reason: reasonGapAnalysisComplete},
					},
					Gaps: []v1alpha1.UpdateCenterGap{
						{Plugin: "git", Version: "5.0.0", RequiredBy: "profile/lts"},
						{Plugin: "workflow-job", Version: "1300.v0", RequiredBy: "profile/lts"},
					},
				},
			},
			wantSubstr: []string{"2 plugin coverage gap", "spec.seed.refs", "spec.pullThrough", "ProvisioningDefaults"},
			notSubstr:  []string{"capped"},
		},
		{
			name: "gap count at the cap discloses truncation",
			uc: &v1alpha1.UpdateCenter{
				Status: v1alpha1.UpdateCenterStatus{
					Conditions: []v1alpha1.UpdateCenterCondition{
						{Type: condTypeReady, Status: metav1.ConditionFalse, Reason: reasonGapAnalysisComplete},
					},
					Gaps: make([]v1alpha1.UpdateCenterGap, maxGaps),
				},
			},
			wantSubstr: []string{fmt.Sprintf("%d plugin coverage gap", maxGaps), "capped at 50"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ucBlockAirgapMessage(tt.uc)
			for _, want := range tt.wantSubstr {
				if !strings.Contains(got, want) {
					t.Errorf("message %q missing substring %q", got, want)
				}
			}
			for _, not := range tt.notSubstr {
				if strings.Contains(got, not) {
					t.Errorf("message %q unexpectedly contains %q", got, not)
				}
			}
		})
	}
}

func TestBuildStatefulSetSpec_Precedence(t *testing.T) {
	rec := newTestReconciler(newTestClient())
	in := stsBuildInputs{
		Name:             "ctl",
		BootstrapSecret:  "ctl-bootstrap",
		InitConfigMap:    "ctl-init",
		CascConfigMap:    "ctl-casc",
		PluginsConfigMap: "ctl-plugins",
		PluginsChecksum:  "sha",
		Policy:           v1alpha1.ReconciliationPolicy{DrainTimeoutSeconds: 30},
	}
	baseCR := func() *v1alpha1.Controller {
		return &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "ctl", Namespace: "ns"},
			Spec:       v1alpha1.ControllerSpec{Version: "2.462.3"},
		}
	}
	res := func(cpu string) *corev1.ResourceRequirements {
		return &corev1.ResourceRequirements{}
	}
	_ = res

	t.Run("spec resources win over class", func(t *testing.T) {
		cr := baseCR()
		cr.Spec.Resources = &corev1.ResourceRequirements{}
		class := &v1alpha1.ControllerClass{Spec: v1alpha1.ControllerClassSpec{Resources: &corev1.ResourceRequirements{}}}
		spec := rec.buildStatefulSetSpec(cr, class, nil, nil, in)
		if spec.ResourcesSource[overlay.JenkinsContainerName] != "spec" {
			t.Errorf("jenkins source = %q, want spec", spec.ResourcesSource[overlay.JenkinsContainerName])
		}
	})
	t.Run("class resources fall back when spec unset", func(t *testing.T) {
		cr := baseCR()
		class := &v1alpha1.ControllerClass{Spec: v1alpha1.ControllerClassSpec{Resources: &corev1.ResourceRequirements{}}}
		spec := rec.buildStatefulSetSpec(cr, class, nil, nil, in)
		if spec.ResourcesSource[overlay.JenkinsContainerName] != "class" {
			t.Errorf("jenkins source = %q, want class", spec.ResourcesSource[overlay.JenkinsContainerName])
		}
		if spec.Resources != class.Spec.Resources {
			t.Error("expected class resources to be used")
		}
	})
	t.Run("no resources anywhere is none", func(t *testing.T) {
		spec := rec.buildStatefulSetSpec(baseCR(), nil, nil, nil, in)
		if spec.ResourcesSource[overlay.JenkinsContainerName] != "none" || spec.Resources != nil {
			t.Errorf("source = %q resources = %v, want none/nil", spec.ResourcesSource[overlay.JenkinsContainerName], spec.Resources)
		}
	})
	t.Run("cr persistence suppresses class entirely", func(t *testing.T) {
		cr := baseCR()
		cr.Spec.Persistence = &v1alpha1.PersistenceSpec{Size: "50Gi"}
		class := &v1alpha1.ControllerClass{Spec: v1alpha1.ControllerClassSpec{
			Persistence: &v1alpha1.PersistenceSpec{Size: "99Gi", StorageClass: "class-sc"},
		}}
		spec := rec.buildStatefulSetSpec(cr, class, nil, nil, in)
		if spec.StorageSize != "50Gi" || spec.StorageClass != "" {
			t.Errorf("size/class = %q/%q, want 50Gi/empty (class not consulted)", spec.StorageSize, spec.StorageClass)
		}
	})
	t.Run("class persistence fills when cr unset, defaults fill storageClass last", func(t *testing.T) {
		cr := baseCR()
		class := &v1alpha1.ControllerClass{Spec: v1alpha1.ControllerClassSpec{
			Persistence: &v1alpha1.PersistenceSpec{Size: "99Gi"},
		}}
		defaults := &v1alpha1.ProvisioningDefaults{Spec: v1alpha1.ProvisioningDefaultsSpec{StorageClass: "default-sc"}}
		spec := rec.buildStatefulSetSpec(cr, class, defaults, nil, in)
		if spec.StorageSize != "99Gi" || spec.StorageClass != "default-sc" {
			t.Errorf("size/class = %q/%q, want 99Gi/default-sc", spec.StorageSize, spec.StorageClass)
		}
	})
	t.Run("pull secrets dedup class-first", func(t *testing.T) {
		class := &v1alpha1.ControllerClass{Spec: v1alpha1.ControllerClassSpec{ImagePullSecrets: []string{"a", "b"}}}
		defaults := &v1alpha1.ProvisioningDefaults{Spec: v1alpha1.ProvisioningDefaultsSpec{ImagePullSecrets: []string{"b", "c"}}}
		spec := rec.buildStatefulSetSpec(baseCR(), class, defaults, nil, in)
		want := []string{"a", "b", "c"}
		if len(spec.ImagePullSecrets) != 3 {
			t.Fatalf("pull secrets = %v, want %v", spec.ImagePullSecrets, want)
		}
		for i, s := range want {
			if spec.ImagePullSecrets[i] != s {
				t.Errorf("pull secrets = %v, want %v", spec.ImagePullSecrets, want)
			}
		}
	})
	t.Run("probes fall back to class", func(t *testing.T) {
		class := &v1alpha1.ControllerClass{Spec: v1alpha1.ControllerClassSpec{Probes: &v1alpha1.ProbesSpec{}}}
		spec := rec.buildStatefulSetSpec(baseCR(), class, nil, nil, in)
		if spec.Probes != class.Spec.Probes {
			t.Error("expected class probes fallback")
		}
	})
	t.Run("power state maps to replicas", func(t *testing.T) {
		for state, want := range map[string]int32{"": 1, "Running": 1, "Stopped": 0, "Hibernated": 1} {
			cr := baseCR()
			cr.Spec.PowerState = state
			spec := rec.buildStatefulSetSpec(cr, nil, nil, nil, in)
			if *spec.Replicas != want {
				t.Errorf("powerState %q → replicas %d, want %d", state, *spec.Replicas, want)
			}
		}
	})
	t.Run("status.hibernated maps to zero replicas", func(t *testing.T) {
		cr := baseCR()
		cr.Status.Hibernated = true
		spec := rec.buildStatefulSetSpec(cr, nil, nil, nil, in)
		if *spec.Replicas != 0 {
			t.Errorf("status.hibernated=true → replicas %d, want 0", *spec.Replicas)
		}
	})
	t.Run("path routing sets pathPrefix", func(t *testing.T) {
		cr := baseCR()
		cr.Spec.IngressSpec = &v1alpha1.IngressSpec{Mode: "path"}
		spec := rec.buildStatefulSetSpec(cr, nil, nil, nil, in)
		if spec.PathPrefix == "" {
			t.Error("expected non-empty pathPrefix in path mode")
		}
		if rec.buildStatefulSetSpec(baseCR(), nil, nil, nil, in).PathPrefix != "" {
			t.Error("expected empty pathPrefix outside path mode")
		}
	})
	t.Run("inputs pass through verbatim", func(t *testing.T) {
		spec := rec.buildStatefulSetSpec(baseCR(), nil, nil, nil, in)
		if spec.Name != "ctl" || spec.BootstrapSecret != "ctl-bootstrap" || spec.PluginsChecksum != "sha" ||
			spec.DrainTimeoutSec != 30 || spec.ServiceAccountName != "ctl-agent" {
			t.Errorf("inputs not passed through: %+v", spec)
		}
	})
}

func TestDeriveMiteHealth(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC))
	fresh := now.Add(-10 * time.Second)
	stale := now.Add(-staleMiteThreshold - time.Minute)

	base := func() miteObservation {
		return miteObservation{
			TransportConnected: true,
			Version:            "v1.2.3",
			LastHeartbeat:      fresh,
			CertExpiry:         now.Add(48 * time.Hour),
			Health:             "healthy",
		}
	}
	findCond := func(cs []v1alpha1.ControllerCondition, t v1alpha1.ControllerConditionType) *v1alpha1.ControllerCondition {
		for i := range cs {
			if cs[i].Type == t {
				return &cs[i]
			}
		}
		return nil
	}

	t.Run("disconnected transport short-circuits", func(t *testing.T) {
		obs := base()
		obs.TransportConnected = false
		got := deriveMiteHealth(obs, nil, now)
		if got.Connected || got.StaleOverride || len(got.Conditions) != 0 {
			t.Errorf("got %+v, want plain disconnect", got)
		}
	})
	t.Run("stale heartbeat overrides connected", func(t *testing.T) {
		obs := base()
		obs.LastHeartbeat = stale
		got := deriveMiteHealth(obs, nil, now)
		if got.Connected || !got.StaleOverride {
			t.Errorf("got %+v, want stale-forced disconnect", got)
		}
	})
	t.Run("connected without snapshot sets only Ready", func(t *testing.T) {
		got := deriveMiteHealth(base(), nil, now)
		if !got.Connected || got.LiveDrift != nil {
			t.Fatalf("got %+v", got)
		}
		if got.MiteStatus.Version != "v1.2.3" || !got.MiteStatus.Connected || got.MiteStatus.LastHealthCheck != nil {
			t.Errorf("mite status = %+v", got.MiteStatus)
		}
		if len(got.Conditions) != 1 || got.Conditions[0].Type != v1alpha1.ConditionReady ||
			got.Conditions[0].Status != metav1.ConditionTrue {
			t.Errorf("conditions = %+v, want single Ready=True", got.Conditions)
		}
	})
	t.Run("unhealthy and unreachable flip Ready", func(t *testing.T) {
		for _, h := range []string{"unhealthy", "unreachable"} {
			obs := base()
			obs.Health = h
			got := deriveMiteHealth(obs, nil, now)
			c := findCond(got.Conditions, v1alpha1.ConditionReady)
			if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "JenkinsUnhealthy" {
				t.Errorf("health %q → %+v, want Ready=False/JenkinsUnhealthy", h, c)
			}
		}
	})
	t.Run("snapshot populates status and ApplyDeferred", func(t *testing.T) {
		obs := base()
		obs.Snapshot = &mitev1.StateSnapshot{JenkinsVersion: "2.462.3", JenkinsHealth: "healthy", ApplyDeferred: true, DeferReason: "2 builds running"}
		got := deriveMiteHealth(obs, nil, now)
		if got.MiteStatus.JenkinsVersion != "2.462.3" || got.MiteStatus.LastHealthCheck == nil {
			t.Errorf("mite status = %+v", got.MiteStatus)
		}
		c := findCond(got.Conditions, v1alpha1.ConditionApplyDeferred)
		if c == nil || c.Status != metav1.ConditionTrue || c.Message != "2 builds running" {
			t.Errorf("ApplyDeferred = %+v", c)
		}
	})
	t.Run("live drift preserves prior DetectedAt", func(t *testing.T) {
		earlier := metav1.NewTime(now.Add(-time.Hour))
		prev := &v1alpha1.LiveDriftStatus{Detected: true, DetectedAt: &earlier}
		obs := base()
		obs.Snapshot = &mitev1.StateSnapshot{LiveDrift: true, LiveConfigHash: "h2"}
		got := deriveMiteHealth(obs, prev, now)
		if got.LiveDrift == nil || !got.LiveDrift.Detected || !got.LiveDrift.DetectedAt.Equal(&earlier) {
			t.Errorf("live drift = %+v, want DetectedAt preserved", got.LiveDrift)
		}
		if c := findCond(got.Conditions, v1alpha1.ConditionLiveDrift); c == nil || c.Status != metav1.ConditionTrue {
			t.Errorf("LiveDrift condition = %+v", c)
		}
	})
	t.Run("in-sync snapshot clears drift", func(t *testing.T) {
		obs := base()
		obs.Snapshot = &mitev1.StateSnapshot{LiveDrift: false, LiveConfigHash: "h1"}
		got := deriveMiteHealth(obs, nil, now)
		if got.LiveDrift == nil || got.LiveDrift.Detected {
			t.Errorf("live drift = %+v, want Detected=false", got.LiveDrift)
		}
		if c := findCond(got.Conditions, v1alpha1.ConditionLiveDrift); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "InSync" {
			t.Errorf("LiveDrift condition = %+v", c)
		}
	})
	t.Run("empty live hash leaves drift untouched", func(t *testing.T) {
		obs := base()
		obs.Snapshot = &mitev1.StateSnapshot{}
		if got := deriveMiteHealth(obs, nil, now); got.LiveDrift != nil {
			t.Errorf("live drift = %+v, want nil (unchanged)", got.LiveDrift)
		}
	})
}
