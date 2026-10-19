// Package jobglob evaluates the folder-relative job-path glob grammar shared
// by tenant pipeline scoping and C7's exports scoping: "*", "**", and "?".
package jobglob

import (
	"regexp"
	"strings"
	"sync"
)

// compiled caches one compiled regular expression per pattern string. A
// pattern compiles once, on first use, and every later call reuses it.
var compiled sync.Map // map[string]*regexp.Regexp

// Match reports whether a folder-relative pattern matches a folder-relative
// job path. The grammar holds three tokens: "*" matches a run of characters
// that holds no "/", "**" matches any run including "/", and "?" matches
// one character (including "/"). Every other character matches itself. A
// pattern is anchored at both ends: a match must cover the whole path.
func Match(pattern, path string) bool {
	return compile(pattern).MatchString(path)
}

// compile returns the cached regular expression for pattern, compiling and
// caching it on first use.
func compile(pattern string) *regexp.Regexp {
	if v, ok := compiled.Load(pattern); ok {
		return v.(*regexp.Regexp)
	}
	re := regexp.MustCompile(toRegexp(pattern))
	actual, _ := compiled.LoadOrStore(pattern, re)
	return actual.(*regexp.Regexp)
}

// toRegexp translates a jobglob pattern to an anchored regular expression
// string. Every literal run is passed through regexp.QuoteMeta so a pattern
// character such as "." or "+" matches only itself.
func toRegexp(pattern string) string {
	return `\A` + RegexpBody(pattern) + `\z`
}

// RegexpBody translates a jobglob pattern to a regular expression body with
// no anchors. A caller that embeds the pattern in a larger expression adds
// its own anchors. The body uses only the escapes regexp.QuoteMeta produces
// and the "[^/]*", ".*", and "." forms, so Java's regular expression engine
// reads it with the same meaning.
func RegexpBody(pattern string) string {
	var out strings.Builder

	var literal strings.Builder
	flushLiteral := func() {
		if literal.Len() > 0 {
			out.WriteString(regexp.QuoteMeta(literal.String()))
			literal.Reset()
		}
	}

	for i := 0; i < len(pattern); {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				flushLiteral()
				out.WriteString(".*")
				i += 2
				continue
			}
			flushLiteral()
			out.WriteString(`[^/]*`)
			i++
		case '?':
			flushLiteral()
			out.WriteString(".")
			i++
		default:
			literal.WriteByte(pattern[i])
			i++
		}
	}
	flushLiteral()
	return out.String()
}
