package controller

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

const (
	javaOptsNeverOff   = "-Dhudson.model.UsageStatistics.disabled=true -Dhudson.model.UpdateCenter.never=true -Dhudson.model.DownloadService.never=true"
	javaOptsOn         = "-Dhudson.model.UsageStatistics.disabled=true"
	javaOptsUpdateOnly = "-Dhudson.model.UsageStatistics.disabled=true -Dhudson.model.UpdateCenter.never=true"
	testUpstreamURL    = "https://updates.jenkins.io"
)

// TestReconcileJenkinsUpstreamRoll asserts that reconcileJenkinsUpstreamRoll
// compares the operator upstream mode with the live StatefulSet, and moves a
// Connected controller to Provisioning only on a difference.
func TestReconcileJenkinsUpstreamRoll(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name        string
		operatorURL string
		stamp       string
		javaOpts    string
		found       bool
		err         error
		wantRoll    bool
	}{
		{name: "stamp on, upstream off rolls", operatorURL: "", stamp: "on", javaOpts: javaOptsOn, found: true, wantRoll: true},
		{name: "stamp off, upstream off is a no-op", operatorURL: "", stamp: "off", javaOpts: javaOptsNeverOff, found: true},
		{name: "stamp off, upstream set rolls", operatorURL: testUpstreamURL, stamp: "off", javaOpts: javaOptsNeverOff, found: true, wantRoll: true},
		{name: "no stamp, live JAVA_OPTS without never token rolls", operatorURL: "", javaOpts: javaOptsOn, found: true, wantRoll: true},
		{name: "no stamp, only the update-center never flag rolls", operatorURL: "", javaOpts: javaOptsUpdateOnly, found: true, wantRoll: true},
		{name: "no stamp, live JAVA_OPTS with never token is a no-op", operatorURL: "", javaOpts: javaOptsNeverOff, found: true},
		{name: "stamp on wins over never token in JAVA_OPTS", operatorURL: testUpstreamURL, stamp: "on", javaOpts: javaOptsNeverOff, found: true},
		{name: "read error is a no-op", operatorURL: "", found: false, err: errors.New("boom")},
		{name: "StatefulSet absent is a no-op", operatorURL: "", found: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient()
			client.stsJenkinsUpstreamStamp = tc.stamp
			client.stsJenkinsUpstreamJavaOpts = tc.javaOpts
			client.stsJenkinsUpstreamFound = tc.found
			client.stsJenkinsUpstreamErr = tc.err
			rec := newTestReconciler(client)
			rec.SetJenkinsUpstreamURL(tc.operatorURL)
			cr := testController("t", "ns", v1alpha1.ControllerPhaseConnected)

			got := rec.reconcileJenkinsUpstreamRoll(ctx, cr)
			if got != tc.wantRoll {
				t.Fatalf("reconcileJenkinsUpstreamRoll() = %v, want %v", got, tc.wantRoll)
			}
			if !tc.wantRoll {
				if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
					t.Errorf("phase = %q, want Connected (unchanged)", cr.Status.Phase)
				}
				return
			}
			if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
				t.Errorf("phase = %q, want Provisioning", cr.Status.Phase)
			}
			if cr.Status.ProvisioningStartedAt == nil {
				t.Error("ProvisioningStartedAt must be set")
			}
			pc := findCondition(cr.Status.Conditions, v1alpha1.ConditionProvisioning)
			if pc == nil || pc.Status != metav1.ConditionTrue || pc.Reason != "ProvisioningStarted" {
				t.Errorf("Provisioning condition = %+v, want True/ProvisioningStarted", pc)
			}
		})
	}
}
