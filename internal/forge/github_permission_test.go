package forge

import (
	"context"
	"net/http"
	"testing"
)

func TestGitHubCollaboratorPermission(t *testing.T) {
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/collaborators/alice/permission": jsonReply(`{"permission":"write","user":{"login":"alice"}}`),
		"GET /repos/o/r/collaborators/root/permission":  jsonReply(`{"permission":"admin"}`),
		"GET /repos/o/r/collaborators/carol/permission": textReply(http.StatusNotFound, `{"message":"Not Found"}`),
	})
	reader, ok := c.(PermissionReader)
	if !ok {
		t.Fatal("the GitHub client does not implement PermissionReader")
	}
	tests := map[string]string{"alice": "write", "root": "admin", "carol": "none"}
	for login, want := range tests {
		got, err := reader.CollaboratorPermission(context.Background(), testRepo, login)
		if err != nil {
			t.Fatalf("%s: %v", login, err)
		}
		if got != want {
			t.Fatalf("%s: permission = %q, want %q", login, got, want)
		}
	}
}

func TestGitHubCollaboratorPermissionEscapesLogin(t *testing.T) {
	c := githubClientFor(t, map[string]http.HandlerFunc{
		// The route key holds the decoded path. The escaped form is checked
		// on the wire, so a slash in a login cannot reach another route.
		"GET /repos/o/r/collaborators/a/b/permission": func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.EscapedPath(); got != "/repos/o/r/collaborators/a%2Fb/permission" {
				t.Errorf("escaped path = %q", got)
			}
			jsonReply(`{"permission":"read"}`)(w, r)
		},
	})
	got, err := c.(PermissionReader).CollaboratorPermission(context.Background(), testRepo, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if got != "read" {
		t.Fatalf("permission = %q, want read", got)
	}
}
