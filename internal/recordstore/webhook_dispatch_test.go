package recordstore

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSignWebhookKnownVector pins the signing input to the documented form,
// "<timestamp>.<body>", with HMAC-SHA256. The expected value comes from an
// independent HMAC implementation.
func TestSignWebhookKnownVector(t *testing.T) {
	t.Parallel()

	got := SignWebhook([]byte("whsec-test"), 1700000000, []byte(`{"event":"run.completed"}`))
	const want = "v1=281bbef26ef6364903cf899473203eb11f3e5911970099fef5a8fca931764c8c"
	if got != want {
		t.Fatalf("SignWebhook = %q, want %q", got, want)
	}
}

// TestWebhookDialerRefusesLoopbackAndLinkLocal asserts the address classes
// the webhook dialer refuses, and that the refusal reaches a real dial.
func TestWebhookDialerRefusesLoopbackAndLinkLocal(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"refuses a loopback address":       "127.0.0.1",
		"refuses an IPv6 loopback address": "::1",
		"refuses a link-local address":     "169.254.169.254",
		"refuses an unspecified address":   "0.0.0.0",
		"refuses a group address":          "224.0.0.1",
	}
	for name, host := range cases {
		t.Run(name, func(t *testing.T) {
			if !refuseWebhookIP(net.ParseIP(host)) {
				t.Fatalf("refuseWebhookIP(%s) = false, want true", host)
			}
		})
	}
	if refuseWebhookIP(net.ParseIP("10.42.0.7")) {
		t.Fatalf("refuseWebhookIP(10.42.0.7) = true, want false: a cluster receiver must stay reachable")
	}

	// A real dial to a loopback listener must fail in the dialer's Control
	// hook, before any connection opens.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	_, err := newWebhookClient().Post(srv.URL, "application/json", strings.NewReader("{}"))
	if err == nil || !strings.Contains(err.Error(), "webhook dial") {
		t.Fatalf("POST to loopback: err = %v, want a refused webhook dial", err)
	}
}

// TestWebhookBackoffSchedule asserts the wait after each failed attempt,
// and that the wait stays at the last value past the table.
func TestWebhookBackoffSchedule(t *testing.T) {
	t.Parallel()

	want := []time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}
	for i, w := range want {
		if got := webhookBackoffAfter(i + 1); got != w {
			t.Fatalf("webhookBackoffAfter(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := webhookBackoffAfter(9); got != 30*time.Minute {
		t.Fatalf("webhookBackoffAfter(9) = %v, want 30m", got)
	}
}

// TestBuildWebhookPayloadOmitsEmptyFields asserts that the optional fields
// are absent from the JSON when they carry no value.
func TestBuildWebhookPayloadOmitsEmptyFields(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(WebhookPayload{
		Event:       webhookEventRunCompleted,
		DeliveryID:  "d1",
		Cluster:     "c",
		Tenant:      "t",
		Job:         "j",
		RunID:       "r",
		Result:      "SUCCESS",
		CompletedAt: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	for _, key := range []string{"number", "started_at", "scm_revision", "url"} {
		if _, ok := fields[key]; ok {
			t.Fatalf("payload carries %q with no value: %s", key, raw)
		}
	}
	for _, key := range []string{"event", "delivery_id", "duration_ms", "completed_at", "result"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("payload omits required %q: %s", key, raw)
		}
	}
}
