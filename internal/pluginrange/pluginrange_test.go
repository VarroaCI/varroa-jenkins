package pluginrange

import (
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/pluginver"
)

func TestParseAndMatch_SixOperators(t *testing.T) {
	// For each operator, test that the right comparisons pass/fail.
	// We use versions where we know the ordering.
	tests := []struct {
		expr    string
		version string
		want    bool
	}{
		// <=
		{"<=2.0", "2.0", true},
		{"<=2.0", "1.9", true},
		{"<=2.0", "2.1", false},

		// >=
		{">=2.0", "2.0", true},
		{">=2.0", "2.1", true},
		{">=2.0", "1.9", false},

		// !=
		{"!=2.0", "2.0", false},
		{"!=2.0", "2.1", true},
		{"!=2.0", "1.9", true},

		// <
		{"<2.0", "1.9", true},
		{"<2.0", "2.0", false},
		{"<2.0", "2.1", false},

		// >
		{">2.0", "2.1", true},
		{">2.0", "2.0", false},
		{">2.0", "1.9", false},

		// =
		{"=2.0", "2.0", true},
		{"=2.0", "2.1", false},
		{"=2.0", "1.9", false},
	}
	for _, tt := range tests {
		e, err := Parse(tt.expr)
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.expr, err)
			continue
		}
		got := e.Match(tt.version)
		if got != tt.want {
			t.Errorf("Expr(%q).Match(%q) = %v, want %v", tt.expr, tt.version, got, tt.want)
		}
	}
}

func TestParse_LongestMatch(t *testing.T) {
	// <=2.0 must be read as <= with operand "2.0", never as < with operand "=2.0".
	e, err := Parse("<=2.0")
	if err != nil {
		t.Fatalf("Parse(<=2.0): %v", err)
	}
	// "2.0" should match <= 2.0 → true.
	if !e.Match("2.0") {
		t.Error("<=2.0 should match 2.0")
	}
	// "1.9" should match <= 2.0 → true.
	if !e.Match("1.9") {
		t.Error("<=2.0 should match 1.9")
	}
	// "2.1" should NOT match <= 2.0 → false.
	if e.Match("2.1") {
		t.Error("<=2.0 should NOT match 2.1")
	}
	// If it were parsed as < with operand "=2.0", then "=2.0" would match exactly.
	// But it shouldn't — "=2.0" (the version string) compared to "=2.0" (the operand)
	// would be equal under =, but here we use <, so it wouldn't match.
	// The key test: if <=2.0 is parsed correctly, "=2.0" (as a version string) should
	// match if Compare("=2.0", "2.0") <= 0.
	// Actually let's just verify the internal clause.
	if len(e.clauses) != 1 {
		t.Fatalf("expected 1 clause, got %d", len(e.clauses))
	}
	if e.clauses[0].op != "<=" {
		t.Errorf("operator = %q, want <=", e.clauses[0].op)
	}
	if e.clauses[0].operand != "2.0" {
		t.Errorf("operand = %q, want 2.0", e.clauses[0].operand)
	}

	// Also test >=1.0 is not read as > with operand =1.0.
	e2, err := Parse(">=1.0")
	if err != nil {
		t.Fatalf("Parse(>=1.0): %v", err)
	}
	if e2.clauses[0].op != ">=" {
		t.Errorf("operator = %q, want >=", e2.clauses[0].op)
	}
	if e2.clauses[0].operand != "1.0" {
		t.Errorf("operand = %q, want 1.0", e2.clauses[0].operand)
	}
}

func TestParse_MultiClauseAND(t *testing.T) {
	// >=4.0.0,<4.1.0 — both must hold.
	e, err := Parse(">=4.0.0,<4.1.0")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if e.Match("4.0.0") != true {
		t.Error("4.0.0 should match >=4.0.0,<4.1.0")
	}
	if e.Match("4.0.5") != true {
		t.Error("4.0.5 should match >=4.0.0,<4.1.0")
	}
	if e.Match("3.9.0") != false {
		t.Error("3.9.0 should NOT match >=4.0.0,<4.1.0")
	}
	if e.Match("4.1.0") != false {
		t.Error("4.1.0 should NOT match >=4.0.0,<4.1.0")
	}
}

func TestEmpty(t *testing.T) {
	// Nil/empty string produces an expression with Empty() == true.
	e, err := Parse("")
	if err != nil {
		t.Fatalf("Parse(\"\"): %v", err)
	}
	if !e.Empty() {
		t.Error("empty expression should report Empty() == true")
	}
	// An empty expression matches everything.
	if !e.Match("any-version") {
		t.Error("empty expression should match any version")
	}

	// A non-empty expression has Empty() == false.
	e2, err := Parse("=1.0")
	if err != nil {
		t.Fatalf("Parse(=1.0): %v", err)
	}
	if e2.Empty() {
		t.Error("non-empty expression should report Empty() == false")
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		input  string
		reason string
	}{
		// Empty clauses: leading, trailing, doubled comma.
		{"", ""}, // empty string → no error, Empty() == true
		{",>=4.0.0", "empty clause"},
		{">=4.0.0,", "empty clause"},
		{">=4.0.0,,<4.1.0", "empty clause"},

		// No recognised comparison operator.
		{"4.0.0", "no recognised comparison operator"},
		{"foo", "no recognised comparison operator"},

		// Empty operand.
		{"<", "empty operand"},
		{"<=", "empty operand"},
		{"<  ", "empty operand"},
	}
	for _, tt := range tests {
		if tt.reason == "" {
			continue
		}
		_, err := Parse(tt.input)
		if err == nil {
			t.Errorf("Parse(%q): expected error, got nil", tt.input)
			continue
		}
		pe, ok := err.(*ParseError)
		if !ok {
			t.Errorf("Parse(%q): expected *ParseError, got %T: %v", tt.input, err, err)
			continue
		}
		if pe.Reason != tt.reason {
			t.Errorf("Parse(%q): reason = %q, want %q", tt.input, pe.Reason, tt.reason)
		}
	}
}

func TestParseError_Error(t *testing.T) {
	pe := &ParseError{Clause: "foo", Reason: "no recognised comparison operator"}
	msg := pe.Error()
	if !strings.Contains(msg, "no recognised comparison operator") {
		t.Errorf("Error() = %q, want it to contain the reason", msg)
	}
	if !strings.Contains(msg, "foo") {
		t.Errorf("Error() = %q, want it to contain the clause", msg)
	}
}

// TestNonTransitiveTriple verifies that matching is unaffected by the known
// non-transitivity triple: Compare("1","1-sp") < 0, Compare("1-sp","1.0a1") < 0,
// yet Compare("1","1.0a1") > 0.
func TestNonTransitiveTriple(t *testing.T) {
	// Pin the non-transitivity first.
	a, b, c := "1", "1-sp", "1.0a1"
	if pluginver.Compare(a, b) >= 0 {
		t.Fatalf("Compare(%q, %q) = %d, want < 0", a, b, pluginver.Compare(a, b))
	}
	if pluginver.Compare(b, c) >= 0 {
		t.Fatalf("Compare(%q, %q) = %d, want < 0", b, c, pluginver.Compare(b, c))
	}
	if pluginver.Compare(a, c) <= 0 {
		t.Fatalf("Compare(%q, %q) = %d, want > 0", a, c, pluginver.Compare(a, c))
	}

	// Matching is pairwise: each clause compares against its operand directly.
	// The non-transitivity doesn't affect individual clause matching.

	// "1" >= "1" is true, "1-sp" >= "1" is true, "1.0a1" >= "1" is false
	// because Compare("1.0a1", "1") = -1 (1.0a1 < 1 due to alpha qualifier).
	eGE, _ := Parse(">=1")
	if !eGE.Match(a) {
		t.Error(">=1 should match 1")
	}
	if !eGE.Match(b) {
		t.Error(">=1 should match 1-sp")
	}
	if eGE.Match(c) {
		t.Error(">=1 should NOT match 1.0a1")
	}

	// "1-sp" <= "1-sp" is true, but "1" <= "1-sp" is true (since "1" < "1-sp"),
	// and "1.0a1" <= "1-sp" is false (since "1-sp" < "1.0a1").
	eLE, _ := Parse("<=1-sp")
	if !eLE.Match(a) {
		t.Error("1 <= 1-sp should be true")
	}
	if !eLE.Match(b) {
		t.Error("1-sp <= 1-sp should be true")
	}
	if eLE.Match(c) {
		t.Error("1.0a1 <= 1-sp should be false")
	}

	// Each version independently compared: matching is deterministic.
}

// TestCompareCycleNonImpact proves that the cycle does not cause Match to
// produce different results on repeated calls.
func TestCompareCycleNonImpact(t *testing.T) {
	e, _ := Parse(">=1,<=1.0a1")
	// "1" >= "1": true. "1" <= "1.0a1": false (1 > 1.0a1). So false.
	if e.Match("1") {
		t.Error(`"1" should not match >=1,<=1.0a1 because 1 > 1.0a1`)
	}
	// "1-sp" >= "1": true. "1-sp" <= "1.0a1": true (1-sp < 1.0a1). So true.
	if !e.Match("1-sp") {
		t.Error(`"1-sp" should match >=1,<=1.0a1`)
	}
	// Repeat: deterministic.
	if e.Match("1-sp") != true {
		t.Error("second call should return same result")
	}
}
