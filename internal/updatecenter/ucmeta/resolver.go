// Package ucmeta resolves a Jenkins plugin (name, version) to its upstream SHA-256
// across one or more update-center metadata sources (the current weekly source plus
// any LTS-line "dynamic-stable-<version>" sources), caching each source with a TTL.
//
// It exists because the current weekly update-center metadata lists only one version
// per plugin, so an LTS-line profile that pins an aged version cannot be resolved from
// the weekly source alone. Consulting the matching dynamic-stable metadata recovers the
// exact version's checksum.
//
// The resolver is a leaf package: it is imported by the update-center server and must
// not import its parent (internal/updatecenter), so it defines its own minimal copy of
// the update-center JSON shape.
package ucmeta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/pluginver"
)

// Source is one upstream update-center metadata endpoint, e.g.
//
//	https://updates.jenkins.io/update-center.actual.json                        (weekly)
//	https://updates.jenkins.io/dynamic-stable-2.555.3/update-center.actual.json (LTS line)
type Source struct {
	URL string
}

// ErrVersionUnavailable is returned when no configured source lists the exact
// (name, version) requested. Callers map this onto an upstream 404.
var ErrVersionUnavailable = errors.New("ucmeta: plugin version not found in any metadata source")

// Resolver resolves (name, version) -> sha256 across a set of Sources, caching each
// source's parsed index with a TTL. Safe for concurrent use.
type Resolver struct {
	sources func() []Source
	ttl     time.Duration
	client  *http.Client
	now     func() time.Time
	logger  *slog.Logger

	mu    sync.Mutex
	cache map[string]*sourceEntry // keyed by Source.URL
}

// sourceEntry is a cached, parsed metadata source. index is nil when the last fetch
// failed; fetchedAt is stamped either way so a failing source is skipped (not retried)
// until the next TTL window. healthy records whether that last fetch succeeded, which
// is what lets a caller tell a provable negative ("every source answered and none
// listed it") from an unprovable one ("a source is down").
type sourceEntry struct {
	fetchedAt time.Time
	healthy   bool
	index     map[verKey]string     // (name,version) -> base64 sha256
	byName    map[string]PluginMeta // name -> the one version this source lists
}

type verKey struct{ name, version string }

// Dep is one entry of an upstream plugin's dependency array. Upstream calls the
// field "version"; it is a MINIMUM, not a pin.
type Dep struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Optional bool   `json:"optional"`
}

// PluginMeta is everything a metadata source says about one plugin. An update-center
// metadata document lists exactly one version per plugin.
type PluginMeta struct {
	Name         string
	Version      string
	SHA256       string // upstream base64, verbatim
	RequiredCore string
	Dependencies []Dep
}

// Outcome distinguishes a resolved lookup from the two ways a lookup can fail.
type Outcome int

const (
	// Resolved means some healthy source answered.
	Resolved Outcome = iota
	// NotListed means every source was healthy and none answered. This is a
	// PROVABLE negative — retrying will not change it.
	NotListed
	// SourcesDegraded means at least one source failed to fetch or parse and no
	// healthy source answered. The negative is UNPROVABLE, so it is retryable.
	SourcesDegraded
)

func (o Outcome) String() string {
	switch o {
	case Resolved:
		return "resolved"
	case NotListed:
		return "not-listed"
	case SourcesDegraded:
		return "sources-degraded"
	default:
		return "unknown"
	}
}

// Resolution is the outcome-typed result of a lookup.
type Resolution struct {
	Outcome Outcome
	// Meta is valid only when Outcome == Resolved.
	Meta PluginMeta
	// Best is the newest version any healthy source listed for the name, even when
	// it did not satisfy the request. It is what populates foundUpstream in a
	// rejection diff, so an "unreachable" result can say "upstream's newest is 3.1,
	// you need 9.0". Nil when no healthy source listed the plugin at all.
	Best *PluginMeta
}

// ucMetadata is the minimal shape of update-center.actual.json this package reads.
// sha256 is upstream's base64-encoded raw digest and is returned verbatim so callers
// can verify against it exactly as before.
type ucMetadata struct {
	Plugins map[string]ucPlugin `json:"plugins"`
}

type ucPlugin struct {
	Version      string  `json:"version"`
	SHA256       string  `json:"sha256"`
	RequiredCore string  `json:"requiredCore"`
	Dependencies []ucDep `json:"dependencies"`
}

type ucDep struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Optional bool   `json:"optional"`
}

// NewResolver builds a Resolver. sources is evaluated on every ResolveSHA256 call and
// is expected to be cheap (an in-memory list or a small local file read); this lets the
// update-center server pick up operator-supplied LTS sources without a restart. A nil
// client defaults to http.DefaultClient and a nil logger to slog.Default().
func NewResolver(sources func() []Source, ttl time.Duration, client *http.Client, logger *slog.Logger) *Resolver {
	if client == nil {
		client = http.DefaultClient
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Resolver{
		sources: sources,
		ttl:     ttl,
		client:  client,
		now:     time.Now,
		logger:  logger,
		cache:   make(map[string]*sourceEntry),
	}
}

// ResolveSHA256 returns the base64 SHA-256 for name@version from the first source (in
// the order sources() returns) whose index contains that exact (name, version). Any
// source whose cache is older than ttl is refreshed first; a source that fails to fetch
// or parse is skipped without failing the call. Returns ErrVersionUnavailable if no
// source lists the exact (name, version).
func (r *Resolver) ResolveSHA256(ctx context.Context, name, version string) (string, error) {
	key := verKey{name: name, version: version}
	for _, src := range r.sources() {
		entry := r.ensureFresh(ctx, src.URL)
		if entry.index == nil {
			continue // failed/skipped source
		}
		if sha, ok := entry.index[key]; ok {
			return sha, nil
		}
	}
	return "", ErrVersionUnavailable
}

// SourceURLs returns the URLs currently returned by sources(), for status reporting.
func (r *Resolver) SourceURLs() []string {
	srcs := r.sources()
	out := make([]string, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, s.URL)
	}
	return out
}

// ensureFresh returns the cached entry for url, refetching when it is missing or older
// than ttl. The lock is held across the fetch: fetches are infrequent, and a burst of
// concurrent requests during warmup shares a single fetch per source rather than
// stampeding the upstream. A failed fetch stamps fetchedAt with a nil index so the
// source is skipped and not retried until the next ttl window.
func (r *Resolver) ensureFresh(ctx context.Context, url string) *sourceEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	if entry, ok := r.cache[url]; ok && r.now().Sub(entry.fetchedAt) < r.ttl {
		return entry
	}

	entry := &sourceEntry{fetchedAt: r.now()}
	index, byName, err := r.fetch(ctx, url)
	if err != nil {
		r.logger.Warn("ucmeta: metadata source unavailable, skipping", "url", url, "error", err)
	} else {
		entry.healthy = true
		entry.index = index
		entry.byName = byName
	}
	r.cache[url] = entry
	return entry
}

func (r *Resolver) fetch(ctx context.Context, url string) (map[verKey]string, map[string]PluginMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("ucmeta: unexpected status %d from %s", resp.StatusCode, url)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	var meta ucMetadata
	if err := json.Unmarshal(body, &meta); err != nil {
		return nil, nil, fmt.Errorf("ucmeta: parse %s: %w", url, err)
	}
	index := make(map[verKey]string, len(meta.Plugins))
	byName := make(map[string]PluginMeta, len(meta.Plugins))
	for name, p := range meta.Plugins {
		if p.Version == "" || p.SHA256 == "" {
			continue
		}
		index[verKey{name: name, version: p.Version}] = p.SHA256
		pm := PluginMeta{
			Name:         name,
			Version:      p.Version,
			SHA256:       p.SHA256,
			RequiredCore: p.RequiredCore,
		}
		for _, d := range p.Dependencies {
			if d.Name == "" {
				continue
			}
			pm.Dependencies = append(pm.Dependencies, Dep(d))
		}
		byName[name] = pm
	}
	return index, byName, nil
}

// ---------------------------------------------------------------------------
// Outcome-typed lookups
// ---------------------------------------------------------------------------

// ResolveExact resolves name@version — the declared/pinned tier. It answers
// Resolved only when some healthy source lists that exact version.
func (r *Resolver) ResolveExact(ctx context.Context, name, version string) Resolution {
	return r.resolve(ctx, name, func(m PluginMeta) bool { return m.Version == version })
}

// ResolveSatisfying returns the highest version any healthy source lists for name
// that satisfies minVersion — the not-declared tier. Each source lists exactly one
// version per plugin, so this is a scan over sources, not over versions.
func (r *Resolver) ResolveSatisfying(ctx context.Context, name, minVersion string) Resolution {
	return r.resolve(ctx, name, func(m PluginMeta) bool { return pluginver.AtLeast(m.Version, minVersion) })
}

// resolve walks every source once, tracking three things: the best version that
// satisfies accept, the best version listed at all (for the rejection diff), and
// whether any source was unhealthy.
//
// Resolved always beats degraded: SourcesDegraded is returned only when no healthy
// source answered AND at least one source was unhealthy. Source health only ever
// decides between a provable negative and an unprovable one.
func (r *Resolver) resolve(ctx context.Context, name string, accept func(PluginMeta) bool) Resolution {
	var (
		res         Resolution
		found       bool
		anyDegraded bool
	)
	for _, src := range r.sources() {
		entry := r.ensureFresh(ctx, src.URL)
		if !entry.healthy {
			anyDegraded = true
			continue
		}
		m, ok := entry.byName[name]
		if !ok {
			continue
		}
		if res.Best == nil || pluginver.Compare(m.Version, res.Best.Version) > 0 {
			best := m
			res.Best = &best
		}
		if !accept(m) {
			continue
		}
		if !found || pluginver.Compare(m.Version, res.Meta.Version) > 0 {
			res.Meta = m
			found = true
		}
	}
	switch {
	case found:
		res.Outcome = Resolved
	case anyDegraded:
		res.Outcome = SourcesDegraded
	default:
		res.Outcome = NotListed
	}
	return res
}
