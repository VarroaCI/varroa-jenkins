package instructions

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// fakeReader answers one canned reply, or err, and records the request.
type fakeReader struct {
	reply forgeapi.ReadReply
	err   error
	got   forgeapi.ReadRequest
}

func (f *fakeReader) Read(_ context.Context, _ string, req forgeapi.ReadRequest) (forgeapi.ReadReply, error) {
	f.got = req
	return f.reply, f.err
}

func agent() *v1alpha1.TenantAgent {
	return &v1alpha1.TenantAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"},
		Spec: v1alpha1.TenantAgentSpec{
			TenantRef:    v1alpha1.TenantAgentTenantRef{Name: "payments"},
			Instructions: &v1alpha1.TenantAgentInstructions{Source: "repo"},
		},
	}
}

func source() *v1alpha1.CatalogSource {
	return &v1alpha1.CatalogSource{Spec: v1alpha1.CatalogSourceSpec{RepoURL: "https://git.example.org/o/r"}}
}

func okFile(content string) forgeapi.ReadReply {
	return forgeapi.ReadReply{
		Status:   forgeapi.Status{OK: true},
		Revision: "sha-1",
		ReadFile: &forgeapi.ReadFile{Size: int64(len(content)), Content: content},
	}
}

// TestRead_Reasons checks the reason of each outcome.
func TestRead_Reasons(t *testing.T) {
	big := strings.Repeat("a", MaxBytes+1)
	cases := []struct {
		name   string
		ta     func() *v1alpha1.TenantAgent
		src    func() *v1alpha1.CatalogSource
		rev    string
		reply  forgeapi.ReadReply
		err    error
		reason string
	}{
		{name: "Resolved", reason: v1alpha1.TenantAgentReasonResolved, reply: okFile("hello")},
		{name: "NotDefaultBranch", src: func() *v1alpha1.CatalogSource {
			s := source()
			s.Spec.Revision = "v1"
			return s
		}, reason: v1alpha1.TenantAgentReasonNotDefaultBranch},
		{name: "NotSynced", rev: "-", reason: v1alpha1.TenantAgentReasonNotSynced},
		{name: "NotFound", src: func() *v1alpha1.CatalogSource { return nil }, reason: v1alpha1.TenantAgentReasonNotFound},
		{name: "OCISourceHasNoGitRepo", src: func() *v1alpha1.CatalogSource {
			s := source()
			s.Spec.RepoURL = ""
			return s
		}, reason: v1alpha1.TenantAgentReasonNotFound},
		{name: "NotConfigured", ta: func() *v1alpha1.TenantAgent {
			a := agent()
			a.Spec.Instructions = nil
			return a
		}, reason: v1alpha1.TenantAgentReasonNotConfigured},
		{name: "Directory", reply: forgeapi.ReadReply{Status: forgeapi.Status{OK: true}, ReadDir: &forgeapi.ReadDir{}}, reason: v1alpha1.TenantAgentReasonNotFound},
		{name: "ReplyNotFound", reply: forgeapi.ReadReply{Status: forgeapi.Status{Code: forgeapi.CodeNotFound}}, reason: v1alpha1.TenantAgentReasonNotFound},
		{name: "TooLarge", reply: okFile(big), reason: v1alpha1.TenantAgentReasonTooLarge},
		{name: "Binary", reply: forgeapi.ReadReply{Status: forgeapi.Status{OK: true}, ReadFile: &forgeapi.ReadFile{Size: 4, Encoding: "binary"}}, reason: v1alpha1.TenantAgentReasonNotText},
		{name: "NUL", reply: okFile("a\x00b"), reason: v1alpha1.TenantAgentReasonNotText},
		{name: "InvalidUTF8", reply: okFile("\xff\xfe"), reason: v1alpha1.TenantAgentReasonNotText},
		{name: "NoMatchingCredential", reply: forgeapi.ReadReply{Status: forgeapi.Status{Code: forgeapi.CodeNoMatchingCredential}}, reason: v1alpha1.TenantAgentReasonFetchFailed},
		{name: "Unreachable", reply: forgeapi.ReadReply{Status: forgeapi.Status{Code: forgeapi.CodeForgeUnreachable}}, reason: v1alpha1.TenantAgentReasonFetchFailed},
		{name: "Timeout", err: errors.New("nats: timeout"), reason: v1alpha1.TenantAgentReasonFetchFailed},
		{name: "RepoURLInvalid", reply: forgeapi.ReadReply{Status: forgeapi.Status{Code: forgeapi.CodeRepoURLInvalid}}, reason: v1alpha1.TenantAgentReasonHostNotAllowed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ta := agent()
			if c.ta != nil {
				ta = c.ta()
			}
			src := source()
			if c.src != nil {
				src = c.src()
			}
			rev := "sha-1"
			if c.rev == "-" {
				rev = ""
			}
			rd := &fakeReader{reply: c.reply, err: c.err}
			got := Read(context.Background(), rd, "core", ta, src, rev)
			if got.Reason != c.reason {
				t.Fatalf("reason = %s (%s), want %s", got.Reason, got.Message, c.reason)
			}
			if c.reason == v1alpha1.TenantAgentReasonResolved && got.Content != "hello" {
				t.Fatalf("content = %q", got.Content)
			}
		})
	}
}

// TestRead_PassesRevisionAndLimit checks the request that the read sends: the
// observed revision, the default path, and the limit one byte above the bound.
func TestRead_PassesRevisionAndLimit(t *testing.T) {
	rd := &fakeReader{reply: okFile("x")}
	Read(context.Background(), rd, "core", agent(), source(), "sha-9")
	if rd.got.Revision != "sha-9" || rd.got.Path != v1alpha1.DefaultTenantAgentInstructionsPath ||
		rd.got.Limit != MaxBytes+1 || rd.got.Tenant != "payments" || rd.got.Repo != "https://git.example.org/o/r" {
		t.Fatalf("request = %+v", rd.got)
	}
}
