package forge

import (
	"context"
	"net/http"
	"testing"
)

// searchReply answers a repository search only when the query asks for the
// owner's private repositories, one at a time, and it fails the test otherwise.
func searchReply(t *testing.T, wantUID, body string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("uid") != wantUID || q.Get("is_private") != "true" ||
			q.Get("exclusive") != "true" || q.Get("limit") != "1" {
			t.Errorf("repository search query = %q, want uid=%s private exclusive limit=1", r.URL.RawQuery, wantUID)
		}
		jsonReply(body)(w, r)
	}
}

func TestGiteaSeesAllOwnerRepos(t *testing.T) {
	tests := []struct {
		name   string
		routes map[string]http.HandlerFunc
		want   bool
		err    bool
	}{
		{
			name: "site administrator with a private repository of the owner",
			routes: map[string]http.HandlerFunc{
				"GET /user":         jsonReply(`{"id":1,"login":"bot","is_admin":true}`),
				"GET /users/o":      jsonReply(`{"id":7}`),
				"GET /repos/search": searchReply(t, "7", `{"ok":true,"data":[{"private":true}]}`),
			},
			want: true,
		},
		{
			name: "site administrator with a public-only token lists no private repository",
			routes: map[string]http.HandlerFunc{
				"GET /user":         jsonReply(`{"id":1,"login":"bot","is_admin":true}`),
				"GET /users/o":      jsonReply(`{"id":7}`),
				"GET /repos/search": searchReply(t, "7", `{"ok":true,"data":[]}`),
			},
			want: false,
		},
		{
			name: "owner account with a private repository",
			routes: map[string]http.HandlerFunc{
				"GET /user":         jsonReply(`{"id":5,"login":"o","is_admin":false}`),
				"GET /repos/search": searchReply(t, "5", `{"ok":true,"data":[{"private":true}]}`),
			},
			want: true,
		},
		{
			name: "owner account without a visible private repository is not a scope",
			routes: map[string]http.HandlerFunc{
				"GET /user":         jsonReply(`{"id":5,"login":"o","is_admin":false}`),
				"GET /repos/search": searchReply(t, "5", `{"ok":true,"data":[]}`),
			},
			want: false,
		},
		{
			name: "search entry that is not private is not a scope",
			routes: map[string]http.HandlerFunc{
				"GET /user":         jsonReply(`{"id":5,"login":"o","is_admin":false}`),
				"GET /repos/search": searchReply(t, "5", `{"ok":true,"data":[{"private":false}]}`),
			},
			want: false,
		},
		{
			name: "organization owner with a private repository",
			routes: map[string]http.HandlerFunc{
				"GET /user":                         jsonReply(`{"id":3,"login":"bot","is_admin":false}`),
				"GET /users/bot/orgs/o/permissions": jsonReply(`{"is_owner":true}`),
				"GET /users/o":                      jsonReply(`{"id":9}`),
				"GET /repos/search":                 searchReply(t, "9", `{"ok":true,"data":[{"private":true}]}`),
			},
			want: true,
		},
		{
			name: "organization owner whose token lists no private repository",
			routes: map[string]http.HandlerFunc{
				"GET /user":                         jsonReply(`{"id":3,"login":"bot","is_admin":false}`),
				"GET /users/bot/orgs/o/permissions": jsonReply(`{"is_owner":true}`),
				"GET /users/o":                      jsonReply(`{"id":9}`),
				"GET /repos/search":                 searchReply(t, "9", `{"ok":true,"data":[]}`),
			},
			want: false,
		},
		{
			name: "organization member is not an owner and never searches",
			routes: map[string]http.HandlerFunc{
				"GET /user":                         jsonReply(`{"id":3,"login":"bot","is_admin":false}`),
				"GET /users/bot/orgs/o/permissions": jsonReply(`{"is_owner":false}`),
			},
			want: false,
		},
		{
			name: "forbidden permissions lookup is not a scope",
			routes: map[string]http.HandlerFunc{
				"GET /user":                         jsonReply(`{"id":3,"login":"bot","is_admin":false}`),
				"GET /users/bot/orgs/o/permissions": textReply(http.StatusForbidden, "x"),
			},
			err: true,
		},
		{
			name: "owner lookup that fails is an error",
			routes: map[string]http.HandlerFunc{
				"GET /user":    jsonReply(`{"id":1,"login":"bot","is_admin":true}`),
				"GET /users/o": textReply(http.StatusNotFound, "x"),
			},
			err: true,
		},
		{
			name: "forbidden repository search is an error",
			routes: map[string]http.HandlerFunc{
				"GET /user":         jsonReply(`{"id":5,"login":"o","is_admin":false}`),
				"GET /repos/search": textReply(http.StatusForbidden, "x"),
			},
			err: true,
		},
		{
			name: "server error on the user lookup is an error",
			routes: map[string]http.HandlerFunc{
				"GET /user": textReply(http.StatusInternalServerError, "x"),
			},
			err: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := giteaClientFor(t, tt.routes)
			scope, ok := c.(OwnerScope)
			if !ok {
				t.Fatal("gitea client does not implement OwnerScope")
			}
			got, err := scope.SeesAllOwnerRepos(context.Background(), "o")
			if (err != nil) != tt.err {
				t.Fatalf("err = %v, want error %v", err, tt.err)
			}
			if got != tt.want {
				t.Fatalf("SeesAllOwnerRepos = %v, want %v", got, tt.want)
			}
		})
	}
}
