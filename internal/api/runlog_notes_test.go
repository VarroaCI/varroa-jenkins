package api

import "testing"

// --- 5.2: console-note removal ---

func TestStripConsoleNotes(t *testing.T) {
	const preamble = "\x1b[8mha:"
	const postamble = "\x1b[0m"
	// A console note's base64 payload never appears literally, but the
	// stripping rule never decodes it — any base64-alphabet run works.
	const payload = "QUJDREVGRw=="

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "a note between two words",
			input: "before " + preamble + payload + postamble + " after",
			want:  "before  after",
		},
		{
			name:  "a bare colour reset survives",
			input: "red text" + postamble + " normal text",
			want:  "red text" + postamble + " normal text",
		},
		{
			name:  "a truncated preamble is left as ordinary text",
			input: "before " + "\x1b[8mh",
			want:  "before " + "\x1b[8mh",
		},
		{
			name:  "a preamble with no base64 run drops the preamble alone",
			input: preamble + "!not base64",
			want:  "!not base64",
		},
		{
			name:  "a note immediately followed by a postamble drops both",
			input: preamble + payload + postamble,
			want:  "",
		},
		{
			name:  "a postamble not immediately following the run survives",
			input: preamble + payload + " gap " + postamble,
			want:  " gap " + postamble,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(stripConsoleNotes([]byte(tc.input)))
			if got != tc.want {
				t.Fatalf("stripConsoleNotes(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
