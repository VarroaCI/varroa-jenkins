package loop

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
)

// MaxResultBytes caps the text of one tool result that the transcript keeps
// and the model receives.
const MaxResultBytes = 32768

// Rebuild turns the events of a session into the provider messages of the
// next call. It reads only the transcript, so every replica rebuilds the same
// messages. The chunks of one turn join into one assistant message, and the
// tool calls of the turn join that message.
func Rebuild(events []agent.Event) ([]provider.Message, error) {
	var msgs []provider.Message
	var turns []int // turn number of each message, or -1 for none
	last := -1      // index of the open assistant message, or -1

	for _, e := range events {
		switch e.Type {
		case agent.EventUser:
			var p agent.UserPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("rebuild: user event %d: %w", e.Seq, err)
			}
			msgs = append(msgs, provider.Message{Role: provider.RoleUser, Text: p.Text})
			turns = append(turns, -1)
			last = -1
		case agent.EventAssistant:
			var p agent.AssistantPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("rebuild: assistant event %d: %w", e.Seq, err)
			}
			if last < 0 || turns[last] != p.Turn {
				msgs = append(msgs, provider.Message{Role: provider.RoleAssistant})
				turns = append(turns, p.Turn)
				last = len(msgs) - 1
			}
			msgs[last].Text += p.Text
		case agent.EventToolCall:
			var p agent.ToolCallPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("rebuild: tool_call event %d: %w", e.Seq, err)
			}
			if last < 0 {
				msgs = append(msgs, provider.Message{Role: provider.RoleAssistant})
				turns = append(turns, -1)
				last = len(msgs) - 1
			}
			msgs[last].ToolCalls = append(msgs[last].ToolCalls, provider.ToolCall{
				ID: p.CallID, Name: p.Name, Arguments: p.Arguments,
			})
		case agent.EventToolResult:
			var p agent.ToolResultPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("rebuild: tool_result event %d: %w", e.Seq, err)
			}
			msgs = append(msgs, provider.Message{
				Role: provider.RoleTool, ToolCallID: p.CallID, ToolName: p.Name,
				Text: p.Content, IsError: p.IsError,
			})
			turns = append(turns, -1)
		}
	}
	return msgs, nil
}

// CutResult shortens a tool result to MaxResultBytes. It cuts on a UTF-8
// boundary and appends a marker that names the limit.
func CutResult(s string) string {
	if len(s) <= MaxResultBytes {
		return s
	}
	n := MaxResultBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + fmt.Sprintf("\n[truncated at %d bytes]", MaxResultBytes)
}

// argsOrEmpty returns the arguments of a call, or an empty object.
func argsOrEmpty(raw json.RawMessage) json.RawMessage {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}
