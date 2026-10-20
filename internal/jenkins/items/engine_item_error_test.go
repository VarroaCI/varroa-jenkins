package items

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/jenkins"
)

// fakeItemsJenkins answers the item endpoints of a Jenkins controller. It
// records each write as "create <path>", "update <path>", or "delete <path>".
type fakeItemsJenkins struct {
	mu sync.Mutex
	// existing holds the item paths that answer a config read with 200.
	existing map[string]bool
	// refuse holds "create <path>" or "update <path>" keys that answer 500.
	refuse map[string]bool
	// checkStatus, when non-zero, answers every config read with that status.
	checkStatus int
	calls       []string
}

func (f *fakeItemsJenkins) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeItemsJenkins) has(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

// itemPathFromURL turns "/job/a/job/j/config.xml" into "a/j".
func itemPathFromURL(p string) string {
	var out []string
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		if seg != "job" && seg != "config.xml" && seg != "api" && seg != "json" && seg != "doDelete" {
			out = append(out, seg)
		}
	}
	return strings.Join(out, "/")
}

func (f *fakeItemsJenkins) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/createItem"):
		name := r.URL.Query().Get("name")
		f.record("create " + name)
		if f.refuse["create "+name] {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("<html><head><title>Jenkins</title></head><body>Logging ID=abcdef12-3456</body></html>"))
			return
		}
		w.WriteHeader(http.StatusOK)
	case strings.HasSuffix(r.URL.Path, "/config.xml") && r.Method == http.MethodGet:
		if f.checkStatus != 0 {
			w.WriteHeader(f.checkStatus)
			return
		}
		if f.existing[itemPathFromURL(r.URL.Path)] {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<project/>"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case strings.HasSuffix(r.URL.Path, "/config.xml") && r.Method == http.MethodPost:
		name := itemPathFromURL(r.URL.Path)
		f.record("update " + name)
		if f.refuse["update "+name] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	case strings.HasSuffix(r.URL.Path, "/doDelete"):
		f.record("delete " + itemPathFromURL(r.URL.Path))
		w.WriteHeader(http.StatusOK)
	case strings.HasSuffix(r.URL.Path, "/api/json"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"building":false}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newFakeItemsEngine(t *testing.T, f *fakeItemsJenkins, managed string) (*Engine, string) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	mf := filepath.Join(t.TempDir(), "managed.json")
	if managed != "" {
		if err := os.WriteFile(mf, []byte(managed), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return &Engine{client: jenkins.NewClient(srv.URL, "test", "token"), managedFile: mf}, mf
}

// TestApplyReportsManagedCacheMkdirFailure forces the cache directory to be
// a regular file, so the cache read fails with ENOTDIR on every platform.
func TestApplyReportsManagedCacheMkdirFailure(t *testing.T) {
	engine, _ := newFakeItemsEngine(t, &fakeItemsJenkins{}, "")
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	engine.managedFile = filepath.Join(blocker, "sub", "managed.json")

	res, err := engine.Apply(context.Background(), `items:
  - kind: folder
    name: repos
`)
	if err != nil {
		t.Fatalf("Apply error = %v, want nil: a failed save is reported in the result", err)
	}
	if res.Written != 1 {
		t.Fatalf("Written = %d, want 1", res.Written)
	}
	if res.CacheError == nil {
		t.Fatal("CacheError = nil, want the cache failure")
	}
	if !strings.Contains(res.CacheError.Error(), engine.managedFile) {
		t.Fatalf("CacheError = %q, want it to name %q", res.CacheError, engine.managedFile)
	}
}

// TestApplyReportsManagedCacheWriteFailure makes the cache path a directory,
// so the cache read fails with EISDIR on every platform. The write path
// is not reached.
func TestApplyReportsManagedCacheWriteFailure(t *testing.T) {
	engine, _ := newFakeItemsEngine(t, &fakeItemsJenkins{}, "")
	engine.managedFile = filepath.Join(t.TempDir(), "managed.json")
	if err := os.Mkdir(engine.managedFile, 0755); err != nil {
		t.Fatal(err)
	}

	res, err := engine.Apply(context.Background(), `items:
  - kind: folder
    name: repos
`)
	if err != nil {
		t.Fatalf("Apply error = %v, want nil: a failed save is reported in the result", err)
	}
	if res.Written != 1 {
		t.Fatalf("Written = %d, want 1", res.Written)
	}
	if res.CacheError == nil {
		t.Fatal("CacheError = nil, want the cache failure")
	}
	if !strings.Contains(res.CacheError.Error(), engine.managedFile) {
		t.Fatalf("CacheError = %q, want it to name %q", res.CacheError, engine.managedFile)
	}
}

// TestApplySavesManagedCacheWithoutError checks the normal path reports no save error.
func TestApplySavesManagedCacheWithoutError(t *testing.T) {
	engine, mf := newFakeItemsEngine(t, &fakeItemsJenkins{}, "")

	res, err := engine.Apply(context.Background(), `items:
  - kind: folder
    name: repos
`)
	if err != nil {
		t.Fatalf("Apply error = %v, want nil", err)
	}
	if res.CacheError != nil {
		t.Fatalf("CacheError = %v, want nil", res.CacheError)
	}
	if _, err := os.Stat(mf); err != nil {
		t.Fatalf("managed cache not written: %v", err)
	}
}

func TestApplyReportsRefusedItemInResult(t *testing.T) {
	f := &fakeItemsJenkins{refuse: map[string]bool{"create repos": true}}
	engine, _ := newFakeItemsEngine(t, f, "")

	res, err := engine.Apply(context.Background(), `items:
  - kind: folder
    name: repos
`)
	if err != nil {
		t.Fatalf("Apply error = %v, want nil: a refused item is reported in the result", err)
	}
	if len(res.Failed) != 1 {
		t.Fatalf("Failed = %d entries, want 1", len(res.Failed))
	}
	if res.Failed[0].Op != ItemOpCreate || res.Failed[0].Path != "repos" {
		t.Fatalf("Failed[0] = {Op:%q Path:%q}, want {Op:create Path:repos}", res.Failed[0].Op, res.Failed[0].Path)
	}
	if !strings.HasPrefix(res.FailureText(), "create repos: ") {
		t.Fatalf("FailureText = %q, want prefix %q", res.FailureText(), "create repos: ")
	}
}

func TestApplyContinuesPastRefusedItem(t *testing.T) {
	f := &fakeItemsJenkins{refuse: map[string]bool{"create a": true}}
	engine, _ := newFakeItemsEngine(t, f, "")

	res, err := engine.Apply(context.Background(), `items:
  - kind: folder
    name: a
  - kind: folder
    name: b
`)
	if err != nil {
		t.Fatalf("Apply error = %v, want nil", err)
	}
	if !f.has("create b") {
		t.Fatalf("create b was not attempted after the refused a; calls = %v", f.calls)
	}
	if len(res.Failed) != 1 || res.Failed[0].Path != "a" {
		t.Fatalf("Failed = %+v, want one failure at a", res.Failed)
	}
	if res.Written != 1 {
		t.Fatalf("Written = %d, want 1", res.Written)
	}
}

func TestApplyKeepsPreviousHashOfRefusedItem(t *testing.T) {
	f := &fakeItemsJenkins{
		existing: map[string]bool{"a": true, "gone": true},
		refuse:   map[string]bool{"update a": true},
	}
	engine, mf := newFakeItemsEngine(t, f, `{"a":"oldhash","gone":"h"}`)

	res, err := engine.Apply(context.Background(), `removeStrategy:
  items: "sync"
items:
  - kind: folder
    name: a
`)
	if err != nil {
		t.Fatalf("Apply error = %v, want nil", err)
	}
	if len(res.Failed) != 1 || res.Failed[0].Path != "a" || res.Failed[0].Op != ItemOpUpdate {
		t.Fatalf("Failed = %+v, want one update failure at a", res.Failed)
	}
	if f.has("delete a") {
		t.Fatalf("refused item a was deleted; calls = %v", f.calls)
	}
	if !f.has("delete gone") {
		t.Fatalf("undeclared item gone was not removed; calls = %v", f.calls)
	}
	saved, err := os.ReadFile(mf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), `"a":"oldhash"`) {
		t.Fatalf("managed cache = %s, want a to keep oldhash so the next apply retries it", saved)
	}
}

func TestApplySkipsChildrenOfRefusedCreate(t *testing.T) {
	f := &fakeItemsJenkins{refuse: map[string]bool{"create a": true}}
	engine, _ := newFakeItemsEngine(t, f, "")

	res, err := engine.Apply(context.Background(), `items:
  - kind: folder
    name: a
    items:
      - kind: folder
        name: j
`)
	if err != nil {
		t.Fatalf("Apply error = %v, want nil", err)
	}
	if f.has("create a/j") || f.has("update a/j") {
		t.Fatalf("child of a refused create was attempted; calls = %v", f.calls)
	}
	if len(res.Failed) != 1 || res.Failed[0].Path != "a" {
		t.Fatalf("Failed = %+v, want one failure at a", res.Failed)
	}
}

func TestApplyAbortsOnExistenceCheckFailure(t *testing.T) {
	f := &fakeItemsJenkins{checkStatus: http.StatusServiceUnavailable}
	engine, mf := newFakeItemsEngine(t, f, `{"old":"h"}`)

	res, err := engine.Apply(context.Background(), `items:
  - kind: folder
    name: repos
`)
	if res != nil {
		t.Fatalf("result = %+v, want nil on an aborted walk", res)
	}
	var itemErr *ItemError
	if !errors.As(err, &itemErr) || itemErr.Op != ItemOpCheck {
		t.Fatalf("error = %v, want an ItemError with op check", err)
	}
	saved, readErr := os.ReadFile(mf)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(saved) != `{"old":"h"}` {
		t.Fatalf("managed cache = %s, want it unchanged after an abort", saved)
	}
}

// TestApplyCorruptManagedCacheFailsAndKeepsFile seeds a cache that is not
// JSON. The apply writes the declared item, reports the cache failure, and
// keeps the corrupt file for inspection.
func TestApplyCorruptManagedCacheFailsAndKeepsFile(t *testing.T) {
	engine, mf := newFakeItemsEngine(t, &fakeItemsJenkins{}, "{not json")

	res, err := engine.Apply(context.Background(), `items:
  - kind: folder
    name: repos
`)
	if err != nil {
		t.Fatalf("Apply error = %v, want nil: a failed cache read is reported in the result", err)
	}
	if res.Written != 1 {
		t.Fatalf("Written = %d, want 1", res.Written)
	}
	if res.CacheError == nil || !strings.Contains(res.CacheError.Error(), mf) {
		t.Fatalf("CacheError = %v, want it to name %q", res.CacheError, mf)
	}
	data, err := os.ReadFile(mf)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{not json" {
		t.Fatalf("corrupt cache was rewritten: %q", data)
	}
}

// TestApplyCorruptManagedCacheDeletesNothing proves an unknown cache never
// removes an item from Jenkins, even under a sync strategy.
func TestApplyCorruptManagedCacheDeletesNothing(t *testing.T) {
	f := &fakeItemsJenkins{existing: map[string]bool{"gone": true}}
	engine, _ := newFakeItemsEngine(t, f, "{not json")

	_, err := engine.Apply(context.Background(), `removeStrategy:
  items: "sync"
items:
  - kind: folder
    name: repos
`)
	if err != nil {
		t.Fatalf("Apply error = %v, want nil", err)
	}
	if f.has("delete gone") {
		t.Fatalf("an item was deleted while the cache is unknown; calls = %v", f.calls)
	}
}

// TestApplyFailedSaveKeepsPreviousCache makes the cache directory read-only,
// so the temporary file cannot be created. The previous cache must stay.
func TestApplyFailedSaveKeepsPreviousCache(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	engine, mf := newFakeItemsEngine(t, &fakeItemsJenkins{}, `{"old":"h"}`)
	dir := filepath.Dir(mf)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	res, err := engine.Apply(context.Background(), `items:
  - kind: folder
    name: repos
`)
	if err != nil {
		t.Fatalf("Apply error = %v, want nil", err)
	}
	if res.CacheError == nil || !strings.Contains(res.CacheError.Error(), mf) {
		t.Fatalf("CacheError = %v, want it to name %q", res.CacheError, mf)
	}
	data, err := os.ReadFile(mf)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"old":"h"}` {
		t.Fatalf("previous cache changed: %q", data)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".managed-items-") {
			t.Fatalf("temporary cache file left behind: %s", e.Name())
		}
	}
}

// TestApplyMissingManagedCacheIsEmpty treats a missing cache as an empty
// record. The apply reports no cache error and creates the file.
func TestApplyMissingManagedCacheIsEmpty(t *testing.T) {
	engine, mf := newFakeItemsEngine(t, &fakeItemsJenkins{}, "")

	res, err := engine.Apply(context.Background(), `items:
  - kind: folder
    name: repos
`)
	if err != nil {
		t.Fatalf("Apply error = %v, want nil", err)
	}
	if res.CacheError != nil {
		t.Fatalf("CacheError = %v, want nil", res.CacheError)
	}
	if _, err := os.Stat(mf); err != nil {
		t.Fatalf("managed cache not created: %v", err)
	}
}

// TestSaveManagedLeavesNoTempFile checks that a successful save leaves only
// the cache file in its directory.
func TestSaveManagedLeavesNoTempFile(t *testing.T) {
	engine, mf := newFakeItemsEngine(t, &fakeItemsJenkins{}, "")

	if _, err := engine.Apply(context.Background(), `items:
  - kind: folder
    name: repos
`); err != nil {
		t.Fatalf("Apply error = %v, want nil", err)
	}
	entries, err := os.ReadDir(filepath.Dir(mf))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(mf) {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory entries = %v, want only %s", names, filepath.Base(mf))
	}
}

// TestApplyLegacyListCacheIsCorrupt seeds the retired list format. The apply
// reports the cache as unreadable and deletes nothing.
func TestApplyLegacyListCacheIsCorrupt(t *testing.T) {
	f := &fakeItemsJenkins{existing: map[string]bool{"a/b": true}}
	engine, mf := newFakeItemsEngine(t, f, `["a/b"]`)

	res, err := engine.Apply(context.Background(), `removeStrategy:
  items: "sync"
items:
  - kind: folder
    name: repos
`)
	if err != nil {
		t.Fatalf("Apply error = %v, want nil", err)
	}
	if res.CacheError == nil || !strings.Contains(res.CacheError.Error(), mf) {
		t.Fatalf("CacheError = %v, want it to name %q", res.CacheError, mf)
	}
	if f.has("delete a/b") {
		t.Fatalf("legacy-recorded item was deleted; calls = %v", f.calls)
	}
}

// TestSaveManagedReportsMkdirFailure needs no root. A regular file in the
// path makes MkdirAll fail with ENOTDIR, and the error names the cache path.
func TestSaveManagedReportsMkdirFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	engine := &Engine{managedFile: filepath.Join(blocker, "sub", "managed.json")}

	err := engine.saveManaged(map[string]string{"a": "h"})
	if err == nil || !strings.Contains(err.Error(), engine.managedFile) {
		t.Fatalf("saveManaged error = %v, want it to name %q", err, engine.managedFile)
	}
}

// TestLoadManagedRejectsNull treats a JSON null cache as corrupt, not empty.
func TestLoadManagedRejectsNull(t *testing.T) {
	engine, mf := newFakeItemsEngine(t, &fakeItemsJenkins{}, "null")

	if _, err := engine.loadManaged(); err == nil || !strings.Contains(err.Error(), mf) {
		t.Fatalf("loadManaged error = %v, want it to name %q", err, mf)
	}
}

// TestForgetDropsPathAndDescendants removes a path and its descendants, and
// keeps a sibling whose name only shares the prefix.
func TestForgetDropsPathAndDescendants(t *testing.T) {
	engine, mf := newFakeItemsEngine(t, &fakeItemsJenkins{}, `{"a":"h1","a/b":"h2","ab":"h3"}`)
	if err := engine.Forget("a"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	got, err := engine.loadManaged()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["ab"] != "h3" {
		t.Fatalf("cache after Forget = %v (file %s), want only ab", got, mf)
	}
}

// TestForgetAbsentPathWritesNothing checks that Forget creates no file when no
// cache exists, and leaves the bytes of a cache that lacks the path unchanged.
func TestForgetAbsentPathWritesNothing(t *testing.T) {
	engine, mf := newFakeItemsEngine(t, &fakeItemsJenkins{}, "")
	if err := engine.Forget("x"); err != nil {
		t.Fatalf("Forget on missing cache: %v", err)
	}
	if _, err := os.Stat(mf); !os.IsNotExist(err) {
		t.Fatalf("cache file created by Forget, stat err = %v", err)
	}

	const body = `{"y":"h"}`
	engine, mf = newFakeItemsEngine(t, &fakeItemsJenkins{}, body)
	if err := engine.Forget("x"); err != nil {
		t.Fatalf("Forget on absent path: %v", err)
	}
	data, err := os.ReadFile(mf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, []byte(body)) {
		t.Fatalf("cache bytes = %q, want %q", data, body)
	}
	temps, _ := filepath.Glob(filepath.Join(filepath.Dir(mf), ".managed-items-*.tmp"))
	if len(temps) != 0 {
		t.Fatalf("temp files left behind: %v", temps)
	}
}

// TestForgetCorruptCacheKeepsFile returns an error that names the cache and
// leaves a corrupt cache byte-identical.
func TestForgetCorruptCacheKeepsFile(t *testing.T) {
	const body = "{not json"
	engine, mf := newFakeItemsEngine(t, &fakeItemsJenkins{}, body)
	err := engine.Forget("x")
	if err == nil || !strings.Contains(err.Error(), mf) {
		t.Fatalf("Forget error = %v, want one naming the cache file", err)
	}
	data, _ := os.ReadFile(mf)
	if string(data) != body {
		t.Fatalf("corrupt cache changed to %q", data)
	}
}

// TestApplyAfterForgetKeepsRecreatedItem shows that an item forgotten after an
// approved delete is not deleted by a later apply, which is the hazard the
// managed-items record guards against. The control case shows the delete does
// happen when no Forget runs.
func TestApplyAfterForgetKeepsRecreatedItem(t *testing.T) {
	const manifest = `removeStrategy:
  items: "sync"
root: ""
items:
  - kind: folder
    name: repos
`
	t.Run("forgotten", func(t *testing.T) {
		f := &fakeItemsJenkins{existing: map[string]bool{"old-job": true}}
		engine, _ := newFakeItemsEngine(t, f, `{"old-job":"h"}`)
		if err := engine.Forget("old-job"); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		if _, err := engine.Apply(context.Background(), manifest); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if f.has("delete old-job") {
			t.Fatal("apply deleted an item that was forgotten after an approved delete")
		}
	})
	t.Run("control", func(t *testing.T) {
		f := &fakeItemsJenkins{existing: map[string]bool{"old-job": true}}
		engine, _ := newFakeItemsEngine(t, f, `{"old-job":"h"}`)
		if _, err := engine.Apply(context.Background(), manifest); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if !f.has("delete old-job") {
			t.Fatal("control: apply did not delete a managed item that is undeclared")
		}
	})
}
