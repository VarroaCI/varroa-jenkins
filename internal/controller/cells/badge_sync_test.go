package cells

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// badgeTenantFor builds a pipelines tenant whose badge switch is set. Its
// working status carries token.
func badgeTenantFor(enabled bool, token string) (*v1alpha1.Tenant, *v1alpha1.TenantStatus) {
	tm := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TenantSpec{
			Pipelines: &v1alpha1.TenantPipelinesSpec{
				Profile: "p",
				Badge:   &v1alpha1.TenantBadgeSpec{Enabled: enabled},
			},
		},
	}
	status := &v1alpha1.TenantStatus{Pipelines: &v1alpha1.TenantPipelinesStatus{BadgeToken: token}}
	return tm, status
}

// TestBadgeTokenFor_OnlyWhileEnabled asserts the announce carries the token
// only while the badge switch is on, and an empty value otherwise.
func TestBadgeTokenFor_OnlyWhileEnabled(t *testing.T) {
	tm, status := badgeTenantFor(true, "tok-on")
	if got := badgeTokenFor(tm.Spec.Pipelines, status); got != "tok-on" {
		t.Errorf("switch on: token = %q, want tok-on", got)
	}
	tm, status = badgeTenantFor(false, "tok-off")
	if got := badgeTokenFor(tm.Spec.Pipelines, status); got != "" {
		t.Errorf("switch off: token = %q, want empty", got)
	}
	if got := badgeTokenFor(nil, status); got != "" {
		t.Errorf("no pipelines: token = %q, want empty", got)
	}
	if got := badgeTokenFor(tm.Spec.Pipelines, &v1alpha1.TenantStatus{}); got != "" {
		t.Errorf("no status token: token = %q, want empty", got)
	}
}

// TestSyncPolicyFor_CarriesBadgeTokenOnlyWhenEnabled asserts the announce
// policy of a tenant reads the token from the working status.
func TestSyncPolicyFor_CarriesBadgeTokenOnlyWhenEnabled(t *testing.T) {
	tm, status := badgeTenantFor(true, "tok-on")
	if got := syncPolicyFor(Inputs{}, tm, status).BadgeToken; got != "tok-on" {
		t.Errorf("switch on: BadgeToken = %q, want tok-on", got)
	}
	tm, status = badgeTenantFor(false, "tok-off")
	if got := syncPolicyFor(Inputs{}, tm, status).BadgeToken; got != "" {
		t.Errorf("switch off: BadgeToken = %q, want empty", got)
	}
}

// TestSyncPolicyEqual_BadgeTokenChangeReannounces asserts a rotation or a
// switch change makes two policies unequal, so the placement pass reannounces.
func TestSyncPolicyEqual_BadgeTokenChangeReannounces(t *testing.T) {
	base := SyncPolicy{Concurrency: "cancelInProgress", BadgeToken: "tok-a"}
	rotated := base
	rotated.BadgeToken = "tok-b"
	switched := base
	switched.BadgeToken = ""
	if base.Equal(rotated) {
		t.Error("a rotated token compares equal, want a reannounce")
	}
	if base.Equal(switched) {
		t.Error("a cleared token compares equal, want a reannounce")
	}
	same := base
	if !base.Equal(same) {
		t.Error("identical policies compare unequal")
	}
}
