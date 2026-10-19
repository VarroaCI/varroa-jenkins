package forgeops

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

const testHost = "git.example.org"
const testRepoURL = "https://git.example.org/team/app"

// fakeSecrets serves tenant-credential records. A record is the Secret data
// with the synthetic _name key, and an annotation map per Secret name.
type fakeSecrets struct {
	records  []map[string][]byte
	ann      map[string]map[string]string
	selector string
}

func (f *fakeSecrets) ListSecrets(_ context.Context, _, labelSelector string) ([]map[string][]byte, error) {
	f.selector = labelSelector
	return f.records, nil
}

func (f *fakeSecrets) GetSecretAnnotations(_ context.Context, name, _ string) (map[string]string, error) {
	return f.ann[name], nil
}

// credSecret returns one usernamePassword credential that lists host.
func credSecret(name, id, token string) (map[string][]byte, map[string]string) {
	return map[string][]byte{"_name": []byte(name), "password": []byte(token)},
		map[string]string{
			v1alpha1.CredentialIDAnnotation:   id,
			v1alpha1.CredentialKindAnnotation: v1alpha1.CredentialKindUsernamePassword,
			"varroa.dev/allowed-hosts":        testHost,
		}
}

// addCred appends one credential Secret to the fake.
func (f *fakeSecrets) addCred(name, id, token string) {
	data, ann := credSecret(name, id, token)
	f.records = append(f.records, data)
	if f.ann == nil {
		f.ann = map[string]map[string]string{}
	}
	f.ann[name] = ann
}

// fakeClient is a forge.Client with canned answers. Read cuts a file at
// limit the way the real client does.
type fakeClient struct {
	info       forge.Info
	repoErr    error
	branches   map[string]string
	headErr    error
	resolveSHA string
	resolveErr error
	files      map[string]forge.Entry
	readErr    error
	// revs maps a revision name to its commit. When set, Resolve answers from
	// it and an unknown name is not found.
	revs map[string]string
	// issueErr is returned by CreateIssue. issueLabels records each call.
	issueErr    error
	issueLabels [][]string
	// bySHA holds the files of each commit. When set, Read answers from the
	// commit named in the call instead of files.
	bySHA map[string]map[string]forge.Entry
}

func (f *fakeClient) Repository(context.Context, forge.Repo) (forge.Info, error) {
	return f.info, f.repoErr
}

func (f *fakeClient) BranchHead(_ context.Context, _ forge.Repo, b string) (string, bool, error) {
	if f.headErr != nil {
		return "", false, f.headErr
	}
	sha, ok := f.branches[b]
	return sha, ok, nil
}

func (f *fakeClient) Resolve(_ context.Context, _ forge.Repo, rev string) (string, error) {
	if f.revs != nil {
		sha, ok := f.revs[rev]
		if !ok {
			return "", forge.ErrNotFound
		}
		return sha, nil
	}
	return f.resolveSHA, f.resolveErr
}

func (f *fakeClient) Read(_ context.Context, _ forge.Repo, sha, p string, limit int) (forge.Entry, error) {
	if f.readErr != nil {
		return forge.Entry{}, f.readErr
	}
	files := f.files
	if f.bySHA != nil {
		files = f.bySHA[sha]
	}
	e, ok := files[p]
	if !ok {
		return forge.Entry{}, forge.ErrNotFound
	}
	if limit <= 0 || limit > forge.MaxRead {
		limit = forge.MaxRead
	}
	if !e.Dir && len(e.Content) > limit {
		e.Content = e.Content[:limit]
	}
	return e, nil
}

func (f *fakeClient) CreateBranch(context.Context, forge.Repo, string, string) error { return nil }
func (f *fakeClient) PutFile(context.Context, forge.Repo, string, string, []byte, string) error {
	return nil
}
func (f *fakeClient) OpenPullRequest(context.Context, forge.Repo, string, string, string, string) (forge.PullRequest, error) {
	return forge.PullRequest{}, nil
}
func (f *fakeClient) DeleteBranch(context.Context, forge.Repo, string) error { return nil }

// CreateIssue answers the canned issue, or issueErr, and records the labels.
func (f *fakeClient) CreateIssue(_ context.Context, _ forge.Repo, _, _ string, labels []string) (forge.Issue, error) {
	f.issueLabels = append(f.issueLabels, labels)
	if f.issueErr != nil {
		return forge.Issue{}, f.issueErr
	}
	return forge.Issue{Number: 9, URL: "https://git.example.org/team/app/issues/9"}, nil
}

// fixture wires an Ops over a Team and a fake forge. clients maps a token
// value to the client that the token builds.
type fixture struct {
	ops     *Ops
	store   *crdstore.Fake
	secrets *fakeSecrets
	kind    forge.Kind
	detectE error
	clients map[forge.Token]forge.Client
	built   []forge.Token
	logBuf  *strings.Builder
}

func newFixture(t *testing.T, teamAgentNS string) *fixture {
	t.Helper()
	store := crdstore.NewFake()
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TeamSpec{
			Namespaces: []string{"default"},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{AgentNamespace: teamAgentNS},
		},
	}
	if teamAgentNS == "" {
		team.Spec.Pipelines = nil
	}
	crdstore.MustSeed(store, team)
	fx := &fixture{
		store:   store,
		secrets: &fakeSecrets{},
		kind:    forge.KindGitea,
		clients: map[forge.Token]forge.Client{},
		logBuf:  &strings.Builder{},
	}
	logger := slog.New(slog.NewJSONHandler(fx.logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ops := New(store, fx.secrets, nil, logger)
	ops.detect = func(context.Context, *http.Client, string) (forge.Kind, error) {
		return fx.kind, fx.detectE
	}
	ops.newClient = func(_ forge.Kind, _ string, tok forge.Token, _ *http.Client) forge.Client {
		fx.built = append(fx.built, tok)
		if c, ok := fx.clients[tok]; ok {
			return c
		}
		return &fakeClient{repoErr: forge.ErrNotFound}
	}
	fx.ops = ops
	return fx
}

// pushable returns a client for a repository that the credential may push to.
func pushable() *fakeClient {
	return &fakeClient{
		info:       forge.Info{DefaultBranch: "main", Push: true},
		branches:   map[string]string{"main": testSHA},
		files:      map[string]forge.Entry{},
		resolveSHA: testSHA,
	}
}

const testSHA = "0123456789abcdef0123456789abcdef01234567"

// checkReq and readReq build the requests of the tests.
func checkReq() forgeapi.CheckRequest {
	return forgeapi.CheckRequest{Team: "payments", Repo: testRepoURL}
}

func readReq(path string) forgeapi.ReadRequest {
	return forgeapi.ReadRequest{Team: "payments", Repo: testRepoURL, Path: path}
}
