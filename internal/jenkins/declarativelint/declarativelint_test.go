package declarativelint

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const failureReply = `{"status":"ok","data":{"result":"failure","errors":[` +
	`{"error":"Unknown stage @ line 3, column 5."}]}}`

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
			got, err := ParseReply([]byte(tc.body))
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
	got, err := ParseReply([]byte(failureReply))
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, err %v", got, err)
	}
	if got[0].Line != 3 || got[0].Column != 5 {
		t.Errorf("line/column = %d/%d, want 3/5", got[0].Line, got[0].Column)
	}
}

// TestValidate_PostsFormWithBearer asserts Validate posts the text as a form
// field to the linter path with the Bearer token.
func TestValidate_PostsFormWithBearer(t *testing.T) {
	var path, auth, form, ctype string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		auth = r.Header.Get("Authorization")
		ctype = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		v, _ := url.ParseQuery(string(raw))
		form = v.Get("jenkinsfile")
		_, _ = w.Write([]byte(failureReply))
	}))
	defer ts.Close()

	got, err := Validate(context.Background(), ts.Client(), ts.URL, "tok", "pipeline {}")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1", len(got))
	}
	if path != "/pipeline-model-converter/validateJenkinsfile" {
		t.Errorf("path = %q", path)
	}
	if auth != "Bearer tok" {
		t.Errorf("Authorization = %q", auth)
	}
	if ctype != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", ctype)
	}
	if form != "pipeline {}" {
		t.Errorf("jenkinsfile = %q", form)
	}
}

// TestValidate_Non200IsError asserts a linter status other than 200 is an
// error that names the status.
func TestValidate_Non200IsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := Validate(context.Background(), client, ts.URL, "tok", "pipeline {}")
	if err == nil || err.Error() != "the cell linter answered status 500" {
		t.Fatalf("err = %v, want status 500 error", err)
	}
}

// TestValidate_TransportErrorKeepsCause asserts a transport failure answers
// the generic message, and that Unwrap returns the transport cause for the log.
func TestValidate_TransportErrorKeepsCause(t *testing.T) {
	client := &http.Client{Timeout: time.Second}
	_, err := Validate(context.Background(), client, "http://127.0.0.1:1", "tok", "pipeline {}")
	var unreachable *UnreachableError
	if !errors.As(err, &unreachable) || unreachable.Unwrap() == nil {
		t.Fatalf("err = %v, want an UnreachableError with a cause", err)
	}
	if err.Error() != "the cell linter is unreachable" {
		t.Errorf("message = %q", err.Error())
	}
}

// TestParseReply_ScriptedJenkinsfile asserts the no-pipeline-step finding
// becomes one ScriptedMessage finding, and the file text stays out of it.
func TestParseReply_ScriptedJenkinsfile(t *testing.T) {
	message := "Jenkinsfile content '" + "node {\n  load 'ci/pipeline.groovy'\n}\n" + "' did not contain the 'pipeline' step"
	raw, err := json.Marshal(map[string]any{
		"status": "ok",
		"data": map[string]any{
			"result": "failure",
			"errors": []map[string]any{{"error": message}},
		},
	})
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	findings, err := ParseReply(raw)
	if err != nil {
		t.Fatalf("ParseReply: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want one", findings)
	}
	f := findings[0]
	if f.Source != "jenkins" || f.Message != ScriptedMessage || f.Line != 0 || f.Column != 0 {
		t.Errorf("finding = %+v, want the scripted message with no location", f)
	}
	if strings.Contains(f.Message, "node {") {
		t.Errorf("message repeats the file text: %q", f.Message)
	}
}
