package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestEnumerationsMatchRegistry pins every enumeration to the values of the
// coordinator registry. A renamed value fails here before it reaches the
// store or the wire.
func TestEnumerationsMatchRegistry(t *testing.T) {
	t.Parallel()

	check := func(name string, got any, want []string) {
		t.Helper()
		var have []string
		switch v := got.(type) {
		case []SessionState:
			have = toStrings(v)
		case []SessionKind:
			have = toStrings(v)
		case []EventType:
			have = toStrings(v)
		case []ApprovalState:
			have = toStrings(v)
		case []SkipReason:
			have = toStrings(v)
		case []ErrorCode:
			have = toStrings(v)
		case []KeySource:
			have = toStrings(v)
		case []TargetKind:
			have = toStrings(v)
		case []DataSharing:
			have = toStrings(v)
		case []ProviderKind:
			have = toStrings(v)
		case []ProviderTrust:
			have = toStrings(v)
		case []ToolClass:
			have = toStrings(v)
		case []string:
			have = v
		}
		if !reflect.DeepEqual(have, want) {
			t.Fatalf("%s = %v, want %v", name, have, want)
		}
	}
	check("SessionStates", SessionStates(), []string{"running", "waiting_approval", "completed", "failed", "cancelled", "skipped", "expired"})
	check("SessionKinds", SessionKinds(), []string{"assistant", "tenant_agent"})
	check("EventTypes", EventTypes(), []string{"user", "assistant", "tool_call", "tool_result", "approval_requested", "approval_resolved", "error", "done"})
	check("ApprovalStates", ApprovalStates(), []string{"pending", "approved", "denied", "expired"})
	check("SkipReasons", SkipReasons(), []string{"budget_day", "kill_switch", "data_sharing_none", "provider_unavailable"})
	check("ErrorCodes", ErrorCodes(), []string{"budget_session", "budget_day", "steps", "kill_switch", "provider_error", "session_key_missing", "lease_lost"})
	check("KeySources", KeySources(), []string{"chart", "user"})
	check("TargetKinds", TargetKinds(), []string{"run", "job", "controller", "none"})
	check("DataLevels", DataLevels(), []string{"none", "metadata", "logs"})
	check("ProviderKinds", ProviderKinds(), []string{"anthropic", "openai-compatible"})
	check("ProviderTrusts", ProviderTrusts(), []string{"internal", "external"})
	check("ToolClasses", ToolClasses(), []string{"read", "write"})
	check("ClientContextKeys", ClientContextKeys(), []string{"route", "tenant", "job", "run", "controller", "cell"})
	check("ServerContextKeys", ServerContextKeys(), []string{"repo", "prompt"})
}

func toStrings[T ~string](in []T) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, string(v))
	}
	return out
}

// TestDecideCreate walks the stop table in order.
func TestDecideCreate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		in    CreateInput
		skip  SkipReason
		start bool
	}{
		{"paused wins over everything", CreateInput{Paused: true, DayLimit: 0, DataLevel: DataNone}, SkipKillSwitch, false},
		{"zero day limit skips", CreateInput{DayLimit: 0, DataLevel: DataLogs, ProviderAvailable: true}, SkipBudgetDay, false},
		{"day total at limit skips", CreateInput{DayTotal: 50, DayLimit: 50, DataLevel: DataLogs, ProviderAvailable: true}, SkipBudgetDay, false},
		{"day total under limit starts", CreateInput{DayTotal: 49, DayLimit: 50, DataLevel: DataLogs, ProviderAvailable: true}, "", true},
		{"data none skips", CreateInput{DayLimit: 100, DataLevel: DataNone, ProviderAvailable: true}, SkipDataSharingNone, false},
		{"invalid data level skips", CreateInput{DayLimit: 100, DataLevel: "bogus", ProviderAvailable: true}, SkipDataSharingNone, false},
		{"no provider skips", CreateInput{DayLimit: 100, DataLevel: DataMetadata}, SkipProviderUnavailable, false},
		{"all clear starts", CreateInput{DayLimit: 100, DataLevel: DataMetadata, ProviderAvailable: true}, "", true},
	}
	for _, c := range cases {
		skip, start := DecideCreate(c.in)
		if skip != c.skip || start != c.start {
			t.Fatalf("%s: DecideCreate = (%q, %v), want (%q, %v)", c.name, skip, start, c.skip, c.start)
		}
	}

	// The total of 50 with limit 50 skips; limit 100 starts.
	if skip, start := DecideCreate(CreateInput{DayTotal: 50, DayLimit: 50, DataLevel: DataLogs, ProviderAvailable: true}); start || skip != SkipBudgetDay {
		t.Fatalf("total 50 limit 50 = (%q, %v), want budget_day skip", skip, start)
	}
	if _, start := DecideCreate(CreateInput{DayTotal: 50, DayLimit: 100, DataLevel: DataLogs, ProviderAvailable: true}); !start {
		t.Fatal("total 50 limit 100 did not start")
	}
}

// TestSessionContextRoundTrip sets a session context and reads it back.
func TestSessionContextRoundTrip(t *testing.T) {
	t.Parallel()

	id := uuid.Must(uuid.NewV7())
	sc := SessionContext{SessionID: id, Values: map[string]any{CtxTenant: "acme", CtxRun: 7}}
	ctx := WithSessionContext(context.Background(), sc)

	got, ok := SessionContextFrom(ctx)
	if !ok {
		t.Fatal("SessionContextFrom reported no context")
	}
	if got.SessionID != id {
		t.Fatalf("session id = %s, want %s", got.SessionID, id)
	}
	if got.String(CtxTenant) != "acme" {
		t.Fatalf("tenant = %q, want acme", got.String(CtxTenant))
	}
	if got.String(CtxRun) != "" {
		t.Fatal("a non-string value must read as empty")
	}
	if _, ok := SessionContextFrom(context.Background()); ok {
		t.Fatal("a bare context reported a session context")
	}
}

// TestNewEventPayloadKeys pins the JSON keys of the event payloads.
func TestNewEventPayloadKeys(t *testing.T) {
	t.Parallel()

	ev, err := NewEvent(EventToolResult, ToolResultPayload{CallID: "c1", Name: "get_run", DataLevel: DataLogs})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	if ev.Type != EventToolResult {
		t.Fatalf("type = %q", ev.Type)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(ev.Payload, &keys); err != nil {
		t.Fatalf("payload is not an object: %v", err)
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	slices.Sort(names)
	want := []string{"call_id", "content", "data_level", "is_error", "name"}
	if !slices.Equal(names, want) {
		t.Fatalf("payload keys = %v, want %v", names, want)
	}
}

// TestParseClientContext covers the accepted and refused keys and values.
func TestParseClientContext(t *testing.T) {
	t.Parallel()

	ok, err := ParseClientContext(json.RawMessage(`{"route":"/x","tenant":"a","job":"b","run":"c","controller":"d","cell":"e"}`))
	if err != nil {
		t.Fatalf("six keys: %v", err)
	}
	if len(ok) != 6 {
		t.Fatalf("six keys parsed to %d entries", len(ok))
	}

	for _, key := range []string{"repo", "prompt", "x"} {
		raw := json.RawMessage(`{"` + key + `":"v"}`)
		_, err := ParseClientContext(raw)
		var unknown *UnknownContextKeyError
		if !errors.As(err, &unknown) || unknown.Key != key {
			t.Fatalf("key %s: error = %v, want *UnknownContextKeyError", key, err)
		}
	}

	if _, err := ParseClientContext(json.RawMessage(`{"tenant":5}`)); err == nil {
		t.Fatal("a non-string value was accepted")
	}

	empty, err := ParseClientContext(nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty raw = (%v, %v), want an empty map", empty, err)
	}
}

// TestTrimFirstMessage pins the list form of a first message: one line, at
// most FirstMessageMax runes, and a cut text ends with an ellipsis.
func TestTrimFirstMessage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"short text unchanged", "why did it fail", "why did it fail"},
		{"white space collapses", "  why did\n\tit fail  ", "why did it fail"},
		{"empty", "", ""},
		{"white space only", " \n\t ", ""},
		{"ascii cut at max", strings.Repeat("a", 200), strings.Repeat("a", FirstMessageMax-1) + "…"},
		{"runes cut by rune not byte", strings.Repeat("ü", 200), strings.Repeat("ü", FirstMessageMax-1) + "…"},
		{"scan bound cuts before collapse", strings.Repeat("a", 1100) + " " + strings.Repeat("b", 50), strings.Repeat("a", FirstMessageMax-1) + "…"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := TrimFirstMessage(c.in); got != c.want {
				t.Fatalf("TrimFirstMessage = %q, want %q", got, c.want)
			}
		})
	}
}
