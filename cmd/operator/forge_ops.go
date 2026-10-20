package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller/forgeops"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// forgeRequestTimeout bounds one forge check or read on the operator, and it
// caps the work of one issue. It stays under the requester's
// forgeapi.RequestTimeout, so the operator returns its own reply first.
const forgeRequestTimeout = 25 * time.Second

// forgePullRequestTimeout limits the work of one pull-request request whose
// deadline is far in the future. The requester sets the deadline, so this is
// only an upper limit.
const forgePullRequestTimeout = 80 * time.Second

// forgeReplyMargin keeps time for the reply to reach the requester after the
// work ends.
const forgeReplyMargin = 5 * time.Second

// forgePullRequestReserve is the time that the operator keeps before the
// requester's deadline. The cleanup after a failed write runs for up to
// forgeops.CleanupTimeout. The deadline compares the requester's clock with
// this replica's clock. The reserve also absorbs a small clock skew.
const forgePullRequestReserve = forgeops.CleanupTimeout + forgeReplyMargin

// forgePullRequestMinBudget is the least work time that a pull request needs.
// A request with less time left is refused before any forge call.
const forgePullRequestMinBudget = 15 * time.Second

// forgeIssueReserve is the time that the operator keeps before an issue
// requester's deadline. No cleanup runs after an issue write. The margin also
// absorbs a small clock skew.
const forgeIssueReserve = forgeReplyMargin

// forgeIssueMinBudget is the least work time that an issue needs. A request
// with less time left is refused before any forge call.
const forgeIssueMinBudget = 10 * time.Second

// forgeConcurrency limits the forge requests that one replica runs at a
// time on one verb subject.
const forgeConcurrency = 4

// forgePullRequestConcurrency runs one pull request at a time on each
// replica, so two calls for one Team never race on one branch.
const forgePullRequestConcurrency = 1

// forgeOps is the slice of *forgeops.Ops that the forge handlers call.
type forgeOps interface {
	Check(ctx context.Context, req forgeapi.CheckRequest) forgeapi.CheckReply
	Read(ctx context.Context, req forgeapi.ReadRequest) forgeapi.ReadReply
	Issue(ctx context.Context, req forgeapi.IssueRequest, releaseNamespace string) forgeapi.IssueReply
	PullRequest(ctx context.Context, req forgeapi.PullRequestRequest) forgeapi.PullRequestReply
}

// shardEnterer registers work on a held ring shard. It returns false when the
// replica does not hold the shard.
type shardEnterer func(ctx context.Context, key string) (context.Context, func(), bool)

// handleForgeCheck answers one operator.<cluster>.forge.check request. A
// replica that does not own the Team's shard returns nil, so it sends no
// reply.
func handleForgeCheck(ctx context.Context, logger *slog.Logger, ops forgeOps, enter shardEnterer, data []byte) []byte {
	var req forgeapi.CheckRequest
	if err := json.Unmarshal(data, &req); err != nil {
		logger.Warn("forge check decode failed", "error", err)
		return encodeForgeReply(forgeapi.CheckReply{Status: invalidForgeStatus()})
	}
	wctx, leave, ok := enter(ctx, forgeops.ShardKey(req.Team))
	if !ok {
		return nil
	}
	defer leave()
	rctx, cancel := context.WithTimeout(wctx, forgeRequestTimeout)
	defer cancel()
	return encodeForgeReply(ops.Check(rctx, req))
}

// handleForgeRead answers one operator.<cluster>.forge.read request. The
// ownership rule is the same as handleForgeCheck.
func handleForgeRead(ctx context.Context, logger *slog.Logger, ops forgeOps, enter shardEnterer, data []byte) []byte {
	var req forgeapi.ReadRequest
	if err := json.Unmarshal(data, &req); err != nil {
		logger.Warn("forge read decode failed", "error", err)
		return encodeForgeReply(forgeapi.ReadReply{Status: invalidForgeStatus()})
	}
	wctx, leave, ok := enter(ctx, forgeops.ShardKey(req.Team))
	if !ok {
		return nil
	}
	defer leave()
	rctx, cancel := context.WithTimeout(wctx, forgeRequestTimeout)
	defer cancel()
	return encodeForgeReply(ops.Read(rctx, req))
}

// handleForgeIssue answers one operator.<cluster>.forge.issue request. The
// ownership rule is the same as handleForgeCheck: the shard owner of the
// Team's key opens the issue, and no other replica replies.
func handleForgeIssue(ctx context.Context, logger *slog.Logger, ops forgeOps, enter shardEnterer, releaseNamespace string, data []byte) []byte {
	var req forgeapi.IssueRequest
	if err := json.Unmarshal(data, &req); err != nil {
		logger.Warn("forge issue decode failed", "error", err)
		return encodeForgeReply(forgeapi.IssueReply{Status: invalidForgeStatus()})
	}
	wctx, leave, ok := enter(ctx, forgeops.ShardKey(req.Team))
	if !ok {
		return nil
	}
	defer leave()
	if req.Deadline.IsZero() {
		return encodeForgeReply(forgeapi.IssueReply{Status: forgeapi.Status{Code: forgeapi.CodeInvalidRequest, Error: "deadline is required"}})
	}
	now := time.Now()
	work, ok := forgeWorkDeadline(now, req.Deadline, forgeIssueReserve, forgeRequestTimeout, forgeIssueMinBudget)
	if !ok {
		logger.Info("forge issue has too little time left", "team", req.Team, "left", req.Deadline.Sub(now).Round(time.Second))
		return encodeForgeReply(forgeapi.IssueReply{Status: forgeapi.Status{Code: forgeapi.CodeRequestExpired,
			Error: "too little time was left for the issue, so none was opened. Call again."}})
	}
	rctx, cancel := context.WithDeadline(wctx, work)
	defer cancel()
	return encodeForgeReply(ops.Issue(rctx, req, releaseNamespace))
}

// handleForgePullRequest answers one operator.<cluster>.forge.pullrequest
// request. The ownership rule is the same as handleForgeCheck.
func handleForgePullRequest(ctx context.Context, logger *slog.Logger, ops forgeOps, enter shardEnterer, data []byte) []byte {
	var req forgeapi.PullRequestRequest
	if err := json.Unmarshal(data, &req); err != nil {
		logger.Warn("forge pullrequest decode failed", "error", err)
		return encodeForgeReply(forgeapi.PullRequestReply{Status: invalidForgeStatus()})
	}
	wctx, leave, ok := enter(ctx, forgeops.ShardKey(req.Team))
	if !ok {
		return nil
	}
	defer leave()
	if req.Deadline.IsZero() {
		return encodeForgeReply(forgeapi.PullRequestReply{Status: forgeapi.Status{Code: forgeapi.CodeInvalidRequest, Error: "deadline is required"}})
	}
	now := time.Now()
	work, ok := forgeWorkDeadline(now, req.Deadline, forgePullRequestReserve, forgePullRequestTimeout, forgePullRequestMinBudget)
	if !ok {
		logger.Info("forge pullrequest has too little time left", "team", req.Team, "left", req.Deadline.Sub(now).Round(time.Second))
		return encodeForgeReply(forgeapi.PullRequestReply{Status: forgeapi.Status{Code: forgeapi.CodeRequestExpired,
			Error: "too little time was left for the pull request, so none was opened. Call again."}})
	}
	rctx, cancel := context.WithDeadline(wctx, work)
	defer cancel()
	return encodeForgeReply(ops.PullRequest(rctx, req))
}

// forgeWorkDeadline returns the end of the work time for a request that the
// requester stops waiting for at deadline. The work ends reserve before the
// deadline and at most limit after now. ok is false when less than minBudget
// of work time is left.
func forgeWorkDeadline(now, deadline time.Time, reserve, limit, minBudget time.Duration) (work time.Time, ok bool) {
	work = deadline.Add(-reserve)
	if ceil := now.Add(limit); work.After(ceil) {
		work = ceil
	}
	return work, work.Sub(now) >= minBudget
}

// invalidForgeStatus is the reply to a request that does not decode.
func invalidForgeStatus() forgeapi.Status {
	return forgeapi.Status{Code: forgeapi.CodeInvalidRequest, Error: "invalid request"}
}

// encodeForgeReply returns the JSON form of a reply. HTML escaping is off.
func encodeForgeReply(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		// The reply types hold only strings and numbers, so this is unreachable.
		return nil
	}
	return buf.Bytes()
}

// forgeRunner serves one forge verb on every replica. It subscribes with no
// queue group, because only the Team's shard owner may answer, and a queue
// group could hand the request to a replica that does not hold the shard.
type forgeRunner struct {
	busConn *bus.Conn
	cluster string
	verb    string
	limit   int
	handle  func(ctx context.Context, data []byte) []byte
	logger  *slog.Logger
}

// NeedLeaderElection reports that the runner runs on every replica.
func (r *forgeRunner) NeedLeaderElection() bool { return false }

// Start serves the verb until ctx ends.
func (r *forgeRunner) Start(ctx context.Context) error {
	subject := bus.OperatorForgeSubject(r.cluster, r.verb)
	sub, err := r.busConn.ServeRequestsConcurrently(subject, "", r.limit, func(data []byte) []byte {
		return r.handle(ctx, data)
	})
	if err != nil {
		return fmt.Errorf("subscribe forge %s: %w", r.verb, err)
	}
	<-ctx.Done()
	if err := sub.Unsubscribe(); err != nil {
		r.logger.Warn("unsubscribe forge verb failed", "verb", r.verb, "error", err)
	}
	return nil
}
