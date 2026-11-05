package api

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestBuildProvisioningStep(t *testing.T) {
	if got := buildProvisioningStep(nil); got != nil {
		t.Fatalf("no conditions: got %+v, want nil", got)
	}
	falseCond := []v1alpha1.ControllerCondition{{
		Type: v1alpha1.ConditionProvisioningStep, Status: metav1.ConditionFalse,
		Reason: v1alpha1.ReasonImagePullInProgress, Message: "stale",
	}}
	if got := buildProvisioningStep(falseCond); got != nil {
		t.Fatalf("False condition: got %+v, want nil", got)
	}
	cases := []struct {
		name   string
		reason string
		want   string
	}{
		{"image pull", v1alpha1.ReasonImagePullInProgress, "imagePull"},
		{"plugin download", v1alpha1.ReasonPluginDownloadInProgress, "pluginDownload"},
		{"unknown reason", "SomethingElse", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conds := []v1alpha1.ControllerCondition{{
				Type:    v1alpha1.ConditionProvisioningStep,
				Status:  metav1.ConditionTrue,
				Reason:  tc.reason,
				Message: "container plugins-init downloads plugins",
			}}
			got := buildProvisioningStep(conds)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
				return
			}
			if got == nil || got.Step != tc.want || got.Message != conds[0].Message {
				t.Fatalf("got %+v, want step %q and message %q", got, tc.want, conds[0].Message)
			}
		})
	}
}
