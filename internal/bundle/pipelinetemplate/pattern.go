package pipelinetemplate

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxPatternLength is the maximum length, in characters, of a pattern.
const maxPatternLength = 200

// maxRepeatBound is the maximum value of m or n in a {m}, {m,}, or {m,n}
// repetition. RE2 rejects a larger repetition count.
const maxRepeatBound = 1000

var repeatBodyPattern = regexp.MustCompile(`^[0-9]+(,[0-9]*)?$`)

// CheckPattern returns every reason pattern falls outside the portable
// construct whitelist from design D3. An empty result means pattern is
// accepted.
func CheckPattern(pattern string) []string {
	var findings []string
	if utf8.RuneCountInString(pattern) > maxPatternLength {
		findings = append(findings, fmt.Sprintf("pattern exceeds the %d character limit", maxPatternLength))
	}
	_, scanFindings := scanPattern(pattern)
	return append(findings, scanFindings...)
}

// NormalizePattern rewrites every unescaped "$" outside a character class
// to "\z", so the pattern means the end of the input in every engine. The
// caller must have already accepted pattern with CheckPattern.
func NormalizePattern(pattern string) string {
	normalized, _ := scanPattern(pattern)
	return normalized
}

// isClassEscape reports whether esc is an allowed escape inside a character
// class: the class escapes, the literal-escape set, and "-".
func isClassEscape(esc rune) bool {
	switch esc {
	case 'd', 'D', 'w', 'W',
		'.', '^', '$', '|', '?', '*', '+', '(', ')', '[', ']', '{', '}', '/', '\\', '-':
		return true
	default:
		return false
	}
}

// scanPattern walks pattern left to right once, in lockstep producing the
// normalized form (every unescaped, unclassed "$" rewritten to "\z") and
// every reason pattern falls outside the whitelist: an escape outside the
// allowed set, "\-" outside a character class, an oversized repetition, a
// quantifier with nothing to quantify or following a closing parenthesis,
// and an unbalanced character class or group.
func scanPattern(pattern string) (string, []string) {
	var out strings.Builder
	var findings []string
	fail := func(format string, args ...any) {
		findings = append(findings, fmt.Sprintf(format, args...))
	}

	runes := []rune(pattern)
	n := len(runes)
	i := 0
	inClass := false
	groupDepth := 0
	// lastAtom is "" (nothing to quantify), "atom" (an ordinary atom, may be
	// quantified), or "group" (a just-closed group, which may never be
	// quantified, per the catastrophic-backtracking rule).
	lastAtom := ""

	checkQuantifierContext := func() {
		switch lastAtom {
		case "":
			fail("quantifier has nothing to quantify")
		case "group":
			fail("a quantifier may not follow a closing parenthesis")
		case "quantified":
			fail("a quantifier may not follow another quantifier")
		}
	}

	for i < n {
		ch := runes[i]
		if inClass {
			switch ch {
			case '\\':
				if i+1 >= n {
					fail("trailing backslash has nothing to escape")
					i++
					continue
				}
				esc := runes[i+1]
				if !isClassEscape(esc) {
					fail("escape \\%c is not in the allowed set", esc)
				}
				out.WriteRune(ch)
				out.WriteRune(esc)
				i += 2
			case ']':
				inClass = false
				out.WriteRune(ch)
				i++
				lastAtom = "atom"
			default:
				out.WriteRune(ch)
				i++
			}
			continue
		}
		switch ch {
		case '\\':
			if i+1 >= n {
				fail("trailing backslash has nothing to escape")
				i++
				continue
			}
			esc := runes[i+1]
			switch esc {
			case 's', 'S':
				fail("escape \\%c is rejected because RE2, java.util.regex, and ECMA-262 disagree on it", esc)
			case 'b':
				fail("escape \\b is not in the allowed set")
			case '-':
				fail("escape \\- is allowed only inside a character class")
			case 'd', 'D', 'w', 'W':
				// allowed class escapes, also usable outside a class
			case '.', '^', '$', '|', '?', '*', '+', '(', ')', '[', ']', '{', '}', '/', '\\':
				// allowed literal escapes
			default:
				fail("escape \\%c is not in the allowed set", esc)
			}
			out.WriteRune(ch)
			out.WriteRune(esc)
			i += 2
			lastAtom = "atom"
		case '[':
			inClass = true
			out.WriteRune(ch)
			i++
			if i < n && runes[i] == '^' {
				out.WriteRune('^')
				i++
			}
		case ']':
			fail("unmatched \"]\" outside a character class")
			out.WriteRune(ch)
			i++
		case '(':
			groupDepth++
			out.WriteRune(ch)
			i++
			switch {
			case i+1 < n && runes[i] == '?' && runes[i+1] == ':':
				out.WriteRune('?')
				out.WriteRune(':')
				i += 2
			case i < n && runes[i] == '?':
				next := rune(0)
				if i+1 < n {
					next = runes[i+1]
				}
				switch next {
				case '=', '!':
					fail("lookahead is not in the allowed set")
				case '<':
					fail("lookbehind or a named group is not in the allowed set")
				default:
					fail("only a non-capturing group \"(?:...)\" may start with \"?\"")
				}
				out.WriteRune('?')
				i++
			}
			lastAtom = ""
		case ')':
			if groupDepth == 0 {
				fail("unmatched \")\"")
			} else {
				groupDepth--
			}
			out.WriteRune(ch)
			i++
			lastAtom = "group"
		case '|':
			out.WriteRune(ch)
			i++
			lastAtom = ""
		case '^':
			out.WriteRune(ch)
			i++
			lastAtom = "atom"
		case '$':
			out.WriteString(`\z`)
			i++
			lastAtom = "atom"
		case '*', '+', '?':
			checkQuantifierContext()
			out.WriteRune(ch)
			i++
			lastAtom = "quantified"
		case '{':
			j := i + 1
			for j < n && runes[j] != '}' {
				j++
			}
			if j >= n {
				fail("unterminated repetition")
				out.WriteRune(ch)
				i++
				continue
			}
			body := string(runes[i+1 : j])
			checkRepeatBody(body, fail)
			checkQuantifierContext()
			out.WriteRune('{')
			out.WriteString(body)
			out.WriteRune('}')
			i = j + 1
			lastAtom = "quantified"
		default:
			out.WriteRune(ch)
			i++
			lastAtom = "atom"
		}
	}
	if inClass {
		fail("unterminated character class")
	}
	if groupDepth != 0 {
		fail("unbalanced parentheses")
	}
	return out.String(), findings
}

// checkRepeatBody validates the m, m,, or m,n body of a {...} repetition:
// each bound is at most maxRepeatBound, RE2's limit, and an upper bound is
// not less than the lower bound.
func checkRepeatBody(body string, fail func(format string, args ...any)) {
	if !repeatBodyPattern.MatchString(body) {
		fail("repetition {%s} is not m, m,, or m,n", body)
		return
	}
	parts := strings.SplitN(body, ",", 2)
	m, err := strconv.Atoi(parts[0])
	if err != nil {
		fail("repetition {%s} does not parse", body)
		return
	}
	if m > maxRepeatBound {
		fail("repetition exceeds the %d limit", maxRepeatBound)
	}
	if len(parts) == 2 && parts[1] != "" {
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			fail("repetition {%s} does not parse", body)
			return
		}
		if n > maxRepeatBound {
			fail("repetition exceeds the %d limit", maxRepeatBound)
		}
		if n < m {
			fail("repetition upper bound is less than the lower bound in {%s}", body)
		}
	}
}
