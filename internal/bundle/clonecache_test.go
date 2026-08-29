package bundle

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newBareFixture creates a bare git repo under a temp dir, returns the
// file:// bare URL and a commit helper that adds a file, commits+pushes to
// main, and returns the resulting HEAD SHA.
func newBareFixture(t *testing.T) (bareURL string, commit func(name, content string) string) {
	t.Helper()
	return newBareFixtureBranch(t, "main")
}

// newBareFixtureBranch is newBareFixture with a caller-chosen default branch,
// so tests can cover repos whose branch name collides with git's
// init.defaultBranch.
func newBareFixtureBranch(t *testing.T, branch string) (bareURL string, commit func(name, content string) string) {
	t.Helper()
	fixtureDir := t.TempDir()
	bare := filepath.Join(fixtureDir, "bare.git")
	must(t, exec.Command("git", "init", "--bare", bare).Run())
	work := filepath.Join(fixtureDir, "work")
	must(t, exec.Command("git", "clone", bare, work).Run())
	must(t, exec.Command("git", "-C", work, "symbolic-ref", "HEAD", "refs/heads/"+branch).Run())
	gitEnv := []string{
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	}

	commit = func(name, content string) string {
		t.Helper()
		fpath := filepath.Join(work, name)
		must(t, os.WriteFile(fpath, []byte(content), 0o644))
		must(t, exec.Command("git", "-C", work, "add", ".").Run())
		c := exec.Command("git", "-C", work, "commit", "-m", "update "+name)
		c.Env = append(os.Environ(), gitEnv...)
		must(t, c.Run())
		p := exec.Command("git", "-C", work, "push", "origin", branch)
		p.Env = append(os.Environ(), gitEnv...)
		must(t, p.Run())
		out, err := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output()
		must(t, err)
		return strings.TrimSpace(string(out))
	}

	// Make an initial commit so the repo has a HEAD.
	_ = commit("initial.txt", "initial")

	// Ensure the bare repo's HEAD points to the requested branch.
	must(t, exec.Command("git", "-C", bare, "symbolic-ref", "HEAD", "refs/heads/"+branch).Run())

	return "file://" + bare, commit
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newCacheForTest(t *testing.T, root string, maxRepos int, maxSizeMiB int64) *CloneCache {
	t.Helper()
	c, err := NewCloneCache(root, maxRepos, maxSizeMiB, slog.New(slog.NewTextHandler(io.Discard, nil)))
	must(t, err)
	c.AllowLocalTransportForTest()
	return c
}

// ---------------------------------------------------------------------------
// 5.1: Key normalisation table test
// ---------------------------------------------------------------------------

func TestNormalizeRepoURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// Credential stripping: HTTPS with user:pass -> no credentials
		{"https://user:pass@host/r", "https://host/r"},
		// Case: HTTPS -> lowercase
		{"HTTPS://GitHub.com/Org/Repo", "https://github.com/Org/Repo"},
		// Trailing slash stripped
		{"https://github.com/org/repo/", "https://github.com/org/repo"},
		// .git suffix kept (distinct)
		{"https://github.com/org/repo", "https://github.com/org/repo"},
		{"https://github.com/org/repo.git", "https://github.com/org/repo.git"},
		// SCP-like keeps user@, lowercases host
		{"git@GitHub.com:org/repo.git", "git@github.com:org/repo.git"},
		// Whitespace trimmed
		{"  https://github.com/org/repo  ", "https://github.com/org/repo"},
		// SCP-like with user@ and trailing /
		{"user@HOST:path/", "user@host:path"},
		// Port preserved
		{"https://example.com:8443/repo", "https://example.com:8443/repo"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeRepoURL(tt.input)
			if got != tt.want {
				t.Errorf("normalizeRepoURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestCacheKey(t *testing.T) {
	// Credential-bearing and credential-free URLs share the same key.
	key1 := cacheKey("https://user:token@git.example.com/org/repo.git")
	key2 := cacheKey("https://git.example.com/org/repo.git")
	if key1 != key2 {
		t.Errorf("credential-bearing and credential-free URLs must share a key: %q != %q", key1, key2)
	}

	// Case variants share the same key.
	key3 := cacheKey("HTTPS://Git.Example.com/org/repo")
	key4 := cacheKey("https://git.example.com/org/repo")
	if key3 != key4 {
		t.Errorf("case variants must share a key: %q != %q", key3, key4)
	}

	// .git suffix distinguishes.
	key5 := cacheKey("https://git.example.com/org/repo")
	key6 := cacheKey("https://git.example.com/org/repo.git")
	if key5 == key6 {
		t.Errorf(".git variants must NOT share a key: %q == %q", key5, key6)
	}

	// Whitespace trimmed.
	key7 := cacheKey("  https://example.com/r  ")
	key8 := cacheKey("https://example.com/r")
	if key7 != key8 {
		t.Errorf("whitespace-trimmed variants must share a key: %q != %q", key7, key8)
	}
}

// ---------------------------------------------------------------------------
// 5.2: Hit/miss lifecycle
// ---------------------------------------------------------------------------

func TestCheckout_HitMissLifecycle(t *testing.T) {
	bareURL, commit := newBareFixture(t)
	root := t.TempDir()
	c := newCacheForTest(t, root, 10, 100)

	headSHA1 := commit("a.txt", "content-v1")

	var fetchCount atomic.Int32
	c.fetchHook = func() { fetchCount.Add(1) }

	// Checkout #1: cold cache -> miss, 1 fetch.
	target1 := filepath.Join(t.TempDir(), "out1")
	must(t, os.MkdirAll(target1, 0o755))
	sha1, hit1, err := c.Checkout(context.Background(), bareURL, "main", target1, nil)
	must(t, err)
	if hit1 {
		t.Error("checkout #1: expected miss (cold cache)")
	}
	if sha1 != headSHA1 {
		t.Errorf("checkout #1: sha = %q, want %q", sha1, headSHA1)
	}
	if n := fetchCount.Load(); n != 1 {
		t.Errorf("checkout #1: fetch count = %d, want 1", n)
	}

	// Verify working tree content + git rev-parse HEAD.
	data, err := os.ReadFile(filepath.Join(target1, "a.txt"))
	must(t, err)
	if string(data) != "content-v1" {
		t.Errorf("checkout #1: file content = %q, want %q", string(data), "content-v1")
	}
	out, err := exec.Command("git", "-C", target1, "rev-parse", "HEAD").Output()
	must(t, err)
	if strings.TrimSpace(string(out)) != headSHA1 {
		t.Errorf("checkout #1: git rev-parse HEAD = %q, want %q", strings.TrimSpace(string(out)), headSHA1)
	}

	// Checkout #2: same repo@ref, unmoved tip -> hit, 0 fetches.
	target2 := filepath.Join(t.TempDir(), "out2")
	must(t, os.MkdirAll(target2, 0o755))
	sha2, hit2, err := c.Checkout(context.Background(), bareURL, "main", target2, nil)
	must(t, err)
	if !hit2 {
		t.Error("checkout #2: expected hit (same revision)")
	}
	if sha2 != headSHA1 {
		t.Errorf("checkout #2: sha = %q, want %q", sha2, headSHA1)
	}
	if n := fetchCount.Load(); n != 1 {
		t.Errorf("checkout #2: total fetch count = %d, want still 1", n)
	}

	// New commit -> miss, new SHA.
	headSHA2 := commit("b.txt", "content-v2")
	target3 := filepath.Join(t.TempDir(), "out3")
	must(t, os.MkdirAll(target3, 0o755))
	sha3, hit3, err := c.Checkout(context.Background(), bareURL, "main", target3, nil)
	must(t, err)
	if hit3 {
		t.Error("checkout #3: expected miss (new commit)")
	}
	if sha3 != headSHA2 {
		t.Errorf("checkout #3: sha = %q, want %q", sha3, headSHA2)
	}
	if n := fetchCount.Load(); n != 2 {
		t.Errorf("checkout #3: total fetch count = %d, want 2", n)
	}

	// Pinned SHA variant: already cached -> hit and zero ls-remote/fetch.
	target4 := filepath.Join(t.TempDir(), "out4")
	must(t, os.MkdirAll(target4, 0o755))
	sha4, hit4, err := c.Checkout(context.Background(), bareURL, headSHA2, target4, nil)
	must(t, err)
	if !hit4 {
		t.Error("checkout #4 (pinned SHA): expected hit")
	}
	if sha4 != headSHA2 {
		t.Errorf("checkout #4 (pinned SHA): sha = %q, want %q", sha4, headSHA2)
	}
	if n := fetchCount.Load(); n != 2 {
		t.Errorf("checkout #4: total fetch count = %d, want still 2", n)
	}

	// Empty revision -> resolves HEAD.
	target5 := filepath.Join(t.TempDir(), "out5")
	must(t, os.MkdirAll(target5, 0o755))
	sha5, _, err := c.Checkout(context.Background(), bareURL, "", target5, nil)
	must(t, err)
	if sha5 != headSHA2 {
		t.Errorf("checkout #5 (empty rev): sha = %q, want %q (HEAD)", sha5, headSHA2)
	}
}

// ---------------------------------------------------------------------------
// 5.3: Credential hygiene
// ---------------------------------------------------------------------------

func TestCheckout_NoCredentialOnDisk(t *testing.T) {
	bareURL, _ := newBareFixture(t)
	root := t.TempDir()
	c := newCacheForTest(t, root, 10, 100)

	target := filepath.Join(t.TempDir(), "out")
	must(t, os.MkdirAll(target, 0o755))
	_, _, err := c.Checkout(context.Background(), bareURL, "main", target, nil)
	must(t, err)

	// Check that meta.json URLs are redacted.
	reposDir := filepath.Join(root, "repos")
	entries, err := os.ReadDir(reposDir)
	must(t, err)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".meta.json") {
			data, err := os.ReadFile(filepath.Join(reposDir, e.Name()))
			must(t, err)
			var m repoMeta
			must(t, json.Unmarshal(data, &m))
			if strings.Contains(m.URL, "://user:") || strings.Contains(m.URL, "sekret") {
				t.Errorf("meta URL contains credentials: %q", m.URL)
			}
		}
	}

	// Check that bare repos have no configured remote.
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".git") {
			out, err := exec.Command("git", "-C", filepath.Join(reposDir, e.Name()), "config", "--get", "remote.origin.url").CombinedOutput()
			if err == nil {
				t.Errorf("bare repo %s has a configured remote: %s", e.Name(), strings.TrimSpace(string(out)))
			}
		}
	}
}

func TestNormalizeURL_NoCredentials(t *testing.T) {
	normalized := normalizeRepoURL("https://user:sekret@example.com/r.git")
	if strings.Contains(normalized, "sekret") {
		t.Errorf("normalized URL contains credential: %q", normalized)
	}
	key := cacheKey("https://user:sekret@example.com/r.git")
	if strings.Contains(key, "sekret") {
		t.Errorf("cache key derived from credential URL: %q", key)
	}
}

// ---------------------------------------------------------------------------
// 5.4: Eviction
// ---------------------------------------------------------------------------

func TestEvict_CountBound(t *testing.T) {
	root := t.TempDir()
	c := newCacheForTest(t, root, 2, 10000)

	bareURL1, commit1 := newBareFixture(t)
	_ = commit1("f1.txt", "repo1")

	bareURL2, commit2 := newBareFixture(t)
	_ = commit2("f2.txt", "repo2")

	bareURL3, commit3 := newBareFixture(t)
	_ = commit3("f3.txt", "repo3")

	var fc atomic.Int32
	c.fetchHook = func() { fc.Add(1) }

	checkout := func(url string) {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "out")
		must(t, os.MkdirAll(dir, 0o755))
		_, _, err := c.Checkout(context.Background(), url, "main", dir, nil)
		must(t, err)
	}
	checkout(bareURL1)
	checkout(bareURL2)
	checkout(bareURL3)

	// With 3 repos and maxRepos=2, one should have been evicted.
	// Re-checkout all three; at least one should be a miss (the evicted one).
	fc.Store(0)
	for _, url := range []string{bareURL1, bareURL2, bareURL3} {
		dir := filepath.Join(t.TempDir(), "out-re")
		must(t, os.MkdirAll(dir, 0o755))
		_, hit, err := c.Checkout(context.Background(), url, "main", dir, nil)
		must(t, err)
		if !hit {
			// Found a miss — eviction worked.
			return
		}
	}
	t.Error("no eviction detected: all three repos were hits after 3 checkouts with maxRepos=2")
}

func TestEvict_SizeBound(t *testing.T) {
	root := t.TempDir()
	// Very small maxSizeMiB so the single entry overshoots.
	c := newCacheForTest(t, root, 50, 1) // 1 MiB bound

	bareURL, commit := newBareFixture(t)
	sha := commit("bigfile.txt", strings.Repeat("x", 2*1024*1024)) // ~2 MiB

	var fc atomic.Int32
	c.fetchHook = func() { fc.Add(1) }

	dir := filepath.Join(t.TempDir(), "out")
	must(t, os.MkdirAll(dir, 0o755))
	_, _, err := c.Checkout(context.Background(), bareURL, "main", dir, nil)
	must(t, err)

	// Second checkout should be a hit (oversized entry retained).
	fc.Store(0)
	dir2 := filepath.Join(t.TempDir(), "out2")
	must(t, os.MkdirAll(dir2, 0o755))
	_, hit, err := c.Checkout(context.Background(), bareURL, "main", dir2, nil)
	must(t, err)
	if !hit {
		t.Error("oversized single entry: expected hit on second checkout")
	}
	if n := fc.Load(); n != 0 {
		t.Errorf("oversized single entry: fetch count = %d, want 0 (hit)", n)
	}
	_ = sha
}

func TestEvict_SkipsInUse(t *testing.T) {
	root := t.TempDir()
	c := newCacheForTest(t, root, 1, 10000) // maxRepos=1

	bareURL1, commit1 := newBareFixture(t)
	_ = commit1("r1.txt", "repo1")

	bareURL2, commit2 := newBareFixture(t)
	_ = commit2("r2.txt", "repo2")

	// Checkout repo1 first.
	dir1 := filepath.Join(t.TempDir(), "out1")
	must(t, os.MkdirAll(dir1, 0o755))
	_, _, err := c.Checkout(context.Background(), bareURL1, "main", dir1, nil)
	must(t, err)

	// Acquire the lock for repo1 (simulate in-flight checkout).
	key1 := cacheKey(bareURL1)
	lock := c.perRepoLock(key1)
	lock.mu.Lock()

	var fc atomic.Int32
	c.fetchHook = func() { fc.Add(1) }

	// Now checkout repo2 -> triggers eviction but repo1 is in-use so skipped.
	dir2 := filepath.Join(t.TempDir(), "out2")
	must(t, os.MkdirAll(dir2, 0o755))
	_, _, err = c.Checkout(context.Background(), bareURL2, "main", dir2, nil)
	must(t, err)

	lock.mu.Unlock()

	if n := fc.Load(); n != 1 {
		t.Errorf("in-use skip: fetch count = %d, want 1", n)
	}
}

func TestStartupCleanup(t *testing.T) {
	root := t.TempDir()
	reposDir := filepath.Join(root, "repos")
	must(t, os.MkdirAll(reposDir, 0o755))

	// 1. Bare repo without meta.
	bareOnly := filepath.Join(reposDir, "bareonly.git")
	must(t, exec.Command("git", "init", "--bare", bareOnly).Run())

	// 2. Meta without bare repo.
	orphanMeta := filepath.Join(reposDir, "orphan.meta.json")
	must(t, os.WriteFile(orphanMeta, []byte(`{"url":"https://x","lastFetch":100,"lastUsed":100}`), 0o644))

	// 3. Unparseable meta.
	badKey := "badmeta"
	badBare := filepath.Join(reposDir, badKey+".git")
	must(t, exec.Command("git", "init", "--bare", badBare).Run())
	badMeta := filepath.Join(reposDir, badKey+".meta.json")
	must(t, os.WriteFile(badMeta, []byte("not json"), 0o644))

	// Now create cache (which runs startupCleanup).
	c, err := NewCloneCache(root, 10, 1000, nil)
	must(t, err)
	_ = c

	// (a) bare-only should now have a synthesized meta file.
	if _, err := os.Stat(filepath.Join(reposDir, "bareonly.meta.json")); os.IsNotExist(err) {
		t.Error("startup: bare-only entry should have synthesized meta, but it's missing")
	}
	// (b) orphan meta should be deleted.
	if _, err := os.Stat(orphanMeta); !os.IsNotExist(err) {
		t.Error("startup: orphan meta should have been deleted")
	}
	// (c) bad meta entry should be deleted (both meta and bare).
	if _, err := os.Stat(badMeta); !os.IsNotExist(err) {
		t.Error("startup: unparseable meta should have been deleted")
	}
	if _, err := os.Stat(badBare); !os.IsNotExist(err) {
		t.Error("startup: bare repo with unparseable meta should have been deleted")
	}
}

// ---------------------------------------------------------------------------
// 5.5: Concurrency — concurrent same-repo Checkouts
// ---------------------------------------------------------------------------

func TestCheckout_ConcurrentSameRepo(t *testing.T) {
	bareURL, commit := newBareFixture(t)
	_ = commit("c.txt", "concurrent")

	root := t.TempDir()
	c := newCacheForTest(t, root, 10, 100)

	var fetchCount atomic.Int32
	c.fetchHook = func() { fetchCount.Add(1) }

	var wg sync.WaitGroup
	errs := make(chan error, 4)
	shas := make(chan string, 4)

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dir := filepath.Join(t.TempDir(), "out")
			if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
				errs <- mkErr
				return
			}
			sha, _, ckErr := c.Checkout(context.Background(), bareURL, "main", dir, nil)
			if ckErr != nil {
				errs <- ckErr
				return
			}
			shas <- sha
		}()
	}
	wg.Wait()
	close(errs)
	close(shas)

	for err := range errs {
		t.Fatal(err)
	}

	var firstSHA string
	for sha := range shas {
		if firstSHA == "" {
			firstSHA = sha
		} else if sha != firstSHA {
			t.Errorf("concurrent checkouts returned different SHAs: %q vs %q", firstSHA, sha)
		}
	}
	if firstSHA == "" {
		t.Fatal("no SHAs returned from concurrent checkouts")
	}

	// Exactly 1 fetch across all goroutines.
	if n := fetchCount.Load(); n != 1 {
		t.Errorf("concurrent checkouts: total fetch count = %d, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// 5.7: Eviction safety — materialized trees survive eviction
// ---------------------------------------------------------------------------

func TestEvictionDoesNotBreakCheckout(t *testing.T) {
	bareURL, commit := newBareFixture(t)
	sha := commit("safe.txt", "this should survive")

	root := t.TempDir()
	c := newCacheForTest(t, root, 10, 100)

	target := filepath.Join(t.TempDir(), "out")
	must(t, os.MkdirAll(target, 0o755))
	_, _, err := c.Checkout(context.Background(), bareURL, "main", target, nil)
	must(t, err)

	// Simulate eviction by removing the bare entry.
	key := cacheKey(bareURL)
	must(t, os.RemoveAll(c.barePath(key)))
	must(t, os.Remove(c.metaPath(key)))

	// The materialized tree should still be fully readable.
	data, err := os.ReadFile(filepath.Join(target, "safe.txt"))
	must(t, err)
	if string(data) != "this should survive" {
		t.Errorf("after eviction: file content = %q, want %q", string(data), "this should survive")
	}

	out, err := exec.Command("git", "-C", target, "rev-parse", "HEAD").Output()
	must(t, err)
	if strings.TrimSpace(string(out)) != sha {
		t.Errorf("after eviction: git rev-parse HEAD = %q, want %q", strings.TrimSpace(string(out)), sha)
	}
}

// ---------------------------------------------------------------------------
// Repos whose branch name matches git's init.defaultBranch must still
// materialize. localClone must never fetch
// into refs/heads/* of the target working tree: git refuses to update the
// checked-out (even unborn) branch of a fresh `git init`, wedging every
// CatalogSource/gitSource tracking such a branch.
// ---------------------------------------------------------------------------

func TestCheckout_DefaultBranchNameCollision(t *testing.T) {
	// Cover both common init.defaultBranch values; whichever matches the
	// environment's git default reproduces the collision on unfixed code.
	for _, branch := range []string{"master", "main"} {
		t.Run(branch, func(t *testing.T) {
			bareURL, commit := newBareFixtureBranch(t, branch)
			sha := commit("file.txt", "content-"+branch)

			cache := newCacheForTest(t, t.TempDir(), 10, 100)
			target := filepath.Join(t.TempDir(), "checkout")

			gotSHA, _, err := cache.Checkout(context.Background(), bareURL, branch, target, nil)
			if err != nil {
				t.Fatalf("Checkout(branch=%s): %v", branch, err)
			}
			if gotSHA != sha {
				t.Errorf("Checkout SHA = %q, want %q", gotSHA, sha)
			}
			data, err := os.ReadFile(filepath.Join(target, "file.txt"))
			must(t, err)
			if string(data) != "content-"+branch {
				t.Errorf("file content = %q, want %q", data, "content-"+branch)
			}
		})
	}
}
