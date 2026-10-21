// Package ucmeta resolves a Jenkins plugin (name, version) to its upstream SHA-256
// across one or more update-center metadata sources (the current weekly source plus
// any LTS-line "dynamic-stable-<version>" sources), caching each source with a TTL.
//
// It exists because the current weekly update-center metadata lists only one version
// per plugin, so an LTS-line profile that pins an aged version cannot be resolved from
// the weekly source alone. Consulting the matching dynamic-stable metadata recovers the
// checksum while the pin is still that line's newest.
//
// Every metadata source shares that one-version-per-plugin property, so once a pin ages
// past current on its own line, no source lists it and metadata alone can never resolve
// it again. That is permanent, not transient. For those versions the resolver falls back
// to the Jenkins Maven repository, which is addressed by coordinate rather than recency
// and publishes a .sha256 sidecar for every version it has ever released (see
// archive.go).
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
	"strings"
	"sync"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/pluginver"
)

// metadataFetchTimeout bounds one metadata fetch. The fetch does not use the caller's
// deadline. The HTTP client's own timeout can end the fetch earlier.
const metadataFetchTimeout = 2 * time.Minute

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

	// archive recovers checksums for versions no metadata source lists any more.
	// Nil disables the fallback.
	archive *archiveResolver

	// fetchTimeout bounds one background metadata fetch. Tests lower it directly.
	fetchTimeout time.Duration

	mu    sync.Mutex
	cache map[string]*sourceEntry // keyed by Source.URL
	// inflight holds one channel per source whose fetch is running. The channel
	// closes when the fetch has filled the cache. Guarded by mu.
	inflight map[string]chan struct{}
}

// sourceEntry is a cached, parsed metadata source. index is nil when the last fetch
// failed; fetchedAt is stamped either way so a failing source is skipped (not retried)
// until the next TTL window. healthy records whether that last fetch succeeded, which
// is what lets a caller tell a provable negative ("every source answered and none
// listed it") from an unprovable one ("a source is down").
type sourceEntry struct {
	fetchedAt time.Time
	healthy   bool
	// stale marks an entry past its ttl that a caller used while a refresh runs. A
	// miss in a stale entry is not proof, because the refresh may list the name.
	stale  bool
	index  map[verKey]string     // (name,version) -> base64 sha256
	byName map[string]PluginMeta // name -> the one version this source lists
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
	// GAV is upstream's "groupId:artifactId:version" coordinate. Only the groupId
	// part is used, and only that part is stable across versions — it is what lets
	// the archive fallback locate an aged version's checksum sidecar in the Jenkins
	// Maven repository, whose layout is group-path addressed.
	GAV string
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
	GAV          string  `json:"gav"`
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
		archive: newArchiveResolver(DefaultArchiveBaseURL, client),
		cache:   make(map[string]*sourceEntry),

		fetchTimeout: metadataFetchTimeout,
		inflight:     make(map[string]chan struct{}),
	}
}

// SetArchiveBaseURL points the archive fallback at a different Maven repository, or
// disables it entirely when baseURL is empty. Intended for tests and for air-gapped
// deployments that mirror the repository internally.
func (r *Resolver) SetArchiveBaseURL(baseURL string) {
	if baseURL == "" {
		r.archive = nil
		return
	}
	r.archive = newArchiveResolver(baseURL, r.client)
}

// Via names the path that supplied a resolved checksum.
type Via string

const (
	// ViaMetadata means a metadata source listed the exact version.
	ViaMetadata Via = "metadata"
	// ViaArchive means the Maven repository sidecar supplied the checksum.
	ViaArchive Via = "archive"
)

// Availability is the answer to "can this server serve name@version?". It is the one
// shape that both pull-through serving and the servability endpoint read.
type Availability struct {
	Outcome Outcome
	// SHA256 is the upstream base64 digest. Set when Outcome == Resolved.
	SHA256 string
	// Via names the path that supplied SHA256. Set when Outcome == Resolved.
	Via Via
	// Meta carries the metadata entry, including its dependencies. Set only when
	// Via == ViaMetadata.
	Meta *PluginMeta
	// ArchiveDisabled is set on NotListed when metadata lists the plugin at another
	// version and the archive fallback is off, so a sidecar could have answered.
	ArchiveDisabled bool
	// Version is the concrete version that resolved. It equals the request except
	// for a latest request. Set when Outcome == Resolved.
	Version string
}

// IsLatest reports whether version asks for the newest version of a plugin. An
// empty version and the word latest, in any case, both ask for it.
func IsLatest(version string) bool {
	v := strings.TrimSpace(version)
	return v == "" || strings.EqualFold(v, "latest")
}

// ResolveAvailable resolves name@version through every path this resolver has: the
// metadata sources in order, then the archive sidecar. It is the single entry point
// for serving decisions; ResolveSHA256 delegates to it.
//
// A latest request (see IsLatest) resolves to the highest version any healthy
// metadata source lists. It never consults the archive, because the archive
// cannot enumerate versions. Availability.Version carries the concrete version.
func (r *Resolver) ResolveAvailable(ctx context.Context, name, version string) Availability {
	if IsLatest(version) {
		return r.resolveLatest(ctx, name)
	}
	key := verKey{name: name, version: version}
	var (
		gav         string
		anyDegraded bool
	)
	for _, src := range r.sources() {
		entry := r.ensureFresh(ctx, src.URL)
		if entry.index == nil {
			anyDegraded = true
			continue // failed/skipped source
		}
		if sha, ok := entry.index[key]; ok {
			pm := entry.byName[name]
			return Availability{Outcome: Resolved, SHA256: sha, Via: ViaMetadata, Meta: &pm, Version: version}
		}
		if entry.stale {
			anyDegraded = true // a refresh in flight may list the version
		}
		// Wrong version, but the listing still supplies a groupId, which is stable
		// across a plugin's versions and is all the archive fallback needs.
		if gav == "" {
			if pm, ok := entry.byName[name]; ok {
				gav = pm.GAV
			}
		}
	}

	// No source lists this exact version. That is the steady state for any pin that
	// has aged past current, not a transient gap, so fall back to the archive rather
	// than failing a download we can otherwise fetch and verify.
	if gav == "" {
		if anyDegraded {
			return Availability{Outcome: SourcesDegraded}
		}
		return Availability{Outcome: NotListed}
	}
	if r.archive == nil {
		// An unhealthy source could have listed the pin, so the negative is unproven.
		if anyDegraded {
			return Availability{Outcome: SourcesDegraded}
		}
		return Availability{Outcome: NotListed, ArchiveDisabled: true}
	}
	sha, err := r.archive.resolve(ctx, gav, name, version)
	switch {
	case err == nil:
		r.logger.Debug("resolved checksum from archive fallback",
			"name", name, "version", version)
		return Availability{Outcome: Resolved, SHA256: sha, Via: ViaArchive, Version: version}
	case errors.Is(err, errArchiveNotFound):
		return Availability{Outcome: NotListed}
	default:
		r.logger.Debug("archive fallback did not resolve checksum",
			"name", name, "version", version, "error", err)
		return Availability{Outcome: SourcesDegraded}
	}
}

// resolveLatest answers a latest request. It takes the highest version any healthy
// source lists for name and never consults the archive.
func (r *Resolver) resolveLatest(ctx context.Context, name string) Availability {
	res := r.resolve(ctx, name, func(PluginMeta) bool { return true })
	if res.Outcome != Resolved {
		return Availability{Outcome: res.Outcome}
	}
	m := res.Meta
	return Availability{Outcome: Resolved, SHA256: m.SHA256, Via: ViaMetadata, Meta: &m, Version: m.Version}
}

// ResolveLatestForCore answers a latest request for a controller on core. It takes the
// highest version a healthy source lists whose requiredCore is at or below core. An
// empty core or an empty requiredCore accepts the entry. It never consults the archive.
func (r *Resolver) ResolveLatestForCore(ctx context.Context, name, core string) Availability {
	res := r.resolve(ctx, name, func(m PluginMeta) bool { return CoreCompatible(m.RequiredCore, core) })
	if res.Outcome != Resolved {
		return Availability{Outcome: res.Outcome}
	}
	m := res.Meta
	return Availability{Outcome: Resolved, SHA256: m.SHA256, Via: ViaMetadata, Meta: &m, Version: m.Version}
}

// CoreCompatible reports whether a plugin that needs requiredCore runs on core.
func CoreCompatible(requiredCore, core string) bool {
	return core == "" || requiredCore == "" || pluginver.AtLeast(core, requiredCore)
}

// ResolveSHA256 returns the base64 SHA-256 for name@version from any path
// ResolveAvailable consults. Returns ErrVersionUnavailable when the call does not
// resolve, whether the answer is a provable negative or an unprovable one.
func (r *Resolver) ResolveSHA256(ctx context.Context, name, version string) (string, error) {
	a := r.ResolveAvailable(ctx, name, version)
	if a.Outcome != Resolved {
		return "", ErrVersionUnavailable
	}
	return a.SHA256, nil
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
// than ttl. The fetch does not use the caller's deadline: it runs in the background
// under fetchTimeout and fills the cache, so a later request uses its result. A caller
// whose deadline expires first gets the stale entry when one exists. Otherwise it gets
// an unhealthy entry that is not stored, so the degraded answer is not cached for the
// ttl. At most one fetch runs per source at a time. A failed fetch stamps fetchedAt
// with an unhealthy entry, so the source is skipped and not retried until the next
// ttl window.
func (r *Resolver) ensureFresh(ctx context.Context, url string) *sourceEntry {
	r.mu.Lock()
	cached, ok := r.cache[url]
	if ok && r.now().Sub(cached.fetchedAt) < r.ttl {
		r.mu.Unlock()
		return cached
	}
	done, running := r.inflight[url]
	if !running {
		done = make(chan struct{})
		r.inflight[url] = done
		go r.refresh(ctx, url, r.now(), done) // stamp now under the lock
	}
	r.mu.Unlock()

	select {
	case <-done:
		r.mu.Lock()
		e := r.cache[url]
		r.mu.Unlock()
		return e
	case <-ctx.Done():
		if ok {
			stale := *cached // stale entry; better than a degraded answer
			stale.stale = true
			return &stale
		}
		return &sourceEntry{} // unhealthy for this call only, never stored in r.cache
	}
}

// refresh runs one metadata fetch detached from the caller's context, stores the
// result, and closes done. The fetch itself is bounded by fetchTimeout.
func (r *Resolver) refresh(parent context.Context, url string, fetchedAt time.Time, done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), r.fetchTimeout)
	defer cancel()

	entry := &sourceEntry{fetchedAt: fetchedAt}
	index, byName, err := r.fetch(ctx, url)
	if err != nil {
		r.logger.Warn("ucmeta: metadata source unavailable, skipping", "url", url, "error", err)
	} else {
		entry.healthy = true
		entry.index = index
		entry.byName = byName
	}

	r.mu.Lock()
	r.cache[url] = entry
	delete(r.inflight, url)
	r.mu.Unlock()
	close(done)
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
			GAV:          p.GAV,
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
			anyDegraded = anyDegraded || entry.stale
			continue
		}
		if res.Best == nil || pluginver.Compare(m.Version, res.Best.Version) > 0 {
			best := m
			res.Best = &best
		}
		if !accept(m) {
			anyDegraded = anyDegraded || entry.stale
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
