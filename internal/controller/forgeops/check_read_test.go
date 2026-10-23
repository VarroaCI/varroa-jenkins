package forgeops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// oneWriter sets a single pushing credential "a" that answers with c.
func oneWriter(fx *fixture, c *fakeClient) {
	fx.secrets.addCred("sa", "a", "tok-a")
	fx.clients[forge.NewToken([]byte("tok-a"))] = c
}

func noForgeCall(fx *fixture) {
	fx.detectE = errors.New("forge must not be called")
}

func TestCheck_Success(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	oneWriter(fx, pushable())
	rep := fx.ops.Check(context.Background(), checkReq())
	if !rep.OK || rep.Forge != string(forge.KindGitea) || rep.BaseBranch != "main" || rep.CredentialID != "a" {
		t.Fatalf("reply %+v", rep)
	}
}

func TestCheck_Codes(t *testing.T) {
	cases := []struct {
		name  string
		setup func(fx *fixture)
		req   forgeapi.CheckRequest
		want  string
	}{
		{"empty tenant", func(*fixture) {}, forgeapi.CheckRequest{Repo: testRepoURL}, forgeapi.CodeInvalidRequest},
		{"bad repo", func(*fixture) {}, forgeapi.CheckRequest{Tenant: "payments", Repo: "http://x/y/z"}, forgeapi.CodeRepoURLInvalid},
		{"no credential", func(*fixture) {}, checkReq(), forgeapi.CodeNoMatchingCredential},
		{"unsupported", func(fx *fixture) {
			fx.secrets.addCred("sa", "a", "tok-a")
			fx.detectE = forge.ErrUnsupported
		}, checkReq(), forgeapi.CodeForgeUnsupported},
		{"auth failed", func(fx *fixture) {
			oneWriter(fx, &fakeClient{repoErr: forge.ErrAuth})
		}, checkReq(), forgeapi.CodeForgeAuthFailed},
		{"write denied", func(fx *fixture) {
			oneWriter(fx, &fakeClient{info: forge.Info{DefaultBranch: "main"}})
		}, checkReq(), forgeapi.CodeForgeWriteDenied},
		{"unreachable", func(fx *fixture) {
			oneWriter(fx, &fakeClient{repoErr: errors.New("boom")})
		}, checkReq(), forgeapi.CodeForgeUnreachable},
		{"empty repository", func(fx *fixture) {
			c := pushable()
			c.branches = map[string]string{}
			oneWriter(fx, c)
		}, checkReq(), forgeapi.CodeNotFound},
		{"jenkinsfile exists", func(fx *fixture) {
			c := pushable()
			c.files["Jenkinsfile"] = forge.Entry{Size: 3, Content: []byte("x")}
			oneWriter(fx, c)
		}, checkReq(), forgeapi.CodeJenkinsfileExists},
		{"link named Jenkinsfile", func(fx *fixture) {
			c := pushable()
			c.files["Jenkinsfile"] = forge.Entry{Other: true}
			oneWriter(fx, c)
		}, checkReq(), forgeapi.CodeJenkinsfileExists},
		{"proposal branch exists", func(fx *fixture) {
			c := pushable()
			c.branches[forgeapi.ProposalBranch] = testSHA
			oneWriter(fx, c)
		}, checkReq(), forgeapi.CodeProposalBranchExists},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t, "agents-payments")
			tc.setup(fx)
			rep := fx.ops.Check(context.Background(), tc.req)
			if rep.OK || rep.Code != tc.want {
				t.Fatalf("reply %+v, want code %s", rep.Status, tc.want)
			}
		})
	}
}

func TestCheck_DirectoryJenkinsfileRefused(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.files["Jenkinsfile"] = forge.Entry{Dir: true}
	oneWriter(fx, c)
	rep := fx.ops.Check(context.Background(), checkReq())
	if rep.OK || rep.Code != forgeapi.CodeJenkinsfileExists {
		t.Fatalf("reply %+v", rep.Status)
	}
}

func TestRead_PathRule(t *testing.T) {
	for _, p := range []string{"..", "a/../b", "/abs", "a//b"} {
		fx := newFixture(t, "agents-payments")
		noForgeCall(fx)
		rep := fx.ops.Read(context.Background(), readReq(p))
		if rep.OK || rep.Code != forgeapi.CodeInvalidRequest {
			t.Fatalf("path %q: reply %+v", p, rep.Status)
		}
	}
}

func TestRead_DefaultRevisionResolvesToSHA(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.files["README.md"] = forge.Entry{Size: 2, Content: []byte("hi")}
	oneWriter(fx, c)
	rep := fx.ops.Read(context.Background(), readReq("README.md"))
	if !rep.OK || rep.Revision != testSHA || rep.ReadFile == nil || rep.Content != "hi" {
		t.Fatalf("reply %+v", rep)
	}
}

func TestRead_GivenRevision(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.files["README.md"] = forge.Entry{Size: 2, Content: []byte("hi")}
	oneWriter(fx, c)
	req := readReq("README.md")
	req.Revision = "feat/x"
	rep := fx.ops.Read(context.Background(), req)
	if !rep.OK || rep.Revision != testSHA {
		t.Fatalf("reply %+v", rep)
	}
}

func TestRead_LimitCutsWithSign(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.files["big.txt"] = forge.Entry{Size: 10000, Content: []byte(strings.Repeat("x", 10000))}
	oneWriter(fx, c)
	req := readReq("big.txt")
	req.Limit = 4096
	rep := fx.ops.Read(context.Background(), req)
	if !rep.OK || rep.Offset != 0 || rep.Length != 4096 || !rep.Truncated || rep.Size != 10000 {
		t.Fatalf("reply %+v file %+v", rep.Status, rep.ReadFile)
	}
}

func TestRead_LargeFileCutNotRefused(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.files["big.txt"] = forge.Entry{Size: 400000, Content: []byte(strings.Repeat("y", 400000))}
	oneWriter(fx, c)
	rep := fx.ops.Read(context.Background(), readReq("big.txt"))
	if !rep.OK || rep.Length != forgeapi.MaxRead || !rep.Truncated {
		t.Fatalf("reply %+v file %+v", rep.Status, rep.ReadFile)
	}
}

func TestRead_DirectoryEntriesSorted(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.files["lib"] = forge.Entry{Dir: true, Children: []forge.Child{{Name: "a.groovy", Type: "file"}, {Name: "vars", Type: "dir"}}}
	oneWriter(fx, c)
	rep := fx.ops.Read(context.Background(), readReq("lib"))
	if !rep.OK || rep.ReadDir == nil || len(rep.Entries) != 2 || rep.Entries[1].Type != "dir" {
		t.Fatalf("reply %+v", rep)
	}
}

// TestRead_ReadOnlyCredentialServesRead checks that a credential which can
// read but not push serves a read. The read path does not need push access.
func TestRead_ReadOnlyCredentialServesRead(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	fx.secrets.addCred("sa", "a", "tok-a")
	fx.secrets.addCred("sb", "b", "tok-b")
	reader := &fakeClient{info: forge.Info{DefaultBranch: "main"}, resolveSHA: testSHA,
		files: map[string]forge.Entry{"": {Dir: true, Children: []forge.Child{{Name: "Jenkinsfile", Type: "file"}}}}}
	fx.clients[forge.NewToken([]byte("tok-a"))] = reader
	fx.clients[forge.NewToken([]byte("tok-b"))] = pushable()
	rep := fx.ops.Read(context.Background(), readReq("."))
	if !rep.OK || rep.ReadDir == nil || len(rep.Entries) != 1 {
		t.Fatalf("reply %+v", rep)
	}
}

// TestRead_RevisionAnswersThatRevision checks that a read at a named revision
// answers the commit of that revision, not the default branch.
func TestRead_RevisionAnswersThatRevision(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.revs = map[string]string{"main": "sha-new", "v1": "sha-old"}
	c.bySHA = map[string]map[string]forge.Entry{
		"sha-new": {"README.md": {Size: 3, Content: []byte("new")}},
		"sha-old": {"README.md": {Size: 3, Content: []byte("old")}},
	}
	oneWriter(fx, c)
	req := readReq("README.md")
	req.Revision = "v1"
	rep := fx.ops.Read(context.Background(), req)
	if !rep.OK || rep.Revision != "sha-old" || rep.Content != "old" {
		t.Fatalf("reply %+v, want the v1 commit", rep)
	}
}

// TestRead_ForeignHostNoCredential checks that a credential whose allowed-hosts
// annotation does not list the repository host is never used, so no forge
// call is made.
func TestRead_ForeignHostNoCredential(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	fx.secrets.addCred("sa", "a", "tok-a")
	fx.secrets.ann["sa"]["varroa.dev/allowed-hosts"] = "other.example.org"
	fx.clients[forge.NewToken([]byte("tok-a"))] = pushable()
	noForgeCall(fx)
	rep := fx.ops.Read(context.Background(), readReq("README.md"))
	if rep.OK || rep.Code != forgeapi.CodeNoMatchingCredential {
		t.Fatalf("reply %+v, want no_matching_credential", rep)
	}
	if len(fx.built) != 0 {
		t.Fatalf("a client was built for a foreign host: %d", len(fx.built))
	}
}

func TestRead_LimitOutOfRange(t *testing.T) {
	for _, lim := range []int{-1, forgeapi.MaxRead + 1} {
		fx := newFixture(t, "agents-payments")
		noForgeCall(fx)
		req := readReq("a")
		req.Limit = lim
		if rep := fx.ops.Read(context.Background(), req); rep.Code != forgeapi.CodeInvalidRequest {
			t.Fatalf("limit %d: reply %+v", lim, rep.Status)
		}
	}
}

func TestRead_NotFound(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	oneWriter(fx, pushable())
	rep := fx.ops.Read(context.Background(), readReq("missing.txt"))
	if rep.Code != forgeapi.CodeNotFound {
		t.Fatalf("reply %+v", rep.Status)
	}
}

func TestRead_LinkIsNotFound(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.files["link"] = forge.Entry{Other: true}
	oneWriter(fx, c)
	rep := fx.ops.Read(context.Background(), readReq("link"))
	if rep.Code != forgeapi.CodeNotFound {
		t.Fatalf("reply %+v", rep.Status)
	}
}

func TestRead_BinaryContent(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.files["logo.bin"] = forge.Entry{Size: 2, Content: []byte{0xff, 0xfe}}
	oneWriter(fx, c)
	rep := fx.ops.Read(context.Background(), readReq("logo.bin"))
	if !rep.OK || rep.Encoding != "binary" || rep.Content != "" || !rep.Truncated || rep.Size != 2 {
		t.Fatalf("reply %+v file %+v", rep.Status, rep.ReadFile)
	}
}

func TestRead_OversizeEncodedReplyShrinks(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	body := bytes.Repeat([]byte{0x01}, 256<<10)
	c.files["ctl.txt"] = forge.Entry{Size: int64(len(body)), Content: body}
	oneWriter(fx, c)
	rep := fx.ops.Read(context.Background(), readReq("ctl.txt"))
	if !rep.OK || !rep.Truncated || rep.Length == 0 {
		t.Fatalf("reply %+v file %+v", rep.Status, rep.ReadFile)
	}
	enc := encodeReply(rep)
	if len(enc) > textReplyLimit {
		t.Fatalf("encoded %d bytes, above the limit", len(enc))
	}
	if int64(len(rep.Content)) != rep.Length {
		t.Fatalf("length %d does not match content %d", rep.Length, len(rep.Content))
	}
}

func TestReply_JSONShape(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.files["README.md"] = forge.Entry{Size: 2, Content: []byte("hi")}
	c.files["lib"] = forge.Entry{Dir: true, Children: []forge.Child{{Name: "a", Type: "file"}}}
	oneWriter(fx, c)
	keys := func(v any) map[string]bool {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(encodeReply(v), &m); err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for k := range m {
			out[k] = true
		}
		return out
	}
	file := keys(fx.ops.Read(context.Background(), readReq("README.md")))
	wantFile := []string{"ok", "revision", "path", "size", "offset", "length", "truncated", "content"}
	if len(file) != len(wantFile) {
		t.Fatalf("file keys %v", file)
	}
	for _, k := range wantFile {
		if !file[k] {
			t.Fatalf("file reply lacks %q: %v", k, file)
		}
	}
	dir := keys(fx.ops.Read(context.Background(), readReq("lib")))
	for _, k := range []string{"ok", "revision", "path", "entries"} {
		if !dir[k] {
			t.Fatalf("dir reply lacks %q: %v", k, dir)
		}
	}
	if len(dir) != 4 {
		t.Fatalf("dir keys %v", dir)
	}
}

// TestCheck_RateLimitedProbeIsUnreachable checks that a rate-limited probe
// answers unreachable, not a write refusal or an auth failure.
func TestCheck_RateLimitedProbeIsUnreachable(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	oneWriter(fx, &fakeClient{repoErr: &forge.RateLimitError{Status: 429}})
	rep := fx.ops.Check(context.Background(), checkReq())
	if rep.OK || rep.Code != forgeapi.CodeForgeUnreachable {
		t.Fatalf("reply %+v, want code %s", rep.Status, forgeapi.CodeForgeUnreachable)
	}
	if !strings.Contains(rep.Error, "retry later") {
		t.Fatalf("error %q lacks the retry hint", rep.Error)
	}
}
