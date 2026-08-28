package controller

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestWrapGroovyForClassification(t *testing.T) {
	script := "import foo.Bar\nprintln 'hi'\nreturn \"ok\""
	wrapped := wrapGroovyForClassification(script)

	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	if !strings.Contains(wrapped, encoded) {
		t.Fatalf("wrapped harness does not embed the base64 script:\n%s", wrapped)
	}
	if !strings.Contains(wrapped, groovyOKSentinel) || !strings.Contains(wrapped, groovyFailedSentinel) {
		t.Fatalf("wrapped harness missing sentinels:\n%s", wrapped)
	}
	// The raw script must never appear verbatim — arbitrary content (quotes,
	// ${}, sentinels) could otherwise break out of the harness.
	if strings.Contains(wrapped, "import foo.Bar") {
		t.Fatalf("wrapped harness embeds the raw script:\n%s", wrapped)
	}
}

func TestClassifyGroovyOutput(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantOut    string
		wantErr    bool
		wantReason string
	}{
		{
			name:    "success with printed output and result",
			raw:     "hello\nResult: ok\n" + groovyOKSentinel + "\n",
			wantOut: "hello\nResult: ok",
		},
		{
			name:    "success with no output",
			raw:     groovyOKSentinel + "\n",
			wantOut: "",
		},
		{
			name: "compile failure",
			raw: "org.codehaus.groovy.control.MultipleCompilationErrorsException: startup failed:\nScript1.groovy: 2: unable to resolve class foo.Bar\n" +
				groovyFailedSentinel + " org.codehaus.groovy.control.MultipleCompilationErrorsException: startup failed:\n",
			wantOut:    "org.codehaus.groovy.control.MultipleCompilationErrorsException: startup failed:\nScript1.groovy: 2: unable to resolve class foo.Bar",
			wantErr:    true,
			wantReason: "MultipleCompilationErrorsException",
		},
		{
			name: "runtime exception after partial output",
			raw: "partial\njava.lang.IllegalStateException: boom\n\tat Script1.run(Script1.groovy:3)\n" +
				groovyFailedSentinel + " java.lang.IllegalStateException: boom\n",
			wantOut:    "partial\njava.lang.IllegalStateException: boom\n\tat Script1.run(Script1.groovy:3)",
			wantErr:    true,
			wantReason: "IllegalStateException: boom",
		},
		{
			name:       "failed sentinel with no summary",
			raw:        groovyFailedSentinel + "\n",
			wantOut:    "",
			wantErr:    true,
			wantReason: "script threw",
		},
		{
			name:       "no sentinel at all",
			raw:        "something interrupted the harness",
			wantOut:    "something interrupted the harness",
			wantErr:    true,
			wantReason: "no completion marker",
		},
		{
			name: "sentinel mentioned mid-output does not count",
			// Only a sentinel on the final line is a verdict.
			raw:        "script printed " + groovyOKSentinel + " itself\ntrailing",
			wantOut:    "script printed " + groovyOKSentinel + " itself\ntrailing",
			wantErr:    true,
			wantReason: "no completion marker",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := classifyGroovyOutput(tc.raw)
			if out != tc.wantOut {
				t.Errorf("output = %q, want %q", out, tc.wantOut)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (output %q)", out)
				}
				if !strings.Contains(err.Error(), tc.wantReason) {
					t.Errorf("error %q does not mention %q", err.Error(), tc.wantReason)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
