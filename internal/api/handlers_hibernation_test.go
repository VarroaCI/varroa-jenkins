package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func TestHibernationRedirectUsesSharedInterstitial(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"jenkins": {
			ObjectMeta: metav1.ObjectMeta{Name: "jenkins", Namespace: "team-a"},
			Status: v1alpha1.ControllerStatus{
				WakeToken: "wake-token",
				Endpoint:  "https://jenkins.example.com/",
				Phase:     v1alpha1.ControllerPhaseHibernated,
			},
		},
	}
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Logger: slog.Default()})
	req := httptest.NewRequest(http.MethodGet, "/hibernation/wake-token/clusters/core/ns/team-a/redirect/jenkins/job/example/", nil)
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`var statusPath = "/hibernation/wake-token/clusters/core/ns/team-a/status/jenkins";`,
		`var targetURL = "https://jenkins.example.com/job/example/";`,
		"var redirectOnNonWake = false;",
		"d.varroaWake === true",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("response body missing %q", want)
		}
	}
}

// TestHibernationQueueRejectsCell asserts task 5.7: the controller-shape
// queue endpoint answers 401 for a cell. A cell never mints a wake token
// (mintWakeToken's cell exclusion), so status.wakeToken stays empty even
// though spec.hibernation.enabled is legally set and ignored — and
// checkWakeToken already refuses any empty stored token.
func TestHibernationQueueRejectsCell(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"cell1": {
			ObjectMeta: metav1.ObjectMeta{Name: "cell1", Namespace: "ns1"},
			Spec: v1alpha1.ControllerSpec{
				Cell:        &v1alpha1.CellSpec{Enabled: true},
				Hibernation: &v1alpha1.HibernationSpec{Enabled: true},
			},
			Status: v1alpha1.ControllerStatus{
				WakeToken: "",
				Phase:     v1alpha1.ControllerPhaseConnected,
			},
		},
	}
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Logger: slog.Default()})
	req := httptest.NewRequest(http.MethodPost, "/hibernation/anything/clusters/core/ns/ns1/queue/cell1/github-webhook/", nil)
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestHibernationStatusIncludesWakeMarker(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"jenkins": {
			ObjectMeta: metav1.ObjectMeta{Name: "jenkins", Namespace: "team-a"},
			Status: v1alpha1.ControllerStatus{
				WakeToken: "wake-token",
				Phase:     v1alpha1.ControllerPhaseConnected,
			},
		},
	}
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Logger: slog.Default()})
	req := httptest.NewRequest(http.MethodGet, "/hibernation/wake-token/clusters/core/ns/team-a/status/jenkins", nil)
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != `{"varroaWake":true,"phase":"Connected"}` {
		t.Fatalf("status response = %d %q", rec.Code, rec.Body.String())
	}
}

// --- 10.1/10.3: a headless controller on the redirect and status surface ---

// headlessHibernatedController builds a hibernated, headless Controller CRD
// carrying a wake token, for the D5 redirect tests.
func headlessHibernatedController(name, ns, token string) *v1alpha1.Controller {
	return &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       v1alpha1.ControllerSpec{Headless: true},
		Status: v1alpha1.ControllerStatus{
			WakeToken: token,
			Phase:     v1alpha1.ControllerPhaseHibernated,
		},
	}
}

// TestHibernationRedirectHeadlessValidTokenAnswers404NoWake asserts task
// 10.1: a headless controller's redirect endpoint answers 404 after the
// token guard passes, and publishes no wake command.
func TestHibernationRedirectHeadlessValidTokenAnswers404NoWake(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"jenkins": headlessHibernatedController("jenkins", "team-a", "wake-token"),
	}
	conn := tenantQueueTestBus(t)
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), BusConn: conn, Logger: slog.Default()})

	received := make(chan struct{}, 1)
	sub, err := conn.Subscribe(bus.WakeSubject("core", "team-a", "jenkins"), func(*nats.Msg) {
		received <- struct{}{}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	req := httptest.NewRequest(http.MethodGet, "/hibernation/wake-token/clusters/core/ns/team-a/redirect/jenkins/job/example/", nil)
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	select {
	case <-received:
		t.Fatal("wake command published for a headless controller's redirect")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestHibernationRedirectHeadlessInvalidTokenAnswers401 asserts task 10.1's
// other half: an invalid token still answers 401, matching any other
// controller, and publishes no wake command either.
func TestHibernationRedirectHeadlessInvalidTokenAnswers401(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"jenkins": headlessHibernatedController("jenkins", "team-a", "wake-token"),
	}
	conn := tenantQueueTestBus(t)
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), BusConn: conn, Logger: slog.Default()})

	received := make(chan struct{}, 1)
	sub, err := conn.Subscribe(bus.WakeSubject("core", "team-a", "jenkins"), func(*nats.Msg) {
		received <- struct{}{}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	req := httptest.NewRequest(http.MethodGet, "/hibernation/wrong-token/clusters/core/ns/team-a/redirect/jenkins/job/example/", nil)
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	select {
	case <-received:
		t.Fatal("wake command published for a headless controller's redirect with an invalid token")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestHibernationQueueHeadlessStillAccepts202 asserts task 10.2: the queue
// endpoint stays unchanged for a headless, hibernated controller — a
// webhook still returns 202 and is persisted for replay.
func TestHibernationQueueHeadlessStillAccepts202(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"jenkins": headlessHibernatedController("jenkins", "team-a", "wake-token"),
	}
	conn := tenantQueueTestBus(t)
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), BusConn: conn, Logger: slog.Default()})

	received := make(chan []byte, 1)
	sub, err := conn.SubscribeData(bus.WebhookSubject("core", "team-a", "jenkins"), func(data []byte) {
		received <- data
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	body := []byte(`{"ref":"refs/heads/main"}`)
	req := httptest.NewRequest(http.MethodPost, "/hibernation/wake-token/clusters/core/ns/team-a/queue/jenkins/github-webhook/", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	select {
	case data := <-received:
		var env map[string]any
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("unmarshal published envelope: %v", err)
		}
		if env["controller"] != "jenkins" || env["namespace"] != "team-a" {
			t.Fatalf("envelope identity = %+v, want controller=jenkins namespace=team-a", env)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no envelope persisted within timeout")
	}
}

// --- 11.1: the replaced guard routes both shapes ---

// TestHibernationWebhookDispatch_ControllerShapeStillRoutesEveryURL is task 11.1's
// table test: every previously valid controller-shape URL still routes to
// the same action as before the switch replaced the single guard.
func TestHibernationWebhookDispatch_ControllerShapeStillRoutesEveryURL(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"jenkins": {
			ObjectMeta: metav1.ObjectMeta{Name: "jenkins", Namespace: "team-a"},
			Status: v1alpha1.ControllerStatus{
				WakeToken: "wake-token",
				Endpoint:  "https://jenkins.example.com/",
				Phase:     v1alpha1.ControllerPhaseConnected,
			},
		},
	}
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Logger: slog.Default()})

	cases := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{"status", http.MethodGet, "/hibernation/wake-token/clusters/core/ns/team-a/status/jenkins", http.StatusOK},
		{"redirect", http.MethodGet, "/hibernation/wake-token/clusters/core/ns/team-a/redirect/jenkins/job/x/", http.StatusOK},
		{"queue non-allowlisted path 404s past auth", http.MethodPost, "/hibernation/wake-token/clusters/core/ns/team-a/queue/jenkins/scriptText", http.StatusNotFound},
		{"unknown action 404s", http.MethodGet, "/hibernation/wake-token/clusters/core/ns/team-a/bogus/jenkins", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			srv.HandleHibernationDispatch(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

// TestHibernationWebhookDispatch_FiveSegmentClustersPathRefused asserts task
// 11.1's other half: a clusters-shape URL with only five segments (no
// action) is refused, not routed with an arbitrary segment read as the
// action.
func TestHibernationWebhookDispatch_FiveSegmentClustersPathRefused(t *testing.T) {
	srv := NewServer(&Dependencies{Logger: slog.Default()})
	req := httptest.NewRequest(http.MethodGet, "/hibernation/token/clusters/core/ns/team-a", nil)
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a five-segment clusters path", rec.Code)
	}
}

// --- 11.2: the tenant queue handler ---

// tenantQueueTestBus starts an embedded JetStream-enabled NATS server and
// returns a connected *bus.Conn with the varroa_webhooks stream ensured.
func tenantQueueTestBus(t *testing.T) *bus.Conn {
	t.Helper()
	opts := &server.Options{Port: -1, StoreDir: t.TempDir(), JetStream: true}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(s.Shutdown)

	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(conn.Close)

	if err := conn.EnsureStream(bus.WebhookStreamConfig(bus.WebhookStreamName, 1)); err != nil {
		t.Fatalf("ensure webhook stream: %v", err)
	}
	return conn
}

// placedPipelinesTenant builds a Tenant CRD carrying a pipelines door token
// and secret, for the tenant queue tests.
func placedPipelinesTenant(name, token, secret string) *v1alpha1.Tenant {
	return &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.TenantSpec{Namespaces: []string{"ns1"}},
		Status: v1alpha1.TenantStatus{
			Pipelines: &v1alpha1.TenantPipelinesStatus{
				WebhookToken:  token,
				WebhookSecret: secret,
			},
		},
	}
}

func githubSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestTenantWebhookQueue_UnknownTenantAnswers401(t *testing.T) {
	store := crdstore.NewFake()
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Store: store, Logger: slog.Default()})

	req := httptest.NewRequest(http.MethodPost, "/hibernation/tok/tenants/nosuchteam/queue/github-webhook/", bytes.NewReader([]byte("{}")))
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an unknown tenant", rec.Code)
	}
}

func TestTenantWebhookQueue_CrossTenantTokenAnswers401(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, placedPipelinesTenant("payments", "payments-token", "payments-secret"))
	crdstore.MustSeed(store, placedPipelinesTenant("search", "search-token", "search-secret"))
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Store: store, Logger: slog.Default()})

	// The request names the "payments" Tenant but carries "search"'s token.
	req := httptest.NewRequest(http.MethodPost, "/hibernation/search-token/tenants/payments/queue/github-webhook/", bytes.NewReader([]byte("{}")))
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a cross-tenant token", rec.Code)
	}
}

func TestTenantWebhookQueue_NonAllowlistedPathAnswers404(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, placedPipelinesTenant("payments", "tok", "secret"))
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Store: store, Logger: slog.Default()})

	req := httptest.NewRequest(http.MethodPost, "/hibernation/tok/tenants/payments/queue/scriptText", bytes.NewReader([]byte("{}")))
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a non-allowlisted path", rec.Code)
	}
}

func TestTenantWebhookQueue_RedirectNotServed404(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, placedPipelinesTenant("payments", "tok", "secret"))
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Store: store, Logger: slog.Default()})

	req := httptest.NewRequest(http.MethodGet, "/hibernation/tok/tenants/payments/redirect/", nil)
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: redirect stays controller-only", rec.Code)
	}
}

func TestTenantWebhookQueue_BodyTooLargeAnswers413(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, placedPipelinesTenant("payments", "tok", "secret"))
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Store: store, Logger: slog.Default()})

	big := bytes.Repeat([]byte("a"), maxWebhookBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/hibernation/tok/tenants/payments/queue/github-webhook/", bytes.NewReader(big))
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestTenantWebhookQueue_MissingGitHubSignatureAnswers400(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, placedPipelinesTenant("payments", "tok", "secret"))
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Store: store, Logger: slog.Default()})

	req := httptest.NewRequest(http.MethodPost, "/hibernation/tok/tenants/payments/queue/github-webhook/", bytes.NewReader([]byte("{}")))
	req.Header.Set("X-GitHub-Event", "push")
	// No X-Hub-Signature-256.
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unsigned GitHub delivery", rec.Code)
	}
}

func TestTenantWebhookQueue_WrongSecretSignatureAnswers400(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, placedPipelinesTenant("payments", "tok", "secret"))
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Store: store, Logger: slog.Default()})

	body := []byte(`{"ref":"refs/heads/main"}`)
	req := httptest.NewRequest(http.MethodPost, "/hibernation/tok/tenants/payments/queue/github-webhook/", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", githubSignature("some-other-secret", body))
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a mismatched signature", rec.Code)
	}
}

func TestTenantWebhookQueue_EmptyStoredSecretAnswers503(t *testing.T) {
	store := crdstore.NewFake()
	// A token exists (so the request clears step 2), but the secret is
	// still empty: the placement pass has not finished minting it.
	crdstore.MustSeed(store, placedPipelinesTenant("payments", "tok", ""))
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Store: store, Logger: slog.Default()})

	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/hibernation/tok/tenants/payments/queue/github-webhook/", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", githubSignature("whatever", body))
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for an empty stored secret", rec.Code)
	}
}

// TestTenantWebhookQueue_GitLabTokenPublishesTenantEnvelope covers the GitLab
// token path end to end: a valid delivery is accepted, published on
// webhook.<cluster>.tenant.<tenant>, and the envelope carries the request's
// forge/team/delivery identity but neither secret header.
// tenantDoorFixture returns a tenant door server for tenant payments, with
// the door token "tok" and the secret "secret", and the subject a
// delivery publishes on.
func tenantDoorFixture(t *testing.T) (*Server, *bus.Conn, string) {
	t.Helper()
	store := crdstore.NewFake()
	crdstore.MustSeed(store, placedPipelinesTenant("payments", "tok", "secret"))
	client := newFakeResourceClient()
	conn := tenantQueueTestBus(t)
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Store: store, BusConn: conn, Logger: slog.Default()})
	return srv, conn, bus.WebhookTenantSubject("core", "payments")
}

// postTenantDoor sends one delivery to the tenant door and returns the
// status code.
func postTenantDoor(t *testing.T, srv *Server, path string, headers map[string]string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/hibernation/tok/tenants/payments/queue/"+path, bytes.NewReader([]byte(`{}`)))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.HandleHibernationDispatch(rec, req)
	return rec.Code
}

func TestTenantWebhookQueue_GenericWithoutTokenAnswers400(t *testing.T) {
	srv, _, _ := tenantDoorFixture(t)
	code := postTenantDoor(t, srv, "generic-webhook-trigger/invoke", map[string]string{"X-Varroa-Token": "secret"})
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

func TestTenantWebhookQueue_GenericWithOtherTenantTokenAnswers400(t *testing.T) {
	srv, _, _ := tenantDoorFixture(t)
	code := postTenantDoor(t, srv, "generic-webhook-trigger/invoke?token=billing:x", map[string]string{"X-Varroa-Token": "secret"})
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

func TestTenantWebhookQueue_GenericPrefixOnlyTokenAnswers400(t *testing.T) {
	srv, _, _ := tenantDoorFixture(t)
	code := postTenantDoor(t, srv, "generic-webhook-trigger/invoke?token=payments:", map[string]string{"X-Varroa-Token": "secret"})
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

func TestTenantWebhookQueue_GitLabOnGitHubPathAnswers400(t *testing.T) {
	srv, _, _ := tenantDoorFixture(t)
	code := postTenantDoor(t, srv, "github-webhook/", map[string]string{
		"X-Gitlab-Event": "Push Hook",
		"X-Gitlab-Token": "secret",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

func TestTenantWebhookQueue_GenericTokenKeptInEnvelopeQuery(t *testing.T) {
	srv, conn, subject := tenantDoorFixture(t)
	received := make(chan []byte, 1)
	sub, err := conn.SubscribeData(subject, func(data []byte) {
		received <- data
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	code := postTenantDoor(t, srv, "generic-webhook-trigger/invoke?token=payments:gl", map[string]string{"X-Varroa-Token": "secret"})
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	select {
	case data := <-received:
		var env map[string]any
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		query, _ := env["query"].(string)
		if !strings.Contains(query, "token=payments:gl") {
			t.Fatalf("envelope query = %q, want it to hold token=payments:gl", query)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no envelope published within timeout")
	}
}

func TestTenantPathServes(t *testing.T) {
	q := func(raw string) url.Values {
		v, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatalf("parse query %q: %v", raw, err)
		}
		return v
	}
	tests := []struct {
		name  string
		path  string
		forge string
		query string
		want  bool
	}{
		{"github path on github delivery", "github-webhook/", "github", "", true},
		{"github path on gitlab delivery", "github-webhook/", "gitlab", "", false},
		{"github path on generic delivery", "github-webhook/", "generic", "", false},
		{"gitea path on gitea delivery", "gitea-webhook/", "gitea", "", true},
		{"gitea path on github delivery", "gitea-webhook/", "github", "", false},
		{"gitea path on generic delivery", "gitea-webhook/", "generic", "", false},
		{"generic path with own tenant token", "generic-webhook-trigger/invoke", "generic", "token=payments:gl", true},
		{"generic path with own tenant token on gitlab", "generic-webhook-trigger/invoke", "gitlab", "token=payments:x", true},
		{"generic path without token", "generic-webhook-trigger/invoke", "generic", "", false},
		{"generic path with other tenant token", "generic-webhook-trigger/invoke", "generic", "token=billing:x", false},
		{"generic path with tenant name as prefix of another tenant", "generic-webhook-trigger/invoke", "generic", "token=payments-2:x", false},
		{"generic path with prefix only", "generic-webhook-trigger/invoke", "generic", "token=payments:", false},
		{"generic path with bare tenant name", "generic-webhook-trigger/invoke", "generic", "token=payments", false},
		{"generic path with empty token", "generic-webhook-trigger/invoke", "generic", "token=", false},
		{"generic path on the header token only", "generic-webhook-trigger/invoke", "generic", "x=payments:gl", false},
		{"generic path with a trailing slash and own token", "generic-webhook-trigger/invoke/", "generic", "token=payments:gl", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tenantPathServes(tt.path, tt.forge, "payments", q(tt.query)); got != tt.want {
				t.Fatalf("tenantPathServes(%q, %q, payments, %q) = %v, want %v", tt.path, tt.forge, tt.query, got, tt.want)
			}
		})
	}
}

func TestTenantWebhookQueue_GitLabTokenPublishesTenantEnvelope(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, placedPipelinesTenant("payments", "tok", "secret"))
	client := newFakeResourceClient()
	conn := tenantQueueTestBus(t)
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Store: store, BusConn: conn, Logger: slog.Default()})

	received := make(chan []byte, 1)
	sub, err := conn.SubscribeData(bus.WebhookTenantSubject("core", "payments"), func(data []byte) {
		received <- data
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	body := []byte(`{"object_kind":"push"}`)
	req := httptest.NewRequest(http.MethodPost, "/hibernation/tok/tenants/payments/queue/generic-webhook-trigger/invoke?token=payments:gl", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Event", "Push Hook")
	req.Header.Set("X-Gitlab-Token", "secret")
	req.Header.Set("X-Gitlab-Event-UUID", "gl-uuid-1")
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}

	select {
	case data := <-received:
		var env map[string]any
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("unmarshal published envelope: %v", err)
		}
		if env["tenant"] != "payments" || env["forge"] != "gitlab" || env["deliveryId"] != "gl-uuid-1" {
			t.Fatalf("envelope identity = %+v, want tenant=payments forge=gitlab deliveryId=gl-uuid-1", env)
		}
		if _, hasNS := env["namespace"]; hasNS {
			t.Errorf("tenant envelope carries a namespace: %+v", env)
		}
		if _, hasCtrl := env["controller"]; hasCtrl {
			t.Errorf("tenant envelope carries a controller: %+v", env)
		}
		headers, _ := env["headers"].(map[string]any)
		if _, ok := headers["X-Gitlab-Token"]; ok {
			t.Errorf("tenant envelope carries X-Gitlab-Token: %+v", headers)
		}
		if _, ok := headers["X-Varroa-Token"]; ok {
			t.Errorf("tenant envelope carries X-Varroa-Token: %+v", headers)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no envelope published within timeout")
	}
}

func TestTenantWebhookQueue_UnverifiedHeaderIsReadFromRequestNotAllowlist(t *testing.T) {
	// X-Varroa-Token is never in the header allowlist (verified against
	// HeaderAllowlist directly), yet generic verification must still read
	// it from the request.
	for _, h := range HeaderAllowlist {
		if h == "X-Varroa-Token" {
			t.Fatal("X-Varroa-Token must not be in the header allowlist")
		}
	}

	store := crdstore.NewFake()
	crdstore.MustSeed(store, placedPipelinesTenant("payments", "tok", "secret"))
	client := newFakeResourceClient()
	conn := tenantQueueTestBus(t)
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Store: store, BusConn: conn, Logger: slog.Default()})

	received := make(chan []byte, 1)
	sub, err := conn.SubscribeData(bus.WebhookTenantSubject("core", "payments"), func(data []byte) {
		received <- data
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/hibernation/tok/tenants/payments/queue/generic-webhook-trigger/invoke?token=payments:x", bytes.NewReader(body))
	req.Header.Set("X-Varroa-Token", "secret")
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	select {
	case data := <-received:
		var env map[string]any
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if env["forge"] != "generic" {
			t.Fatalf("forge = %v, want generic", env["forge"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no envelope published within timeout")
	}
}

func giteaHexSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestIdentifyForge_GiteaBeforeGitHub(t *testing.T) {
	// Gitea and Forgejo also send X-GitHub-Event, so the gitea check must win.
	for _, hdr := range []string{"X-Gitea-Event", "X-Forgejo-Event"} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set(hdr, "push")
		if got := identifyForge(req); got != "gitea" {
			t.Errorf("%s with X-GitHub-Event: forge = %q, want gitea", hdr, got)
		}
	}
}

func TestTenantQueue_GiteaSignatureAccepted(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, placedPipelinesTenant("payments", "tok", "secret"))
	client := newFakeResourceClient()
	conn := tenantQueueTestBus(t)
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Store: store, BusConn: conn, Logger: slog.Default()})

	received := make(chan []byte, 1)
	sub, err := conn.SubscribeData(bus.WebhookTenantSubject("core", "payments"), func(data []byte) {
		received <- data
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	body := []byte(`{"ref":"refs/heads/main"}`)
	req := httptest.NewRequest(http.MethodPost, "/hibernation/tok/tenants/payments/queue/gitea-webhook/post", bytes.NewReader(body))
	req.Header.Set("X-Gitea-Event", "push")
	req.Header.Set("X-Gitea-Signature", giteaHexSignature("secret", body))
	req.Header.Set("X-Gitea-Delivery", "gt-delivery-1")
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	select {
	case data := <-received:
		var env map[string]any
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("unmarshal published envelope: %v", err)
		}
		if env["forge"] != "gitea" || env["deliveryId"] != "gt-delivery-1" {
			t.Fatalf("envelope identity = %+v, want forge=gitea deliveryId=gt-delivery-1", env)
		}
		headers, _ := env["headers"].(map[string]any)
		if headers["X-Gitea-Event"] != "push" {
			t.Errorf("envelope headers = %+v, want X-Gitea-Event carried", headers)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no envelope published within timeout")
	}
}

func TestTenantQueue_ForgejoSignatureHeaderAccepted(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, placedPipelinesTenant("payments", "tok", "secret"))
	client := newFakeResourceClient()
	conn := tenantQueueTestBus(t)
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Store: store, BusConn: conn, Logger: slog.Default()})

	received := make(chan []byte, 1)
	sub, err := conn.SubscribeData(bus.WebhookTenantSubject("core", "payments"), func(data []byte) {
		received <- data
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	body := []byte(`{"ref":"refs/heads/main"}`)
	req := httptest.NewRequest(http.MethodPost, "/hibernation/tok/tenants/payments/queue/gitea-webhook/post", bytes.NewReader(body))
	req.Header.Set("X-Forgejo-Event", "pull_request")
	req.Header.Set("X-Forgejo-Signature", giteaHexSignature("secret", body))
	req.Header.Set("X-Forgejo-Delivery", "fj-delivery-1")
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	select {
	case data := <-received:
		var env map[string]any
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("unmarshal published envelope: %v", err)
		}
		if env["forge"] != "gitea" || env["deliveryId"] != "fj-delivery-1" {
			t.Fatalf("envelope identity = %+v, want forge=gitea deliveryId=fj-delivery-1", env)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no envelope published within timeout")
	}
}

func TestTenantQueue_GiteaBadSignatureRejected(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, placedPipelinesTenant("payments", "tok", "secret"))
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{Brood: newFakeBrood(client), Store: store, Logger: slog.Default()})

	body := []byte(`{"ref":"refs/heads/main"}`)
	req := httptest.NewRequest(http.MethodPost, "/hibernation/tok/tenants/payments/queue/gitea-webhook/post", bytes.NewReader(body))
	req.Header.Set("X-Gitea-Event", "push")
	req.Header.Set("X-Gitea-Signature", giteaHexSignature("some-other-secret", body))
	rec := httptest.NewRecorder()

	srv.HandleHibernationDispatch(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a mismatched Gitea signature", rec.Code)
	}
}
