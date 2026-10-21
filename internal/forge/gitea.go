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
	"strconv"
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

// SeesAllOwnerRepos reports whether the credential sees every repository
// that owner holds. It answers true for a site administrator, for the owner
// account itself, and for an organization owner. A lookup error answers
// false with the error.
func (g *giteaClient) SeesAllOwnerRepos(ctx context.Context, owner string) (bool, error) {
	var me struct {
		Login   string `json:"login"`
		IsAdmin bool   `json:"is_admin"`
	}
	if err := g.getJSON(ctx, "/user", nil, &me); err != nil {
		return false, err
	}
	if me.IsAdmin || (me.Login != "" && strings.EqualFold(me.Login, owner)) {
		return true, nil
	}
	if me.Login == "" {
		return false, errors.New("forge: the credential reports no login")
	}
	var perm struct {
		IsOwner bool `json:"is_owner"`
	}
	p := "/users/" + url.PathEscape(me.Login) + "/orgs/" + url.PathEscape(owner) + "/permissions"
	if err := g.getJSON(ctx, p, nil, &perm); err != nil {
		return false, err
	}
	return perm.IsOwner, nil
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

// CommitStatuses returns the latest status of each context on sha. Gitea
// names the state under "status" in the combined endpoint.
func (g *giteaClient) CommitStatuses(ctx context.Context, r Repo, sha string) ([]CommitStatus, error) {
	return readStatusPages(func(page int) ([]CommitStatus, int, error) {
		var out struct {
			TotalCount int `json:"total_count"`
			Statuses   []struct {
				Context     string `json:"context"`
				Status      string `json:"status"`
				State       string `json:"state"`
				TargetURL   string `json:"target_url"`
				Description string `json:"description"`
			} `json:"statuses"`
		}
		q := url.Values{"limit": {strconv.Itoa(statusPageSize)}, "page": {strconv.Itoa(page)}}
		if err := g.getJSON(ctx, repoPath(r)+"/commits/"+url.PathEscape(sha)+"/status", q, &out); err != nil {
			return nil, 0, err
		}
		items := make([]CommitStatus, 0, len(out.Statuses))
		for _, s := range out.Statuses {
			state := s.Status
			if state == "" {
				state = s.State
			}
			items = append(items, CommitStatus{Context: s.Context, State: state, TargetURL: s.TargetURL, Description: s.Description})
		}
		return items, out.TotalCount, nil
	})
}

// SetCommitStatus writes one status on sha.
func (g *giteaClient) SetCommitStatus(ctx context.Context, r Repo, sha string, s CommitStatus) error {
	in := map[string]string{
		"state":       s.State,
		"target_url":  s.TargetURL,
		"description": s.Description,
		"context":     s.Context,
	}
	return g.sendJSON(ctx, http.MethodPost, repoPath(r)+"/statuses/"+url.PathEscape(sha), in, nil)
}
