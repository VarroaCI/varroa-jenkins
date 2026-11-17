package v1alpha1

import (
	"fmt"
	"regexp"
	"strings"
)

// javaCommentsFlag finds an inline flag group that sets Java COMMENTS mode,
// such as "(?x)" or "(?ix:". In that mode Java skips white space and "#"
// comments, so checkJavaPattern cannot read the pattern and accepts it.
var javaCommentsFlag = regexp.MustCompile(`\(\?[a-zA-Z-]*x`)

// checkJavaPattern returns the reason that java.util.regex.Pattern.compile
// refuses p, or "" when it finds none. It reports only a syntax error that
// Java always refuses, so it never refuses a pattern that Java compiles. An
// empty result does not prove that Java compiles p. A pattern with "\Q" or
// COMMENTS mode is accepted unread, because Java quotes and comments move
// token starts. TestCheckJavaPatternMatchesJava must pass after any change.
func checkJavaPattern(p string) string {
	if javaCommentsFlag.MatchString(p) || strings.Contains(p, `\Q`) {
		return ""
	}
	n := len(p)
	depth := 0
	// atStart: a quantifier here has no atom (pattern start, after "(" or "|").
	// afterQuant: a quantifier here follows a complete quantifier.
	atStart, afterQuant := true, false
	for i := 0; i < n; {
		c := p[i]
		switch c {
		case '\\':
			if i+1 >= n {
				return "unescaped trailing backslash"
			}
			end, known := skipJavaEscape(p, i)
			if !known {
				return ""
			}
			i = end
			atStart, afterQuant = false, false
		case '[':
			end, closed, known := skipJavaClass(p, i)
			if !known {
				return ""
			}
			if !closed {
				return "unclosed character class"
			}
			i = end
			atStart, afterQuant = false, false
		case '(':
			depth++
			if i+1 < n && p[i+1] == '?' {
				if i+2 < n && strings.IndexByte(":=!<>imsducxU-)", p[i+2]) < 0 {
					return `unknown inline modifier after "(?"`
				}
				i += 2
				atStart = false
			} else {
				i++
				atStart = true
			}
			afterQuant = false
		case ')':
			depth--
			if depth < 0 {
				return "unmatched closing ')'"
			}
			i++
			atStart, afterQuant = false, false
		case '|':
			i++
			atStart, afterQuant = true, false
		case '*', '+', '?':
			if atStart || afterQuant {
				return fmt.Sprintf("dangling meta character '%c'; a Pattern scope takes a regex, not a glob, so write .* for any text", c)
			}
			i++
			if i < n && (p[i] == '?' || p[i] == '+') {
				i++
			}
			afterQuant = true
		case '{':
			if i+1 >= n || p[i+1] < '0' || p[i+1] > '9' {
				return "illegal repetition: '{' must start a count such as {2}; a Pattern scope takes a regex, not a glob"
			}
			i++
			atStart, afterQuant = false, false
		default:
			i++
			atStart, afterQuant = false, false
		}
	}
	if depth > 0 {
		return "unclosed group"
	}
	return ""
}

// skipJavaEscape returns the index after the escape that starts at p[i], a
// backslash with at least one byte after it. known is false for an escape
// whose end it cannot find. \p{..}, \x{..}, \N{..}, and \b{g} take a braced
// argument, and \c takes the next byte, also "(" or "[".
func skipJavaEscape(p string, i int) (end int, known bool) {
	n := len(p)
	switch p[i+1] {
	case 'p', 'P', 'x', 'N', 'b', 'B':
		if i+2 < n && p[i+2] == '{' {
			brace := strings.IndexByte(p[i+3:], '}')
			if brace < 0 {
				return 0, false
			}
			return i + 3 + brace + 1, true
		}
	case 'c':
		if i+2 >= n {
			return 0, false
		}
		return i + 3, true
	}
	return i + 2, true
}

// skipJavaClass reads the character class that starts with "[" at p[i] the
// way Java reads it. A "[" inside it opens a nested class. A "]" closes the
// class only after the class holds an item, so a first "]" is a literal.
// "&&" adds no item. end is the index after the closing "]". closed is false
// when the pattern ends inside the class. known is false for an escape that
// skipJavaEscape cannot read.
func skipJavaClass(p string, i int) (end int, closed, known bool) {
	n := len(p)
	i++
	if i < n && p[i] == '^' {
		i++
	}
	hasItem := false
	for i < n {
		switch {
		case p[i] == '[':
			e, c, k := skipJavaClass(p, i)
			if !k || !c {
				return e, c, k
			}
			i = e
			hasItem = true
		case p[i] == ']':
			if hasItem {
				return i + 1, true, true
			}
			hasItem = true
			i++
		case p[i] == '\\':
			if i+1 >= n {
				return n, false, true
			}
			e, k := skipJavaEscape(p, i)
			if !k {
				return n, false, false
			}
			i = e
			hasItem = true
		case p[i] == '&' && i+1 < n && p[i+1] == '&':
			i += 2
		default:
			hasItem = true
			i++
		}
	}
	return n, false, true
}
