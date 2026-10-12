package cells

import "testing"

// TestSyncPolicyEqual_ComparesWebhook asserts a webhook change makes two
// policies unequal, so a URL or key change re-announces the tenant.
func TestSyncPolicyEqual_ComparesWebhook(t *testing.T) {
	t.Parallel()

	base := SyncPolicy{RehydrateBuilds: 10}
	withHook := base
	withHook.Webhook = &WebhookSync{URL: "https://a.example.test", Key: "k1"}

	if base.Equal(withHook) || withHook.Equal(base) {
		t.Fatalf("a policy with a webhook equals one without")
	}
	other := base
	other.Webhook = &WebhookSync{URL: "https://a.example.test", Key: "k1"}
	if !withHook.Equal(other) {
		t.Fatalf("two policies with the same webhook differ")
	}
	rekeyed := base
	rekeyed.Webhook = &WebhookSync{URL: "https://a.example.test", Key: "k2"}
	if withHook.Equal(rekeyed) {
		t.Fatalf("a key change does not change the policy")
	}
}
