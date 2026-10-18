package forge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// giteaClient implements Client for a Gitea-API forge.
type giteaClient struct {
	*httpClient
}

// detectGitea probes the Gitea version route on host. A JSON body with a
// version string answers KindGitea. Any other answer is ErrUnsupported.
func detectGitea(ctx context.Context, h *http.Client, host string) (Kind, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+hostURL(host)+"/api/v1/version", nil)
	if err != nil {
		return "", fmt.Errorf("forge: build detect request: %w", err)
	}
	resp, err := h.Do(req)
	if err != nil {
		return "", fmt.Errorf("forge: detect: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", ErrUnsupported
	}
	var out struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&out); err != nil || out.Version == "" {
		return "", ErrUnsupported
	}
	return KindGitea, nil
}

// giteaRawPrefix is the raw file endpoint of the Gitea API.
const giteaRawPrefix = "/raw/"

// Repository returns the default branch and the push permission.
func (g *giteaClient) Repository(ctx context.Context, r Repo) (Info, error) {
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
func (g *giteaClient) BranchHead(ctx context.Context, r Repo, branch string) (string, bool, error) {
	var out struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	err := g.getJSON(ctx, repoPath(r)+"/branches/"+escapePath(branch), nil, &out)
	if errors.Is(err, ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return out.Commit.ID, true, nil
}

// Resolve returns the full commit SHA of a revision.
func (g *giteaClient) Resolve(ctx context.Context, r Repo, revision string) (string, error) {
	q := url.Values{"stat": {"false"}, "verification": {"false"}, "files": {"false"}}
	var out struct {
		SHA string `json:"sha"`
	}
	if err := g.getJSON(ctx, repoPath(r)+"/git/commits/"+escapePath(revision), q, &out); err != nil {
		return "", err
	}
	sha := strings.ToLower(out.SHA)
	if !isSHA(sha) {
		return "", errors.New("forge: revision did not resolve to a commit")
	}
	return sha, nil
}

// Read returns a directory listing or the first bytes of a file.
func (g *giteaClient) Read(ctx context.Context, r Repo, sha, path string, limit int) (Entry, error) {
	return g.read(ctx, repoPath(r), sha, path, limit, giteaRawPrefix, nil)
}

// CreateBranch creates a branch at fromSHA.
func (g *giteaClient) CreateBranch(ctx context.Context, r Repo, branch, fromSHA string) error {
	in := map[string]string{"new_branch_name": branch, "old_ref_name": fromSHA}
	return g.sendJSON(ctx, http.MethodPost, repoPath(r)+"/branches", in, nil)
}

// PutFile creates or replaces one file on branch in one commit.
func (g *giteaClient) PutFile(ctx context.Context, r Repo, branch, path string, content []byte, message string) error {
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
	method := http.MethodPost
	if sha != "" {
		in["sha"] = sha
		method = http.MethodPut
	}
	return g.sendJSON(ctx, method, repo+"/contents/"+escapePath(path), in, nil)
}

// OpenPullRequest opens a pull request from head into base.
func (g *giteaClient) OpenPullRequest(ctx context.Context, r Repo, base, head, title, bodyText string) (PullRequest, error) {
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

// DeleteBranch deletes a branch.
func (g *giteaClient) DeleteBranch(ctx context.Context, r Repo, branch string) error {
	return g.sendJSON(ctx, http.MethodDelete, repoPath(r)+"/branches/"+escapePath(branch), nil, nil)
}
