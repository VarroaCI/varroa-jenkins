package items

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/jenkins"
)

func TestApplyReturnsItemErrorWithPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("<html><head><title>Jenkins</title></head><body>Logging ID=abcdef12-3456</body></html>"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	engine := NewEngine(jenkins.NewClient(srv.URL, "test", "token"))
	_, err := engine.Apply(context.Background(), `items:
  - kind: folder
    name: repos
`)
	if err == nil {
		t.Fatal("Apply succeeded, want a create failure")
	}

	var itemErr *ItemError
	if !errors.As(err, &itemErr) {
		t.Fatalf("errors.As(*ItemError) = false for %v", err)
	}
	if itemErr.Path != "repos" || itemErr.Op != "create" {
		t.Fatalf("ItemError = {Op:%q Path:%q}, want {Op:create Path:repos}", itemErr.Op, itemErr.Path)
	}
	if !strings.HasPrefix(err.Error(), "create repos: ") {
		t.Fatalf("error text = %q, want prefix %q", err.Error(), "create repos: ")
	}
}
