// Package forge is the operator-only client for a repository forge. It reads
// and writes repositories on GitHub and on Gitea-API forges. It imports the
// standard library only. The wire types that carry its results to other
// tiers live in forge/forgeapi.
package forge

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// Kind names a forge API family.
type Kind string

// The forge API families this package speaks.
const (
	KindGitHub Kind = "github"
	KindGitea  Kind = "gitea"
)

// MaxRead is the largest file body that one Read call returns.
const MaxRead = 256 << 10

// Repo names a repository by its owner and its name.
type Repo struct {
	Owner string
	Name  string
}

// Info holds the repository facts that the write path needs.
type Info struct {
	DefaultBranch string
	Push          bool
}

// Child is one entry of a directory listing. Type is "file" or "dir".
type Child struct {
	Name string
	Type string
}

// Entry is the result of Read. A directory fills Children. A file fills
// Size and Content, where Content holds at most the requested limit.
type Entry struct {
	Revision string
	Dir      bool
	// Other marks a symlink or a submodule. It has no content and no children.
	Other    bool
	Children []Child
	Size     int64
	Content  []byte
}

// PullRequest identifies an opened pull request.
type PullRequest struct {
	URL    string
	Number int
}

// Sentinel errors returned by every Client method.
var (
	ErrNotFound    = errors.New("forge: not found")
	ErrAuth        = errors.New("forge: authentication failed")
	ErrForbidden   = errors.New("forge: forbidden")
	ErrConflict    = errors.New("forge: conflict")
	ErrUnsupported = errors.New("forge: unsupported forge")
	ErrRedirect    = errors.New("forge: redirect refused")
	// ErrListingTruncated reports a directory whose listing reached the
	// forge's entry cap. The entries past the cap are not readable here.
	ErrListingTruncated = errors.New("forge: directory listing is truncated")
)

// Client reads and writes one forge. Each method takes the repository and
// returns an error that never carries a response body or a credential.
type Client interface {
	// Repository returns the default branch and the push permission.
	Repository(ctx context.Context, r Repo) (Info, error)
	// BranchHead returns the head commit of a branch. ok is false when the
	// branch does not exist.
	BranchHead(ctx context.Context, r Repo, branch string) (sha string, ok bool, err error)
	// Resolve turns a branch name or commit SHA into a full commit SHA.
	Resolve(ctx context.Context, r Repo, revision string) (sha string, err error)
	// Read returns a directory listing or the first limit bytes of a file.
	// A limit of zero or less, or above MaxRead, means MaxRead.
	Read(ctx context.Context, r Repo, sha, path string, limit int) (Entry, error)
	// CreateBranch creates branch at the commit fromSHA.
	CreateBranch(ctx context.Context, r Repo, branch, fromSHA string) error
	// PutFile creates the file, or replaces it when it exists, in one commit.
	PutFile(ctx context.Context, r Repo, branch, path string, content []byte, message string) error
	// OpenPullRequest opens a pull request from head into base.
	OpenPullRequest(ctx context.Context, r Repo, base, head, title, body string) (PullRequest, error)
	// DeleteBranch deletes a branch.
	DeleteBranch(ctx context.Context, r Repo, branch string) error
}

// StatusError reports an HTTP status that has no sentinel error. It never
// holds the response body.
type StatusError struct {
	Status int
}

// Error returns the status code only.
func (e *StatusError) Error() string {
	return "forge: HTTP " + strconv.Itoa(e.Status)
}

// Detect names the forge API that answers on host. The host github.com
// needs no request. Any other host must answer the Gitea version route.
func Detect(ctx context.Context, h *http.Client, host string) (Kind, error) {
	if host == "github.com" {
		return KindGitHub, nil
	}
	return detectGitea(ctx, h, host)
}

// New returns the client for kind on host. The token authenticates every
// request, and h carries the transport rules of NewHTTPClient.
func New(kind Kind, host string, token Token, h *http.Client) Client {
	switch kind {
	case KindGitHub:
		return newClient(KindGitHub, "https://api.github.com", token, h)
	case KindGitea:
		return newClient(KindGitea, "https://"+hostURL(host)+"/api/v1", token, h)
	}
	return nil
}

// hostURL returns host in the form a URL takes. An IPv6 literal needs brackets,
// or its colons read as a port.
func hostURL(host string) string {
	if ip := net.ParseIP(host); ip != nil && strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// newClient builds the client for kind over apiBase. Tests use it to point a
// client at an httptest server.
func newClient(kind Kind, apiBase string, token Token, h *http.Client) Client {
	base := &httpClient{base: apiBase, token: token, h: h}
	switch kind {
	case KindGitHub:
		base.auth = "Bearer "
		base.hdr = http.Header{
			"Accept":               []string{"application/vnd.github+json"},
			"X-Github-Api-Version": []string{"2022-11-28"},
		}
		return &githubClient{httpClient: base}
	case KindGitea:
		base.auth = "token "
		return &giteaClient{httpClient: base}
	}
	return nil
}
