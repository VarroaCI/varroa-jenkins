package ucmeta

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/jenkinsver"
)

// tiersPath is the upstream document that lists the LTS core tiers. Upstream
// publishes a dynamic-stable metadata document only for a listed tier.
const tiersPath = "/tiers.json"

// tiersFetchTimeout bounds one read of tiers.json.
const tiersFetchTimeout = 30 * time.Second

// tiersMaxBytes caps the tiers.json body. The real document is a few hundred
// bytes, so the cap only guards against an unbounded body.
const tiersMaxBytes = 1 << 20

// Doer performs HTTP requests. *http.Client satisfies it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Tiers is the decoded tiers.json document. StableCores lists each LTS core
// tier that upstream publishes metadata for.
type Tiers struct {
	StableCores []string `json:"stableCores"`
}

// FetchTiers reads <base>/tiers.json. It follows redirects. It returns an
// error for a non-200 status, an unparseable body, or a document with no
// exact MAJOR.MINOR.PATCH entry.
func FetchTiers(ctx context.Context, doer Doer, base string) (Tiers, error) {
	if doer == nil {
		doer = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, tiersFetchTimeout)
	defer cancel()
	url := strings.TrimRight(base, "/") + tiersPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Tiers{}, err
	}
	resp, err := doer.Do(req)
	if err != nil {
		return Tiers{}, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return Tiers{}, fmt.Errorf("ucmeta: unexpected status %d from %s", resp.StatusCode, url)
	}
	var t Tiers
	if err := json.NewDecoder(io.LimitReader(resp.Body, tiersMaxBytes)).Decode(&t); err != nil {
		return Tiers{}, fmt.Errorf("ucmeta: decode %s: %w", url, err)
	}
	if len(t.parsed()) == 0 {
		return Tiers{}, fmt.Errorf("ucmeta: %s lists no stable core tier", url)
	}
	return t, nil
}

// ltsPatch parses v as an exact MAJOR.MINOR.PATCH core version.
func ltsPatch(v string) ([]int, bool) {
	segs, ok := jenkinsver.Core(strings.TrimSpace(v))
	if !ok || len(segs) != 3 {
		return nil, false
	}
	return segs, true
}

// parsed returns the entries of StableCores that parse as exact patches,
// trimmed, in list order.
func (t Tiers) parsed() []string {
	var out []string
	for _, s := range t.StableCores {
		s = strings.TrimSpace(s)
		if _, ok := ltsPatch(s); ok {
			out = append(out, s)
		}
	}
	return out
}

// StableTierFor maps an LTS core to the tier that carries its metadata: the
// highest tier at or below core, else the oldest tier. It returns ok=false
// when core is not an exact patch or the list holds no usable tier.
func (t Tiers) StableTierFor(core string) (string, bool) {
	coreSegs, ok := ltsPatch(core)
	if !ok {
		return "", false
	}
	tiers := t.parsed()
	if len(tiers) == 0 {
		return "", false
	}
	oldest := tiers[0]
	oldestSegs, _ := ltsPatch(oldest)
	best := ""
	var bestSegs []int
	for _, tier := range tiers {
		segs, _ := ltsPatch(tier)
		if jenkinsver.Compare(segs, oldestSegs) < 0 {
			oldest, oldestSegs = tier, segs
		}
		if jenkinsver.Compare(segs, coreSegs) <= 0 && (bestSegs == nil || jenkinsver.Compare(segs, bestSegs) > 0) {
			best, bestSegs = tier, segs
		}
	}
	if best == "" {
		return oldest, true
	}
	return best, true
}

// SourceURL returns the dynamic-stable metadata URL for the tier that carries
// core. With no usable tier it returns the URL of core itself.
func (t Tiers) SourceURL(base, core string) string {
	if tier, ok := t.StableTierFor(core); ok {
		return DynamicStableURL(base, tier)
	}
	return DynamicStableURL(base, core)
}

// DynamicStableURL builds the dynamic-stable update-center URL for version.
// Every dynamic-stable URL is built here or through Tiers.SourceURL.
func DynamicStableURL(base, version string) string {
	return strings.TrimRight(base, "/") + "/dynamic-stable-" + version + "/update-center.actual.json"
}

// TierIndex caches the tiers.json list of each upstream base URL. One index
// is shared by every reconciler that maps an LTS core to its tier.
type TierIndex struct {
	doer             Doer
	ttl              time.Duration
	failureRetryBase time.Duration
	now              func() time.Time
	logger           *slog.Logger

	mu    sync.Mutex
	cache map[string]*tierEntry
}

// tierEntry is the cached state of one upstream base URL.
type tierEntry struct {
	tiers     Tiers
	good      bool
	expiresAt time.Time
	failures  int
}

// NewTierIndex creates a TierIndex that re-reads each base URL after ttl.
// A nil doer uses http.DefaultClient. A nil logger uses slog.Default.
func NewTierIndex(doer Doer, ttl time.Duration, logger *slog.Logger) *TierIndex {
	if doer == nil {
		doer = http.DefaultClient
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &TierIndex{
		doer:             doer,
		ttl:              ttl,
		failureRetryBase: failureRetryBase,
		now:              time.Now,
		logger:           logger,
		cache:            make(map[string]*tierEntry),
	}
}

// Lookup returns the tier list of base. It reads tiers.json when the cached
// copy has expired. A failed read keeps the last good list and retries after
// a backoff. The bool is false when no list is available.
func (x *TierIndex) Lookup(ctx context.Context, base string) (Tiers, bool) {
	if x == nil {
		return Tiers{}, false
	}
	key := strings.TrimRight(base, "/")

	x.mu.Lock()
	prev := x.cache[key]
	if prev != nil && x.now().Before(prev.expiresAt) {
		t, good := prev.tiers, prev.good
		x.mu.Unlock()
		return t, good
	}
	x.mu.Unlock()

	fetched, err := FetchTiers(ctx, x.doer, key)

	x.mu.Lock()
	defer x.mu.Unlock()
	prev = x.cache[key]
	if err == nil {
		if prev != nil && prev.failures > 0 {
			x.logger.Info("ucmeta: tiers.json readable again", "base", key)
		}
		x.cache[key] = &tierEntry{tiers: fetched, good: true, expiresAt: x.now().Add(x.ttl)}
		return fetched, true
	}

	entry := &tierEntry{}
	if prev != nil {
		entry.failures = prev.failures
		entry.tiers = prev.tiers
		entry.good = prev.good
	}
	entry.failures++
	wait := failureBackoff(entry.failures, x.failureRetryBase, x.ttl)
	entry.expiresAt = x.now().Add(wait)
	x.cache[key] = entry
	x.logger.Warn("ucmeta: tiers.json unavailable; LTS metadata sources use the last good tier list or the core version",
		"base", key, "error", err, "failures", entry.failures, "retry_in", wait, "serving_stale", entry.good)
	return entry.tiers, entry.good
}

// SourceURL returns the dynamic-stable URL for the tier that carries core,
// read through the cache. A nil index returns the URL of core itself.
func (x *TierIndex) SourceURL(ctx context.Context, base, core string) string {
	t, _ := x.Lookup(ctx, base)
	return t.SourceURL(base, core)
}
