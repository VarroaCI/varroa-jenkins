// Console-note removal (design D5; run-log-viewer, "Console notes SHALL
// be removed by a bounded rule"). Jenkins encodes a ConsoleNote into the
// byte stream with ConsoleNote.encodeTo; the blob keeps the bytes
// unchanged, so every reader of a line strips them at read time.
package api

import (
	"bytes"
	"regexp"
	"strings"
)

// ansiEscapePattern matches an OSC sequence (ended by BEL, by ESC \, or
// by the end of the line, because the caller strips one line at a time),
// a CSI sequence, an escape sequence with optional intermediate bytes
// (such as the charset select ESC ( B), and a lone escape byte, in that
// order of preference. The lone-byte branch makes sure no escape byte is
// left after the strip.
var ansiEscapePattern = regexp.MustCompile(
	`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\|$)|` +
		`\x1b\[[0-?]*[ -/]*[@-~]|` +
		`\x1b[ -/]*[0-~]|` +
		`\x1b`,
)

// stripANSIEscapes removes every ANSI escape sequence from a log line.
// A caller that shows a line as plain text calls it after
// stripConsoleNotes, because a console note starts with an escape byte.
func stripANSIEscapes(s string) string {
	if strings.IndexByte(s, 0x1b) < 0 {
		return s
	}
	return ansiEscapePattern.ReplaceAllString(s, "")
}

const (
	// consoleNotePreamble is ConsoleNote.PREAMBLE_STR: the escape
	// character followed by "[8mha:".
	consoleNotePreamble = "\x1b[8mha:"
	// consoleNotePostamble is ConsoleNote.POSTAMBLE_STR: the escape
	// character followed by "[0m". It is also the ordinary ANSI colour
	// reset build tools emit constantly, so it is only ever dropped when
	// it immediately follows a note's base64 run.
	consoleNotePostamble = "\x1b[0m"
)

// isConsoleNoteBase64Byte reports whether b belongs to a console note's
// base64 payload alphabet.
func isConsoleNoteBase64Byte(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	case b == '+' || b == '/' || b == '=':
		return true
	}
	return false
}

// stripConsoleNotes removes every console note from data by the bounded
// rule the spec states exactly: find the preamble, consume the longest
// base64 run after it, drop both, and drop an immediately following
// postamble too — but only when a base64 run was actually consumed. It
// never scans forward for a bare postamble, and it never decodes a
// note's payload.
func stripConsoleNotes(data []byte) []byte {
	pre := []byte(consoleNotePreamble)
	post := []byte(consoleNotePostamble)

	out := make([]byte, 0, len(data))
	i := 0
	for i < len(data) {
		idx := bytes.Index(data[i:], pre)
		if idx < 0 {
			out = append(out, data[i:]...)
			break
		}
		out = append(out, data[i:i+idx]...)

		runStart := i + idx + len(pre)
		k := runStart
		for k < len(data) && isConsoleNoteBase64Byte(data[k]) {
			k++
		}
		hadRun := k > runStart
		if hadRun && k+len(post) <= len(data) && bytes.Equal(data[k:k+len(post)], post) {
			k += len(post)
		}
		i = k
	}
	return out
}
