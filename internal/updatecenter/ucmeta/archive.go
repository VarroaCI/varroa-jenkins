package ucmeta

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultArchiveBaseURL is the Jenkins Maven repository, which keeps every released
// plugin artifact and — unlike update-center metadata — keeps them all indefinitely.
const DefaultArchiveBaseURL = "https://repo.jenkins-ci.org/releases"

// archiveTTL bounds how long a sidecar lookup (hit or miss) is remembered. Sidecars for
// a released version never change, so the TTL exists only to retry transient failures
// and to bound memory, not for freshness.
const archiveTTL = 6 * time.Hour

// archiveResolver recovers a checksum for a version no metadata source lists.
//
// Update-center metadata — weekly and dynamic-stable alike — carries exactly one
// version per plugin: the newest for that line. The moment a pinned version stops
// being the newest, every metadata source drops it, and pull-through can no longer
// verify a download it is otherwise perfectly able to fetch. That is not a transient
// condition; it is permanent and it worsens with the age of the pin, so a metadata-only
// resolver cannot serve a pinned fleet over time.
//
// The Maven repository does not have this property: it is addressed by coordinate
// rather than by recency, and publishes a .sha256 sidecar next to every artifact it
// has ever released. Reading that sidecar yields the same digest the metadata would
// have carried, for arbitrarily old versions.
//
// The groupId needed to address it is taken from the plugin's "gav" coordinate in
// whatever metadata source does list the plugin (at whatever version). A plugin's
// groupId is stable across its versions, so the current listing supplies a usable
// group path even when its version is wrong.
type archiveResolver struct {
	baseURL string
	client  *http.Client

	mu    sync.Mutex
	cache map[verKey]archiveEntry
}

type archiveEntry struct {
	sha256B64 string // empty when the lookup failed
	err       error
	at        time.Time
}

func newArchiveResolver(baseURL string, client *http.Client) *archiveResolver {
	if baseURL == "" {
		baseURL = DefaultArchiveBaseURL
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &archiveResolver{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		client:  client,
		cache:   make(map[verKey]archiveEntry),
	}
}

// groupPath extracts the groupId from a "groupId:artifactId:version" coordinate and
// converts it to the Maven repository's directory layout. Returns "" when gav is
// malformed, which callers treat as "archive lookup not possible".
func groupPath(gav string) string {
	group, _, ok := strings.Cut(gav, ":")
	if !ok || group == "" {
		return ""
	}
	// Reject anything that could escape the repository path. Group ids are
	// dot-separated identifiers; nothing else is legitimate here.
	if strings.ContainsAny(group, "/\\") || strings.Contains(group, "..") {
		return ""
	}
	return strings.ReplaceAll(group, ".", "/")
}

// resolve fetches the .sha256 sidecar for name@version and returns the digest
// re-encoded as base64, matching the encoding update-center metadata uses so callers
// verify identically regardless of which path supplied the checksum.
func (a *archiveResolver) resolve(ctx context.Context, gav, name, version string) (string, error) {
	key := verKey{name: name, version: version}

	a.mu.Lock()
	if e, ok := a.cache[key]; ok && time.Since(e.at) < archiveTTL {
		a.mu.Unlock()
		return e.sha256B64, e.err
	}
	a.mu.Unlock()

	sha, err := a.fetch(ctx, gav, name, version)

	a.mu.Lock()
	a.cache[key] = archiveEntry{sha256B64: sha, err: err, at: time.Now()}
	a.mu.Unlock()

	return sha, err
}

func (a *archiveResolver) fetch(ctx context.Context, gav, name, version string) (string, error) {
	gp := groupPath(gav)
	if gp == "" {
		return "", fmt.Errorf("ucmeta: no usable groupId for %s (gav %q)", name, gav)
	}
	url := fmt.Sprintf("%s/%s/%s/%s/%s-%s.hpi.sha256", a.baseURL, gp, name, version, name, version)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("ucmeta: build archive request: %w", err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ucmeta: fetch archive checksum: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ucmeta: archive checksum for %s@%s: HTTP %d", name, version, resp.StatusCode)
	}

	// Sidecars are small; cap the read so a misrouted response cannot balloon memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil {
		return "", fmt.Errorf("ucmeta: read archive checksum: %w", err)
	}

	// The sidecar is bare lowercase hex, but tolerate the "<digest>  <filename>"
	// form some Maven repositories emit, plus surrounding whitespace.
	field := strings.TrimSpace(string(body))
	if i := strings.IndexAny(field, " \t\r\n"); i >= 0 {
		field = field[:i]
	}
	raw, err := hex.DecodeString(field)
	if err != nil {
		return "", fmt.Errorf("ucmeta: archive checksum for %s@%s is not hex: %w", name, version, err)
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("ucmeta: archive checksum for %s@%s is %d bytes, want 32", name, version, len(raw))
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}
