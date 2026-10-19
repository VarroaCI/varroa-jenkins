package jenkins

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// jenkinsErrorPage renders a body shaped like the Stapler error page a
// refused /varroa-run/* request answers.
func jenkinsErrorPage(title string) string {
	return `<!DOCTYPE html><html data-disable-sticky="false"><head resURL="/static/abc">` +
		`<title>` + title + `</title><script src="x.js"></script></head>` +
		`<body><h1>Oops!</h1><p>` + strings.Repeat("filler ", 400) + `</p></body></html>`
}

func TestRefusalTextTakesTheHTMLTitle(t *testing.T) {
	got := refusalText([]byte(jenkinsErrorPage("Not Found: no build or queue item carries run id r1 - Jenkins")))
	if got != "Not Found: no build or queue item carries run id r1" {
		t.Fatalf("refusalText = %q", got)
	}
}

func TestRefusalTextStripsTheJenkinsSuffix(t *testing.T) {
	got := refusalText([]byte("<html><head><TITLE>\n  Forbidden - Jenkins\n</TITLE></head></html>"))
	if got != "Forbidden" {
		t.Fatalf("refusalText = %q, want Forbidden", got)
	}
}

func TestRefusalTextUnescapesEntities(t *testing.T) {
	got := refusalText([]byte(jenkinsErrorPage("input &quot;deploy&quot; is not pending &amp; gone - Jenkins")))
	if got != `input "deploy" is not pending & gone` {
		t.Fatalf("refusalText = %q", got)
	}
}

func TestRefusalTextCapsALongBody(t *testing.T) {
	got := refusalText([]byte(strings.Repeat("é", 4000)))
	if n := utf8.RuneCountInString(got); n != refusalTextMaxRunes {
		t.Fatalf("rune count = %d, want %d", n, refusalTextMaxRunes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("refusalText cut a rune: %q", got)
	}
}

func TestRefusalTextKeepsAShortPlainBody(t *testing.T) {
	got := refusalText([]byte("  job team/app does not exist\n"))
	if got != "job team/app does not exist" {
		t.Fatalf("refusalText = %q", got)
	}
}

func TestRefusalTextOopsPageKeepsLoggingID(t *testing.T) {
	body := `<!DOCTYPE html><html><head><title>Jenkins</title></head>` +
		`<body><h1>Oops!</h1><p>Logging ID=5f3a9c2e-1b7d-4e8a</p></body></html>`
	got := refusalText([]byte(body))
	if got != "Jenkins internal error (Logging ID=5f3a9c2e-1b7d-4e8a)" {
		t.Fatalf("refusalText = %q", got)
	}
}

func TestRefusalTextPlainTitleUnchanged(t *testing.T) {
	body := `<html><head><title>Forbidden - Jenkins</title></head><body>Logging ID=5f3a9c2e-1b7d-4e8a</body></html>`
	if got := refusalText([]byte(body)); got != "Forbidden" {
		t.Fatalf("refusalText = %q, want Forbidden", got)
	}
}
