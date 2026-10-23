// Package forgeops answers the operator forge verbs: check, read, issue, and
// pullrequest. A request names a Tenant and a repository. The operator on the Tenant's shard
// owner reads the Tenant's tenant credentials, picks one that may push, and
// calls the forge. The package never imports internal/controller.
package forgeops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	"github.com/varroaci/varroa-jenkins/internal/forgeurl"
)

// SecretReader reads tenant-credential Secrets in one namespace. The
// controller's ResourceClient satisfies it.
type SecretReader interface {
	ListSecrets(ctx context.Context, namespace, labelSelector string) ([]map[string][]byte, error)
	GetSecretAnnotations(ctx context.Context, name, namespace string) (map[string]string, error)
}

// Credential is one tenant credential that may reach the forge host.
type Credential struct {
	ID         string
	SecretName string
	Token      forge.Token
}

// Ops answers the forge verbs for one operator.
type Ops struct {
	store     crdstore.Backend
	secrets   SecretReader
	http      *http.Client
	detect    func(ctx context.Context, h *http.Client, host string) (forge.Kind, error)
	newClient func(kind forge.Kind, host string, tok forge.Token, h *http.Client) forge.Client
	logger    *slog.Logger
}

// New returns the forge answerer. h must be the client from forge.NewHTTPClient.
func New(store crdstore.Backend, secrets SecretReader, h *http.Client, logger *slog.Logger) *Ops {
	if logger == nil {
		logger = slog.Default()
	}
	return &Ops{
		store:     store,
		secrets:   secrets,
		http:      h,
		detect:    forge.Detect,
		newClient: forge.New,
		logger:    logger,
	}
}

// ShardKey returns the ring key of a Tenant. Every forge verb and every
// Tenant-owned operation keys on this one form.
func ShardKey(tenant string) string { return "tenant/" + tenant }

// replyError is a reply code with its message. Its text never holds a
// response body or a credential value.
type replyError struct {
	Code    string
	Message string
}

// Error returns the code and the message.
func (e *replyError) Error() string { return e.Code + ": " + e.Message }

func replyErr(code, format string, args ...any) error {
	return &replyError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// asStatus converts an error into the reply envelope. A non-reply error is
// internal, and its text is never sent.
func asStatus(err error) forgeapi.Status {
	var re *replyError
	if errors.As(err, &re) {
		return forgeapi.Status{Code: re.Code, Error: re.Message}
	}
	return forgeapi.Status{Code: forgeapi.CodeInternal, Error: "internal error"}
}

// forgeFailure maps a forge client error onto a reply code. The message names
// the credential and the HTTP status, never the response body. The forbidden
// message carries a fixed hint about the token scope and access.
func forgeFailure(err error, credID string) error {
	var se *forge.StatusError
	var rl *forge.RateLimitError
	switch {
	case errors.Is(err, forge.ErrNotFound):
		return replyErr(forgeapi.CodeNotFound, "repository not found")
	case errors.Is(err, forge.ErrAuth):
		return replyErr(forgeapi.CodeForgeAuthFailed, "credential %q was refused", credID)
	case errors.Is(err, forge.ErrForbidden):
		return replyErr(forgeapi.CodeForgeWriteDenied, "credential %q is forbidden; check the token scope and the repository access of the credential", credID)
	case errors.Is(err, forge.ErrListingTruncated):
		return replyErr(forgeapi.CodeForgeUnreachable, "credential %q: directory listing is truncated", credID)
	case errors.Is(err, forge.ErrRedirect):
		return replyErr(forgeapi.CodeForgeUnreachable, "credential %q: redirect refused", credID)
	case errors.As(err, &rl):
		if rl.Reset.IsZero() {
			return replyErr(forgeapi.CodeForgeUnreachable, "credential %q hit the forge rate limit; retry later", credID)
		}
		return replyErr(forgeapi.CodeForgeUnreachable, "credential %q hit the forge rate limit; retry after %s", credID, rl.Reset.UTC().Format(time.RFC3339))
	case errors.As(err, &se):
		return replyErr(forgeapi.CodeForgeUnreachable, "credential %q: HTTP %d", credID, se.Status)
	}
	return replyErr(forgeapi.CodeForgeUnreachable, "credential %q: request failed", credID)
}

// Check reports whether a repository can take a proposal: it has a writer,
// a base branch with a commit, no Jenkinsfile, and no proposal branch.
func (o *Ops) Check(ctx context.Context, req forgeapi.CheckRequest) forgeapi.CheckReply {
	fail := func(err error) forgeapi.CheckReply { return forgeapi.CheckReply{Status: asStatus(err)} }
	if req.Tenant == "" {
		return fail(replyErr(forgeapi.CodeInvalidRequest, "tenant is required"))
	}
	repo, err := forgeurl.NormalizeRepoURL(req.Repo)
	if err != nil {
		return fail(replyErr(forgeapi.CodeRepoURLInvalid, "%v", err))
	}
	creds, err := o.chooseCredential(ctx, req.Tenant, repo.Host)
	if err != nil {
		return fail(err)
	}
	w, err := o.pickWriter(ctx, repo.Host, forge.Repo{Owner: repo.Owner, Name: repo.Name}, creds)
	if err != nil {
		return fail(err)
	}
	fr := forge.Repo{Owner: repo.Owner, Name: repo.Name}

	sha, ok, err := w.Client.BranchHead(ctx, fr, w.Info.DefaultBranch)
	if err != nil {
		return fail(forgeFailure(err, w.CredentialID))
	}
	if !ok {
		return fail(replyErr(forgeapi.CodeNotFound, "base branch %q has no commit", w.Info.DefaultBranch))
	}

	entry, err := w.Client.Read(ctx, fr, sha, "Jenkinsfile", 1)
	switch {
	case err == nil && (entry.Dir || entry.Other):
		// PutFile cannot write a file over a directory, a symlink, or a
		// submodule, so the proposal would fail later. Refuse it now.
		return fail(replyErr(forgeapi.CodeJenkinsfileExists, "a directory or link named Jenkinsfile exists"))
	case err == nil:
		return fail(replyErr(forgeapi.CodeJenkinsfileExists, "the repository already holds a Jenkinsfile"))
	case errors.Is(err, forge.ErrNotFound):
		// No file at the path: the check stays open.
	default:
		return fail(forgeFailure(err, w.CredentialID))
	}

	_, exists, err := w.Client.BranchHead(ctx, fr, forgeapi.ProposalBranch)
	if err != nil {
		return fail(forgeFailure(err, w.CredentialID))
	}
	if exists {
		return fail(replyErr(forgeapi.CodeProposalBranchExists, "branch %q already exists", forgeapi.ProposalBranch))
	}
	return forgeapi.CheckReply{
		Status:       forgeapi.Status{OK: true},
		Forge:        string(w.Kind),
		BaseBranch:   w.Info.DefaultBranch,
		CredentialID: w.CredentialID,
		Revision:     sha,
	}
}

// Read returns a directory listing or a file of a repository at a revision.
// Every request check runs before the first forge call.
func (o *Ops) Read(ctx context.Context, req forgeapi.ReadRequest) forgeapi.ReadReply {
	fail := func(err error) forgeapi.ReadReply { return forgeapi.ReadReply{Status: asStatus(err)} }
	if req.Tenant == "" {
		return fail(replyErr(forgeapi.CodeInvalidRequest, "tenant is required"))
	}
	repo, err := forgeurl.NormalizeRepoURL(req.Repo)
	if err != nil {
		return fail(replyErr(forgeapi.CodeRepoURLInvalid, "%v", err))
	}
	path, err := forgeurl.CleanReadPath(req.Path)
	if err != nil {
		return fail(replyErr(forgeapi.CodeInvalidRequest, "%v", err))
	}
	if req.Revision != "" {
		if err := forgeurl.ValidateRevision(req.Revision); err != nil {
			return fail(replyErr(forgeapi.CodeInvalidRequest, "%v", err))
		}
	}
	if req.Limit != 0 && (req.Limit < 1 || req.Limit > forgeapi.MaxRead) {
		return fail(replyErr(forgeapi.CodeInvalidRequest, "limit must be 1 to %d", forgeapi.MaxRead))
	}
	creds, err := o.chooseCredential(ctx, req.Tenant, repo.Host)
	if err != nil {
		return fail(err)
	}
	w, err := o.pickReader(ctx, repo.Host, forge.Repo{Owner: repo.Owner, Name: repo.Name}, creds)
	if err != nil {
		return fail(err)
	}
	fr := forge.Repo{Owner: repo.Owner, Name: repo.Name}

	rev := req.Revision
	if rev == "" {
		rev = w.Info.DefaultBranch
	}
	sha, err := w.Client.Resolve(ctx, fr, rev)
	if errors.Is(err, forge.ErrNotFound) {
		return fail(replyErr(forgeapi.CodeNotFound, "revision %q not found", rev))
	}
	if err != nil {
		return fail(forgeFailure(err, w.CredentialID))
	}
	limit := req.Limit
	if limit == 0 {
		limit = forgeapi.MaxRead
	}
	entry, err := w.Client.Read(ctx, fr, sha, path, limit)
	if errors.Is(err, forge.ErrNotFound) || (err == nil && entry.Other) {
		return fail(replyErr(forgeapi.CodeNotFound, "path %q not found", path))
	}
	if err != nil {
		return fail(forgeFailure(err, w.CredentialID))
	}
	if entry.Dir {
		entries := make([]forgeapi.DirEntry, 0, len(entry.Children))
		for _, c := range entry.Children {
			entries = append(entries, forgeapi.DirEntry{Name: c.Name, Type: c.Type})
		}
		return forgeapi.ReadReply{Status: forgeapi.Status{OK: true}, Revision: sha, Path: path, ReadDir: &forgeapi.ReadDir{Entries: entries}}
	}
	return fileReply(sha, path, entry)
}

// textReplyLimit bounds the encoded reply of a file. The NATS payload cap is
// 1 MiB, and the reply keeps headroom below it.
const textReplyLimit = 768 << 10

// fileReply builds a file reply. A file that is not valid UTF-8 returns no
// content and Encoding "binary". A text file whose encoded reply is larger
// than textReplyLimit is cut again, at a rune boundary, until it fits.
func fileReply(sha, path string, e forge.Entry) forgeapi.ReadReply {
	data := e.Content
	if int64(len(data)) < e.Size {
		data = trimPartialRune(data)
	}
	if !utf8.Valid(data) {
		return forgeapi.ReadReply{
			Status:   forgeapi.Status{OK: true},
			Revision: sha,
			Path:     path,
			ReadFile: &forgeapi.ReadFile{Size: e.Size, Truncated: true, Encoding: "binary"},
		}
	}
	text := string(data)
	reply := forgeapi.ReadReply{
		Status:   forgeapi.Status{OK: true},
		Revision: sha,
		Path:     path,
		ReadFile: &forgeapi.ReadFile{
			Size:      e.Size,
			Length:    int64(len(text)),
			Truncated: int64(len(text)) < e.Size,
			Content:   text,
		},
	}
	rf := reply.ReadFile
	for len(encodeReply(reply)) > textReplyLimit && len(rf.Content) > 0 {
		n := len(rf.Content) / 2
		for n > 0 && !utf8.RuneStart(rf.Content[n]) {
			n--
		}
		rf.Content = rf.Content[:n]
		rf.Length = int64(n)
		rf.Truncated = true
	}
	return reply
}

// trimPartialRune removes an incomplete UTF-8 sequence at the end of a cut
// read, so that a cut inside a multi-byte rune is not taken for binary.
func trimPartialRune(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return b[:i]
			}
			return b
		}
	}
	return b
}

// pickWriter returns the first credential that may push to the repository.
// A credential that reads but cannot push is not a writer, and it still
// counts as authenticated.
func (o *Ops) pickWriter(ctx context.Context, host string, repo forge.Repo, creds []Credential) (writer, error) {
	return o.pickCredential(ctx, host, repo, creds, true)
}

// pickReader returns the first credential that may read the repository. A
// read-only credential serves a read, so a read does not need push access.
func (o *Ops) pickReader(ctx context.Context, host string, repo forge.Repo, creds []Credential) (writer, error) {
	return o.pickCredential(ctx, host, repo, creds, false)
}

// pickCredential returns the first credential that answers the repository
// read. When needPush is set, the credential must also be allowed to push. A
// probe error skips its credential, as in candidates.
func (o *Ops) pickCredential(ctx context.Context, host string, repo forge.Repo, creds []Credential, needPush bool) (writer, error) {
	kind, err := o.detect(ctx, o.http, host)
	if errors.Is(err, forge.ErrUnsupported) {
		return writer{}, replyErr(forgeapi.CodeForgeUnsupported, "host %q is not a supported forge", host)
	}
	if err != nil {
		o.logger.Warn("forge detect failed", "host", host, "error", err)
		return writer{}, replyErr(forgeapi.CodeForgeUnreachable, "forge %q is unreachable", host)
	}
	var probe probeResult
	for _, c := range creds {
		cl := o.newClient(kind, host, c.Token, o.http)
		if cl == nil {
			return writer{}, replyErr(forgeapi.CodeForgeUnsupported, "host %q is not a supported forge", host)
		}
		info, err := cl.Repository(ctx, repo)
		switch {
		case err == nil && (info.Push || !needPush):
			return writer{Kind: kind, Client: cl, CredentialID: c.ID, Info: info}, nil
		case err == nil:
			probe.authed = true
		default:
			probe.note(o.logger, host, c.ID, err)
		}
	}
	return writer{}, probe.refusal(needPush)
}

// candidates returns the credentials that may serve a write, in order. The
// credentials that may push come first, in id order. When needPush is false,
// the read-only credentials follow. A probe error skips its credential (see
// probeResult).
func (o *Ops) candidates(ctx context.Context, host string, repo forge.Repo, creds []Credential, needPush bool) ([]writer, error) {
	kind, err := o.detect(ctx, o.http, host)
	if errors.Is(err, forge.ErrUnsupported) {
		return nil, replyErr(forgeapi.CodeForgeUnsupported, "host %q is not a supported forge", host)
	}
	if err != nil {
		o.logger.Warn("forge detect failed", "host", host, "error", err)
		return nil, replyErr(forgeapi.CodeForgeUnreachable, "forge %q is unreachable", host)
	}
	var pushers, readers []writer
	var probe probeResult
	for _, c := range creds {
		cl := o.newClient(kind, host, c.Token, o.http)
		if cl == nil {
			return nil, replyErr(forgeapi.CodeForgeUnsupported, "host %q is not a supported forge", host)
		}
		info, err := cl.Repository(ctx, repo)
		w := writer{Kind: kind, Client: cl, CredentialID: c.ID, Info: info}
		switch {
		case err == nil && info.Push:
			pushers = append(pushers, w)
		case err == nil:
			readers = append(readers, w)
			probe.authed = true
		default:
			probe.note(o.logger, host, c.ID, err)
		}
	}
	cands := pushers
	if !needPush {
		cands = append(cands, readers...)
	}
	if len(cands) > 0 {
		return cands, nil
	}
	return nil, probe.refusal(needPush)
}

// forbiddenRefusal is the refusal when every candidate answered 403. A rate
// limit never reaches here. One
// credential keeps the single-credential message. Several name each one.
func forbiddenRefusal(ids []string) error {
	if len(ids) == 1 {
		return forgeFailure(forge.ErrForbidden, ids[0])
	}
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = strconv.Quote(id)
	}
	return replyErr(forgeapi.CodeForgeWriteDenied, "credentials %s are forbidden; check the token scope and the repository access of each credential", strings.Join(quoted, ", "))
}

// probeResult holds what a repository probe learns from the credentials that
// serve no verb. Every probe uses it, so every verb has one skip rule.
type probeResult struct {
	authed   bool // a credential authenticated: a 403, a 404, or a read without push
	firstErr error
	firstID  string
}

// note records a failed probe. A failed probe always skips its credential.
// A rate limit is a probe error, not a refusal.
func (p *probeResult) note(logger *slog.Logger, host, credID string, err error) {
	switch {
	case errors.Is(err, forge.ErrAuth):
		logger.Warn("forge credential refused", "credential", credID, "host", host)
	case errors.Is(err, forge.ErrForbidden), errors.Is(err, forge.ErrNotFound):
		p.authed = true
	default:
		logger.Warn("forge repository read failed", "credential", credID, "host", host, "error", err)
		if p.firstErr == nil {
			p.firstErr, p.firstID = err, credID
		}
	}
}

// refusal is the reply when no credential serves the verb. A probe error wins
// over a scope refusal, because that credential can serve after a retry.
func (p *probeResult) refusal(needPush bool) error {
	if p.firstErr != nil {
		return forgeFailure(p.firstErr, p.firstID)
	}
	if p.authed && needPush {
		return replyErr(forgeapi.CodeForgeWriteDenied, "no credential may push to the repository")
	}
	if p.authed {
		return replyErr(forgeapi.CodeForgeAuthFailed, "no credential may read the repository")
	}
	return replyErr(forgeapi.CodeForgeAuthFailed, "no credential authenticated to the forge")
}

// writer is the credential and client that the verb uses.
type writer struct {
	Kind         forge.Kind
	Client       forge.Client
	CredentialID string
	Info         forge.Info
}

// encodeReply returns the JSON form of a reply. HTML escaping is off, so a
// character that the page escapes is sent as the character itself.
func encodeReply(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return buf.Bytes()
}
