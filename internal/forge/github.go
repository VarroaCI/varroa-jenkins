package forge

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
)

// githubRawHeader selects the raw file body of the contents endpoint.
var githubRawHeader = http.Header{"Accept": {"application/vnd.github.raw"}}

// githubClient implements Client for the GitHub REST API.
type githubClient struct {
	*httpClient
}

// Repository returns the default branch and the push permission.
func (g *githubClient) Repository(ctx context.Context, r Repo) (Info, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
		Permissions   struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if err := g.getJSON(ctx, repoPath(r), nil, &out); err != nil {
		return Info{}, err
	}
	return Info{DefaultBranch: out.DefaultBranch, Push: out.Permissions.Push}, nil
}

// BranchHead returns the head commit of a branch.
func (g *githubClient) BranchHead(ctx context.Context, r Repo, branch string) (string, bool, error) {
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := g.getJSON(ctx, repoPath(r)+"/git/ref/heads/"+escapePath(branch), nil, &out)
	if errors.Is(err, ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return out.Object.SHA, true, nil
}

// Resolve returns the full commit SHA of a revision.
func (g *githubClient) Resolve(ctx context.Context, r Repo, revision string) (string, error) {
	hdr := http.Header{"Accept": {"application/vnd.github.sha"}}
	resp, err := g.do(ctx, http.MethodGet, repoPath(r)+"/commits/"+escapePath(revision), nil, nil, hdr)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return "", err
	}
	sha := strings.ToLower(strings.TrimSpace(string(b)))
	if !isSHA(sha) {
		return "", errors.New("forge: revision did not resolve to a commit")
	}
	return sha, nil
}

// Read returns a directory listing or the first bytes of a file.
func (g *githubClient) Read(ctx context.Context, r Repo, sha, path string, limit int) (Entry, error) {
	return g.read(ctx, repoPath(r), sha, path, limit, "/contents/", githubRawHeader)
}

// CreateBranch creates a branch at fromSHA.
func (g *githubClient) CreateBranch(ctx context.Context, r Repo, branch, fromSHA string) error {
	in := map[string]string{"ref": "refs/heads/" + branch, "sha": fromSHA}
	return g.sendJSON(ctx, http.MethodPost, repoPath(r)+"/git/refs", in, nil)
}

// PutFile creates or replaces one file on branch in one commit.
func (g *githubClient) PutFile(ctx context.Context, r Repo, branch, path string, content []byte, message string) error {
	repo := repoPath(r)
	sha, err := g.existingBlob(ctx, repo, branch, path)
	if err != nil {
		return err
	}
	in := map[string]string{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
		"branch":  branch,
	}
	if sha != "" {
		in["sha"] = sha
	}
	return g.sendJSON(ctx, http.MethodPut, repo+"/contents/"+escapePath(path), in, nil)
}

// OpenPullRequest opens a pull request from head into base.
func (g *githubClient) OpenPullRequest(ctx context.Context, r Repo, base, head, title, bodyText string) (PullRequest, error) {
	in := map[string]string{"title": title, "body": bodyText, "head": head, "base": base}
	var out struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	if err := g.sendJSON(ctx, http.MethodPost, repoPath(r)+"/pulls", in, &out); err != nil {
		return PullRequest{}, err
	}
	return PullRequest{URL: out.HTMLURL, Number: out.Number}, nil
}

// CreateIssue opens an issue. The labels are sent by name, which GitHub
// accepts for a label that the repository carries.
func (g *githubClient) CreateIssue(ctx context.Context, r Repo, title, bodyText string, labels []string) (Issue, error) {
	in := map[string]any{"title": title, "body": bodyText}
	if len(labels) > 0 {
		in["labels"] = labels
	}
	var out struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	if err := g.sendJSON(ctx, http.MethodPost, repoPath(r)+"/issues", in, &out); err != nil {
		return Issue{}, err
	}
	return Issue{Number: out.Number, URL: out.HTMLURL}, nil
}

// DeleteBranch deletes a branch.
func (g *githubClient) DeleteBranch(ctx context.Context, r Repo, branch string) error {
	return g.sendJSON(ctx, http.MethodDelete, repoPath(r)+"/git/refs/heads/"+escapePath(branch), nil, nil)
}
