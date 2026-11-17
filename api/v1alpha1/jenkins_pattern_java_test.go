package v1alpha1

import (
	"bytes"
	"encoding/hex"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// javaPatternTokens are the pieces that random patterns join. They cover
// the metacharacters, the escapes, and the group and class forms that the
// checker reads.
var javaPatternTokens = []string{
	"(", ")", "[", "]", "{", "}", "*", "+", "?", "|", `\`, "^", "$", "-", "&", "&&", ".", ":", "=", "!", "<", ">", "#", " ", "\t", "\n",
	"x", "Q", "E", "p", "k", "c", "b", "g", "N", "P", "1", "2", ",", "a", "/", "é", "日",
	"(?", "(?x)", "(?i)", "(?i:", "(?-", "(?<", "(?<n>", "(?<=", "(?!", "(?>", "(?P<n>", "(?#",
	`\Q`, `\E`, `\p{L}`, `\p{`, `\x{`, `\x{41}`, `\N{`, `\b{g}`, `\B{g}`, `\k<n>`, `\c`, `\\`, `\d`, `\é`,
	"[^", "[[", "]]", "{2}", "{2,}", "{,2}", "*+", "?+", "++",
}

// TestCheckJavaPatternMatchesJava checks checkJavaPattern against the JDK
// regex compiler. The test skips when java is not on PATH. Run it locally
// after any change to the checker.
func TestCheckJavaPatternMatchesJava(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if _, err := exec.LookPath("java"); err != nil {
		t.Skip("java is not on PATH")
	}
	rng := rand.New(rand.NewSource(1))
	const count = 30000
	patterns := make([]string, 0, count)
	for i := 0; i < count; i++ {
		n := 1 + rng.Intn(24)
		var b strings.Builder
		for j := 0; j < n; j++ {
			b.WriteString(javaPatternTokens[rng.Intn(len(javaPatternTokens))])
		}
		patterns = append(patterns, b.String())
	}

	var in bytes.Buffer
	for _, p := range patterns {
		in.WriteString(hex.EncodeToString([]byte(p)))
		in.WriteByte('\n')
	}

	dir := filepath.Join("testdata", "javapattern")
	cmd := exec.Command("java", filepath.Join(dir, "Oracle.java"))
	cmd.Stdin = &in
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("java oracle: %v", err)
	}
	verdicts := strings.Fields(string(out))
	if len(verdicts) != len(patterns) {
		t.Fatalf("java oracle printed %d verdicts for %d patterns", len(verdicts), len(patterns))
	}
	for i, p := range patterns {
		if verdicts[i] != "1" {
			continue
		}
		if reason := checkJavaPattern(p); reason != "" {
			t.Errorf("checkJavaPattern(%q) refused a pattern Java compiles: %s", p, reason)
		}
	}
}
