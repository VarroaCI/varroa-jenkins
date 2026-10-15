package api

import (
	"net/url"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// repoHost answers the lowercased host of a repository URL, without user
// information or a port. It reads the http, https, and ssh URL forms and
// the scp-like form user@host:path. It answers the empty string for any
// other form.
func repoHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		switch u.Scheme {
		case "http", "https", "ssh":
		default:
			return ""
		}
		return strings.ToLower(u.Hostname())
	}
	// The scp-like form has no scheme. Its host ends at the first colon,
	// and the path after the colon never holds a slash before it.
	at := strings.Index(raw, "@")
	colon := strings.Index(raw, ":")
	if at < 0 || colon < at || strings.Contains(raw[:colon], "/") {
		return ""
	}
	return strings.ToLower(raw[at+1 : colon])
}

// jobForgeHints maps a host to the forge that hosts it for one job. The
// last run fills the map first. The item declaration overwrites it, so a
// declared multibranch source wins over a run on the same host. A forge
// outside the known set is dropped.
func jobForgeHints(item *items.Item, lastRun *recordstore.Run) map[string]string {
	hints := map[string]string{}
	if lastRun != nil {
		o := runOriginOf(lastRun.Payload)
		if host := repoHost(o.SCMURL); host != "" && o.SCMForge != "" {
			hints[host] = o.SCMForge
		}
	}
	if item != nil && item.Kind == "multibranch" {
		if remote, forge := multibranchRemote(item.SourcesList); forge != "" {
			if host := repoHost(remote); host != "" {
				hints[host] = forge
			}
		}
	}
	return hints
}

// forgeForRepo answers the forge that hosts repo, using the host map from
// jobForgeHints. It answers the empty string when the host has no entry.
func forgeForRepo(hints map[string]string, repo string) string {
	host := repoHost(repo)
	if host == "" {
		return ""
	}
	return knownForge(hints[host])
}
