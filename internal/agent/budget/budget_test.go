package budget

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// TestCheckStopRows walks the stop rows of the budget in order.
func TestCheckStopRows(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		usage    Usage
		limits   Limits
		dayTotal int64
		dayLimit int64
		want     agent.ErrorCode
		stop     bool
	}{
		{"session spent", Usage{PromptTokens: 60, CompletionTokens: 40}, Limits{Session: 100, Steps: 10}, 0, 1000, agent.CodeBudgetSession, true},
		{"day spent", Usage{}, Limits{Session: 100, Steps: 10}, 1000, 1000, agent.CodeBudgetDay, true},
		{"steps spent", Usage{Steps: 32}, Limits{Session: 1000, Steps: 32}, 0, 1000, agent.CodeSteps, true},
		{"session wins over day", Usage{PromptTokens: 100}, Limits{Session: 100, Steps: 10}, 1000, 1000, agent.CodeBudgetSession, true},
		{"nothing spent", Usage{PromptTokens: 10, Steps: 1}, Limits{Session: 100, Steps: 10}, 10, 1000, "", false},
	}
	for _, c := range cases {
		code, stop := Check(c.usage, c.limits, c.dayTotal, c.dayLimit)
		if code != c.want || stop != c.stop {
			t.Fatalf("%s: Check = (%q, %v), want (%q, %v)", c.name, code, stop, c.want, c.stop)
		}
	}
}

// TestStepBudgetStopsBeforeCall33 asserts the step limit of 32 stops the
// loop before call number 33.
func TestStepBudgetStopsBeforeCall33(t *testing.T) {
	t.Parallel()

	const limit = 32
	for steps := 0; steps < 33; steps++ {
		stopped := StepsSpent(steps, limit)
		if want := steps >= limit; stopped != want {
			t.Fatalf("steps %d: StepsSpent = %v, want %v", steps, stopped, want)
		}
	}
	if !StepsSpent(32, limit) {
		t.Fatal("call 33 is allowed")
	}
}

func TestMaxTokens(t *testing.T) {
	t.Parallel()

	if got := MaxTokens(Usage{}, 100000); got != MaxTokensPerCall {
		t.Fatalf("large budget: MaxTokens = %d, want %d", got, MaxTokensPerCall)
	}
	if got := MaxTokens(Usage{PromptTokens: 9000}, 10000); got != 1000 {
		t.Fatalf("near the limit: MaxTokens = %d, want 1000", got)
	}
	if got := MaxTokens(Usage{PromptTokens: 10000}, 10000); got != 0 {
		t.Fatalf("spent: MaxTokens = %d, want 0", got)
	}
	if got := MaxTokens(Usage{}, 0); got != 0 {
		t.Fatalf("zero limit: MaxTokens = %d, want 0", got)
	}
}
