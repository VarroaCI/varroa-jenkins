package controller

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// secretReader is a ResourceClient that answers GetSecret from a map. Any
// other method panics, because the embedded interface is nil.
type secretReader struct {
	ResourceClient
	secrets map[string]map[string][]byte
}

func (c secretReader) GetSecret(_ context.Context, name, namespace string) (map[string][]byte, error) {
	data, ok := c.secrets[namespace+"/"+name]
	if !ok {
		return nil, context.Canceled
	}
	return data, nil
}

// webhookTenant returns a pipelines Tenant whose webhook names secretName and,
// when key is not empty, the data key.
func webhookTenant(secretName, key string) *v1alpha1.Tenant {
	return &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TenantSpec{Pipelines: &v1alpha1.TenantPipelinesSpec{
			Profile:        "lts",
			AgentNamespace: "payments-agents",
			Notifications: &v1alpha1.TenantNotificationsSpec{
				Webhook: &v1alpha1.TenantWebhookSpec{
					URL:       "https://hooks.example.test/varroa",
					SecretRef: v1alpha1.TenantWebhookSecretRef{Name: secretName, Key: key},
				},
			},
		}},
	}
}

func webhookReconciler(secrets map[string]map[string][]byte) *TenantReconciler {
	return &TenantReconciler{client: secretReader{secrets: secrets}, logger: slog.Default()}
}

// TestResolveTenantWebhook_NoNotifications asserts a Tenant with no webhook
// resolves to nothing and clears the Notifications condition.
func TestResolveTenantWebhook_NoNotifications(t *testing.T) {
	t.Parallel()

	tenant := &v1alpha1.Tenant{Spec: v1alpha1.TenantSpec{Pipelines: &v1alpha1.TenantPipelinesSpec{
		Profile: "lts", AgentNamespace: "payments-agents",
	}}}
	hook, cond := webhookReconciler(nil).resolveTenantWebhook(context.Background(), tenant)
	if hook != nil || cond != nil {
		t.Fatalf("resolveTenantWebhook = (%+v, %+v), want (nil, nil)", hook, cond)
	}
}

// TestResolveTenantWebhook_SecretMissingSetsCondition asserts a missing Secret,
// and a Secret with an empty key, both resolve to no webhook and a False
// condition with the WebhookSecretMissing reason.
func TestResolveTenantWebhook_SecretMissingSetsCondition(t *testing.T) {
	t.Parallel()

	cases := map[string]map[string]map[string][]byte{
		"absent secret": nil,
		"empty key": {
			"payments-agents/hook": {"hmac-key": {}},
		},
	}
	for name, secrets := range cases {
		hook, cond := webhookReconciler(secrets).resolveTenantWebhook(context.Background(), webhookTenant("hook", ""))
		if hook != nil {
			t.Fatalf("%s: webhook = %+v, want nil", name, hook)
		}
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.TenantReasonWebhookSecretMissing {
			t.Fatalf("%s: condition = %+v, want False/WebhookSecretMissing", name, cond)
		}
	}
}

// TestResolveTenantWebhook_DefaultKeyName asserts an empty secretRef key reads
// the hmac-key data key.
func TestResolveTenantWebhook_DefaultKeyName(t *testing.T) {
	t.Parallel()

	secrets := map[string]map[string][]byte{
		"payments-agents/hook": {"hmac-key": []byte("whsec-default")},
	}
	hook, cond := webhookReconciler(secrets).resolveTenantWebhook(context.Background(), webhookTenant("hook", ""))
	if hook == nil || hook.Key != "whsec-default" || hook.URL != "https://hooks.example.test/varroa" {
		t.Fatalf("webhook = %+v, want the hmac-key value and the declared URL", hook)
	}
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.TenantReasonWebhookReady {
		t.Fatalf("condition = %+v, want True/WebhookReady", cond)
	}
}

// TestResolveTenantWebhook_CustomKey asserts a named data key is read in place
// of the default.
func TestResolveTenantWebhook_CustomKey(t *testing.T) {
	t.Parallel()

	secrets := map[string]map[string][]byte{
		"payments-agents/hook": {"hmac-key": []byte("wrong"), "signing": []byte("whsec-custom")},
	}
	hook, _ := webhookReconciler(secrets).resolveTenantWebhook(context.Background(), webhookTenant("hook", "signing"))
	if hook == nil || hook.Key != "whsec-custom" {
		t.Fatalf("webhook = %+v, want the signing value", hook)
	}
}

// TestNATSSyncAnnouncer_SendsNotificationWebhook asserts the announce request
// carries the resolved webhook on its own field.
func TestNATSSyncAnnouncer_SendsNotificationWebhook(t *testing.T) {
	conn := startTestSyncBus(t)

	got := make(chan recordstore.TenantSyncRequest, 1)
	sub, err := conn.SubscribeRequest("varroa.recordstore.tenant.sync", "recorder", func(data []byte) []byte {
		var req recordstore.TenantSyncRequest
		_ = json.Unmarshal(data, &req)
		got <- req
		resp, _ := json.Marshal(recordstore.TenantSyncResponse{Ok: true})
		return resp
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	policy := cells.SyncPolicy{Webhook: &cells.WebhookSync{URL: "https://hooks.example.test/varroa", Key: "whsec-test"}}
	if _, err := NewNATSSyncAnnouncer(conn)(context.Background(), cells.AnnounceRequest{
		Cluster: "core", Tenant: "payments", Cell: "cell-a", Generation: 1, Policy: policy,
	}); err != nil {
		t.Fatalf("announce: %v", err)
	}
	req := <-got
	if req.NotificationWebhook == nil || req.NotificationWebhook.URL != "https://hooks.example.test/varroa" ||
		req.NotificationWebhook.Key != "whsec-test" {
		t.Fatalf("NotificationWebhook = %+v, want the resolved webhook", req.NotificationWebhook)
	}
}

// TestMarshalSyncPolicy_NeverEncodesWebhookKey asserts the policy JSON, the
// value the recorder stores in tenant_placements.policy, never carries the
// signing key or the webhook.
func TestMarshalSyncPolicy_NeverEncodesWebhookKey(t *testing.T) {
	t.Parallel()

	raw, err := marshalSyncPolicy(cells.SyncPolicy{
		Webhook: &cells.WebhookSync{URL: "https://hooks.example.test/varroa", Key: "whsec-secret-value"},
	})
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	if s := string(raw); strings.Contains(s, "whsec") || strings.Contains(s, "webhook") {
		t.Fatalf("policy JSON carries the webhook: %s", s)
	}
}
