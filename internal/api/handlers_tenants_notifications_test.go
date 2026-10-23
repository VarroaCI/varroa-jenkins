package api

import (
	"encoding/json"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// webhookEntry returns a wire pipelines block whose completion webhook names
// the given URL and Secret.
func webhookEntry(url, secret string) *tenantPipelinesEntry {
	return &tenantPipelinesEntry{
		Profile:        "lts",
		AgentNamespace: "payments-agents",
		Notifications: &tenantNotificationsEntry{Webhook: &tenantWebhookEntry{
			URL:       url,
			SecretRef: tenantWebhookSecretEntry{Name: secret, Key: "signing"},
		}},
	}
}

// TestTenantPipelinesEntry_RoundTripsNotifications asserts the webhook survives
// the spec-to-wire and wire-to-spec mapping in both directions, so an edit
// that does not touch it cannot drop it.
func TestTenantPipelinesEntry_RoundTripsNotifications(t *testing.T) {
	t.Parallel()

	in := webhookEntry("https://hooks.example.test/varroa", "hook-secret")
	spec := tenantPipelinesSpecFromEntry(in)
	if spec.Notifications == nil || spec.Notifications.Webhook == nil {
		t.Fatalf("spec lost the webhook: %+v", spec.Notifications)
	}
	out := tenantPipelinesEntryFromSpec(spec)
	if out.Notifications == nil || out.Notifications.Webhook == nil {
		t.Fatalf("wire entry lost the webhook")
	}
	got := out.Notifications.Webhook
	if got.URL != "https://hooks.example.test/varroa" || got.SecretRef.Name != "hook-secret" || got.SecretRef.Key != "signing" {
		t.Fatalf("webhook after round trip = %+v", got)
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	var back tenantPipelinesEntry
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}
	if back.Notifications == nil || back.Notifications.Webhook == nil || back.Notifications.Webhook.SecretRef.Key != "signing" {
		t.Fatalf("JSON round trip lost the webhook: %s", raw)
	}
}

// TestValidateTenantPipelines_WebhookNeedsURLAndSecret asserts a webhook names a
// URL and a Secret name, and that a complete webhook passes.
func TestValidateTenantPipelines_WebhookNeedsURLAndSecret(t *testing.T) {
	t.Parallel()

	namespaces := []string{"payments-agents"}
	if msg := validateTenantPipelines(webhookEntry("https://hooks.example.test", "hook"), namespaces); msg != "" {
		t.Fatalf("complete webhook refused: %s", msg)
	}
	if msg := validateTenantPipelines(webhookEntry("", "hook"), namespaces); msg == "" {
		t.Fatalf("webhook with no url accepted")
	}
	if msg := validateTenantPipelines(webhookEntry("https://hooks.example.test", ""), namespaces); msg == "" {
		t.Fatalf("webhook with no secret name accepted")
	}
}

// TestUpdateTenant_TenantAdminCannotChangeNotifications asserts the exports-only
// exception does not cover the webhook: a change to it breaks the equality the
// tenant-admin write path checks.
func TestUpdateTenant_TenantAdminCannotChangeNotifications(t *testing.T) {
	t.Parallel()

	base := v1alpha1.TenantSpec{Pipelines: &v1alpha1.TenantPipelinesSpec{
		Profile:        "lts",
		AgentNamespace: "payments-agents",
		Notifications: &v1alpha1.TenantNotificationsSpec{Webhook: &v1alpha1.TenantWebhookSpec{
			URL:       "https://hooks.example.test/a",
			SecretRef: v1alpha1.TenantWebhookSecretRef{Name: "hook"},
		}},
	}}
	changed := base
	changed.Pipelines = base.Pipelines.DeepCopy()
	changed.Pipelines.Notifications.Webhook.URL = "https://attacker.example.test"

	if tenantSpecEqualExceptTenantOwned(base, changed) {
		t.Fatalf("a webhook change counts as an exports-only change")
	}
	if !tenantSpecEqualExceptTenantOwned(base, base) {
		t.Fatalf("an unchanged spec differs from itself")
	}
}
