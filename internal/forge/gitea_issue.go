package forge

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// maxLabelPage is the page size of one label listing. maxLabelPages bounds
// the listing so a large repository cannot loop the call.
const (
	maxLabelPage  = 50
	maxLabelPages = 20
)

// allNamesFound reports whether every name is already in the map.
func allNamesFound(byName map[string]int64, names []string) bool {
	for _, n := range names {
		if _, ok := byName[n]; !ok {
			return false
		}
	}
	return true
}

// CreateIssue opens an issue. Gitea takes label ids, so the names resolve
// first. A name the repository does not carry fails with a LabelError.
func (g *giteaClient) CreateIssue(ctx context.Context, r Repo, title, bodyText string, labels []string) (Issue, error) {
	in := map[string]any{"title": title, "body": bodyText}
	if len(labels) > 0 {
		ids, err := g.labelIDs(ctx, r, labels)
		if err != nil {
			return Issue{}, err
		}
		in["labels"] = ids
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

// labelIDs maps label names to the ids of the repository. It fails on the
// first name that the repository does not carry.
func (g *giteaClient) labelIDs(ctx context.Context, r Repo, names []string) ([]int64, error) {
	byName := make(map[string]int64)
	for page := 1; page <= maxLabelPages; page++ {
		var list []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}
		q := url.Values{"limit": {strconv.Itoa(maxLabelPage)}, "page": {strconv.Itoa(page)}}
		if err := g.getJSON(ctx, repoPath(r)+"/labels", q, &list); err != nil {
			return nil, err
		}
		for _, l := range list {
			byName[l.Name] = l.ID
		}
		if len(list) < maxLabelPage || allNamesFound(byName, names) {
			break
		}
	}
	ids := make([]int64, 0, len(names))
	for _, n := range names {
		id, ok := byName[n]
		if !ok {
			return nil, &LabelError{Name: n}
		}
		ids = append(ids, id)
	}
	return ids, nil
}
