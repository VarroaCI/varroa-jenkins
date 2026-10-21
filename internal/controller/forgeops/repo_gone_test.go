package forgeops

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/forge"
)

// scopedClient is a fake client that also answers the owner-scope lookup.
type scopedClient struct {
	*fakeClient
	all      bool
	scopeErr error
}

func (s *scopedClient) SeesAllOwnerRepos(context.Context, string) (bool, error) {
	return s.all, s.scopeErr
}

const goneRepoURL = "https://" + testHost + "/o/r"

func TestRepositoryGone(t *testing.T) {
	notFound := &scopedClient{fakeClient: &fakeClient{repoErr: forge.ErrNotFound}, all: true}
	tests := []struct {
		name    string
		kind    forge.Kind
		detect  error
		creds   []string // credential ids, each with its own client below
		clients map[string]forge.Client
		noCred  bool
		url     string
		want    bool
	}{
		{
			name:    "gitea 404 with owner scope is gone",
			kind:    forge.KindGitea,
			creds:   []string{"a"},
			clients: map[string]forge.Client{"a": notFound},
			want:    true,
		},
		{
			name:    "gitea 404 without owner scope is not gone",
			kind:    forge.KindGitea,
			creds:   []string{"a"},
			clients: map[string]forge.Client{"a": &scopedClient{fakeClient: &fakeClient{repoErr: forge.ErrNotFound}}},
			want:    false,
		},
		{
			name:    "a scope lookup error is not gone",
			kind:    forge.KindGitea,
			creds:   []string{"a"},
			clients: map[string]forge.Client{"a": &scopedClient{fakeClient: &fakeClient{repoErr: forge.ErrNotFound}, all: true, scopeErr: forge.ErrForbidden}},
			want:    false,
		},
		{
			name:    "a 404 without owner scope on one credential and a scoped 404 on the next is gone",
			kind:    forge.KindGitea,
			creds:   []string{"a", "b"},
			clients: map[string]forge.Client{"a": &scopedClient{fakeClient: &fakeClient{repoErr: forge.ErrNotFound}}, "b": notFound},
			want:    true,
		},
		{
			name:    "a present repository on one credential keeps it",
			kind:    forge.KindGitea,
			creds:   []string{"a", "b"},
			clients: map[string]forge.Client{"a": &scopedClient{fakeClient: &fakeClient{info: forge.Info{DefaultBranch: "main"}}}, "b": notFound},
			want:    false,
		},
		{
			name:    "github 404 is never gone",
			kind:    forge.KindGitHub,
			creds:   []string{"a"},
			clients: map[string]forge.Client{"a": notFound},
			want:    false,
		},
		{
			name:    "no tenant credential is not gone",
			kind:    forge.KindGitea,
			noCred:  true,
			clients: map[string]forge.Client{},
			want:    false,
		},
		{
			name:    "unsupported forge is not gone",
			detect:  forge.ErrUnsupported,
			creds:   []string{"a"},
			clients: map[string]forge.Client{"a": notFound},
			want:    false,
		},
		{
			name:    "refused URL is not gone",
			kind:    forge.KindGitea,
			creds:   []string{"a"},
			clients: map[string]forge.Client{"a": notFound},
			url:     "git@" + testHost + ":o/r.git",
			want:    false,
		},
		{
			name:    "unauthorized is not gone",
			kind:    forge.KindGitea,
			creds:   []string{"a"},
			clients: map[string]forge.Client{"a": &scopedClient{fakeClient: &fakeClient{repoErr: forge.ErrAuth}, all: true}},
			want:    false,
		},
		{
			name:    "forbidden is not gone",
			kind:    forge.KindGitea,
			creds:   []string{"a"},
			clients: map[string]forge.Client{"a": &scopedClient{fakeClient: &fakeClient{repoErr: forge.ErrForbidden}, all: true}},
			want:    false,
		},
		{
			name:    "rate limit is not gone",
			kind:    forge.KindGitea,
			creds:   []string{"a"},
			clients: map[string]forge.Client{"a": &scopedClient{fakeClient: &fakeClient{repoErr: &forge.RateLimitError{Status: http.StatusTooManyRequests}}, all: true}},
			want:    false,
		},
		{
			name:    "server error is not gone",
			kind:    forge.KindGitea,
			creds:   []string{"a"},
			clients: map[string]forge.Client{"a": &scopedClient{fakeClient: &fakeClient{repoErr: &forge.StatusError{Status: http.StatusBadGateway}}, all: true}},
			want:    false,
		},
		{
			name:    "network error is not gone",
			kind:    forge.KindGitea,
			creds:   []string{"a"},
			clients: map[string]forge.Client{"a": &scopedClient{fakeClient: &fakeClient{repoErr: errors.New("dial tcp: timeout")}, all: true}},
			want:    false,
		},
		{
			name:    "redirect is not gone",
			kind:    forge.KindGitea,
			creds:   []string{"a"},
			clients: map[string]forge.Client{"a": &scopedClient{fakeClient: &fakeClient{repoErr: forge.ErrRedirect}, all: true}},
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFixture(t, "agents-payments")
			if tt.noCred {
				fx.secrets = &fakeSecrets{}
			}
			for _, id := range tt.creds {
				fx.secrets.addCred("s"+id, id, "tok-"+id)
				fx.clients[forge.NewToken([]byte("tok-"+id))] = tt.clients[id]
			}
			fx.kind = tt.kind
			fx.detectE = tt.detect
			url := tt.url
			if url == "" {
				url = goneRepoURL
			}
			if got := fx.ops.RepositoryGone(context.Background(), "payments", url); got != tt.want {
				t.Fatalf("RepositoryGone = %v, want %v", got, tt.want)
			}
		})
	}
}
