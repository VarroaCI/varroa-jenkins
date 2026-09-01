package pluginresolve

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/hpi"
)

// Fetcher retrieves a plugin's .hpi bytes at a pinned version. ctx bounds and
// can cancel the underlying request; a caller running on a shared reconcile
// loop must be able to abort a stalled download.
type Fetcher func(ctx context.Context, name, version string) ([]byte, error)

// hpiDownloadTimeout bounds a single HTTPFetcher request. An .hpi is a binary
// artifact, potentially several MB, so it gets more headroom than a metadata
// fetch (see candidateHTTPTimeout in internal/controller) — but it is its own
// constant rather than a reuse of that one, so the two stay independently
// tunable. Two minutes comfortably fits inside the reconciler's five-minute
// requeue backoff, so a single hung download still lets the candidate retry
// on schedule rather than consuming the whole backoff window.
const hpiDownloadTimeout = 2 * time.Minute

// BootstrapEntry mirrors pluginlock.BootstrapEntry. It is declared here rather
// than imported so a ResolveClosure caller stays a pure text-in/text-out
// tool, with no dependency on the embedded lock format a downstream consumer
// generates.
type BootstrapEntry struct {
	ArtifactID string
	Version    string
	Mins       []string
}

// HTTPFetcher returns a Fetcher that downloads a plugin's .hpi from base,
// following updates.jenkins.io's URL layout. The returned Fetcher shares one
// client, bounded by hpiDownloadTimeout, across every call; ctx additionally
// lets a caller abort a request in flight.
func HTTPFetcher(base string) Fetcher {
	return httpFetcherWithClient(base, &http.Client{Timeout: hpiDownloadTimeout})
}

// httpFetcherWithClient is HTTPFetcher's implementation with the client
// factored out, so a test can inject a client with a short timeout instead of
// waiting out hpiDownloadTimeout to prove a stall is bounded.
func httpFetcherWithClient(base string, client *http.Client) Fetcher {
	base = strings.TrimRight(base, "/")
	return func(ctx context.Context, name, version string) ([]byte, error) {
		url := fmt.Sprintf("%s/download/plugins/%s/%s/%s.hpi", base, name, version, name)
		// #nosec G107 -- base is an operator-supplied flag/config value, not
		// end-user input; the request is bounded by hpiDownloadTimeout and ctx,
		// whether run from the bootstrapdeps CLI or the candidate reconciler.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("build request %s: %w", url, err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", url, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}
}

// normalizeRootVersion truncates at the first space. A snapshot build stamps
// `1.0-SNAPSHOT (private-<timestamp>-<user>)` into Plugin-Version, and that
// suffix must never reach a committed lock.
func normalizeRootVersion(v string) string {
	if i := strings.IndexByte(v, ' '); i >= 0 {
		return v[:i]
	}
	return v
}

// ResolveClosure walks a root plugin's MANDATORY dependency closure.
//
// Optional dependencies are neither required nor traversed: Jenkins itself
// tolerates their absence, so requiring one would assert something Jenkins does
// not. Every member must be present in the resolved set; the ROOT is exempt,
// because it is baked into the image and is not, and never will be, a lock
// member. Presence is the whole assertion — no version comparison happens here.
//
// The visited set is keyed by name, so a diamond fetches each HPI at most once.
func ResolveClosure(ctx context.Context, rootHPI []byte, resolved map[string]string, fetch Fetcher) ([]BootstrapEntry, error) {
	rootMF, err := hpi.ParseHPIBytes(rootHPI)
	if err != nil {
		return nil, fmt.Errorf("parse root HPI: %w", err)
	}

	root := BootstrapEntry{
		ArtifactID: rootMF.ShortName,
		Version:    normalizeRootVersion(rootMF.Version),
	}

	// parent tracks how each member was reached, so a failure can print the
	// full chain from the root and the operator sees WHY a seemingly unrelated
	// plugin is required.
	parent := map[string]string{}
	mins := map[string]map[string]struct{}{}
	order := []string{}
	visited := map[string]bool{rootMF.ShortName: true}

	type queued struct {
		name string
		deps []hpi.Dependency
	}
	queue := []queued{{name: rootMF.ShortName, deps: rootMF.Dependencies}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		for _, d := range cur.deps {
			if d.Optional {
				continue
			}
			if _, ok := parent[d.Name]; !ok {
				parent[d.Name] = cur.name
			}
			pin, present := resolved[d.Name]
			if !present {
				return nil, fmt.Errorf(
					"mandatory dependency %q is missing from the resolved plugin set; required via %s",
					d.Name, renderChain(parent, rootMF.ShortName, d.Name))
			}
			if mins[d.Name] == nil {
				mins[d.Name] = map[string]struct{}{}
			}
			mins[d.Name][d.Min] = struct{}{}

			if visited[d.Name] {
				continue
			}
			visited[d.Name] = true
			order = append(order, d.Name)

			b, err := fetch(ctx, d.Name, pin)
			if err != nil {
				return nil, fmt.Errorf("fetch %s@%s (required via %s): %w",
					d.Name, pin, renderChain(parent, rootMF.ShortName, d.Name), err)
			}
			mf, err := hpi.ParseHPIBytes(b)
			if err != nil {
				return nil, fmt.Errorf("parse %s@%s: %w", d.Name, pin, err)
			}
			queue = append(queue, queued{name: d.Name, deps: mf.Dependencies})
		}
	}

	out := make([]BootstrapEntry, 0, len(order)+1)
	out = append(out, root)
	for _, name := range order {
		declared := make([]string, 0, len(mins[name]))
		for m := range mins[name] {
			declared = append(declared, m)
		}
		// De-duplicated by exact string equality and sorted lexicographically
		// for byte-stability across runs. NOT reduced to a greatest minimum:
		// picking one would require comparing versions.
		sort.Strings(declared)
		out = append(out, BootstrapEntry{
			ArtifactID: name,
			Version:    resolved[name],
			Mins:       declared,
		})
	}
	return out, nil
}

// renderChain renders "root → … → target" using the recorded parent links.
func renderChain(parent map[string]string, root, target string) string {
	chain := []string{target}
	seen := map[string]bool{target: true}
	for cur := target; cur != root; {
		p, ok := parent[cur]
		if !ok || seen[p] {
			break
		}
		chain = append(chain, p)
		seen[p] = true
		cur = p
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return strings.Join(chain, " → ")
}

// AssertBootstrapClosure re-verifies a resolved Closure's presence against
// rootHPI's mandatory dependency closure. It mirrors ResolveClosure's
// presence-only assertion: a bootstrap member absent from closure fails the
// assert even when every other member is pinned.
func AssertBootstrapClosure(ctx context.Context, rootHPI []byte, closure Closure, fetch Fetcher) error {
	resolved := make(map[string]string, len(closure.Plugins))
	for _, p := range closure.Plugins {
		resolved[p.ArtifactID] = p.Version
	}
	_, err := ResolveClosure(ctx, rootHPI, resolved, fetch)
	return err
}
