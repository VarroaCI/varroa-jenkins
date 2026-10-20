// Package forgerelay carries forge requests from a requester to the operator
// shard that owns the Team. The requester is the BFF or the agent runner. The
// operator answers on operator.<cluster>.forge.<verb>. The package holds the
// request and reply framing only, so every requester uses one path.
package forgerelay

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// Relay sends forge requests over the bus.
type Relay struct {
	Conn *bus.Conn
}

// Check asks the operator of the cluster whether a repository can take a
// proposal.
func (r Relay) Check(ctx context.Context, cluster string, req forgeapi.CheckRequest) (forgeapi.CheckReply, error) {
	var reply forgeapi.CheckReply
	err := r.call(ctx, cluster, forgeapi.VerbCheck, forgeapi.RequestTimeout, req, &reply)
	return reply, err
}

// Read asks the operator of the cluster for one file or directory.
func (r Relay) Read(ctx context.Context, cluster string, req forgeapi.ReadRequest) (forgeapi.ReadReply, error) {
	var reply forgeapi.ReadReply
	err := r.call(ctx, cluster, forgeapi.VerbRead, forgeapi.RequestTimeout, req, &reply)
	return reply, err
}

// PullRequest asks the operator of the cluster to open a pull request.
func (r Relay) PullRequest(ctx context.Context, cluster string, req forgeapi.PullRequestRequest) (forgeapi.PullRequestReply, error) {
	var reply forgeapi.PullRequestReply
	req.Deadline = requestDeadline(ctx, time.Now(), forgeapi.PullRequestTimeout)
	err := r.call(ctx, cluster, forgeapi.VerbPullRequest, forgeapi.PullRequestTimeout, req, &reply)
	return reply, err
}

// requestDeadline returns the time when this requester stops waiting for a
// reply. It is now plus timeout, or the caller's deadline when that is
// earlier.
func requestDeadline(ctx context.Context, now time.Time, timeout time.Duration) time.Time {
	deadline := now.Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		return d
	}
	return deadline
}

// Issue asks the operator of the cluster to open an issue.
func (r Relay) Issue(ctx context.Context, cluster string, req forgeapi.IssueRequest) (forgeapi.IssueReply, error) {
	var reply forgeapi.IssueReply
	req.Deadline = requestDeadline(ctx, time.Now(), forgeapi.RequestTimeout)
	err := r.call(ctx, cluster, forgeapi.VerbIssue, forgeapi.RequestTimeout, req, &reply)
	return reply, err
}

// call sends one request and decodes the reply. The timeout bounds the wait
// for the reply. A bus error or a reply that does not decode is returned to
// the caller.
func (r Relay) call(ctx context.Context, cluster, verb string, timeout time.Duration, req, reply any) error {
	if r.Conn == nil {
		return fmt.Errorf("forge relay has no bus connection")
	}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode forge %s request: %w", verb, err)
	}
	msg, err := r.Conn.RequestWithContext(ctx, bus.OperatorForgeSubject(cluster, verb), data, timeout)
	if err != nil {
		return fmt.Errorf("forge %s request: %w", verb, err)
	}
	if err := json.Unmarshal(msg.Data, reply); err != nil {
		return fmt.Errorf("decode forge %s reply: %w", verb, err)
	}
	return nil
}
