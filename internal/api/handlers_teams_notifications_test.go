package api

import (
	"encoding/json"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// webhookEntry returns a wire pipelines block whose completion webhook names
// the given URL and Secret.
func webhookEntry(url, secret string) *teamPipelinesEntry {
	return &teamPipelinesEntry{
		Profile:        "lts",
		AgentNamespace: "payments-agents",
		Notifications: &teamNotificationsEntry{Webhook: &teamWebhookEntry{
			URL:       url,
			SecretRef: teamWebhookSecretEntry{Name: secret, Key: "signing"},
		}},
	}
}

// TestTeamPipelinesEntry_RoundTripsNotifications asserts the webhook survives
// the spec-to-wire and wire-to-spec mapping in both directions, so an edit
// that does not touch it cannot drop it.
func TestTeamPipelinesEntry_RoundTripsNotifications(t *testing.T) {
	t.Parallel()

	in := webhookEntry("https://hooks.example.test/varroa", "hook-secret")
	spec := teamPipelinesSpecFromEntry(in)
	if spec.Notifications == nil || spec.Notifications.Webhook == nil {
		t.Fatalf("spec lost the webhook: %+v", spec.Notifications)
	}
	out := teamPipelinesEntryFromSpec(spec)
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
	var back teamPipelinesEntry
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}
	if back.Notifications == nil || back.Notifications.Webhook == nil || back.Notifications.Webhook.SecretRef.Key != "signing" {
		t.Fatalf("JSON round trip lost the webhook: %s", raw)
	}
}

// TestValidateTeamPipelines_WebhookNeedsURLAndSecret asserts a webhook names a
// URL and a Secret name, and that a complete webhook passes.
func TestValidateTeamPipelines_WebhookNeedsURLAndSecret(t *testing.T) {
	t.Parallel()

	namespaces := []string{"payments-agents"}
	if msg := validateTeamPipelines(webhookEntry("https://hooks.example.test", "hook"), namespaces); msg != "" {
		t.Fatalf("complete webhook refused: %s", msg)
	}
	if msg := validateTeamPipelines(webhookEntry("", "hook"), namespaces); msg == "" {
		t.Fatalf("webhook with no url accepted")
	}
	if msg := validateTeamPipelines(webhookEntry("https://hooks.example.test", ""), namespaces); msg == "" {
		t.Fatalf("webhook with no secret name accepted")
	}
}

// TestUpdateTeam_TenantAdminCannotChangeNotifications asserts the exports-only
// exception does not cover the webhook: a change to it breaks the equality the
// tenant-admin write path checks.
func TestUpdateTeam_TenantAdminCannotChangeNotifications(t *testing.T) {
	t.Parallel()

	base := v1alpha1.TeamSpec{Pipelines: &v1alpha1.TeamPipelinesSpec{
		Profile:        "lts",
		AgentNamespace: "payments-agents",
		Notifications: &v1alpha1.TeamNotificationsSpec{Webhook: &v1alpha1.TeamWebhookSpec{
			URL:       "https://hooks.example.test/a",
			SecretRef: v1alpha1.TeamWebhookSecretRef{Name: "hook"},
		}},
	}}
	changed := base
	changed.Pipelines = base.Pipelines.DeepCopy()
	changed.Pipelines.Notifications.Webhook.URL = "https://attacker.example.test"

	if teamSpecEqualExceptTenantOwned(base, changed) {
		t.Fatalf("a webhook change counts as an exports-only change")
	}
	if !teamSpecEqualExceptTenantOwned(base, base) {
		t.Fatalf("an unchanged spec differs from itself")
	}
}
