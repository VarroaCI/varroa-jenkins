package forge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// giteaClientFor starts a TLS server and builds the Gitea client through New,
// so the test covers the /api/v1 base that New derives from the host.
func giteaClientFor(t *testing.T, routes map[string]http.HandlerFunc) Client {
	t.Helper()
	prefixed := make(map[string]http.HandlerFunc, len(routes))
	for k, v := range routes {
		method, path, _ := strings.Cut(k, " ")
		prefixed[method+" /api/v1"+path] = v
	}
	srv := serveRoutes(t, prefixed)
	host := strings.TrimPrefix(srv.URL, "https://")
	return New(KindGitea, host, NewToken([]byte("canary-tok")), srv.Client())
}

func TestGitea_Repository(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r": jsonReply(`{"default_branch":"trunk","permissions":{"push":false}}`),
	})
	info, err := c.Repository(context.Background(), testRepo)
	if err != nil {
		t.Fatal(err)
	}
	if info.DefaultBranch != "trunk" || info.Push {
		t.Fatalf("got %+v", info)
	}
}

func TestGitea_BranchHead(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/branches/main": jsonReply(`{"commit":{"id":"` + testSHA + `"}}`),
		"GET /repos/o/r/branches/gone": textReply(http.StatusNotFound, "missing"),
	})
	sha, ok, err := c.BranchHead(context.Background(), testRepo, "main")
	if err != nil || !ok || sha != testSHA {
		t.Fatalf("found: sha=%q ok=%v err=%v", sha, ok, err)
	}
	if _, ok, err := c.BranchHead(context.Background(), testRepo, "gone"); err != nil || ok {
		t.Fatalf("absent: ok=%v err=%v", ok, err)
	}
}

func TestGitea_Resolve(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/git/commits/main": jsonReply(`{"sha":"` + testSHA + `"}`),
	})
	sha, err := c.Resolve(context.Background(), testRepo, "main")
	if err != nil || sha != testSHA {
		t.Fatalf("sha=%q err=%v", sha, err)
	}
}

func TestGitea_ReadDirAndFile(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/contents": jsonReply(`[
			{"name":"b","type":"dir","size":0},
			{"name":"a.txt","type":"file","size":5,"sha":"x"}]`),
		"GET /repos/o/r/raw/a.txt": func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Range"); got != "bytes=0-4" {
				t.Errorf("Range = %q", got)
			}
			_, _ = io.WriteString(w, "hello")
		},
	})
	dir, err := c.Read(context.Background(), testRepo, testSHA, "", 0)
	if err != nil || !dir.Dir || len(dir.Children) != 2 || dir.Children[0].Name != "a.txt" {
		t.Fatalf("dir %+v err %v", dir, err)
	}
	e, err := c.Read(context.Background(), testRepo, testSHA, "a.txt", 0)
	if err != nil || string(e.Content) != "hello" || e.Size != 5 {
		t.Fatalf("file %+v err %v", e, err)
	}
}

func TestGitea_CreateBranch(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"POST /repos/o/r/branches": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"new_branch_name":"b"`) || !strings.Contains(string(body), `"old_ref_name":"`+testSHA+`"`) {
				t.Errorf("body %s", body)
			}
			_, _ = io.WriteString(w, `{}`)
		},
	})
	if err := c.CreateBranch(context.Background(), testRepo, "b", testSHA); err != nil {
		t.Fatal(err)
	}
}

func TestGitea_PutFileCreatePOST(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/contents": jsonReply(`[]`),
		"POST /repos/o/r/contents/Jenkinsfile": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{}`)
		},
	})
	if err := c.PutFile(context.Background(), testRepo, "b", "Jenkinsfile", []byte("x"), "msg"); err != nil {
		t.Fatal(err)
	}
}

func TestGitea_PutFileReplacePUT(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/contents": jsonReply(`[{"name":"Jenkinsfile","type":"file","size":1,"sha":"blob9"}]`),
		"PUT /repos/o/r/contents/Jenkinsfile": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"sha":"blob9"`) {
				t.Errorf("body %s", body)
			}
			_, _ = io.WriteString(w, `{}`)
		},
	})
	if err := c.PutFile(context.Background(), testRepo, "b", "Jenkinsfile", []byte("x"), "msg"); err != nil {
		t.Fatal(err)
	}
}

func TestGitea_OpenPullRequest(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"POST /repos/o/r/pulls": jsonReply(`{"html_url":"https://example.com/o/r/pulls/3","number":3}`),
	})
	pr, err := c.OpenPullRequest(context.Background(), testRepo, "main", "varroa/propose-pipeline", "t", "b")
	if err != nil || pr.Number != 3 {
		t.Fatalf("pr %+v err %v", pr, err)
	}
}

func TestGitea_DeleteBranch(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"DELETE /repos/o/r/branches/b": textReply(http.StatusNoContent, ""),
	})
	if err := c.DeleteBranch(context.Background(), testRepo, "b"); err != nil {
		t.Fatal(err)
	}
}

func TestGitea_TokenHeader(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r": func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "token canary-tok" {
				t.Errorf("Authorization = %q", got)
			}
			_, _ = io.WriteString(w, `{"default_branch":"main"}`)
		},
	})
	if _, err := c.Repository(context.Background(), testRepo); err != nil {
		t.Fatal(err)
	}
}

func TestGitea_NotFoundMapping(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r": textReply(http.StatusNotFound, "canary-tok"),
	})
	if _, err := c.Repository(context.Background(), testRepo); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}
