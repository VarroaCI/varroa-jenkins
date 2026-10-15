package recordstore

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCapWebhookErrorKeepsValidUTF8AtTheLimit asserts a multi-byte character
// that crosses the stored limit is dropped whole, so the text is valid UTF-8
// and Postgres accepts it.
func TestCapWebhookErrorKeepsValidUTF8AtTheLimit(t *testing.T) {
	t.Parallel()

	// Each "é" is two bytes. A run of them is longer than the limit, and an
	// odd limit boundary falls inside one character.
	// The single leading byte puts the limit boundary inside a two-byte
	// character. An even-length input would never split one.
	long := "x" + strings.Repeat("é", webhookErrorLimit)
	got := capWebhookError(long)
	if !utf8.ValidString(got) {
		t.Fatalf("capWebhookError produced invalid UTF-8")
	}
	if len(got) > webhookErrorLimit {
		t.Fatalf("len = %d, want at most %d", len(got), webhookErrorLimit)
	}
	if short := capWebhookError("HTTP 500"); short != "HTTP 500" {
		t.Fatalf("short message changed: %q", short)
	}
}
