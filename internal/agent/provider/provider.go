// Package provider holds the model-provider contract of the agent runtime. An
// adapter turns one Request into a stream of Deltas. The package also holds the
// provider registry, the decoder of the assistant configuration, and the
// key handling. A key reaches an adapter only through Key.SetHeader.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// CallDeadline bounds one provider call from start to end.
const CallDeadline = 120 * time.Second

// IdleLimit bounds the silence between two stream lines of one provider call.
const IdleLimit = 60 * time.Second

// ConfigEnv names the environment variable that holds the assistant config.
const ConfigEnv = "VARROA_ASSISTANT_CONFIG"

// DefaultKeyDir is the directory that holds one provider key file per provider.
const DefaultKeyDir = "/etc/varroa/assistant"

// Errors that a stream reports. Callers match them with errors.Is.
var (
	ErrIdle           = errors.New("provider: stream idle limit reached")
	ErrDeadline       = errors.New("provider: call deadline reached")
	ErrBrokenStream   = errors.New("provider: stream ended before its terminal event")
	ErrKeyUnavailable = errors.New("provider: key is unavailable")
)

// Role is the author of one message.
type Role string

// The message roles.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is one tool call that the model asked for.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Message is one message of the conversation sent to a provider.
type Message struct {
	Role       Role
	Text       string
	ToolCalls  []ToolCall
	ToolCallID string
	ToolName   string
	IsError    bool
}

// Request is one provider call.
type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []agent.ToolDef
	MaxTokens int
}

// ToolCallDelta is one fragment of a tool call. Fragments with the same Index
// belong to one call.
type ToolCallDelta struct {
	Index             int
	ID                string
	Name              string
	ArgumentsFragment string
}

// Usage is the token usage of one call. Estimated is true when the provider
// sent no usage and the counts are a size estimate.
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	Estimated        bool
}

// StopReason says why a call ended.
type StopReason string

// The stop reasons.
const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopError     StopReason = "error"
)

// Delta is one item of a provider stream. Exactly one delta of a stream has a
// non-empty Stop, and it is the last delta before the channel closes.
type Delta struct {
	Text     string
	ToolCall *ToolCallDelta
	Usage    *Usage
	Stop     StopReason
	Err      error
}

// Provider streams one call.
type Provider interface {
	Stream(ctx context.Context, key Key, req Request) (<-chan Delta, error)
}

// Response is the joined result of one stream.
type Response struct {
	Text      string
	ToolCalls []ToolCall
	Usage     Usage
	Stop      StopReason
}

// Collect reads a stream to its end. It joins the tool-call fragments by index,
// in first-seen order. A StopError stream returns the response with the error
// of its error delta. A stream that closes with no stop delta returns
// ErrBrokenStream.
func Collect(ch <-chan Delta) (Response, error) {
	var resp Response
	var streamErr error
	stopped := false
	type partial struct {
		call ToolCall
		args []byte
	}
	var order []int
	calls := map[int]*partial{}
	for d := range ch {
		resp.Text += d.Text
		if d.ToolCall != nil {
			p, ok := calls[d.ToolCall.Index]
			if !ok {
				p = &partial{}
				calls[d.ToolCall.Index] = p
				order = append(order, d.ToolCall.Index)
			}
			if d.ToolCall.ID != "" {
				p.call.ID = d.ToolCall.ID
			}
			if d.ToolCall.Name != "" {
				p.call.Name = d.ToolCall.Name
			}
			p.args = append(p.args, d.ToolCall.ArgumentsFragment...)
		}
		if d.Usage != nil {
			resp.Usage = *d.Usage
		}
		if d.Stop != "" {
			resp.Stop = d.Stop
			stopped = true
			if d.Stop == StopError {
				streamErr = d.Err
			}
		}
	}
	for _, idx := range order {
		p := calls[idx]
		p.call.Arguments = json.RawMessage(p.args)
		resp.ToolCalls = append(resp.ToolCalls, p.call)
	}
	if !stopped {
		return resp, ErrBrokenStream
	}
	if resp.Stop == StopError {
		if streamErr == nil {
			streamErr = ErrBrokenStream
		}
		return resp, streamErr
	}
	return resp, nil
}

// Send delivers one delta unless the call context ends first. An adapter sends
// every delta through Send, so a consumer that stops reading cannot leave the
// stream goroutine blocked.
func Send(ctx context.Context, ch chan<- Delta, d Delta) error {
	select {
	case ch <- d:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// finalGrace bounds how long the closing delta waits for a consumer that has
// stopped reading.
const finalGrace = 5 * time.Second

// SendFinal delivers the closing delta of a stream. It stops at the call
// context, or after finalGrace, so a consumer that has stopped reading cannot
// hold the stream goroutine.
func SendFinal(ctx context.Context, ch chan<- Delta, d Delta) {
	timer := time.NewTimer(finalGrace)
	defer timer.Stop()
	select {
	case ch <- d:
	case <-ctx.Done():
	case <-timer.C:
	}
}
