package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestDetect_GitHubNoCall(t *testing.T) {
	// A nil client proves that no request is made for github.com.
	kind, err := Detect(context.Background(), nil, "github.com")
	if err != nil || kind != KindGitHub {
		t.Fatalf("kind %q err %v", kind, err)
	}
}

func TestDetect_Gitea(t *testing.T) {
	srv := serveRoutes(t, map[string]http.HandlerFunc{
		"GET /api/v1/version": jsonReply(`{"version":"1.22.0"}`),
	})
	kind, err := Detect(context.Background(), srv.Client(), strings.TrimPrefix(srv.URL, "https://"))
	if err != nil || kind != KindGitea {
		t.Fatalf("kind %q err %v", kind, err)
	}
}

func TestDetect_UnsupportedHTML(t *testing.T) {
	srv := serveRoutes(t, map[string]http.HandlerFunc{
		"GET /api/v1/version": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<html>login</html>")
		},
	})
	_, err := Detect(context.Background(), srv.Client(), strings.TrimPrefix(srv.URL, "https://"))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err %v, want ErrUnsupported", err)
	}
}

func TestDetect_Unsupported404(t *testing.T) {
	srv := serveRoutes(t, map[string]http.HandlerFunc{
		"GET /api/v1/version": textReply(http.StatusNotFound, "nope"),
	})
	_, err := Detect(context.Background(), srv.Client(), strings.TrimPrefix(srv.URL, "https://"))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err %v, want ErrUnsupported", err)
	}
}

func TestToken_Redacted(t *testing.T) {
	tok := NewToken([]byte("canary-tok"))
	var buf strings.Builder
	for _, f := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		fmt.Fprintf(&buf, f, tok)
	}
	js, err := json.Marshal(struct{ T Token }{tok})
	if err != nil {
		t.Fatal(err)
	}
	buf.Write(js)

	logBuf := &strings.Builder{}
	jsonLog := slog.New(slog.NewJSONHandler(logBuf, nil))
	jsonLog.Info("probe", "token", tok)
	textLog := slog.New(slog.NewTextHandler(logBuf, nil))
	textLog.Info("probe", "token", tok)
	buf.WriteString(logBuf.String())

	out := buf.String()
	if strings.Contains(out, "canary-tok") {
		t.Fatalf("token value leaked: %s", out)
	}
	if !strings.Contains(out, "[redacted]") {
		t.Fatalf("no redaction marker: %s", out)
	}
}

func TestStatusError_NoBody(t *testing.T) {
	srv := serveRoutes(t, map[string]http.HandlerFunc{
		"GET /repos/o/r": textReply(http.StatusInternalServerError, "canary-tok echoed by the server"),
	})
	c := newClient(KindGitHub, srv.URL, NewToken([]byte("canary-tok")), srv.Client())
	_, err := c.Repository(context.Background(), testRepo)
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(err.Error(), "canary") {
		t.Fatalf("body reached the error: %v", err)
	}
}
