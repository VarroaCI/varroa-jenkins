package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/jenkins"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

const (
	refusedConfigMark = "REFUSED-CONFIG"
	refusedRBACMark   = "REFUSED-RBAC"
	configRefusalText = "Invalid configuration elements for type: class hudson.model.Hudson : bogusKey"
	rbacRefusalText   = "Unclosed counted closure near index 6 (?x)a{2"
)

// cascDocServer starts a Jenkins stub whose JCasC reload reads the live CASC
// directory, as JCasC does. A document that holds a refused mark fails the
// reload with the JCasC error page. It returns the reload count.
func cascDocServer(t *testing.T, cascDir string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var reloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Jenkins-Session", "sess-1")
		switch {
		case strings.HasPrefix(r.URL.Path, "/crumbIssuer"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"crumb":"c","crumbRequestField":"Jenkins-Crumb"}`)
		case r.URL.Path == "/configuration-as-code/reload":
			reloads.Add(1)
			refusal := ""
			for _, name := range []string{"config.yaml", "rbac.yaml"} {
				data, _ := os.ReadFile(filepath.Join(cascDir, name))
				switch {
				case refusal != "":
				case strings.Contains(string(data), refusedConfigMark):
					refusal = configRefusalText
				case strings.Contains(string(data), refusedRBACMark):
					refusal = rbacRefusalText
				}
			}
			if refusal == "" {
				http.Redirect(w, r, "/configuration-as-code/", http.StatusFound)
				return
			}
			fmt.Fprintf(w, "<html><body><h1>Error loading configuration</h1><p>%s</p></body></html>", refusal)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &reloads
}

// seedCascDocs writes live CASC documents. An empty value writes no file.
func seedCascDocs(t *testing.T, cascDir, config, rbac string) {
	t.Helper()
	if err := os.MkdirAll(cascDir, 0755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"config.yaml": config, "rbac.yaml": rbac} {
		if content == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(cascDir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// liveCascDoc answers the live content of one document, or "absent".
func liveCascDoc(t *testing.T, cascDir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cascDir, name))
	if errors.Is(err, os.ErrNotExist) {
		return "absent"
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestApplyConfigViaReloadIsolatesRefusedDocument(t *testing.T) {
	cases := []struct {
		name                       string
		liveConfig, liveRBAC       string
		config, rbac               string
		wantConfigOK, wantRBACOK   bool
		wantConfigErr, wantRBACErr string
		wantLiveConfig, wantLive   string
		wantReloads                int32
	}{
		{
			name:       "refused rbac keeps the accepted config",
			liveConfig: "cfg-old", liveRBAC: "rbac-old",
			config: "cfg-new", rbac: "rbac-new " + refusedRBACMark,
			wantConfigOK: true, wantRBACErr: rbacRefusalText,
			wantLiveConfig: "cfg-new", wantLive: "rbac-old", wantReloads: 2,
		},
		{
			name:       "refused config keeps the accepted rbac",
			liveConfig: "cfg-old", liveRBAC: "rbac-old",
			config: "cfg-new " + refusedConfigMark, rbac: "rbac-new",
			wantRBACOK: true, wantConfigErr: configRefusalText,
			wantLiveConfig: "cfg-old", wantLive: "rbac-new", wantReloads: 3,
		},
		{
			name:       "both refused roll back with their own errors",
			liveConfig: "cfg-old", liveRBAC: "rbac-old",
			config: "cfg-new " + refusedConfigMark, rbac: "rbac-new " + refusedRBACMark,
			wantConfigErr: configRefusalText, wantRBACErr: rbacRefusalText,
			wantLiveConfig: "cfg-old", wantLive: "rbac-old", wantReloads: 4,
		},
		{
			name:       "unchanged config stays accepted",
			liveConfig: "cfg-same", liveRBAC: "rbac-old",
			config: "cfg-same", rbac: "rbac-new " + refusedRBACMark,
			wantConfigOK: true, wantRBACErr: rbacRefusalText,
			wantLiveConfig: "cfg-same", wantLive: "rbac-old", wantReloads: 2,
		},
		{
			name:       "refused new rbac is removed",
			liveConfig: "cfg-old",
			config:     "cfg-new", rbac: "rbac-new " + refusedRBACMark,
			wantConfigOK: true, wantRBACErr: rbacRefusalText,
			wantLiveConfig: "cfg-new", wantLive: "absent", wantReloads: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cascDir := t.TempDir()
			t.Setenv("CASC_JENKINS_CONFIG", cascDir)
			seedCascDocs(t, cascDir, tc.liveConfig, tc.liveRBAC)
			srv, reloads := cascDocServer(t, cascDir)
			client := jenkins.NewClient(srv.URL, "varroa-mite", "test-token")
			agent := &Agent{metrics: noopMiteMetrics(), Logger: slog.Default()}

			out := agent.applyConfigViaReload(context.Background(), client,
				&mitev1.DesiredStateCommand{JcascYaml: tc.config, RbacYaml: tc.rbac})

			if out.configOK != tc.wantConfigOK || out.rbacOK != tc.wantRBACOK {
				t.Fatalf("verdict = config %v rbac %v, want config %v rbac %v (%+v)",
					out.configOK, out.rbacOK, tc.wantConfigOK, tc.wantRBACOK, out)
			}
			checkCascErr(t, "config", out.configErr, tc.wantConfigErr)
			checkCascErr(t, "rbac", out.rbacErr, tc.wantRBACErr)
			if got := liveCascDoc(t, cascDir, "config.yaml"); got != tc.wantLiveConfig {
				t.Errorf("live config.yaml = %q, want %q", got, tc.wantLiveConfig)
			}
			if got := liveCascDoc(t, cascDir, "rbac.yaml"); got != tc.wantLive {
				t.Errorf("live rbac.yaml = %q, want %q", got, tc.wantLive)
			}
			if n := reloads.Load(); n != tc.wantReloads {
				t.Errorf("reloads = %d, want %d", n, tc.wantReloads)
			}
		})
	}
}

// checkCascErr checks one failure text. An empty want needs an empty text.
// A refusal text must carry the Jenkins error and the rollback outcome.
func checkCascErr(t *testing.T, doc, got, want string) {
	t.Helper()
	if want == "" {
		if got != "" {
			t.Errorf("%s error = %q, want none", doc, got)
		}
		return
	}
	if !strings.Contains(got, want) || !strings.Contains(got, cascRolledBack) {
		t.Errorf("%s error = %q, want %q and %q", doc, got, want, cascRolledBack)
	}
}

// TestRollbackRemovesRefusedFirstWrite proves that a first apply with no
// last-good copy removes the refused documents from the live directory.
func TestRollbackRemovesRefusedFirstWrite(t *testing.T) {
	cascDir := t.TempDir()
	t.Setenv("CASC_JENKINS_CONFIG", cascDir)
	seedCascDocs(t, cascDir, "", "")
	srv, reloads := cascDocServer(t, cascDir)
	client := jenkins.NewClient(srv.URL, "varroa-mite", "test-token")
	agent := &Agent{metrics: noopMiteMetrics(), Logger: slog.Default()}

	out := agent.applyConfigViaReload(context.Background(), client,
		&mitev1.DesiredStateCommand{
			JcascYaml: "cfg-new " + refusedConfigMark,
			RbacYaml:  "rbac-new " + refusedRBACMark,
		})

	if out.configOK || out.rbacOK {
		t.Fatalf("verdict = config %v rbac %v, want both refused", out.configOK, out.rbacOK)
	}
	if got := liveCascDoc(t, cascDir, "config.yaml"); got != "absent" {
		t.Errorf("live config.yaml = %q, want absent", got)
	}
	if got := liveCascDoc(t, cascDir, "rbac.yaml"); got != "absent" {
		t.Errorf("live rbac.yaml = %q, want absent", got)
	}
	if !strings.Contains(out.rbacErr, cascNoLastGood) {
		t.Errorf("rbac error = %q, want suffix %q", out.rbacErr, cascNoLastGood)
	}
	if n := reloads.Load(); n != 3 {
		t.Errorf("reloads = %d, want 3 (no rollback reload without a last-good copy)", n)
	}
}

// TestSaveLastGoodDropsStaleCopy proves that a last-good copy from an earlier
// pod never survives a write that creates the live file.
func TestSaveLastGoodDropsStaleCopy(t *testing.T) {
	cascDir := t.TempDir()
	t.Setenv("CASC_JENKINS_CONFIG", cascDir)
	lastGood := filepath.Join(cascDir+"-last-good", "rbac.yaml")
	if err := os.MkdirAll(filepath.Dir(lastGood), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lastGood, []byte("rbac-of-an-old-pod"), 0600); err != nil {
		t.Fatal(err)
	}
	agent := &Agent{metrics: noopMiteMetrics(), Logger: slog.Default()}

	agent.saveLastGood("rbac.yaml")

	if agent.hasLastGood("rbac.yaml") {
		t.Fatal("the stale last-good rbac.yaml survived a save with no live file")
	}
}

// TestHandleDesiredStateReportsRBACRefusalApart drives a push whose
// rbac.yaml Jenkins refuses. The config section applies, and the rbac
// section alone carries the Jenkins error.
func TestHandleDesiredStateReportsRBACRefusalApart(t *testing.T) {
	cascDir := t.TempDir()
	t.Setenv("CASC_JENKINS_CONFIG", cascDir)
	seedCascDocs(t, cascDir, "cfg-old", "rbac-old")
	srv, _ := cascDocServer(t, cascDir)
	agent := newTestAgentWithJenkins(t, srv.URL)

	cmd := &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-rbac-refused",
		JcascYaml:           "cfg-new",
		RbacYaml:            "rbac-new " + refusedRBACMark,
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
	}
	res := agent.handleDesiredState(context.Background(), cmd)

	if !res.ConfigSuccess || res.ConfigError != "" {
		t.Fatalf("config = (%v, %q), want an accepted config section", res.ConfigSuccess, res.ConfigError)
	}
	if res.RbacSuccess || !strings.Contains(res.RbacError, rbacRefusalText) {
		t.Fatalf("rbac = (%v, %q), want a refused rbac section with the Jenkins error", res.RbacSuccess, res.RbacError)
	}
	if data, err := os.ReadFile(filepath.Join(agent.miteDir, "last-jcasc.yaml")); err != nil || string(data) != "cfg-new" {
		t.Errorf("last-jcasc.yaml = (%q, %v), want cfg-new", data, err)
	}
	if _, err := os.Stat(filepath.Join(agent.miteDir, "last-rbac.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("last-rbac.yaml exists (err %v), want no record of a refused rbac.yaml", err)
	}
	if agent.hasLastAppliedHash() {
		t.Error("the convergence marker was written for a push with a refused section")
	}
}

// TestHandleDesiredStateWithoutRBACReportsRBACSuccess proves that a push
// with no rbac.yaml reports the rbac section as converged.
func TestHandleDesiredStateWithoutRBACReportsRBACSuccess(t *testing.T) {
	cascDir := t.TempDir()
	t.Setenv("CASC_JENKINS_CONFIG", cascDir)
	srv, _ := cascDocServer(t, cascDir)
	agent := newTestAgentWithJenkins(t, srv.URL)

	res := agent.handleDesiredState(context.Background(), &mitev1.DesiredStateCommand{
		DesiredStateHash:    "hash-no-rbac",
		JcascYaml:           "cfg-new",
		MiteJenkinsToken:    "eyJtest-token",
		MiteJenkinsTokenExp: 9999999999,
	})

	if !res.ConfigSuccess || !res.RbacSuccess || res.RbacError != "" {
		t.Fatalf("result = %+v, want accepted config and a converged rbac section", res)
	}
}
