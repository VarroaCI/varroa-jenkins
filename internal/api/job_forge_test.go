package api

import (
	"encoding/json"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestRepoHost_Forms asserts each supported URL form yields its lowercased host.
func TestRepoHost_Forms(t *testing.T) {
	cases := map[string]string{
		"https://u:p@Gitea.Example.com:3000/acme/app.git": "gitea.example.com",
		"ssh://git@gitea.example.com:2222/acme/app":       "gitea.example.com",
		"git@gitea.example.com:acme/app.git":              "gitea.example.com",
		"":                                                "",
		"not a url":                                       "",
	}
	for raw, want := range cases {
		if got := repoHost(raw); got != want {
			t.Errorf("repoHost(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestJobForgeHints_RunForgeNamesItsHost asserts a run's forge maps its host,
// and an ssh form of the same repository resolves through forgeForRepo.
func TestJobForgeHints_RunForgeNamesItsHost(t *testing.T) {
	run := &recordstore.Run{Payload: json.RawMessage(
		`{"run.started":{"payload":{"scm_url":"https://gitea.example.com/acme/svc.git","scm_forge":"gitea"}}}`)}
	hints := jobForgeHints(nil, run)
	if got := forgeForRepo(hints, "https://gitea.example.com/acme/svc.git"); got != "gitea" {
		t.Errorf("https form forge = %q, want gitea", got)
	}
	if got := forgeForRepo(hints, "ssh://git@gitea.example.com:2222/acme/svc"); got != "gitea" {
		t.Errorf("ssh form forge = %q, want gitea", got)
	}
}

// TestJobForgeHints_DeclaredSourceWinsOverRun asserts a declared Gitea
// source overrides a run that names github for the same host.
func TestJobForgeHints_DeclaredSourceWinsOverRun(t *testing.T) {
	item := &items.Item{Kind: "multibranch", SourcesList: []items.BranchSourceEntry{
		{BranchSource: items.BranchSource{Source: items.BranchSourceConfig{Gitea: &items.GiteaSCMSource{
			ServerURL:  "https://gitea.example.com",
			RepoOwner:  "acme",
			Repository: "svc",
		}}}},
	}}
	run := &recordstore.Run{Payload: json.RawMessage(
		`{"run.started":{"payload":{"scm_url":"https://gitea.example.com/acme/svc","scm_forge":"github"}}}`)}
	hints := jobForgeHints(item, run)
	if got := forgeForRepo(hints, "https://gitea.example.com/acme/svc"); got != "gitea" {
		t.Errorf("forge = %q, want gitea from the declaration", got)
	}
}

// TestForgeForRepo_UnknownHostAnswersEmpty asserts that no hint, a
// non-multibranch item, and an unknown forge value all answer empty.
func TestForgeForRepo_UnknownHostAnswersEmpty(t *testing.T) {
	if got := forgeForRepo(jobForgeHints(nil, nil), "https://gitea.example.com/acme/svc"); got != "" {
		t.Errorf("nil run forge = %q, want empty", got)
	}
	pipeline := &items.Item{Kind: "pipeline"}
	if got := forgeForRepo(jobForgeHints(pipeline, nil), "https://gitea.example.com/acme/svc"); got != "" {
		t.Errorf("pipeline item forge = %q, want empty", got)
	}
	run := &recordstore.Run{Payload: json.RawMessage(
		`{"run.started":{"payload":{"scm_url":"https://bitbucket.example.com/acme/svc","scm_forge":"bitbucket"}}}`)}
	if got := forgeForRepo(jobForgeHints(nil, run), "https://bitbucket.example.com/acme/svc"); got != "" {
		t.Errorf("unknown forge value = %q, want empty", got)
	}
}
