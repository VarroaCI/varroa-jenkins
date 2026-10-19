package forge

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// TestCreateIssue_GitHub checks that the issue is posted with its labels and
// that the issue number and URL come back.
func TestCreateIssue_GitHub(t *testing.T) {
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"POST /repos/o/r/issues": jsonReply(`{"html_url":"https://example.com/o/r/issues/4","number":4}`),
	})
	issue, err := c.CreateIssue(context.Background(), testRepo, "t", "b", []string{"ops"})
	if err != nil || issue.Number != 4 || issue.URL == "" {
		t.Fatalf("issue %+v err %v", issue, err)
	}
}

// TestCreateIssue_Gitea checks that the label names resolve to ids before the
// issue is posted.
func TestCreateIssue_Gitea(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/labels":  jsonReply(`[{"id":11,"name":"ops"},{"id":12,"name":"bug"}]`),
		"POST /repos/o/r/issues": jsonReply(`{"html_url":"https://example.com/o/r/issues/5","number":5}`),
	})
	issue, err := c.CreateIssue(context.Background(), testRepo, "t", "b", []string{"ops"})
	if err != nil || issue.Number != 5 {
		t.Fatalf("issue %+v err %v", issue, err)
	}
}

// TestCreateIssue_GiteaMissingLabel refuses a label that the repository does
// not carry, with an error that matches ErrNoSuchLabel and names the label.
func TestCreateIssue_GiteaMissingLabel(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/labels": jsonReply(`[{"id":11,"name":"ops"}]`),
	})
	_, err := c.CreateIssue(context.Background(), testRepo, "t", "b", []string{"nope"})
	if !errors.Is(err, ErrNoSuchLabel) {
		t.Fatalf("err = %v, want ErrNoSuchLabel", err)
	}
	var le *LabelError
	if !errors.As(err, &le) || le.Name != "nope" {
		t.Fatalf("label error = %+v", le)
	}
}

// TestNew_GitHubEnterpriseBase checks that a GitHub host other than github.com
// takes the /api/v3 base.
func TestNew_GitHubEnterpriseBase(t *testing.T) {
	c := New(KindGitHub, "git.example.org", NewToken([]byte("t")), http.DefaultClient)
	gh, ok := c.(*githubClient)
	if !ok {
		t.Fatalf("client type %T", c)
	}
	if got := gh.base; got != "https://git.example.org/api/v3" {
		t.Fatalf("base = %s", got)
	}
}
