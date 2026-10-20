package forgeops

import (
	"context"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	"github.com/varroaci/varroa-jenkins/internal/forgeurl"
)

// probeVerb is one verb whose repository probe runs before the write.
type probeVerb struct {
	name string
	// setup prepares the credential b for the verb.
	setup func(b *fakeClient)
	// seed prepares the fixture for the verb.
	seed func(t *testing.T, fx *fixture)
	// run calls the verb and returns its error. The reply-shaped verbs
	// report through rep and the commit-status verb through err.
	run func(fx *fixture) (ok bool, code, msg string, err error)
	// succeeded reports whether b took the write.
	succeeded func(b *fakeClient) bool
}

func probeVerbs() []probeVerb {
	return []probeVerb{
		{
			name:  "check",
			setup: func(*fakeClient) {},
			run: func(fx *fixture) (bool, string, string, error) {
				rep := fx.ops.Check(context.Background(), checkReq())
				return rep.OK, rep.Code, rep.Error, nil
			},
			succeeded: func(b *fakeClient) bool { return true },
		},
		{
			name:  "read",
			setup: func(b *fakeClient) { b.files = map[string]forge.Entry{"": {Dir: true}} },
			run: func(fx *fixture) (bool, string, string, error) {
				rep := fx.ops.Read(context.Background(), readReq(""))
				return rep.OK, rep.Code, rep.Error, nil
			},
			succeeded: func(b *fakeClient) bool { return true },
		},
		{
			name:  "open_issue",
			setup: func(*fakeClient) {},
			seed:  func(t *testing.T, fx *fixture) { seedAgent(t, fx, nil) },
			run: func(fx *fixture) (bool, string, string, error) {
				rep := fx.ops.Issue(context.Background(), issueReq(), "agents-payments")
				return rep.OK, rep.Code, rep.Error, nil
			},
			succeeded: func(b *fakeClient) bool { return len(b.issueLabels) == 1 },
		},
		{
			name: "open_pull_request",
			setup: func(b *fakeClient) {
				b.pr = forge.PullRequest{URL: prURL, Number: 4}
			},
			run: func(fx *fixture) (bool, string, string, error) {
				rep := fx.ops.PullRequest(context.Background(), prReq())
				return rep.OK, rep.Code, rep.Error, nil
			},
			succeeded: func(b *fakeClient) bool { return true },
		},
		{
			name: "EndPendingStatus",
			setup: func(b *fakeClient) {
				b.statuses = []forge.CommitStatus{{Context: "ci", State: forge.StatePending, TargetURL: statusBuildURL}}
			},
			run: func(fx *fixture) (bool, string, string, error) {
				n, err := fx.ops.EndPendingStatus(context.Background(), endReq())
				return err == nil && n == 1, "", "", err
			},
			succeeded: func(b *fakeClient) bool { return len(b.statusWrites) == 1 },
		},
	}
}

// TestRateLimitedProbe_SkipsOnEveryVerb checks that a rate-limited probe on the
// first credential does not stop any verb. The second credential serves it.
func TestRateLimitedProbe_SkipsOnEveryVerb(t *testing.T) {
	for _, v := range probeVerbs() {
		t.Run(v.name, func(t *testing.T) {
			fx := newFixture(t, "agents-payments")
			if v.seed != nil {
				v.seed(t, fx)
			}
			a := pushable()
			a.repoErr = rateLimitErr()
			b := pushable()
			v.setup(b)
			twoCredentials(fx, a, b)

			ok, code, msg, err := v.run(fx)
			if err != nil {
				t.Fatalf("verb error %v", err)
			}
			if !ok {
				t.Fatalf("verb refused: code %s, message %q", code, msg)
			}
			if !v.succeeded(b) {
				t.Fatalf("credential b did not take the write")
			}
		})
	}
}

// TestRateLimitedProbe_EveryCredentialAnswersRateLimit checks that a verb whose
// every credential is rate-limited answers forge_unreachable with the reset
// time, and that no write runs.
func TestRateLimitedProbe_EveryCredentialAnswersRateLimit(t *testing.T) {
	for _, v := range probeVerbs() {
		t.Run(v.name, func(t *testing.T) {
			fx := newFixture(t, "agents-payments")
			if v.seed != nil {
				v.seed(t, fx)
			}
			a := pushable()
			a.repoErr = rateLimitErr()
			b := pushable()
			b.repoErr = rateLimitErr()
			v.setup(b)
			twoCredentials(fx, a, b)

			ok, code, msg, err := v.run(fx)
			if ok {
				t.Fatalf("verb succeeded with every credential rate-limited")
			}
			if err != nil {
				code = codeOf(t, err)
				msg = err.Error()
			}
			if code != forgeapi.CodeForgeUnreachable {
				t.Fatalf("code %q, want %s", code, forgeapi.CodeForgeUnreachable)
			}
			for _, want := range []string{"rate limit", "2026-10-20T12:00:00Z", "a"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("message %q lacks %q", msg, want)
				}
			}
			if wroteAnything(a) || wroteAnything(b) {
				t.Fatalf("a write ran on a rate-limited client")
			}
		})
	}
}

// TestCheck_RateLimitWinsOverScopeRefusal checks that a probe error on one
// credential wins over a scope refusal on another.
func TestCheck_RateLimitWinsOverScopeRefusal(t *testing.T) {
	cases := map[string]error{
		"read-only": nil,
		"auth":      forge.ErrAuth,
	}
	for name, bErr := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newFixture(t, "agents-payments")
			a := pushable()
			a.repoErr = rateLimitErr()
			b := &fakeClient{info: forge.Info{DefaultBranch: "main"}, repoErr: bErr}
			twoCredentials(fx, a, b)

			rep := fx.ops.Check(context.Background(), checkReq())
			if rep.OK || rep.Code != forgeapi.CodeForgeUnreachable {
				t.Fatalf("reply code %q, want %s", rep.Code, forgeapi.CodeForgeUnreachable)
			}
			if !strings.Contains(rep.Error, "2026-10-20T12:00:00Z") {
				t.Fatalf("error %q lacks the reset time", rep.Error)
			}
		})
	}
}

// TestCheck_OtherProbeErrorSkipsCredential checks that a 502 on one credential
// skips it, and the next credential serves the check.
func TestCheck_OtherProbeErrorSkipsCredential(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	a := pushable()
	a.repoErr = &forge.StatusError{Status: 502}
	b := pushable()
	twoCredentials(fx, a, b)

	rep := fx.ops.Check(context.Background(), checkReq())
	if !rep.OK || rep.CredentialID != "b" {
		t.Fatalf("reply %+v, want OK with credential b", rep)
	}
}

// TestPickCredential_FirstReaderInIDOrder checks that a read takes the first
// credential in id order that may read, even a read-only one.
func TestPickCredential_FirstReaderInIDOrder(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	a := &fakeClient{info: forge.Info{DefaultBranch: "main"}}
	b := pushable()
	twoCredentials(fx, a, b)

	repo, err := forgeurl.NormalizeRepoURL(testRepoURL)
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	creds, err := fx.ops.chooseCredential(context.Background(), "payments", repo.Host)
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	w, err := fx.ops.pickReader(context.Background(), repo.Host, forge.Repo{Owner: repo.Owner, Name: repo.Name}, creds)
	if err != nil {
		t.Fatalf("pickReader: %v", err)
	}
	if w.CredentialID != "a" {
		t.Fatalf("pickReader chose %q, want a", w.CredentialID)
	}
}

// wroteAnything reports whether the fake recorded any write call.
func wroteAnything(c *fakeClient) bool {
	return len(c.calls) > 0 || len(c.issueLabels) > 0 || len(c.statusWrites) > 0 || len(c.prBodies) > 0
}
