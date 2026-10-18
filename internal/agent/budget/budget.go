// Package budget holds the spend and step checks of an agent session. The
// caller passes every limit, so a team agent may pass a lower limit than the
// assistant's.
package budget

import (
	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// MaxTokensPerCall caps the completion tokens of one provider call.
const MaxTokensPerCall = 4096

// Limits are the session limits of one run.
type Limits struct {
	Session int64
	Steps   int
}

// Usage is the usage of one session so far.
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	Steps            int
}

// Total returns the prompt and completion tokens together.
func (u Usage) Total() int64 { return u.PromptTokens + u.CompletionTokens }

// SessionSpent reports whether the session has used its token limit. A limit
// of zero or less means the session is spent.
func SessionSpent(u Usage, limit int64) bool {
	return limit <= 0 || u.Total() >= limit
}

// DaySpent reports whether the day total has reached its limit. A limit of
// zero or less means the day is spent.
func DaySpent(total, limit int64) bool {
	return limit <= 0 || total >= limit
}

// StepsSpent reports whether the step count has reached its limit. A limit
// of zero or less means no step is left.
func StepsSpent(steps, limit int) bool {
	return limit <= 0 || steps >= limit
}

// Check returns the error code of the first spent budget: the session, then
// the day, then the steps. It returns false when no budget is spent.
func Check(u Usage, l Limits, dayTotal, dayLimit int64) (agent.ErrorCode, bool) {
	switch {
	case SessionSpent(u, l.Session):
		return agent.CodeBudgetSession, true
	case DaySpent(dayTotal, dayLimit):
		return agent.CodeBudgetDay, true
	case StepsSpent(u.Steps, l.Steps):
		return agent.CodeSteps, true
	}
	return "", false
}

// MaxTokens returns the completion token cap for the next call. It is the
// smaller of MaxTokensPerCall and the tokens the session has left. It
// returns zero when the session has no tokens left.
func MaxTokens(u Usage, sessionLimit int64) int {
	left := sessionLimit - u.Total()
	if sessionLimit <= 0 || left <= 0 {
		return 0
	}
	if left < MaxTokensPerCall {
		return int(left)
	}
	return MaxTokensPerCall
}
