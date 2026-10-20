package items

import (
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
// a regular file, so MkdirAll fails with ENOTDIR on every platform.
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
	if res.SaveError == nil {
		t.Fatal("SaveError = nil, want the cache save failure")
	}
	if !strings.Contains(res.SaveError.Error(), engine.managedFile) {
		t.Fatalf("SaveError = %q, want it to name %q", res.SaveError, engine.managedFile)
	}
}

// TestApplyReportsManagedCacheWriteFailure makes the cache path a directory,
// so WriteFile fails with EISDIR on every platform.
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
	if res.SaveError == nil {
		t.Fatal("SaveError = nil, want the cache save failure")
	}
	if !strings.Contains(res.SaveError.Error(), engine.managedFile) {
		t.Fatalf("SaveError = %q, want it to name %q", res.SaveError, engine.managedFile)
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
	if res.SaveError != nil {
		t.Fatalf("SaveError = %v, want nil", res.SaveError)
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
