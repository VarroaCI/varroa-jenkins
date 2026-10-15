package lint

import "testing"

// TestCellRules_OrderAndIDs asserts the cell rule set keeps its order and
// ids, and that each call returns a new slice.
func TestCellRules_OrderAndIDs(t *testing.T) {
	want := []string{"no-poll-scm", "no-global-credentials", "no-controller-config-dependency", "no-job-dsl"}
	rules := CellRules()
	if len(rules) != len(want) {
		t.Fatalf("CellRules() has %d rules, want %d", len(rules), len(want))
	}
	for i, id := range want {
		if got := rules[i].ID(); got != id {
			t.Errorf("rule %d id = %q, want %q", i, got, id)
		}
	}
	rules[0] = nil
	if CellRules()[0] == nil {
		t.Error("CellRules() shares its slice between calls")
	}
}

// TestSummary_JoinsFindings asserts Summary names each finding's rule, path,
// and message, joined by "; ".
func TestSummary_JoinsFindings(t *testing.T) {
	got := Summary([]Finding{
		{Rule: "no-poll-scm", Path: "payments/job", Message: "pollSCM"},
		{Rule: "no-job-dsl", Path: "payments/dsl", Message: "dsl"},
	})
	want := `no-poll-scm at "payments/job": pollSCM; no-job-dsl at "payments/dsl": dsl`
	if got != want {
		t.Errorf("Summary() = %q, want %q", got, want)
	}
}
