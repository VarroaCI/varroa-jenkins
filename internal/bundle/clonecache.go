package bundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"
)

// repoLock serialises concurrent Checkout calls for the same cache key. It
// is a one-slot channel, not a mutex, so that a waiter stops when its
// context is done. A git process that holds the lock can take as long as
// its own context allows, and a waiter must not inherit that time.
type repoLock struct {
	sem   chan struct{}
	inUse int
}

func newRepoLock() *repoLock {
	return &repoLock{sem: make(chan struct{}, 1)}
}

// lock takes the lock, or returns ctx.Err() when ctx is done first.
func (l *repoLock) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case l.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// tryLock takes the lock only when it is free.
func (l *repoLock) tryLock() bool {
	select {
	case l.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (l *repoLock) unlock() { <-l.sem }

// repoMeta is the per-entry metadata persisted on disk.
type repoMeta struct {
	URL       string `json:"url"`
	LastFetch int64  `json:"lastFetch"`
	LastUsed  int64  `json:"lastUsed"`
	// Size is the bare repo's on-disk size in bytes, measured after each
	// fetch so eviction passes don't have to walk every repo.
	Size int64 `json:"size,omitempty"`
}

// CloneCache is a per-replica on-disk cache of bare git repositories.
// Nil *CloneCache is valid and means "disabled": each call fetches into a
// temporary bare repository (see GitCloner.Clone).
type CloneCache struct {
	root                string // e.g. /var/cache/varroa-git
	maxRepos            int    // eviction bound, default 50
	maxSizeMiB          int64  // eviction bound, default 2048
	cloner              *GitCloner
	logger              *slog.Logger
	allowLocalTransport bool

	cacheHits   metric.Int64Counter
	cacheMisses metric.Int64Counter

	mu    sync.Mutex
	locks map[string]*repoLock

	// fetchHook is a test-only callback invoked immediately before running a fetch.
	fetchHook func()
}

// NewCloneCache creates a new CloneCache. It creates the hit and miss
// counters on the "varroa-operator" meter of mp, ensures the repos
// subdirectory exists (MkdirAll), and runs the startup cleanup+eviction pass.
func NewCloneCache(mp metric.MeterProvider, root string, maxRepos int, maxSizeMiB int64, logger *slog.Logger) (*CloneCache, error) {
	meter := mp.Meter("varroa-operator")
	cacheHits, err := meter.Int64Counter("varroa.bundle.git.cache.hits")
	if err != nil {
		return nil, fmt.Errorf("create git cache hits counter: %w", err)
	}
	cacheMisses, err := meter.Int64Counter("varroa.bundle.git.cache.misses")
	if err != nil {
		return nil, fmt.Errorf("create git cache misses counter: %w", err)
	}
	reposDir := filepath.Join(root, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache repos dir %s: %w", reposDir, err)
	}
	c := &CloneCache{
		root:        root,
		maxRepos:    maxRepos,
		maxSizeMiB:  maxSizeMiB,
		cloner:      NewGitCloner(),
		logger:      logger,
		cacheHits:   cacheHits,
		cacheMisses: cacheMisses,
		locks:       make(map[string]*repoLock),
	}
	c.startupCleanup()
	c.evict()
	return c, nil
}

// AllowLocalTransportForTest enables file:// URLs for test fixtures on both
// the cache and its internal cloner.
func (c *CloneCache) AllowLocalTransportForTest() {
	c.allowLocalTransport = true
	c.cloner.AllowLocalTransportForTest()
}

// normalizeRepoURL normalises a raw repository URL for cache-key derivation.
// It follows the normalisation table in design.md §1.
func normalizeRepoURL(raw string) string {
	s := strings.TrimSpace(raw)

	// Strip one trailing slash.
	s = strings.TrimSuffix(s, "/")

	// Scheme form: <scheme>://...
	if i := strings.Index(s, "://"); i >= 0 {
		scheme := strings.ToLower(s[:i])
		rest := s[i+3:] // after "://"

		// Strip userinfo (user[:pass]@) before the host.
		if atIdx := strings.Index(rest, "@"); atIdx >= 0 {
			rest = rest[atIdx+1:]
		}

		// Lowercase the host portion (up to first '/' or ':' after authority).
		hostEnd := strings.IndexAny(rest, "/:")
		if hostEnd < 0 {
			rest = strings.ToLower(rest)
		} else {
			rest = strings.ToLower(rest[:hostEnd]) + rest[hostEnd:]
		}

		return scheme + "://" + rest
	}

	// scp-like [user@]host:path — no "://"
	// Lowercase only the host between the optional '@' and the first ':'.
	if atIdx := strings.Index(s, "@"); atIdx >= 0 {
		// Has user@ prefix — keep user@, lowercase the host after @ and before ':'
		colonIdx := strings.Index(s[atIdx+1:], ":")
		if colonIdx >= 0 {
			host := strings.ToLower(s[atIdx+1 : atIdx+1+colonIdx])
			return s[:atIdx+1] + host + s[atIdx+1+colonIdx:]
		}
		// No colon after @? Not a valid scp-like, return as-is.
		return s
	}

	// No '@' — just lowercase the host before ':'.
	if colonIdx := strings.Index(s, ":"); colonIdx >= 0 {
		host := strings.ToLower(s[:colonIdx])
		return host + s[colonIdx:]
	}

	return s
}

// cacheKey returns the hex-encoded SHA-256 of the normalised URL.
func cacheKey(raw string) string {
	h := sha256.Sum256([]byte(normalizeRepoURL(raw)))
	return hex.EncodeToString(h[:])
}

// perRepoLock acquires or creates the per-key lock.
func (c *CloneCache) perRepoLock(key string) *repoLock {
	c.mu.Lock()
	defer c.mu.Unlock()
	rl, ok := c.locks[key]
	if !ok {
		rl = newRepoLock()
		c.locks[key] = rl
	}
	rl.inUse++
	return rl
}

// releaseRepoLock decrements inUse for the given key's lock.
func (c *CloneCache) releaseRepoLock(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rl, ok := c.locks[key]; ok {
		rl.inUse--
		if rl.inUse <= 0 {
			delete(c.locks, key)
		}
	}
}

// reposDir returns the directory holding bare repos and meta files.
func (c *CloneCache) reposDir() string {
	return filepath.Join(c.root, "repos")
}

// barePath returns the path to a bare repository for the given key.
func (c *CloneCache) barePath(key string) string {
	return filepath.Join(c.reposDir(), key+".git")
}

// metaPath returns the path to the metadata file for the given key.
func (c *CloneCache) metaPath(key string) string {
	return filepath.Join(c.reposDir(), key+".meta.json")
}

// readMeta reads the metadata file for the given key.
func (c *CloneCache) readMeta(key string) (*repoMeta, error) {
	data, err := os.ReadFile(c.metaPath(key))
	if err != nil {
		return nil, err
	}
	var m repoMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// writeMeta writes the metadata for the given key. The URL stored in meta is
// always the redacted normalised URL.
func (c *CloneCache) writeMeta(key string, m *repoMeta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(c.metaPath(key), data, 0o644)
}

// Checkout materializes repoURL@revision into targetDir and returns the
// resolved commit SHA and whether the bare store already had it (hit).
// targetDir must not exist or must be empty (caller prepares it). The target
// receives the files of the tree at the commit and no .git directory.
func (c *CloneCache) Checkout(ctx context.Context, repoURL, revision, targetDir string, auth *GitAuth) (sha string, hit bool, err error) {
	// Step 1: validate the repository URL.
	if err := validateRepoURL(repoURL, c.allowLocalTransport); err != nil {
		return "", false, err
	}

	// Step 2: resolve the desired SHA.
	sha, err = c.cloner.resolveRevision(ctx, repoURL, revision, auth)
	if err != nil {
		return "", false, err
	}

	key := cacheKey(repoURL)

	// Step 3: per-repo lock.
	lock := c.perRepoLock(key)
	if err := lock.lock(ctx); err != nil {
		c.releaseRepoLock(key)
		return "", false, fmt.Errorf("wait for git cache lock on %s: %w", redactURL(repoURL), err)
	}
	defer func() {
		lock.unlock()
		c.releaseRepoLock(key)
	}()

	bare := c.barePath(key)

	// Step 4: ensure bare repository exists.
	if _, statErr := os.Stat(bare); os.IsNotExist(statErr) {
		if err := initBareRepo(ctx, bare); err != nil {
			return "", false, err
		}
		m := &repoMeta{URL: redactURL(normalizeRepoURL(repoURL))}
		if err := c.writeMeta(key, m); err != nil {
			return "", false, fmt.Errorf("write meta for %s: %w", key, err)
		}
	}

	// Step 5: hit check.
	hit = commitExists(ctx, bare, sha)
	if !hit {
		// MISS — fetch.
		if c.fetchHook != nil {
			c.fetchHook()
		}
		sha, err = fetchCommit(ctx, bare, repoURL, sha, revision, auth, c.allowLocalTransport)
		if err != nil {
			return "", false, err
		}

		// Update lastFetch and re-measure the repo (its size only changes on
		// fetch), so eviction can total sizes from metadata alone.
		m, _ := c.readMeta(key)
		if m == nil {
			m = &repoMeta{URL: redactURL(normalizeRepoURL(repoURL))}
		}
		m.LastFetch = time.Now().Unix()
		m.Size = dirSize(bare)
		_ = c.writeMeta(key, m)
	}

	// Step 6: metrics.
	if hit {
		c.cacheHits.Add(ctx, 1)
	} else {
		c.cacheMisses.Add(ctx, 1)
	}

	if c.logger != nil {
		c.logger.Debug("git cache checkout",
			"hit", hit,
			"url", redactURL(repoURL),
			"key", key[:12],
		)
	}

	// Step 7: materialize the tree at sha from the bare store.
	if err := archiveCommit(ctx, bare, sha, targetDir); err != nil {
		return "", false, err
	}

	// Step 8: update lastUsed and evict (best-effort).
	m, _ := c.readMeta(key)
	if m != nil {
		m.LastUsed = time.Now().Unix()
		_ = c.writeMeta(key, m)
	}
	if !hit {
		c.evict()
	}

	return sha, hit, nil
}

// initBareRepo creates an empty bare repository at bare. The directory may
// exist and be empty.
func initBareRepo(ctx context.Context, bare string) error {
	cmd := gitCommand(ctx, "init", "--bare", bare)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("git init --bare %s: %w", bare, ctx.Err())
		}
		return fmt.Errorf("git init --bare %s: %s", bare, stderr.String())
	}
	return nil
}

// commitExists reports whether sha is a commit object in the bare repository.
func commitExists(ctx context.Context, bare, sha string) bool {
	cmd := gitCommand(ctx, "-C", bare, "cat-file", "-e", sha+"^{commit}")
	return cmd.Run() == nil
}

// fetchCommit fetches the commit for revision into the bare repository and
// returns the commit SHA to archive. A branch or tag can move between the
// ls-remote and the fetch, so the SHA is re-read from the fetched ref when
// the first SHA is missing.
func fetchCommit(ctx context.Context, bare, repoURL, sha, revision string, auth *GitAuth, allowLocal bool) (string, error) {
	if err := fetchBareRef(ctx, bare, repoURL, sha, revision, auth, allowLocal); err != nil {
		return "", err
	}
	if !commitExists(ctx, bare, sha) && !isCommitSHA(revision) {
		ref := revision
		if ref == "" {
			ref = "HEAD"
		}
		out, err := gitCommand(ctx, "-C", bare, "rev-parse", "refs/varroa/"+ref).Output()
		if err == nil {
			sha = strings.TrimSpace(string(out))
		}
	}
	if !commitExists(ctx, bare, sha) {
		if ctx.Err() != nil {
			return "", fmt.Errorf("check fetched SHA %s for %s: %w", sha, redactURL(repoURL), ctx.Err())
		}
		return "", fmt.Errorf("desired SHA %s is absent after fetch for %s", sha, redactURL(repoURL))
	}
	return sha, nil
}

// fetchBareRef performs a shallow fetch into the bare repository. The URL is
// passed as argv, never configured as a remote. The fetch stops when ctx is
// done. A pinned SHA is fetched into refs/varroa/pinned/<sha>; a ref is
// fetched into refs/varroa/<ref>.
func fetchBareRef(ctx context.Context, bare, repoURL, sha, revision string, auth *GitAuth, allowLocal bool) error {
	gitEnv, cleanup, err := remoteGitEnv(auth, allowLocal)
	if err != nil {
		return err
	}
	defer cleanup()

	var stderr bytes.Buffer

	if isCommitSHA(revision) {
		// Pinned SHA: fetch into a ref so it's reachable for clone.
		ref := "refs/varroa/pinned/" + sha
		cmd := gitCommand(ctx, "-C", bare, "fetch", "--depth=1", repoURL, "+"+sha+":"+ref)
		cmd.Env = gitEnv
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("git fetch %s: %w", redactURL(repoURL), ctx.Err())
			}
			return fmt.Errorf("git fetch %s: %s", redactURL(repoURL), stderr.String())
		}
	} else {
		ref := revision
		if ref == "" {
			ref = "HEAD"
		}
		cmd := gitCommand(ctx, "-C", bare, "fetch", "--depth=1", "--no-tags", repoURL, "+"+ref+":refs/varroa/"+ref)
		cmd.Env = gitEnv
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("git fetch %s %s: %w", redactURL(repoURL), ref, ctx.Err())
			}
			return fmt.Errorf("git fetch %s %s: %s", redactURL(repoURL), ref, stderr.String())
		}
	}
	return nil
}

// evict runs one eviction pass: removes entries until count <= maxRepos AND
// total size <= maxSizeMiB*1024*1024. Errors are logged, not returned.
func (c *CloneCache) evict() {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries, totalSize, err := c.listEntries()
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("git cache eviction: list entries failed", "error", err)
		}
		return
	}

	maxBytes := c.maxSizeMiB * 1024 * 1024
	initialCount := len(entries)
	evicted := 0
	var freedBytes int64

	for (len(entries) > c.maxRepos || totalSize > maxBytes) && len(entries) > 0 {
		// Sort by LastFetch ascending (oldest first), tie-break by LastUsed,
		// then by cache key for determinism.
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].meta.LastFetch != entries[j].meta.LastFetch {
				return entries[i].meta.LastFetch < entries[j].meta.LastFetch
			}
			if entries[i].meta.LastUsed != entries[j].meta.LastUsed {
				return entries[i].meta.LastUsed < entries[j].meta.LastUsed
			}
			return entries[i].key < entries[j].key
		})

		// Find the first evictable entry.
		var victim *entryInfo
		for _, e := range entries {
			if rl, ok := c.locks[e.key]; ok && rl.inUse > 0 {
				continue
			}
			if rl, ok := c.locks[e.key]; ok {
				if !rl.tryLock() {
					continue
				}
				rl.unlock()
			}
			victim = e
			break
		}
		if victim == nil {
			// Nothing further evictable — stop.
			break
		}

		// Remove the victim.
		_ = os.RemoveAll(c.barePath(victim.key))
		_ = os.Remove(c.metaPath(victim.key))
		totalSize -= victim.size
		evicted++
		freedBytes += victim.size

		// Rebuild entry list (removing the evicted one).
		var remaining []*entryInfo
		for _, e := range entries {
			if e.key != victim.key {
				remaining = append(remaining, e)
			}
		}
		entries = remaining
	}

	if evicted > 0 && c.logger != nil {
		c.logger.Info("git cache eviction",
			"entriesEvicted", evicted,
			"bytesFreed", freedBytes,
			"entriesBefore", initialCount,
			"entriesAfter", len(entries),
		)
	}

	// Log if a single oversized entry remains over the size bound.
	if len(entries) == 1 && totalSize > maxBytes && c.logger != nil {
		c.logger.Info("git cache over size bound by single oversized entry",
			"entry", entries[0].key,
			"size", entries[0].size,
			"maxSizeMiB", c.maxSizeMiB,
		)
	}
}

// entryInfo holds information about a single cache entry for eviction.
type entryInfo struct {
	key  string
	meta *repoMeta
	size int64
}

// dirSize returns the total size in bytes of all files under path. Walk
// errors are ignored: an unreadable file just doesn't count toward the size
// estimate, which only feeds LRU eviction.
func dirSize(path string) int64 {
	var size int64
	_ = filepath.WalkDir(path, func(_ string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // skip unreadable entries, keep walking
		}
		if !d.IsDir() {
			fi, err := d.Info()
			if err == nil {
				size += fi.Size()
			}
		}
		return nil
	})
	return size
}

// listEntries reads all cache entries from disk and computes total size.
// Sizes come from the per-entry metadata stamped at fetch time; only entries
// whose meta predates size tracking (or is missing) get walked.
func (c *CloneCache) listEntries() ([]*entryInfo, int64, error) {
	reposDir := c.reposDir()
	dirEntries, err := os.ReadDir(reposDir)
	if err != nil {
		return nil, 0, err
	}

	var entries []*entryInfo
	totalSize := int64(0)

	for _, de := range dirEntries {
		if !de.IsDir() || !strings.HasSuffix(de.Name(), ".git") {
			continue
		}
		key := strings.TrimSuffix(de.Name(), ".git")
		m, err := c.readMeta(key)
		if err != nil {
			if c.logger != nil {
				c.logger.Debug("git cache: skipping entry with unreadable meta", "key", key, "error", err)
			}
			// Synthesise meta from dir mtime if missing.
			if os.IsNotExist(err) {
				fi, fiErr := os.Stat(c.barePath(key))
				if fiErr == nil {
					m = &repoMeta{
						URL:       "",
						LastFetch: fi.ModTime().Unix(),
						LastUsed:  fi.ModTime().Unix(),
					}
				}
			}
			if m == nil {
				continue
			}
		}
		if m.Size <= 0 {
			// Legacy entry from before size tracking: measure once and
			// persist, so it isn't re-walked on every eviction pass.
			m.Size = dirSize(filepath.Join(reposDir, de.Name()))
			_ = c.writeMeta(key, m)
		}
		entries = append(entries, &entryInfo{
			key:  key,
			meta: m,
			size: m.Size,
		})
		totalSize += m.Size
	}

	return entries, totalSize, nil
}

// startupCleanup repairs or removes corrupt/orphaned entries.
func (c *CloneCache) startupCleanup() {
	reposDir := c.reposDir()
	dirEntries, err := os.ReadDir(reposDir)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("git cache startup: cannot read repos dir", "error", err)
		}
		return
	}

	// Build a set of keys that have a meta file.
	type entry struct {
		key      string
		hasMeta  bool
		hasBare  bool
		metaPath string
		barePath string
	}
	keys := make(map[string]*entry)

	for _, de := range dirEntries {
		name := de.Name()
		if strings.HasSuffix(name, ".meta.json") {
			key := strings.TrimSuffix(name, ".meta.json")
			if _, ok := keys[key]; !ok {
				keys[key] = &entry{key: key}
			}
			keys[key].hasMeta = true
			keys[key].metaPath = filepath.Join(reposDir, name)
		} else if de.IsDir() && strings.HasSuffix(name, ".git") {
			key := strings.TrimSuffix(name, ".git")
			if _, ok := keys[key]; !ok {
				keys[key] = &entry{key: key}
			}
			keys[key].hasBare = true
			keys[key].barePath = filepath.Join(reposDir, name)
		}
	}

	for _, e := range keys {
		if !e.hasMeta && e.hasBare {
			// Bare present, meta absent — synthesise meta from dir mtime.
			fi, err := os.Stat(e.barePath)
			if err != nil {
				continue
			}
			m := &repoMeta{
				URL:       "",
				LastFetch: fi.ModTime().Unix(),
				LastUsed:  fi.ModTime().Unix(),
			}
			if err := c.writeMeta(e.key, m); err != nil && c.logger != nil {
				c.logger.Warn("git cache: failed to synthesise meta", "key", e.key, "error", err)
			}
		} else if e.hasMeta && !e.hasBare {
			// Meta present, bare absent — delete meta.
			_ = os.Remove(e.metaPath)
		} else if e.hasMeta && e.hasBare {
			// Both present — validate meta is parseable.
			_, err := c.readMeta(e.key)
			if err != nil {
				// Unparseable meta — delete both.
				_ = os.Remove(e.metaPath)
				_ = os.RemoveAll(e.barePath)
			}
		}
	}
}
