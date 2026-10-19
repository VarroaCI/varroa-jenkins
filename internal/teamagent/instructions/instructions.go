// Package instructions reads the standing instructions of a Team agent from
// the repository that its CatalogSource names. The read goes through the
// forge relay, so the credential and the host rules of the operator apply.
package instructions

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// MaxBytes bounds the instructions file. A larger file is refused, not cut.
const MaxBytes = 32 << 10

// Reader reads one file through the forge relay.
type Reader interface {
	Read(ctx context.Context, cluster string, req forgeapi.ReadRequest) (forgeapi.ReadReply, error)
}

// Result is the outcome of one instructions read. Reason is one of the
// TeamAgentReason values of the API package.
type Result struct {
	Content  string
	Revision string
	Size     int64
	Reason   string
	Message  string
}

// Read returns the instructions of ta from the repository of src, at the
// revision that the CatalogSource observed. Every failed read names a reason
// and a message that a person can act on.
func Read(ctx context.Context, rd Reader, cluster string, ta *v1alpha1.TeamAgent, src *v1alpha1.CatalogSource, revision string) Result {
	fail := func(reason, msg string) Result { return Result{Reason: reason, Message: msg} }
	if ta.Spec.Instructions == nil {
		return fail(v1alpha1.TeamAgentReasonNotConfigured, "the team agent names no instructions source")
	}
	if src == nil {
		return fail(v1alpha1.TeamAgentReasonNotFound, "the instructions source does not exist")
	}
	if src.Spec.Revision != "" {
		return fail(v1alpha1.TeamAgentReasonNotDefaultBranch, "the instructions source pins a revision; it must follow the default branch")
	}
	if src.Spec.RepoURL == "" {
		return fail(v1alpha1.TeamAgentReasonNotFound, "source has no git repo")
	}
	if revision == "" {
		return fail(v1alpha1.TeamAgentReasonNotSynced, "the instructions source has not synced yet")
	}
	path := ta.Spec.Instructions.Path
	if path == "" {
		path = v1alpha1.DefaultTeamAgentInstructionsPath
	}
	reply, err := rd.Read(ctx, cluster, forgeapi.ReadRequest{
		Team:     ta.Spec.TeamRef.Name,
		Repo:     src.Spec.RepoURL,
		Path:     path,
		Revision: revision,
		Limit:    MaxBytes + 1,
	})
	if err != nil {
		return fail(v1alpha1.TeamAgentReasonFetchFailed, "the instructions read did not complete: "+err.Error())
	}
	if !reply.OK {
		return mapCode(reply.Code, reply.Error)
	}
	if reply.ReadDir != nil || reply.ReadFile == nil {
		return fail(v1alpha1.TeamAgentReasonNotFound, path+" is a directory, not a file")
	}
	file := reply.ReadFile
	if file.Size > MaxBytes {
		return fail(v1alpha1.TeamAgentReasonTooLarge, path+" is larger than the instructions limit")
	}
	if file.Encoding == "binary" || strings.ContainsRune(file.Content, 0) || !utf8.ValidString(file.Content) {
		return fail(v1alpha1.TeamAgentReasonNotText, path+" is not a text file")
	}
	return Result{Content: file.Content, Revision: reply.Revision, Size: file.Size, Reason: v1alpha1.TeamAgentReasonResolved}
}

// mapCode maps a failed forge reply to a reason. An unknown code is a fetch
// failure, so the instructions read never reports success for it.
func mapCode(code, text string) Result {
	switch code {
	case forgeapi.CodeNotFound, forgeapi.CodeInvalidRequest:
		return Result{Reason: v1alpha1.TeamAgentReasonNotFound, Message: messageOr(text, "the file or revision was not found")}
	case forgeapi.CodeRepoURLInvalid:
		return Result{Reason: v1alpha1.TeamAgentReasonHostNotAllowed, Message: messageOr(text, "the repository URL is not allowed")}
	case forgeapi.CodeNoMatchingCredential:
		return Result{Reason: v1alpha1.TeamAgentReasonFetchFailed, Message: "no credential of the Team covers the repository host, or the Team's agent namespace does not hold the Secret"}
	case forgeapi.CodeForgeUnreachable:
		return Result{Reason: v1alpha1.TeamAgentReasonFetchFailed, Message: messageOr(text, "the forge is unreachable")}
	default:
		return Result{Reason: v1alpha1.TeamAgentReasonFetchFailed, Message: messageOr(text, "the forge refused the read")}
	}
}

// messageOr returns the forge's own message when it has one, else the fallback.
func messageOr(text, fallback string) string {
	if text != "" {
		return text
	}
	return fallback
}
