package bus

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// streamSubjectFilters lists the subject filter of every JetStream stream that
// the bus creates. Keep it in step with the Subjects fields of jetstream.go.
var streamSubjectFilters = []string{
	"mite.*.*.*.out",
	BuildsWildcard,
	ActivityWildcard,
	WebhookWildcard,
}

// natsSubjectMatches reports whether a NATS subject matches a filter. A "*"
// token matches one token, and a trailing ">" matches one or more tokens.
func natsSubjectMatches(filter, subject string) bool {
	f := strings.Split(filter, ".")
	s := strings.Split(subject, ".")
	for i, tok := range f {
		if tok == ">" {
			return i < len(s)
		}
		if i >= len(s) {
			return false
		}
		if tok != "*" && tok != s[i] {
			return false
		}
	}
	return len(f) == len(s)
}

// TestNoStreamCapturesCredentialSubjects pins that no JetStream stream holds a
// credential subject. The key subject and the desired-state subject are core
// NATS requests that carry credential values.
func TestNoStreamCapturesCredentialSubjects(t *testing.T) {
	credential := map[string]string{
		"assistant key set":   OperatorAssistantKeysSubject("core", "set"),
		"assistant key clear": OperatorAssistantKeysSubject("core", "clear"),
		"desired state":       MiteDesiredSubject("core", "varroa-system", "smoke"),
	}
	for name, subject := range credential {
		for _, filter := range streamSubjectFilters {
			if natsSubjectMatches(filter, subject) {
				t.Errorf("stream filter %q captures the %s subject %q", filter, name, subject)
			}
		}
	}
}

// TestNATSSubjectMatchesFilter pins the matcher the stream test relies on.
func TestNATSSubjectMatchesFilter(t *testing.T) {
	cases := []struct {
		filter, subject string
		want            bool
	}{
		{"mite.*.*.*.out", "mite.core.ns.name.out", true},
		{"mite.*.*.*.out", "mite.core.ns.name.desired", false},
		{"activity.>", "activity.a.b", true},
		{"activity.>", "activity", false},
		{"operator.>", "operator.core.assistantkeys.set", true},
	}
	for _, tc := range cases {
		if got := natsSubjectMatches(tc.filter, tc.subject); got != tc.want {
			t.Errorf("natsSubjectMatches(%q, %q) = %v, want %v", tc.filter, tc.subject, got, tc.want)
		}
	}
}

// TestAssistantKeyRequestRedactsInLogs pins that every log and format path of
// the key request hides the key.
func TestAssistantKeyRequestRedactsInLogs(t *testing.T) {
	const key = "sk-redact-probe-b41c"
	req := AssistantKeyRequest{Provider: "smoke", APIKey: []byte(key)}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("key request", "req", req)
	logger.Info("key request by value", slog.Any("req", req))
	logger.Info("key request pointer", "req", &req)

	outputs := []string{
		buf.String(),
		fmt.Sprintf("%v", req),
		fmt.Sprintf("%+v", req),
		fmt.Sprintf("%#v", req),
		req.String(),
		fmt.Sprintf("%v", &req),
	}
	for i, out := range outputs {
		if strings.Contains(out, key) {
			t.Fatalf("output %d holds the key: %s", i, out)
		}
	}
	if !strings.Contains(outputs[1], "smoke") {
		t.Fatalf("redacted form drops the provider: %s", outputs[1])
	}
}
