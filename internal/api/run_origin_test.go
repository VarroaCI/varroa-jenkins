package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestRunOriginOf_PrefersRunSourceURL asserts the run.source copy of
// scm_url wins over the run.started copy.
func TestRunOriginOf_PrefersRunSourceURL(t *testing.T) {
	payload := json.RawMessage(`{"run.source":{"scm_url":"https://github.com/o/r"},` +
		`"run.started":{"payload":{"scm_url":"https://example.org/other"}}}`)
	if got := runOriginOf(payload).SCMURL; got != "https://github.com/o/r" {
		t.Fatalf("scmUrl = %q", got)
	}
}

// TestRunOriginOf_FallsBackToStartedURL asserts the run.started copy
// serves when no run.source copy exists.
func TestRunOriginOf_FallsBackToStartedURL(t *testing.T) {
	payload := json.RawMessage(`{"run.started":{"payload":{"scm_url":"https://gitlab.com/o/r"}}}`)
	if got := runOriginOf(payload).SCMURL; got != "https://gitlab.com/o/r" {
		t.Fatalf("scmUrl = %q", got)
	}
}

// TestRunOriginOfPairsTheForgeWithItsURL asserts the forge comes from the
// same event as the URL it describes.
func TestRunOriginOfPairsTheForgeWithItsURL(t *testing.T) {
	payload := json.RawMessage(`{"run.source":{"scm_url":"https://gitea.example.com/o/r","scm_forge":"gitea"},` +
		`"run.started":{"payload":{"scm_url":"https://github.com/o/r","scm_forge":"github"}}}`)
	if got := runOriginOf(payload); got.SCMURL != "https://gitea.example.com/o/r" || got.SCMForge != "gitea" {
		t.Fatalf("runOriginOf = %+v, want the run.source pair", got)
	}
	started := json.RawMessage(`{"run.started":{"payload":{"scm_url":"https://github.com/o/r","scm_forge":"github"}}}`)
	if got := runOriginOf(started); got.SCMURL != "https://github.com/o/r" || got.SCMForge != "github" {
		t.Fatalf("runOriginOf = %+v, want the run.started pair", got)
	}
}

// TestRunOriginOfDropsAnUnknownForge asserts a forge outside the three
// UI forges answers the empty string and keeps the URL.
func TestRunOriginOfDropsAnUnknownForge(t *testing.T) {
	payload := json.RawMessage(`{"run.source":{"scm_url":"https://example.org/o/r","scm_forge":"bitbucket"}}`)
	if got := runOriginOf(payload); got.SCMURL != "https://example.org/o/r" || got.SCMForge != "" {
		t.Fatalf("runOriginOf = %+v, want the URL with no forge", got)
	}
}

// TestRunOriginOf_MalformedPayloadAnswersEmpty asserts a payload that
// is not a JSON object yields two empty strings.
func TestRunOriginOf_MalformedPayloadAnswersEmpty(t *testing.T) {
	for _, payload := range []json.RawMessage{nil, json.RawMessage(`{"run.started":`), json.RawMessage(`[1]`)} {
		if got := runOriginOf(payload); got != (runOrigin{}) {
			t.Fatalf("runOriginOf(%s) = %+v, want the zero value", payload, got)
		}
	}
}

// TestClassifyTriggerSource asserts each cause class maps to its trigger
// source, an unknown class maps to other, and precedence follows the table.
func TestClassifyTriggerSource(t *testing.T) {
	cases := []struct {
		name   string
		causes []string
		want   string
	}{
		{"replay", []string{"VarroaReplayCause"}, "replay"},
		{"replay plain", []string{"ReplayCause"}, "replay"},
		{"upstream", []string{"UpstreamCause"}, "upstream"},
		{"build upstream", []string{"BuildUpstreamCause"}, "upstream"},
		{"user", []string{"UserIdCause"}, "user"},
		{"user cause", []string{"UserCause"}, "user"},
		{"varroa user", []string{"VarroaUserCause"}, "user"},
		{"timer", []string{"TimerTriggerCause"}, "cron"},
		{"parameterized timer", []string{"ParameterizedTimerTriggerCause"}, "cron"},
		{"github push", []string{"GitHubPushCause"}, "webhook"},
		{"github pr", []string{"GitHubPRCause"}, "webhook"},
		{"gitlab", []string{"GitLabWebHookCause"}, "webhook"},
		{"branch event", []string{"BranchEventCause"}, "webhook"},
		{"generic", []string{"GenericCause"}, "webhook"},
		{"remote", []string{"RemoteCause"}, "webhook"},
		{"scm trigger", []string{"SCMTriggerCause"}, "scm"},
		{"branch indexing", []string{"BranchIndexingCause"}, "scm"},
		{"unknown", []string{"SomeOtherCause"}, "other"},
		{"empty", nil, ""},
		{"upstream beats user", []string{"UserIdCause", "UpstreamCause"}, "upstream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyTriggerSource(tc.causes); got != tc.want {
				t.Fatalf("classifyTriggerSource(%v) = %q, want %q", tc.causes, got, tc.want)
			}
		})
	}
}

// TestGetRunCarriesScmForge asserts GET /runs/{run} returns the forge of
// the run's repository from the run.source payload.
func TestGetRunCarriesScmForge(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r2"] = recordstore.Run{
		RunID: "r2", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
		Payload: json.RawMessage(`{"run.source":{"scm_url":"https://gitea.example.com/o/r","scm_forge":"gitea"}}`),
	}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r2", map[string]string{"run": "r2"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.SCMForge != "gitea" {
		t.Fatalf("scmForge = %q, want gitea", detail.SCMForge)
	}
}

// TestGetRunCarriesScmURLAndTriggerSource asserts GET /runs/{run} returns
// the repository URL and the trigger source from the run payload.
func TestGetRunCarriesScmURLAndTriggerSource(t *testing.T) {
	reader := newFakeRecordReader()
	reader.runs["r1"] = recordstore.Run{
		RunID: "r1", Tenant: "test-ns/ctrl-a", JobPath: "svc/main", Status: "COMPLETED",
		Payload: json.RawMessage(`{"run.started":{"payload":{"scm_url":"https://github.com/o/r",` +
			`"cause_types":["TimerTriggerCause"]}}}`),
	}
	srv := newRunFixtureServer(reader, nil)

	rec := doRunRequest(t, srv.handleGetRun, "/runs/r1", map[string]string{"run": "r1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.SCMURL != "https://github.com/o/r" || detail.TriggerSource != "cron" {
		t.Fatalf("scmUrl = %q, triggerSource = %q", detail.SCMURL, detail.TriggerSource)
	}
}
