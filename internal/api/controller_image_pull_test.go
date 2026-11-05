package api

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestBuildImagePull(t *testing.T) {
	if got := buildImagePull(nil); got != nil {
		t.Fatalf("no conditions: got %+v, want nil", got)
	}
	falseCond := []v1alpha1.ControllerCondition{{
		Type: v1alpha1.ConditionImagePulling, Status: metav1.ConditionFalse, Message: "stale",
	}}
	if got := buildImagePull(falseCond); got != nil {
		t.Fatalf("False condition: got %+v, want nil", got)
	}
	conds := []v1alpha1.ControllerCondition{{
		Type:    v1alpha1.ConditionImagePulling,
		Status:  metav1.ConditionTrue,
		Message: "pulling image jenkins/jenkins:2.528 for container plugins-init",
	}}
	got := buildImagePull(conds)
	if got == nil || got.Message != conds[0].Message {
		t.Fatalf("got %+v, want message %q", got, conds[0].Message)
	}
}
