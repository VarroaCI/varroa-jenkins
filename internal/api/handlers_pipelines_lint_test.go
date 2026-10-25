package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	mitesrv "github.com/varroaci/varroa-jenkins/internal/mite"
)

// lintFixture is a cell-hosted tenant whose cell answers the linter at the
// returned test server. The cell starts Connected.
type lintFixture struct {
	srv    *Server
	client *fakeResourceClient
	hits   *lintHits
}

// lintHits records what the fake linter received.
type lintHits struct {
	path, auth, form string
}

// newLintFixture builds the fixture. reply is the linter's JSON body, and
// status is its HTTP status.
func newLintFixture(t *testing.T, phase v1alpha1.ControllerPhase, reply string, status int) lintFixture {
	t.Helper()
	client, reader := tenantCellFixture(t, []string{tenantCellTenant})
	client.controllers[tenantCellName].Status.Phase = phase
	seedTenant(t, client.crdStore, tenantCellObject("core", v1alpha1.TenantReasonTenantPlaced))

	hits := &lintHits{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.path = r.URL.Path
		hits.auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		hits.form = form.Get("jenkinsfile")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(ts.Close)

	srv := tenantCellServer(client, tenantCellAuthorizer([]string{tenantCellTenant}, nil), reader)
	signer, err := mitesrv.NewMiteTokenSigner()
	if err != nil {
		t.Fatalf("NewMiteTokenSigner: %v", err)
	}
	srv.deps.JenkinsTokenSigner = signer
	srv.runnerDoorServiceURL = func(*v1alpha1.Controller) string { return ts.URL }
	return lintFixture{srv: srv, client: client, hits: hits}
}

const lintSuccessReply = `{"status":"ok","data":{"result":"success"}}`

const lintFailureReply = `{"status":"ok","data":{"result":"failure","errors":[` +
	`{"error":"Unknown stage @ line 3, column 5."}]}}`

const lintPlainTextBody = "pipeline {\n  agent any\n}\n"

// lintCall posts text to the lint route through the service.
func lintCall(t *testing.T, fx lintFixture, text string) (JenkinsfileLintResult, *ServiceError) {
	t.Helper()
	return fx.srv.LintJenkinsfile(t.Context(), tenantCellClaims, tenantCellTenant, text)
}

// TestLintJenkinsfile_ValidAnswersValid asserts a clean linter reply
// answers valid with no errors, and that the linter received the text
// with an operator token.
func TestLintJenkinsfile_ValidAnswersValid(t *testing.T) {
	fx := newLintFixture(t, v1alpha1.ControllerPhaseConnected, lintSuccessReply, http.StatusOK)
	body, serr := lintCall(t, fx, lintPlainTextBody)
	if serr != nil {
		t.Fatalf("serr = %v", serr)
	}
	if !body.Valid || len(body.Errors) != 0 {
		t.Fatalf("body = %+v, want valid with no errors", body)
	}
	if body.Cell != tenantCellName || body.Namespace != tenantCellNamespace {
		t.Errorf("cell/namespace = %q/%q, want %q/%q", body.Cell, body.Namespace, tenantCellName, tenantCellNamespace)
	}
	if fx.hits.path != "/pipeline-model-converter/validateJenkinsfile" {
		t.Errorf("linter path = %q", fx.hits.path)
	}
	if !strings.HasPrefix(fx.hits.auth, "Bearer ") {
		t.Errorf("authorization = %q, want a bearer token", fx.hits.auth)
	}
	if fx.hits.form != lintPlainTextBody {
		t.Errorf("linter received %q, want the posted text", fx.hits.form)
	}
}

// TestLintJenkinsfile_FailureCarriesLineAndColumn asserts a linter failure
// answers valid false with the source, message, and location of each error.
func TestLintJenkinsfile_FailureCarriesLineAndColumn(t *testing.T) {
	fx := newLintFixture(t, v1alpha1.ControllerPhaseRunning, lintFailureReply, http.StatusOK)
	body, serr := lintCall(t, fx, lintPlainTextBody)
	if serr != nil {
		t.Fatalf("serr = %v", serr)
	}
	if body.Valid || len(body.Errors) != 1 {
		t.Fatalf("body = %+v, want one error and valid false", body)
	}
	e := body.Errors[0]
	if e.Source != "jenkins" || e.Line != 3 || e.Column != 5 || !strings.Contains(e.Message, "Unknown stage") {
		t.Errorf("error = %+v, want a jenkins error at 3:5", e)
	}
}

// TestLintJenkinsfile_CellRuleReportsJobDsl asserts the Varroa cell rule
// runs on a cell-hosted tenant after the linter answers success.
func TestLintJenkinsfile_CellRuleReportsJobDsl(t *testing.T) {
	fx := newLintFixture(t, v1alpha1.ControllerPhaseConnected, lintSuccessReply, http.StatusOK)
	body, serr := lintCall(t, fx, "pipeline {\n  agent any\n  stages { stage('seed') { steps { jobDsl targets: 'jobs.groovy' } } }\n}\n")
	if serr != nil {
		t.Fatalf("serr = %v", serr)
	}
	if body.Valid || len(body.Errors) != 1 {
		t.Fatalf("body = %+v, want one varroa finding", body)
	}
	if e := body.Errors[0]; e.Source != "varroa" || e.Rule != "no-job-dsl" {
		t.Errorf("error = %+v, want source varroa and rule no-job-dsl", e)
	}
}

// TestLintJenkinsfile_CellPhaseRefusals asserts the cell phase decides
// the answer and that the route never wakes a cell.
func TestLintJenkinsfile_CellPhaseRefusals(t *testing.T) {
	tests := []struct {
		name   string
		phase  v1alpha1.ControllerPhase
		status int
	}{
		{"hibernated", v1alpha1.ControllerPhaseHibernated, http.StatusConflict},
		{"stopped", v1alpha1.ControllerPhaseStopped, http.StatusConflict},
		{"provisioning", v1alpha1.ControllerPhaseProvisioning, http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newLintFixture(t, tc.phase, lintSuccessReply, http.StatusOK)
			_, serr := lintCall(t, fx, lintPlainTextBody)
			if serr == nil || serr.Status != tc.status {
				t.Fatalf("serr = %+v, want status %d", serr, tc.status)
			}
			if fx.hits.path != "" {
				t.Errorf("the linter was called for a %s cell", tc.name)
			}
		})
	}
}

// TestLintJenkinsfile_MemberClusterAnswers409 asserts a tenant placed on
// another cluster is refused, because only a core-cluster cell is linted.
func TestLintJenkinsfile_MemberClusterAnswers409(t *testing.T) {
	fx := newLintFixture(t, v1alpha1.ControllerPhaseConnected, lintSuccessReply, http.StatusOK)
	seedTenant(t, fx.client.crdStore, tenantCellObject("east", v1alpha1.TenantReasonTenantPlaced))
	_, serr := lintCall(t, fx, lintPlainTextBody)
	if serr == nil || serr.Status != http.StatusConflict {
		t.Fatalf("serr = %+v, want 409", serr)
	}
}

// TestLintJenkinsfile_RequestLimits asserts an empty text answers 400 and
// a text over the cap answers 413.
func TestLintJenkinsfile_RequestLimits(t *testing.T) {
	fx := newLintFixture(t, v1alpha1.ControllerPhaseConnected, lintSuccessReply, http.StatusOK)
	if _, serr := lintCall(t, fx, "  \n"); serr == nil || serr.Status != http.StatusBadRequest {
		t.Errorf("empty text serr = %+v, want 400", serr)
	}
	if _, serr := lintCall(t, fx, strings.Repeat("a", jenkinsfileLintMaxText+1)); serr == nil || serr.Status != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize text serr = %+v, want 413", serr)
	}
}

// TestLintJenkinsfile_LinterFailureAnswers502 asserts a linter that answers
// a non-200 status, or an unparseable reply, is a 502.
func TestLintJenkinsfile_LinterFailureAnswers502(t *testing.T) {
	fx := newLintFixture(t, v1alpha1.ControllerPhaseConnected, "boom", http.StatusInternalServerError)
	if _, serr := lintCall(t, fx, lintPlainTextBody); serr == nil || serr.Status != http.StatusBadGateway {
		t.Errorf("non-200 serr = %+v, want 502", serr)
	}
	fx = newLintFixture(t, v1alpha1.ControllerPhaseConnected, "not json", http.StatusOK)
	if _, serr := lintCall(t, fx, lintPlainTextBody); serr == nil || serr.Status != http.StatusBadGateway {
		t.Errorf("unparseable serr = %+v, want 502", serr)
	}
}

// TestLintJenkinsfile_HandlerRejectsOversizeBody asserts the HTTP route
// answers 413 for a body over the request cap.
func TestLintJenkinsfile_HandlerRejectsOversizeBody(t *testing.T) {
	fx := newLintFixture(t, v1alpha1.ControllerPhaseConnected, lintSuccessReply, http.StatusOK)
	big := strings.Repeat("a", jenkinsfileLintMaxBody+1)
	raw, _ := json.Marshal(map[string]string{"jenkinsfile": big})
	req := httptest.NewRequest(http.MethodPost, "/pipelines/"+tenantCellTenant+"/lint", strings.NewReader(string(raw)))
	req.SetPathValue("tenant", tenantCellTenant)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), tenantCellClaims))
	rec := httptest.NewRecorder()
	fx.srv.handleLintJenkinsfile(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

// TestParseLinterReply asserts both error shapes parse, and that an
// unknown envelope is an error.
func TestParseLinterReply(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    int
		wantErr bool
	}{
		{"success", `{"status":"ok","data":{"result":"success"}}`, 0, false},
		{"string error", `{"status":"ok","data":{"result":"failure","errors":[{"error":"x @ line 2, column 9."}]}}`, 1, false},
		{"list error", `{"status":"ok","data":{"result":"failure","errors":[{"error":["a","b"]}]}}`, 2, false},
		{"failure without messages", `{"status":"ok","data":{"result":"failure"}}`, 0, true},
		{"bad status", `{"status":"fail"}`, 0, true},
		{"bad result", `{"status":"ok","data":{"result":"maybe"}}`, 0, true},
		{"not json", `<html>`, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLinterReply([]byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && len(got) != tc.want {
				t.Errorf("findings = %d, want %d", len(got), tc.want)
			}
			if err == nil && got == nil {
				t.Error("findings is nil, want an empty slice")
			}
		})
	}
}

// TestParseLinterReply_LocationFromMessage asserts line and column come from
// the message text.
func TestParseLinterReply_LocationFromMessage(t *testing.T) {
	got, err := parseLinterReply([]byte(lintFailureReply))
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, err %v", got, err)
	}
	if got[0].Line != 3 || got[0].Column != 5 {
		t.Errorf("line/column = %d/%d, want 3/5", got[0].Line, got[0].Column)
	}
}
