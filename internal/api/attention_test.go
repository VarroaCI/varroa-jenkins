package api

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func strPtr(s string) *string { return &s }

func TestBuildAttentionJSON(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC))
	cond := func(typ v1alpha1.ControllerConditionType, reason, msg string) v1alpha1.ControllerCondition {
		return v1alpha1.ControllerCondition{Type: typ, Status: metav1.ConditionTrue, Reason: reason, Message: msg, LastTransitionTime: at}
	}
	cases := []struct {
		name string
		cr   v1alpha1.Controller
		want *AttentionJSON
	}{
		{"healthy", v1alpha1.Controller{Status: v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected}}, nil},
		{"failed phase wins", v1alpha1.Controller{Status: v1alpha1.ControllerStatus{
			Phase:      v1alpha1.ControllerPhaseFailed,
			Conditions: []v1alpha1.ControllerCondition{cond(v1alpha1.ConditionFailed, "ProvisioningTimeout", "provisioning exceeded 30m"), cond(v1alpha1.ConditionReconcileBlocked, "PluginConflict", "x")},
		}}, &AttentionJSON{Kind: "failed", Reason: "ProvisioningTimeout", Message: "provisioning exceeded 30m", Since: strPtr("2026-09-01T10:00:00Z")}},
		{"reconcile blocked", v1alpha1.Controller{Status: v1alpha1.ControllerStatus{
			Phase:                v1alpha1.ControllerPhaseProvisioning,
			LastReconcileErrorAt: &at,
			Conditions:           []v1alpha1.ControllerCondition{cond(v1alpha1.ConditionReconcileBlocked, "PluginConflict", "plugin kubernetes requested at A conflicts with profile lock B")},
		}}, &AttentionJSON{Kind: "reconcileBlocked", Reason: "PluginConflict", Message: "plugin kubernetes requested at A conflicts with profile lock B", Since: strPtr("2026-09-01T10:00:00Z")}},
		{"boot failed", v1alpha1.Controller{Status: v1alpha1.ControllerStatus{
			Phase:      v1alpha1.ControllerPhaseProvisioning,
			Conditions: []v1alpha1.ControllerCondition{cond(v1alpha1.ConditionJenkinsBootFailed, "JenkinsBootFailed", "jenkins container exited with code 5 (283 restarts): Error")},
		}}, &AttentionJSON{Kind: "bootFailed", Reason: "JenkinsBootFailed", Message: "jenkins container exited with code 5 (283 restarts): Error", Since: strPtr("2026-09-01T10:00:00Z")}},
		{"plugin roll failed", v1alpha1.Controller{Status: v1alpha1.ControllerStatus{
			Conditions: []v1alpha1.ControllerCondition{cond(v1alpha1.ConditionPluginRollFailed, "PluginRollFailed", "plugins-init failed")},
		}}, &AttentionJSON{Kind: "pluginRollFailed", Reason: "PluginRollFailed", Message: "plugins-init failed", Since: strPtr("2026-09-01T10:00:00Z")}},
		{"apply failed", v1alpha1.Controller{Status: v1alpha1.ControllerStatus{
			Phase:           v1alpha1.ControllerPhaseConnected,
			LastApplyResult: &v1alpha1.ApplyResult{Succeeded: false, Timestamp: at, Sections: []v1alpha1.ApplySectionResult{{Name: "plugins", OK: true}, {Name: "casc", OK: false, Error: "unknown key"}}},
		}}, &AttentionJSON{Kind: "applyFailed", Reason: "ApplyFailed", Message: "casc: unknown key", Since: strPtr("2026-09-01T10:00:00Z")}},
		{"false conditions are ignored", v1alpha1.Controller{Status: v1alpha1.ControllerStatus{
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionJenkinsBootFailed, Status: metav1.ConditionFalse, Reason: "BootPending"}},
		}}, nil},
		{"hibernated controllers report nothing", v1alpha1.Controller{Status: v1alpha1.ControllerStatus{
			Phase:           v1alpha1.ControllerPhaseHibernated,
			LastApplyResult: &v1alpha1.ApplyResult{Succeeded: false, Timestamp: at, Sections: []v1alpha1.ApplySectionResult{{Name: "casc", OK: false, Error: "stale"}}},
			Conditions:      []v1alpha1.ControllerCondition{cond(v1alpha1.ConditionJenkinsBootFailed, "JenkinsBootFailed", "stale")},
		}}, nil},
		{"stopped controllers report nothing", v1alpha1.Controller{Status: v1alpha1.ControllerStatus{
			Phase:      v1alpha1.ControllerPhaseStopped,
			Conditions: []v1alpha1.ControllerCondition{cond(v1alpha1.ConditionPluginRollFailed, "PluginRollFailed", "stale")},
		}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildAttentionJSON(&tc.cr)
			if !reflect.DeepEqual(tc.want, got) {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}
