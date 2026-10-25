package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// rerunReader answers the change-request and revision lookups of a re-run.
// The embedded fake answers the rest.
type rerunReader struct {
	*fakeRecordReader
	prJobs  map[string][]string
	revJobs map[string][]string
}

func (r *rerunReader) ChangeRequestJobs(_ context.Context, _, _, changeURL string) ([]string, error) {
	return r.prJobs[changeURL], nil
}

func (r *rerunReader) RevisionJobs(_ context.Context, _, _, revision string) ([]string, error) {
	return r.revJobs[revision], nil
}

// rerunPermission answers the forge permission read of a re-run.
type rerunPermission struct {
	reply forgeapi.PermissionReply
	err   error
	calls int
	req   forgeapi.PermissionRequest
}

func (p *rerunPermission) Permission(_ context.Context, _ string, req forgeapi.PermissionRequest) (forgeapi.PermissionReply, error) {
	p.calls++
	p.req = req
	return p.reply, p.err
}

// rerunHarness holds the fakes of one re-run test.
type rerunHarness struct {
	srv      *Server
	transp   *fakeTriggerTransport
	reader   *rerunReader
	perm     *rerunPermission
	conn     *bus.Conn
	subjectG string
}

// newRerunHarness builds a placed pipelines tenant "payments" on cluster core,
// cell cell-a in ns1, with the given triggers. The re-run work runs inline.
func newRerunHarness(t *testing.T, triggers *v1alpha1.TenantTriggersSpec) *rerunHarness {
	t.Helper()
	tm := &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"ns1"},
			Pipelines: &v1alpha1.TenantPipelinesSpec{
				Profile:        "lts",
				AgentNamespace: "ns1",
				Triggers:       triggers,
			},
		},
		Status: v1alpha1.TenantStatus{
			Pipelines: &v1alpha1.TenantPipelinesStatus{WebhookToken: "tok", WebhookSecret: "secret"},
			Placement: &v1alpha1.TenantPlacementStatus{Cluster: "core", Cell: "cell-a", CellNamespace: "ns1"},
		},
	}
	store := crdstore.NewFake()
	crdstore.MustSeed(store, tm)
	reader := &rerunReader{
		fakeRecordReader: newFakeRecordReader(),
		prJobs:           map[string][]string{},
		revJobs:          map[string][]string{},
	}
	transp := &fakeTriggerTransport{}
	perm := &rerunPermission{reply: forgeapi.PermissionReply{Status: forgeapi.Status{OK: true}, Permission: "write"}}
	conn := tenantQueueTestBus(t)
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{
		Brood:           newFakeBrood(client),
		Store:           store,
		BusConn:         conn,
		Logger:          slog.Default(),
		MiteRegistry:    transp,
		RecordStore:     reader,
		ForgePermission: perm,
		CoreCluster:     "core",
	})
	srv.rerunSpawn = func(work func()) { work() }
	return &rerunHarness{
		srv:      srv,
		transp:   transp,
		reader:   reader,
		perm:     perm,
		conn:     conn,
		subjectG: bus.WebhookTenantSubject("core", "payments"),
	}
}

// post sends one GitHub delivery of event to the tenant door and returns the
// status code.
func (h *rerunHarness) post(t *testing.T, event string, body []byte) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/hibernation/tok/tenants/payments/queue/github-webhook/", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", githubSignature("secret", body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.srv.HandleHibernationDispatch(rec, req)
	return rec.Code
}

// issueCommentBody builds an issue_comment delivery on pull request 7 of acme/app.
func issueCommentBody(t *testing.T, action, text, association, login string, onPR bool) []byte {
	t.Helper()
	issue := map[string]any{"number": 7}
	if onPR {
		issue["pull_request"] = map[string]any{"html_url": "https://github.com/acme/app/pull/7"}
	}
	return rerunJSON(t, map[string]any{
		"action":     action,
		"issue":      issue,
		"comment":    map[string]any{"body": text, "author_association": association, "user": map[string]any{"login": login}},
		"repository": map[string]any{"full_name": "acme/app", "html_url": "https://github.com/acme/app"},
	})
}

func rerunJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func rerunBoolPtr(b bool) *bool { return &b }

var retestTriggers = &v1alpha1.TenantTriggersSpec{CommentPattern: "^/retest$"}

func TestTenantDoor_IssueCommentMatchingPatternTriggersPRJob(t *testing.T) {
	h := newRerunHarness(t, retestTriggers)
	h.reader.prJobs["https://github.com/acme/app/pull/7"] = []string{"payments/app/PR-7"}

	code := h.post(t, "issue_comment", issueCommentBody(t, "created", " /retest \n", "MEMBER", "alice", true))
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	calls := h.transp.calls()
	if len(calls) != 1 {
		t.Fatalf("trigger calls = %d, want 1", len(calls))
	}
	tb := calls[0].cmd.TriggerBuild
	if tb.JobPath != "payments/app/PR-7" {
		t.Fatalf("job path = %q", tb.JobPath)
	}
	if tb.Actor != "github:alice" {
		t.Fatalf("actor = %q, want github:alice", tb.Actor)
	}
	if !strings.HasPrefix(tb.IntentKey, "manual:") {
		t.Fatalf("intent key = %q, want a manual: key", tb.IntentKey)
	}
	if want := "Started by a pull request comment of alice on acme/app#7"; tb.Cause != want {
		t.Fatalf("cause = %q, want %q", tb.Cause, want)
	}
	if h.perm.req.Repo != "https://github.com/acme/app" || h.perm.req.Login != "alice" || h.perm.req.Tenant != "payments" {
		t.Fatalf("permission request = %+v", h.perm.req)
	}
}

func TestTenantDoor_IssueCommentWithoutPatternTriggersNothing(t *testing.T) {
	h := newRerunHarness(t, nil)
	h.reader.prJobs["https://github.com/acme/app/pull/7"] = []string{"payments/app/PR-7"}
	if code := h.post(t, "issue_comment", issueCommentBody(t, "created", "/retest", "MEMBER", "alice", true)); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	if n := len(h.transp.calls()); n != 0 {
		t.Fatalf("trigger calls = %d, want 0", n)
	}
	if h.perm.calls != 0 {
		t.Fatalf("permission calls = %d, want 0", h.perm.calls)
	}
}

func TestTenantDoor_IssueCommentNonMatchingBodyTriggersNothing(t *testing.T) {
	h := newRerunHarness(t, retestTriggers)
	h.post(t, "issue_comment", issueCommentBody(t, "created", "/lgtm", "MEMBER", "alice", true))
	if n := len(h.transp.calls()); n != 0 || h.perm.calls != 0 {
		t.Fatalf("triggers = %d, permission calls = %d; want 0 and 0", n, h.perm.calls)
	}
}

func TestTenantDoor_IssueCommentEditedActionIgnored(t *testing.T) {
	h := newRerunHarness(t, retestTriggers)
	h.post(t, "issue_comment", issueCommentBody(t, "edited", "/retest", "MEMBER", "alice", true))
	if n := len(h.transp.calls()); n != 0 {
		t.Fatalf("trigger calls = %d, want 0", n)
	}
}

func TestTenantDoor_IssueCommentOnIssueIgnored(t *testing.T) {
	h := newRerunHarness(t, retestTriggers)
	h.post(t, "issue_comment", issueCommentBody(t, "created", "/retest", "MEMBER", "alice", false))
	if n := len(h.transp.calls()); n != 0 {
		t.Fatalf("trigger calls = %d, want 0", n)
	}
}

func TestTenantDoor_IssueCommentAssociationNoneRefusedWithoutForgeCall(t *testing.T) {
	h := newRerunHarness(t, retestTriggers)
	h.reader.prJobs["https://github.com/acme/app/pull/7"] = []string{"payments/app/PR-7"}
	h.post(t, "issue_comment", issueCommentBody(t, "created", "/retest", "NONE", "mallory", true))
	if n := len(h.transp.calls()); n != 0 || h.perm.calls != 0 {
		t.Fatalf("triggers = %d, permission calls = %d; want 0 and 0", n, h.perm.calls)
	}
}

func TestTenantDoor_IssueCommentReadPermissionRefused(t *testing.T) {
	h := newRerunHarness(t, retestTriggers)
	h.reader.prJobs["https://github.com/acme/app/pull/7"] = []string{"payments/app/PR-7"}
	h.perm.reply = forgeapi.PermissionReply{Status: forgeapi.Status{OK: true}, Permission: "read"}
	h.post(t, "issue_comment", issueCommentBody(t, "created", "/retest", "COLLABORATOR", "carol", true))
	if n := len(h.transp.calls()); n != 0 {
		t.Fatalf("trigger calls = %d, want 0 for read permission", n)
	}
	if h.perm.calls != 1 {
		t.Fatalf("permission calls = %d, want 1", h.perm.calls)
	}
}

func TestTenantDoor_IssueCommentRelayErrorRefused(t *testing.T) {
	h := newRerunHarness(t, retestTriggers)
	h.reader.prJobs["https://github.com/acme/app/pull/7"] = []string{"payments/app/PR-7"}
	h.perm.err = errors.New("relay down")
	h.post(t, "issue_comment", issueCommentBody(t, "created", "/retest", "OWNER", "dave", true))
	if n := len(h.transp.calls()); n != 0 {
		t.Fatalf("trigger calls = %d, want 0 on a relay error", n)
	}
}

func checkRunBody(t *testing.T, action, externalID string) []byte {
	t.Helper()
	return rerunJSON(t, map[string]any{
		"action":     action,
		"check_run":  map[string]any{"external_id": externalID},
		"sender":     map[string]any{"login": "bob"},
		"repository": map[string]any{"full_name": "acme/app", "html_url": "https://github.com/acme/app"},
	})
}

func TestTenantDoor_CheckRunRerequestedTriggersExternalIDJob(t *testing.T) {
	h := newRerunHarness(t, nil)
	h.post(t, "check_run", checkRunBody(t, "rerequested", "payments/app/main#42"))
	calls := h.transp.calls()
	if len(calls) != 1 {
		t.Fatalf("trigger calls = %d, want 1", len(calls))
	}
	tb := calls[0].cmd.TriggerBuild
	if tb.JobPath != "payments/app/main" || tb.Actor != "github:bob" {
		t.Fatalf("trigger = job %q actor %q", tb.JobPath, tb.Actor)
	}
	if want := "Started by a GitHub check re-run request of bob"; tb.Cause != want {
		t.Fatalf("cause = %q, want %q", tb.Cause, want)
	}
}

func TestTenantDoor_CheckRunOtherTenantExternalIDRefused(t *testing.T) {
	h := newRerunHarness(t, nil)
	h.post(t, "check_run", checkRunBody(t, "rerequested", "billing/app/main#42"))
	if n := len(h.transp.calls()); n != 0 {
		t.Fatalf("trigger calls = %d, want 0", n)
	}
}

func TestTenantDoor_CheckRunChecksRerunOffTriggersNothing(t *testing.T) {
	h := newRerunHarness(t, &v1alpha1.TenantTriggersSpec{ChecksRerun: rerunBoolPtr(false)})
	h.post(t, "check_run", checkRunBody(t, "rerequested", "payments/app/main#42"))
	if n := len(h.transp.calls()); n != 0 {
		t.Fatalf("trigger calls = %d, want 0", n)
	}
}

func TestTenantDoor_CheckRunCreatedActionIgnored(t *testing.T) {
	h := newRerunHarness(t, nil)
	h.post(t, "check_run", checkRunBody(t, "created", "payments/app/main#42"))
	if n := len(h.transp.calls()); n != 0 {
		t.Fatalf("trigger calls = %d, want 0", n)
	}
}

func TestTenantDoor_CheckSuiteRerequestedTriggersPRAndBranchJobs(t *testing.T) {
	h := newRerunHarness(t, nil)
	const head = "0123456789abcdef0123456789abcdef01234567"
	h.reader.prJobs["https://github.com/acme/app/pull/7"] = []string{"payments/app/PR-7"}
	h.reader.revJobs[head] = []string{"payments/app/main"}
	body := rerunJSON(t, map[string]any{
		"action": "rerequested",
		"check_suite": map[string]any{
			"head_sha":      head,
			"pull_requests": []map[string]any{{"number": 7}},
		},
		"repository": map[string]any{"full_name": "acme/app", "html_url": "https://github.com/acme/app"},
		"sender":     map[string]any{"login": "erin"},
	})
	h.post(t, "check_suite", body)
	calls := h.transp.calls()
	if len(calls) != 2 {
		t.Fatalf("trigger calls = %d, want 2", len(calls))
	}
	got := map[string]bool{}
	for _, c := range calls {
		got[c.cmd.TriggerBuild.JobPath] = true
		if c.cmd.TriggerBuild.Actor != "github:erin" {
			t.Fatalf("actor = %q, want github:erin", c.cmd.TriggerBuild.Actor)
		}
	}
	if !got["payments/app/PR-7"] || !got["payments/app/main"] {
		t.Fatalf("jobs = %v, want the PR job and the branch job", got)
	}
}

func TestTenantDoor_RerunEventsAreNeverBuffered(t *testing.T) {
	h := newRerunHarness(t, retestTriggers)
	received := make(chan []byte, 4)
	sub, err := h.conn.SubscribeData(h.subjectG, func(data []byte) { received <- data })
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	h.post(t, "issue_comment", issueCommentBody(t, "created", "/retest", "MEMBER", "alice", true))
	h.post(t, "check_run", checkRunBody(t, "rerequested", "payments/app/main#42"))
	select {
	case <-received:
		t.Fatal("a re-run event was buffered to JetStream")
	case <-time.After(300 * time.Millisecond):
	}

	if code := h.post(t, "push", []byte(`{}`)); code != http.StatusAccepted {
		t.Fatalf("push status = %d, want 202", code)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("a push delivery was not buffered")
	}
}

func TestTenantDoor_RerunTriggerTargetsJobHomeSlot(t *testing.T) {
	h := newRerunHarness(t, nil)
	h.reader.jobHomes["core/payments/payments/app/main"] = jobHomeAnswer{namespace: "ns2", cell: "cell-b"}
	h.post(t, "check_run", checkRunBody(t, "rerequested", "payments/app/main#42"))
	calls := h.transp.calls()
	if len(calls) != 1 {
		t.Fatalf("trigger calls = %d, want 1", len(calls))
	}
	if calls[0].ns != "ns2" || calls[0].name != "cell-b" {
		t.Fatalf("sent to %s/%s, want the home slot ns2/cell-b", calls[0].ns, calls[0].name)
	}
}

func TestTenantDoor_RerunJobsCappedAtEight(t *testing.T) {
	h := newRerunHarness(t, retestTriggers)
	var jobs []string
	for i := 0; i < 10; i++ {
		jobs = append(jobs, fmt.Sprintf("payments/app/PR-%d", i))
	}
	h.reader.prJobs["https://github.com/acme/app/pull/7"] = jobs
	h.post(t, "issue_comment", issueCommentBody(t, "created", "/retest", "MEMBER", "alice", true))
	if n := len(h.transp.calls()); n != maxDoorRunsPerEvent {
		t.Fatalf("trigger calls = %d, want %d", n, maxDoorRunsPerEvent)
	}
}

func TestJobPathFromExternalID(t *testing.T) {
	tests := []struct {
		name, externalID, tenant string
		want                     string
		ok                       bool
	}{
		{"branch job", "payments/app/main#42", "payments", "payments/app/main", true},
		{"nested folder", "payments/team/app#1", "payments", "payments/team/app", true},
		{"other tenant", "billing/app#1", "payments", "", false},
		{"tenant prefix only", "payments#1", "payments", "", false},
		{"empty job", "payments/#1", "payments", "", false},
		{"no build number", "payments/app/main", "payments", "", false},
		{"empty build number", "payments/app/main#", "payments", "", false},
		{"non-numeric build number", "payments/app/main#x1", "payments", "", false},
		{"prefix collision", "paymentsx/app#1", "payments", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := jobPathFromExternalID(tt.externalID, tt.tenant)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("jobPathFromExternalID(%q) = %q, %v; want %q, %v", tt.externalID, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestTenantTriggersDefaults(t *testing.T) {
	pattern, checks := tenantTriggers(nil)
	if pattern != nil || !checks {
		t.Fatalf("nil spec: pattern %v checks %v; want nil and true", pattern, checks)
	}
	pattern, checks = tenantTriggers(&v1alpha1.TenantPipelinesSpec{})
	if pattern != nil || !checks {
		t.Fatalf("no triggers block: pattern %v checks %v; want nil and true", pattern, checks)
	}
	pattern, checks = tenantTriggers(&v1alpha1.TenantPipelinesSpec{Triggers: &v1alpha1.TenantTriggersSpec{CommentPattern: "[", ChecksRerun: rerunBoolPtr(false)}})
	if pattern != nil || checks {
		t.Fatalf("invalid pattern: pattern %v checks %v; want nil and false", pattern, checks)
	}
	pattern, _ = tenantTriggers(&v1alpha1.TenantPipelinesSpec{Triggers: retestTriggers})
	if pattern == nil || !pattern.MatchString("/retest") {
		t.Fatal("valid pattern did not compile to a matcher")
	}
}

var _ recordstore.Reader = (*rerunReader)(nil)
