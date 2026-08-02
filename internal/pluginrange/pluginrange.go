// Package pluginrange parses and matches version-range expressions against
// installed plugin versions. It consumes internal/pluginver (total per R12) and
// must never import or extend internal/jenkinsver, which is scoped to Jenkins
// CORE container tags and would truncate plugin versions.
//
// There is no "unknown" match verdict anywhere in this package — pluginver is
// total, so every installed version is orderable and every clause yields true or
// false.
//
// This package is a shared implementation surface allocated to T2.2 by R24.
// T3.2's approved-plugin set must CONSUME this package, not fork it.
package pluginrange

import (
	"fmt"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/pluginver"
)

// Expr is a parsed, immutable conjunction of clauses.
// Its zero value matches everything.
type Expr struct {
	clauses []clause
}

type clause struct {
	op      string
	operand string
}

// operators in longest-prefix-first order — ≤ must match before <.
var operators = []string{"<=", ">=", "!=", "<", ">", "="}

// Parse compiles a version-range expression. A nil or empty string produces an
// expression that matches every version (Match returns true, Empty returns true).
func Parse(s string) (Expr, error) {
	if s == "" {
		return Expr{}, nil
	}

	parts := strings.Split(s, ",")
	clauses := make([]clause, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			return Expr{}, &ParseError{Clause: part, Reason: "empty clause"}
		}

		var op string
		var operand string
		found := false
		for _, candidate := range operators {
			if strings.HasPrefix(trimmed, candidate) {
				op = candidate
				operand = strings.TrimSpace(trimmed[len(candidate):])
				found = true
				break
			}
		}
		if !found {
			return Expr{}, &ParseError{Clause: trimmed, Reason: "no recognised comparison operator"}
		}
		if operand == "" {
			return Expr{}, &ParseError{Clause: trimmed, Reason: "empty operand"}
		}
		clauses = append(clauses, clause{op: op, operand: operand})
	}

	return Expr{clauses: clauses}, nil
}

// Match reports whether an installed version satisfies every clause.
// Total: pluginver is total, so this never fails and has no unknown verdict.
func (e Expr) Match(installed string) bool {
	for _, c := range e.clauses {
		cmp := pluginver.Compare(installed, c.operand)
		if !matchOp(cmp, c.op) {
			return false
		}
	}
	return true
}

// matchOp relates the Compare result to 0 under the given operator.
// All six relations share this one code path.
func matchOp(cmp int, op string) bool {
	switch op {
	case "<=":
		return cmp <= 0
	case ">=":
		return cmp >= 0
	case "!=":
		return cmp != 0
	case "<":
		return cmp < 0
	case ">":
		return cmp > 0
	case "=":
		return cmp == 0
	default:
		// Unreachable: Parse rejects unknown operators.
		return false
	}
}

// Empty reports whether the expression imposes no constraint.
func (e Expr) Empty() bool {
	return len(e.clauses) == 0
}

// ParseError names the offending clause so the handler can put it in the 400 body.
type ParseError struct {
	Clause string
	Reason string // "empty clause" | "no recognised comparison operator" | "empty operand"
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("pluginrange: %s in clause %q", e.Reason, e.Clause)
}
