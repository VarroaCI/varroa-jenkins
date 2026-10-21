package forge

import (
	"context"
	"net/http"
	"testing"
)

func TestGiteaSeesAllOwnerRepos(t *testing.T) {
	tests := []struct {
		name   string
		routes map[string]http.HandlerFunc
		want   bool
		err    bool
	}{
		{
			name: "site administrator sees every repository",
			routes: map[string]http.HandlerFunc{
				"GET /user": jsonReply(`{"login":"bot","is_admin":true}`),
			},
			want: true,
		},
		{
			name: "owner account sees its own repositories",
			routes: map[string]http.HandlerFunc{
				"GET /user": jsonReply(`{"login":"o","is_admin":false}`),
			},
			want: true,
		},
		{
			name: "organization owner sees the organization repositories",
			routes: map[string]http.HandlerFunc{
				"GET /user":                         jsonReply(`{"login":"bot","is_admin":false}`),
				"GET /users/bot/orgs/o/permissions": jsonReply(`{"is_owner":true}`),
			},
			want: true,
		},
		{
			name: "organization member is not an owner",
			routes: map[string]http.HandlerFunc{
				"GET /user":                         jsonReply(`{"login":"bot","is_admin":false}`),
				"GET /users/bot/orgs/o/permissions": jsonReply(`{"is_owner":false}`),
			},
			want: false,
		},
		{
			name: "forbidden permissions lookup is not a scope",
			routes: map[string]http.HandlerFunc{
				"GET /user":                         jsonReply(`{"login":"bot","is_admin":false}`),
				"GET /users/bot/orgs/o/permissions": textReply(http.StatusForbidden, "x"),
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
