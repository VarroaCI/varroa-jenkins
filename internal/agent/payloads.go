package agent

import (
	"encoding/json"
	"strings"
	"time"
)

// UserPayload is the payload of a user event.
type UserPayload struct {
	Text string `json:"text"`
}

// FirstMessageScan is the number of runes of a first user text that
// TrimFirstMessage reads. The Postgres store cuts the text to this length in SQL.
const FirstMessageScan = 1024

// FirstMessageMax is the maximum number of runes in a first message.
const FirstMessageMax = 120

// TrimFirstMessage returns the list form of a first user text: one line of
// plain text with at most FirstMessageMax runes. A cut text ends with "…".
func TrimFirstMessage(text string) string {
	if r := []rune(text); len(r) > FirstMessageScan {
		text = string(r[:FirstMessageScan])
	}
	line := strings.Join(strings.Fields(text), " ")
	r := []rune(line)
	if len(r) <= FirstMessageMax {
		return line
	}
	return string(r[:FirstMessageMax-1]) + "…"
}

// AssistantPayload is the payload of an assistant event. The chunks of one
// turn concatenate to the turn's text, and only the last chunk is final.
type AssistantPayload struct {
	Turn  int    `json:"turn"`
	Text  string `json:"text"`
	Final bool   `json:"final"`
}

// ToolCallPayload is the payload of a tool_call event.
type ToolCallPayload struct {
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Class     ToolClass       `json:"class"`
}

// ToolResultPayload is the payload of a tool_result event.
type ToolResultPayload struct {
	CallID    string      `json:"call_id"`
	Name      string      `json:"name"`
	Content   string      `json:"content"`
	IsError   bool        `json:"is_error"`
	DataLevel DataSharing `json:"data_level"`
}

// ApprovalRequestedPayload is the payload of an approval_requested event.
type ApprovalRequestedPayload struct {
	ApprovalID string     `json:"approval_id"`
	CallID     string     `json:"call_id"`
	Tool       string     `json:"tool"`
	TargetKind TargetKind `json:"target_kind"`
	TargetRef  string     `json:"target_ref"`
	ExpiresAt  time.Time  `json:"expires_at"`
}

// ApprovalResolvedPayload is the payload of an approval_resolved event.
type ApprovalResolvedPayload struct {
	ApprovalID string        `json:"approval_id"`
	State      ApprovalState `json:"state"`
	ResolvedBy string        `json:"resolved_by"`
}

// ErrorPayload is the payload of an error event.
type ErrorPayload struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

// DonePayload is the payload of a done event, which ends a turn or a session.
type DonePayload struct {
	State          SessionState `json:"state"`
	Stop           string       `json:"stop"`
	UsageEstimated bool         `json:"usage_estimated"`
}

// NewEvent marshals a payload into an event of the given type.
func NewEvent(t EventType, payload any) (EventInput, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return EventInput{}, err
	}
	return EventInput{Type: t, Payload: raw}, nil
}
