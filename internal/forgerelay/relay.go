// Package forgerelay carries forge requests from a requester to the operator
// shard that owns the Team. The requester is the BFF or the agent runner. The
// operator answers on operator.<cluster>.forge.<verb>. The package holds the
// request and reply framing only, so every requester uses one path.
package forgerelay

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// Relay sends forge requests over the bus.
type Relay struct {
	Conn *bus.Conn
}

// Read asks the operator of the cluster for one file or directory.
func (r Relay) Read(ctx context.Context, cluster string, req forgeapi.ReadRequest) (forgeapi.ReadReply, error) {
	var reply forgeapi.ReadReply
	err := r.call(ctx, cluster, forgeapi.VerbRead, req, &reply)
	return reply, err
}

// Issue asks the operator of the cluster to open an issue.
func (r Relay) Issue(ctx context.Context, cluster string, req forgeapi.IssueRequest) (forgeapi.IssueReply, error) {
	var reply forgeapi.IssueReply
	err := r.call(ctx, cluster, forgeapi.VerbIssue, req, &reply)
	return reply, err
}

// call sends one request and decodes the reply. A bus error or a reply that
// does not decode is returned to the caller.
func (r Relay) call(ctx context.Context, cluster, verb string, req, reply any) error {
	if r.Conn == nil {
		return fmt.Errorf("forge relay has no bus connection")
	}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode forge %s request: %w", verb, err)
	}
	msg, err := r.Conn.RequestWithContext(ctx, bus.OperatorForgeSubject(cluster, verb), data, forgeapi.RequestTimeout)
	if err != nil {
		return fmt.Errorf("forge %s request: %w", verb, err)
	}
	if err := json.Unmarshal(msg.Data, reply); err != nil {
		return fmt.Errorf("decode forge %s reply: %w", verb, err)
	}
	return nil
}
