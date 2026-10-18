package forge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

// serveRoutes starts a TLS server that answers each "METHOD /path" key. A
// request with no route fails the test.
func serveRoutes(t *testing.T, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func jsonReply(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func textReply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

var testRepo = Repo{Owner: "o", Name: "r"}

func githubClientFor(t *testing.T, routes map[string]http.HandlerFunc) Client {
	t.Helper()
	srv := serveRoutes(t, routes)
	return newClient(KindGitHub, srv.URL, NewToken([]byte("canary-tok")), srv.Client())
}

func TestGitHub_Repository(t *testing.T) {
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r": jsonReply(`{"default_branch":"main","permissions":{"push":true}}`),
	})
	info, err := c.Repository(context.Background(), testRepo)
	if err != nil {
		t.Fatal(err)
	}
	if info.DefaultBranch != "main" || !info.Push {
		t.Fatalf("got %+v", info)
	}
}

func TestGitHub_BranchHead(t *testing.T) {
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/git/ref/heads/main": jsonReply(`{"object":{"sha":"` + testSHA + `"}}`),
		"GET /repos/o/r/git/ref/heads/gone": textReply(http.StatusNotFound, `{"message":"Not Found"}`),
	})
	sha, ok, err := c.BranchHead(context.Background(), testRepo, "main")
	if err != nil || !ok || sha != testSHA {
		t.Fatalf("found: sha=%q ok=%v err=%v", sha, ok, err)
	}
	_, ok, err = c.BranchHead(context.Background(), testRepo, "gone")
	if err != nil || ok {
		t.Fatalf("absent: ok=%v err=%v", ok, err)
	}
}

func TestGitHub_Resolve(t *testing.T) {
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/commits/main": func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Accept"); got != "application/vnd.github.sha" {
				t.Errorf("Accept = %q", got)
			}
			_, _ = io.WriteString(w, strings.ToUpper(testSHA)+"\n")
		},
	})
	sha, err := c.Resolve(context.Background(), testRepo, "main")
	if err != nil || sha != testSHA {
		t.Fatalf("sha=%q err=%v", sha, err)
	}
}

func TestGitHub_ReadDirSortedWithoutSymlinks(t *testing.T) {
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/contents": jsonReply(`[
			{"name":"zeta","type":"file","size":1},
			{"name":"link","type":"symlink","size":0},
			{"name":"alpha","type":"dir","size":0},
			{"name":"Beta","type":"file","size":2}]`),
	})
	e, err := c.Read(context.Background(), testRepo, testSHA, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Dir || e.Revision != testSHA {
		t.Fatalf("entry %+v", e)
	}
	want := []Child{{"Beta", "file"}, {"alpha", "dir"}, {"zeta", "file"}}
	if len(e.Children) != len(want) {
		t.Fatalf("children %+v", e.Children)
	}
	for i := range want {
		if e.Children[i] != want[i] {
			t.Fatalf("child %d = %+v, want %+v", i, e.Children[i], want[i])
		}
	}
}

func TestGitHub_ReadFile(t *testing.T) {
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/contents": jsonReply(`[{"name":"Jenkinsfile","type":"file","size":11,"sha":"b1"}]`),
		"GET /repos/o/r/contents/Jenkinsfile": func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Accept"); got != "application/vnd.github.raw" {
				t.Errorf("Accept = %q", got)
			}
			if got := r.Header.Get("Range"); got != "bytes=0-10" {
				t.Errorf("Range = %q", got)
			}
			_, _ = io.WriteString(w, "pipeline {}")
		},
	})
	e, err := c.Read(context.Background(), testRepo, testSHA, "Jenkinsfile", 0)
	if err != nil {
		t.Fatal(err)
	}
	if e.Dir || e.Size != 11 || string(e.Content) != "pipeline {}" {
		t.Fatalf("entry %+v", e)
	}
}

func TestGitHub_ReadStopsAtLength(t *testing.T) {
	big := strings.Repeat("x", 10000)
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/contents": jsonReply(`[{"name":"big.txt","type":"file","size":10000,"sha":"b2"}]`),
		"GET /repos/o/r/contents/big.txt": func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Range"); got != "bytes=0-4095" {
				t.Errorf("Range = %q", got)
			}
			// The server ignores Range and sends the whole file.
			_, _ = io.WriteString(w, big)
		},
	})
	e, err := c.Read(context.Background(), testRepo, testSHA, "big.txt", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Content) != 4096 || e.Size != 10000 {
		t.Fatalf("content %d bytes, size %d", len(e.Content), e.Size)
	}
}

func TestGitHub_CreateBranch(t *testing.T) {
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"POST /repos/o/r/git/refs": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"ref":"refs/heads/b"`) || !strings.Contains(string(body), testSHA) {
				t.Errorf("body %s", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		},
	})
	if err := c.CreateBranch(context.Background(), testRepo, "b", testSHA); err != nil {
		t.Fatal(err)
	}
}

func TestGitHub_PutFileCreate(t *testing.T) {
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/contents": jsonReply(`[]`),
		"PUT /repos/o/r/contents/Jenkinsfile": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"sha"`) {
				t.Errorf("create sent a sha: %s", body)
			}
			if !strings.Contains(string(body), `"branch":"b"`) {
				t.Errorf("body %s", body)
			}
			_, _ = io.WriteString(w, `{}`)
		},
	})
	if err := c.PutFile(context.Background(), testRepo, "b", "Jenkinsfile", []byte("x"), "msg"); err != nil {
		t.Fatal(err)
	}
}

func TestGitHub_PutFileReplace(t *testing.T) {
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/contents": jsonReply(`[{"name":"Jenkinsfile","type":"file","size":1,"sha":"blob123"}]`),
		"PUT /repos/o/r/contents/Jenkinsfile": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"sha":"blob123"`) {
				t.Errorf("replace sent no blob sha: %s", body)
			}
			_, _ = io.WriteString(w, `{}`)
		},
	})
	if err := c.PutFile(context.Background(), testRepo, "b", "Jenkinsfile", []byte("x"), "msg"); err != nil {
		t.Fatal(err)
	}
}

func TestGitHub_OpenPullRequest(t *testing.T) {
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"POST /repos/o/r/pulls": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"head":"varroa/propose-pipeline"`) {
				t.Errorf("body %s", body)
			}
			_, _ = io.WriteString(w, `{"html_url":"https://example.com/o/r/pull/7","number":7}`)
		},
	})
	pr, err := c.OpenPullRequest(context.Background(), testRepo, "main", "varroa/propose-pipeline", "t", "b")
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 7 || pr.URL != "https://example.com/o/r/pull/7" {
		t.Fatalf("pr %+v", pr)
	}
}

func TestGitHub_DeleteBranch(t *testing.T) {
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"DELETE /repos/o/r/git/refs/heads/b": textReply(http.StatusNoContent, ""),
	})
	if err := c.DeleteBranch(context.Background(), testRepo, "b"); err != nil {
		t.Fatal(err)
	}
}

func TestGitHub_BearerHeader(t *testing.T) {
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r": func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer canary-tok" {
				t.Errorf("Authorization = %q", got)
			}
			if got := r.Header.Get("X-Github-Api-Version"); got != "2022-11-28" {
				t.Errorf("api version = %q", got)
			}
			_, _ = io.WriteString(w, `{"default_branch":"main"}`)
		},
	})
	if _, err := c.Repository(context.Background(), testRepo); err != nil {
		t.Fatal(err)
	}
}

// TestGitHub_LookupPastListingCap finds a child that a capped listing omits.
// The object media type answers the path directly.
func TestGitHub_LookupPastListingCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < listingCap; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"name":"f` + strconv.Itoa(i) + `","type":"file","size":1,"sha":"s"}`)
	}
	b.WriteString("]")
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/contents/src": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, b.String())
		},
		"GET /repos/o/r/contents/src/main.go": func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Accept"); got != "application/vnd.github.object" {
				t.Errorf("Accept = %q", got)
			}
			_, _ = io.WriteString(w, `{"type":"file","size":5,"sha":"blob7","name":"main.go"}`)
		},
		"PUT /repos/o/r/contents/src/main.go": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"sha":"blob7"`) {
				t.Errorf("replace sent no blob sha: %s", body)
			}
			_, _ = io.WriteString(w, `{}`)
		},
	})
	if err := c.PutFile(context.Background(), testRepo, "b", "src/main.go", []byte("x"), "msg"); err != nil {
		t.Fatal(err)
	}
}

// TestGitHub_ReadCappedDirectoryRefused refuses a directory listing that
// reached the entry cap, instead of returning it as complete.
func TestGitHub_ReadCappedDirectoryRefused(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < listingCap; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"name":"f` + strconv.Itoa(i) + `","type":"file","size":1,"sha":"s"}`)
	}
	b.WriteString("]")
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/contents": jsonReply(`[{"name":"src","type":"dir","size":0}]`),
		"GET /repos/o/r/contents/src": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, b.String())
		},
	})
	if _, err := c.Read(context.Background(), testRepo, testSHA, "src", 0); !errors.Is(err, ErrListingTruncated) {
		t.Fatalf("err %v, want ErrListingTruncated", err)
	}
}

// TestHostURL_BracketsIPv6 keeps the colons of an IPv6 literal out of the port.
func TestHostURL_BracketsIPv6(t *testing.T) {
	if got := hostURL("fd00::10"); got != "[fd00::10]" {
		t.Fatalf("ipv6 host %q", got)
	}
	if got := hostURL("::ffff:10.0.0.1"); got != "[::ffff:10.0.0.1]" {
		t.Fatalf("mapped ipv6 host %q", got)
	}
	if got := hostURL("git.example.org"); got != "git.example.org" {
		t.Fatalf("dns host %q", got)
	}
}

func TestGitHub_StatusMapping(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrAuth},
		{http.StatusForbidden, ErrForbidden},
		{http.StatusNotFound, ErrNotFound},
		{http.StatusUnprocessableEntity, ErrConflict},
	}
	for _, tc := range cases {
		c := githubClientFor(t, map[string]http.HandlerFunc{
			"GET /repos/o/r": textReply(tc.status, "canary-tok in body"),
		})
		_, err := c.Repository(context.Background(), testRepo)
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: got %v, want %v", tc.status, err, tc.want)
		}
	}
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r": textReply(http.StatusInternalServerError, "canary-tok in body"),
	})
	_, err := c.Repository(context.Background(), testRepo)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusInternalServerError {
		t.Fatalf("500: got %v", err)
	}
}
