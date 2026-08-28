package controller

import (
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/jenkinsver"
)

// versionBelow returns a version string one minor/patch below the given one,
// e.g. "2.570" -> "2.569", "2.479.3" -> "2.479.2".
func versionBelow(v string) string {
	cv, ok := jenkinsver.Core(v)
	if !ok || len(cv) == 0 {
		return v
	}
	cv[len(cv)-1]--
	out := fmt.Sprintf("%d", cv[0])
	for i := 1; i < len(cv); i++ {
		out = fmt.Sprintf("%s.%d", out, cv[i])
	}
	return out
}

func versionAbove(v string) string {
	cv, ok := jenkinsver.Core(v)
	if !ok || len(cv) == 0 {
		return v
	}
	cv[len(cv)-1]++
	out := fmt.Sprintf("%d", cv[0])
	for i := 1; i < len(cv); i++ {
		out = fmt.Sprintf("%s.%d", out, cv[i])
	}
	return out
}

func TestEvaluateCoreCompat(t *testing.T) {
	baseline := pluginlock.Baseline()
	if baseline == "" {
		t.Fatal("pluginlock.Baseline() returned empty")
	}
	below := versionBelow(baseline)
	above := versionAbove(baseline)
	if below == baseline || above == baseline {
		t.Fatalf("failed to compute below/above for baseline %q: below=%q above=%q", baseline, below, above)
	}

	// Ensure parse helpers work
	bv, ok := jenkinsver.Core(below)
	if !ok || len(bv) == 0 {
		t.Fatalf("below version %q is unparseable", below)
	}
	av, ok := jenkinsver.Core(above)
	if !ok || len(av) == 0 {
		t.Fatalf("above version %q is unparseable", above)
	}
	if jenkinsver.Compare(bv, av) != -1 {
		t.Fatalf("below %q should sort before above %q", below, above)
	}

	tests := []struct {
		name              string
		version           string
		profile           *v1alpha1.JenkinsVersionProfile
		kind              MatchKind
		pluginSetReady    bool
		wantVerdict       CompatVerdict
		wantReason        string
		wantUsingBaseline bool
		wantOK            bool // for AtLeast-style check (not used directly)
	}{
		{
			name:              "empty version uses baseline",
			version:           "",
			profile:           nil,
			kind:              MatchBaseline,
			pluginSetReady:    false,
			wantVerdict:       CompatOK,
			wantReason:        v1alpha1.ReasonCoreCompatible,
			wantUsingBaseline: true,
		},
		{
			name:              "lts uses baseline",
			version:           "lts",
			profile:           nil,
			kind:              MatchBaseline,
			pluginSetReady:    false,
			wantVerdict:       CompatOK,
			wantReason:        v1alpha1.ReasonCoreCompatible,
			wantUsingBaseline: true,
		},
		{
			name:    "matched ready profile is compatible",
			version: "2.560",
			profile: &v1alpha1.JenkinsVersionProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "test-profile"},
				Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.560"},
			},
			kind:              MatchExact,
			pluginSetReady:    true,
			wantVerdict:       CompatOK,
			wantReason:        v1alpha1.ReasonCoreCompatible,
			wantUsingBaseline: false,
		},
		{
			name:    "matched not-ready core below baseline",
			version: below,
			profile: &v1alpha1.JenkinsVersionProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "below-profile"},
				Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: below},
			},
			kind:              MatchExact,
			pluginSetReady:    false,
			wantVerdict:       CompatUnsafe,
			wantReason:        v1alpha1.ReasonCoreOlderThanPluginBaseline,
			wantUsingBaseline: true,
		},
		{
			name:    "matched not-ready core above baseline",
			version: above,
			profile: &v1alpha1.JenkinsVersionProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "above-profile"},
				Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: above},
			},
			kind:              MatchExact,
			pluginSetReady:    false,
			wantVerdict:       CompatOK,
			wantReason:        v1alpha1.ReasonCoreCompatible,
			wantUsingBaseline: true,
		},
		{
			name:              "no profile core below baseline",
			version:           below,
			profile:           nil,
			kind:              MatchBaseline,
			pluginSetReady:    false,
			wantVerdict:       CompatUnsafe,
			wantReason:        v1alpha1.ReasonCoreOlderThanPluginBaseline,
			wantUsingBaseline: true,
		},
		{
			name:              "no profile core above baseline",
			version:           above,
			profile:           nil,
			kind:              MatchBaseline,
			pluginSetReady:    false,
			wantVerdict:       CompatOK,
			wantReason:        v1alpha1.ReasonCoreCompatible,
			wantUsingBaseline: true,
		},
		{
			name:              "unparseable version",
			version:           "fancy",
			profile:           nil,
			kind:              MatchBaseline,
			pluginSetReady:    false,
			wantVerdict:       CompatUnknown,
			wantReason:        v1alpha1.ReasonUnparseableVersion,
			wantUsingBaseline: true,
		},
		{
			name:              "suffixed tag no profile above baseline",
			version:           "2.579.1-jdk17",
			profile:           nil,
			kind:              MatchBaseline,
			pluginSetReady:    false,
			wantVerdict:       CompatOK,
			wantReason:        v1alpha1.ReasonCoreCompatible,
			wantUsingBaseline: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := EvaluateCoreCompat(tc.version, tc.profile, tc.kind, tc.pluginSetReady, baseline)
			if res.Verdict != tc.wantVerdict {
				t.Errorf("Verdict = %v, want %v", res.Verdict, tc.wantVerdict)
			}
			if res.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", res.Reason, tc.wantReason)
			}
			if res.UsingBaseline != tc.wantUsingBaseline {
				t.Errorf("UsingBaseline = %v, want %v", res.UsingBaseline, tc.wantUsingBaseline)
			}
			if tc.wantUsingBaseline && res.EffectiveSource != baseline {
				t.Errorf("EffectiveSource = %q, want baseline %q", res.EffectiveSource, baseline)
			}
			if tc.wantVerdict == CompatOK && tc.wantReason != "" && res.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", res.Reason, tc.wantReason)
			}
		})
	}
}

func TestProfilePluginSetReady(t *testing.T) {
	if ProfilePluginSetReady(nil) {
		t.Error("ProfilePluginSetReady(nil) should be false")
	}
}
