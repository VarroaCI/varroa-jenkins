package main

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// hit is one host literal found in a source file.
type hit struct {
	Host string
	File string // slash-separated, relative to the repository root
	Line int
}

// urlPattern matches a URL with a scheme. The host stops at the first path,
// query, fragment, quote, or space. Userinfo and the port are removed later.
var urlPattern = regexp.MustCompile(`(?i)\b(?:https?|wss?|oci|ssh|git|tls|nats)://([^/\s"'` + "`" + `<>?#]+)`)

// schemeRelativePattern matches a protocol-relative URL in a line of text,
// such as "//cdn.host.io/x.js". The "//" must not follow a scheme byte.
var schemeRelativePattern = regexp.MustCompile(`(?i)(?:^|[^:\w/])//([^/\s"'` + "`" + `<>?#();,{}]+)`)

// schemeRelativeLiteral matches a string literal whose whole value is a
// protocol-relative URL.
var schemeRelativeLiteral = regexp.MustCompile(`(?i)^//([^/\s"'` + "`" + `<>?#();,{}]+)`)

// goImagePattern matches an image reference inside a Go string literal. The
// reference must carry a tag. The tag may be empty, as in "jenkins/jenkins:".
var goImagePattern = regexp.MustCompile(`^((?:[a-z0-9-]+\.)+[a-z]{2,}(?::\d+)?/)?[a-z0-9._-]+(?:/[a-z0-9._-]+)+:[A-Za-z0-9._-]*$`)

// keyImagePattern matches an image reference held in a YAML or text key.
// Chart values often name a repository without a slash, such as "nats", and the
// tag sits under its own key. The tag is therefore optional here.
var keyImagePattern = regexp.MustCompile(`^((?:[a-z0-9-]+\.)+[a-z]{2,}(?::\d+)?/)?[a-z0-9._-]+(?:/[a-z0-9._-]+)*(?::[A-Za-z0-9._-]+)?$`)

// imageKeyLine matches a text line that names an image in a template or script.
var imageKeyLine = regexp.MustCompile(`^\s*-?\s*(image|imageName|repository)\s*:\s*(.*)$`)

// goTemplateComment matches a Go template comment, with or without trim markers.
var goTemplateComment = regexp.MustCompile(`(?s)\{\{-?\s*/\*.*?\*/\s*-?\}\}`)

// yamlImageKeys names the keys whose scalar value is an image reference.
var yamlImageKeys = map[string]bool{"image": true, "repository": true, "imageName": true}

// urlHosts returns the host of every URL with a scheme in s.
func urlHosts(s string) []string {
	matches := urlPattern.FindAllStringSubmatch(s, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, hostOf(m[1]))
	}
	return out
}

// hostOf returns the bare, lowercase host of an authority such as
// "user@host:8443". It drops userinfo and the port.
func hostOf(authority string) string {
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		authority = authority[i+1:]
	}
	if strings.HasPrefix(authority, "[") {
		// An IPv6 literal keeps its address without the brackets or the port.
		if end := strings.Index(authority, "]"); end > 0 {
			return strings.ToLower(authority[1:end])
		}
		return strings.ToLower(authority)
	}
	if i := strings.Index(authority, ":"); i >= 0 {
		authority = authority[:i]
	}
	return strings.ToLower(authority)
}

// imageHost returns the registry host of an image reference. A reference
// whose first segment has no dot or port belongs to docker.io.
func imageHost(ref string) string {
	first, _, found := strings.Cut(ref, "/")
	if !found || !strings.ContainsAny(first, ".:") {
		return "docker.io"
	}
	host, _, _ := strings.Cut(first, ":")
	return strings.ToLower(host)
}

// literalURLHits returns one hit per URL with a scheme in val.
func literalURLHits(file string, line int, val string) []hit {
	hosts := urlHosts(val)
	out := make([]hit, 0, len(hosts))
	for _, host := range hosts {
		out = append(out, hit{Host: host, File: file, Line: line})
	}
	return out
}

// literalHits returns the URL and image hits for one string value.
func literalHits(file string, line int, val string, goRules bool) []hit {
	out := literalURLHits(file, line, val)
	pattern := keyImagePattern
	if goRules {
		pattern = goImagePattern
	}
	if pattern.MatchString(val) {
		out = append(out, hit{Host: imageHost(val), File: file, Line: line})
	}
	return out
}

// extractGo parses one Go file and returns the literal hits. The walk skips
// import specs, so an import path is never read as a destination. Comments are
// not part of the AST walk, so comment text never produces a hit.
func extractGo(rel string, src []byte) ([]hit, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []hit
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ImportSpec:
			return false
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return true
			}
			val, err := strconv.Unquote(x.Value)
			if err != nil {
				return true
			}
			out = append(out, literalHits(rel, fset.Position(x.Pos()).Line, val, true)...)
		}
		return true
	})
	return out, nil
}

// extractYAML walks every document of a YAML file. The parser drops comments,
// so a URL in a comment is never a hit. Scalars under an image key are
// checked as image references.
func extractYAML(rel string, src []byte) ([]hit, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var out []hit
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		walkYAML(&doc, rel, "", &out)
	}
}

func walkYAML(n *yaml.Node, rel, key string, out *[]hit) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			walkYAML(c, rel, "", out)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			walkYAML(n.Content[i+1], rel, n.Content[i].Value, out)
		}
	case yaml.ScalarNode:
		*out = append(*out, literalURLHits(rel, n.Line, n.Value)...)
		if yamlImageKeys[key] && keyImagePattern.MatchString(n.Value) {
			*out = append(*out, hit{Host: imageHost(n.Value), File: rel, Line: n.Line})
		}
	}
}

// extractText scans a template or a script. A line that starts with "#" is a
// comment, and so is a Go template comment, so both are removed before the
// scan. Line numbers are kept.
func extractText(rel string, src []byte) []hit {
	s := goTemplateComment.ReplaceAllStringFunc(string(src), func(m string) string {
		return strings.Repeat("\n", strings.Count(m, "\n"))
	})
	var out []hit
	for i, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, host := range urlHosts(line) {
			out = append(out, hit{Host: host, File: rel, Line: i + 1})
		}
		if m := imageKeyLine.FindStringSubmatch(line); m != nil {
			ref := strings.Trim(strings.TrimSpace(m[2]), `"'`)
			if keyImagePattern.MatchString(ref) {
				out = append(out, hit{Host: imageHost(ref), File: rel, Line: i + 1})
			}
		}
	}
	return out
}

// extractJava scans Java or Groovy source. A small lexer drops line and block
// comments and reads string and character literals. Only the contents of
// literals are scanned for URLs.
func extractJava(rel string, src []byte) []hit {
	var out []hit
	n := len(src)
	line := 1
	i := 0
	for i < n {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			i += 2
			for i < n && (src[i] != '*' || i+1 >= n || src[i+1] != '/') {
				if src[i] == '\n' {
					line++
				}
				i++
			}
			i += 2
		case bytes.HasPrefix(src[i:], []byte(`"""`)):
			start := line
			i += 3
			j := i
			for i < n && !bytes.HasPrefix(src[i:], []byte(`"""`)) {
				if src[i] == '\n' {
					line++
				}
				if src[i] == '\\' {
					i++
				}
				i++
			}
			end := min(i, n)
			out = append(out, literalURLHits(rel, start, string(src[j:end]))...)
			i += 3
		case c == '"' || c == '\'':
			start := line
			quote := c
			i++
			j := i
			for i < n && src[i] != quote && src[i] != '\n' {
				if src[i] == '\\' {
					i++
				}
				i++
			}
			end := min(i, n)
			out = append(out, literalURLHits(rel, start, string(src[j:end]))...)
			i++
		default:
			i++
		}
	}
	return out
}

// extractTS scans TypeScript, TSX, or JavaScript source. A small lexer drops
// line and block comments, skips regular expression literals, and reads string
// and template literals. Only the text of those literals is scanned for URLs.
//
// Limits: JSX text with an apostrophe opens a string that ends at the line
// end, so a mismatch stays on one line. A "//" in JSX text that is not part of
// a URL scheme reads as a comment.
func extractTS(rel string, src []byte) []hit {
	lx := &tsLexer{rel: rel, src: src, line: 1}
	lx.lexCode(false)
	return lx.out
}

// tsLexer holds the scan state of extractTS.
type tsLexer struct {
	rel       string
	src       []byte
	i         int
	line      int
	out       []hit
	prev      byte   // last significant code byte, 0 at the start of input
	prevWord  string // last identifier, valid while prevIdent is true
	prevIdent bool
}

// tsKeywordsBeforeRegex lists the words after which a slash starts a regex.
var tsKeywordsBeforeRegex = map[string]bool{
	"return": true, "typeof": true, "case": true, "do": true, "else": true,
	"in": true, "of": true, "new": true, "delete": true, "void": true,
	"throw": true, "instanceof": true, "yield": true, "await": true,
}

// tsOperatorsBeforeRegex lists the bytes after which a slash starts a regex.
// A "<" is absent on purpose: "</p>" in JSX is a closing tag, not a regex.
const tsOperatorsBeforeRegex = "(,=:[!&|?{};+-*%>~^"

func isASCIILetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isTSIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func (lx *tsLexer) setPrev(c byte) {
	lx.prev = c
	lx.prevIdent = false
}

func (lx *tsLexer) peekNext() byte {
	if lx.i+1 < len(lx.src) {
		return lx.src[lx.i+1]
	}
	return 0
}

// regexAllowed reports whether a slash at the current position starts a regex.
func (lx *tsLexer) regexAllowed() bool {
	if lx.prevIdent {
		return tsKeywordsBeforeRegex[lx.prevWord]
	}
	return lx.prev == 0 || strings.IndexByte(tsOperatorsBeforeRegex, lx.prev) >= 0
}

// lexCode scans code until the end of input. With stopAtBrace set, it stops
// after the "}" that closes a template expression at depth zero.
func (lx *tsLexer) lexCode(stopAtBrace bool) {
	n := len(lx.src)
	depth := 0
	for lx.i < n {
		c := lx.src[lx.i]
		switch {
		case c == '\n':
			lx.line++
			lx.i++
		case c == ' ' || c == '\t' || c == '\r':
			lx.i++
		case c == '/' && lx.peekNext() == '/':
			lx.lineComment()
		case c == '/' && lx.peekNext() == '*':
			lx.blockComment()
		case c == '"' || c == '\'':
			lx.str(c)
		case c == '`':
			lx.template()
		case c == '/' && lx.regexAllowed():
			lx.regex()
		case c == '{':
			depth++
			lx.setPrev(c)
			lx.i++
		case c == '}':
			lx.i++
			if stopAtBrace && depth == 0 {
				return
			}
			depth--
			lx.setPrev(c)
		case isTSIdentByte(c):
			start := lx.i
			for lx.i < n && isTSIdentByte(lx.src[lx.i]) {
				lx.i++
			}
			lx.prevWord = string(lx.src[start:lx.i])
			lx.prevIdent = true
		default:
			lx.setPrev(c)
			lx.i++
		}
	}
}

// lineComment skips a "//" comment. The exception is a URL scheme in JSX text,
// such as "Visit https://host.io", where the "//" belongs to the URL.
func (lx *tsLexer) lineComment() {
	end := bytes.IndexByte(lx.src[lx.i:], '\n')
	if end < 0 {
		end = len(lx.src)
	} else {
		end += lx.i
	}
	if k, ok := tsSchemeBefore(lx.src, lx.i); ok {
		if m := urlPattern.FindSubmatchIndex(lx.src[k:end]); m != nil && m[0] == 0 {
			lx.out = append(lx.out, hit{Host: hostOf(string(lx.src[k+m[2] : k+m[3]])), File: lx.rel, Line: lx.line})
			lx.i = k + m[1]
			lx.setPrev('a')
			return
		}
	}
	lx.i = end
}

// tsSchemeBefore reports whether the bytes before index i spell a URL scheme
// followed by a colon, and returns the index where the scheme starts.
func tsSchemeBefore(src []byte, i int) (int, bool) {
	j := i - 1
	if j < 0 || src[j] != ':' {
		return 0, false
	}
	k := j
	for k > 0 && (isTSIdentByte(src[k-1]) || src[k-1] == '+' || src[k-1] == '.' || src[k-1] == '-') {
		k--
	}
	if k == j || !isASCIILetter(src[k]) {
		return 0, false
	}
	return k, true
}

// textURLHits returns one hit per URL in val. A hit carries the line of its
// own match, so a URL inside a multi-line template is reported where it sits.
func textURLHits(file string, line int, val string) []hit {
	matches := urlPattern.FindAllStringSubmatchIndex(val, -1)
	out := make([]hit, 0, len(matches))
	for _, m := range matches {
		out = append(out, hit{Host: hostOf(val[m[2]:m[3]]), File: file, Line: line + strings.Count(val[:m[0]], "\n")})
	}
	return out
}

// templateHits returns the URL hits of a template's text. A text that starts
// with "//" is also a protocol-relative URL.
func templateHits(file string, line int, val string) []hit {
	out := textURLHits(file, line, val)
	if m := schemeRelativeLiteral.FindStringSubmatch(val); m != nil {
		out = append(out, hit{Host: hostOf(m[1]), File: file, Line: line})
	}
	return out
}

// blockComment skips a "/* */" comment and counts its newlines.
func (lx *tsLexer) blockComment() {
	n := len(lx.src)
	lx.i += 2
	for lx.i < n && (lx.src[lx.i] != '*' || lx.peekNext() != '/') {
		if lx.src[lx.i] == '\n' {
			lx.line++
		}
		lx.i++
	}
	lx.i = min(lx.i+2, n)
}

// str reads a quoted string. It stops at the matching quote or at a newline,
// so a broken literal cannot hide the rest of the file.
func (lx *tsLexer) str(quote byte) {
	n := len(lx.src)
	start := lx.line
	lx.i++
	j := lx.i
	for lx.i < n && lx.src[lx.i] != quote && lx.src[lx.i] != '\n' {
		if lx.src[lx.i] == '\\' {
			lx.i++
			if lx.i < n && lx.src[lx.i] == '\n' {
				lx.line++
			}
		}
		lx.i++
	}
	end := min(lx.i, n)
	body := string(lx.src[j:end])
	lx.out = append(lx.out, literalURLHits(lx.rel, start, body)...)
	if m := schemeRelativeLiteral.FindStringSubmatch(body); m != nil {
		lx.out = append(lx.out, hit{Host: hostOf(m[1]), File: lx.rel, Line: start})
	}
	if lx.i < n && lx.src[lx.i] == quote {
		lx.i++
	}
	lx.setPrev(quote)
}

// template reads a template literal. The static text is collected, and each
// "${" expression is scanned as code. A placeholder stays in the text as "${}",
// so "https://${host}/x" yields the host "${}", which isExcludedHost drops,
// while "https://github.com/${o}" still yields "github.com".
func (lx *tsLexer) template() {
	n := len(lx.src)
	start := lx.line
	lx.i++
	var text strings.Builder
	for lx.i < n {
		c := lx.src[lx.i]
		switch {
		case c == '`':
			lx.i++
			lx.out = append(lx.out, templateHits(lx.rel, start, text.String())...)
			lx.setPrev('`')
			return
		case c == '\\' && lx.i+1 < n:
			if lx.src[lx.i+1] == '\n' {
				lx.line++
			}
			text.WriteByte(c)
			text.WriteByte(lx.src[lx.i+1])
			lx.i += 2
		case c == '$' && lx.peekNext() == '{':
			text.WriteString("${}")
			lx.i += 2
			lx.setPrev('{')
			before := lx.line
			lx.lexCode(true)
			text.WriteString(strings.Repeat("\n", lx.line-before))
		default:
			if c == '\n' {
				lx.line++
			}
			text.WriteByte(c)
			lx.i++
		}
	}
	lx.out = append(lx.out, templateHits(lx.rel, start, text.String())...)
}

// regex skips a regular expression literal, including its flags. A regex body
// never yields a hit. The scan stops at a newline, so a misread slash stays on
// one line.
func (lx *tsLexer) regex() {
	n := len(lx.src)
	lx.i++
	inClass := false
	for lx.i < n && lx.src[lx.i] != '\n' {
		c := lx.src[lx.i]
		if c == '\\' {
			lx.i += 2
			continue
		}
		if c == '[' {
			inClass = true
		} else if c == ']' {
			inClass = false
		} else if c == '/' && !inClass {
			lx.i++
			break
		}
		lx.i++
	}
	for lx.i < n && isTSIdentByte(lx.src[lx.i]) {
		lx.i++
	}
	lx.i = min(lx.i, n)
	lx.setPrev(')')
}

// cssComment and htmlComment match the comments that extractStripped removes.
var (
	cssComment  = regexp.MustCompile(`(?s)/\*.*?\*/`)
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
)

// extractCSS scans a stylesheet. A comment is removed first, so a URL in a
// comment is never a hit. The rule covers url(...) and @import.
func extractCSS(rel string, src []byte) []hit {
	return extractStripped(rel, src, cssComment)
}

// extractHTML scans an HTML page. A comment is removed first.
func extractHTML(rel string, src []byte) []hit {
	return extractStripped(rel, src, htmlComment)
}

// extractStripped replaces each comment with its newlines, so line numbers
// hold, then scans every line for URLs.
func extractStripped(rel string, src []byte, comment *regexp.Regexp) []hit {
	s := comment.ReplaceAllStringFunc(string(src), func(m string) string {
		return strings.Repeat("\n", strings.Count(m, "\n"))
	})
	var out []hit
	for i, line := range strings.Split(s, "\n") {
		for _, host := range urlHosts(line) {
			out = append(out, hit{Host: host, File: rel, Line: i + 1})
		}
		for _, m := range schemeRelativePattern.FindAllStringSubmatch(line, -1) {
			out = append(out, hit{Host: hostOf(m[1]), File: rel, Line: i + 1})
		}
	}
	return out
}

// isFrontendTestFile reports whether a frontend path is test code. Test files
// and the test support tree hold fixture URLs that no browser loads.
func isFrontendTestFile(rel string) bool {
	if strings.HasPrefix(rel, "frontend/src/test/") {
		return true
	}
	for _, suffix := range []string{
		".test.ts", ".test.tsx", ".test.js", ".test.jsx", ".test.mjs",
		".spec.ts", ".spec.tsx", ".spec.js", ".spec.jsx", ".spec.mjs",
	} {
		if strings.HasSuffix(rel, suffix) {
			return true
		}
	}
	return false
}

// walkFiles calls visit for every regular file under dir (relative to root).
// A missing directory is not an error, so an optional tree may be absent.
// Hidden directories and testdata directories are skipped.
func walkFiles(root, dir string, visit func(rel, abs string) error) error {
	base := filepath.Join(root, dir)
	if _, err := os.Stat(base); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != base && (name == "testdata" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		return visit(normalizeSlash(rel), path)
	})
}

// collectHits extracts every host literal from the scanned sources. It applies
// the Go file rules, the excluded paths, and the host exclusions.
func collectHits(root string, inv *Inventory) ([]hit, error) {
	excluded := func(rel string) bool {
		for _, ex := range inv.ExcludePaths {
			if strings.HasPrefix(rel, ex.Path) {
				return true
			}
		}
		return false
	}
	var raw []hit

	// Go: non-test source under the four code trees.
	for _, dir := range []string{"cmd", "internal", "pkg", "api"} {
		err := walkFiles(root, dir, func(rel, abs string) error {
			name := filepath.Base(rel)
			if filepath.Ext(name) != ".go" || excluded(rel) {
				return nil
			}
			switch {
			case strings.HasSuffix(name, "_test.go"), strings.HasSuffix(name, "_gen.go"),
				strings.HasPrefix(name, "zz_generated."),
				strings.HasPrefix(rel, "pkg/client/"), strings.HasPrefix(rel, "internal/mite/proto/"):
				return nil
			}
			src, err := os.ReadFile(abs)
			if err != nil {
				return err
			}
			hits, err := extractGo(rel, src)
			if err != nil {
				return err
			}
			raw = append(raw, hits...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	// Chart values: one file, read in full so a missing file fails the scan.
	valuesRel := "charts/varroa/values.yaml"
	src, err := os.ReadFile(filepath.Join(root, valuesRel))
	if err != nil {
		return nil, err
	}
	hits, err := extractYAML(valuesRel, src)
	if err != nil {
		return nil, err
	}
	raw = append(raw, hits...)

	// Chart templates are Go templates, not YAML.
	err = walkFiles(root, "charts/varroa/templates", func(rel, abs string) error {
		if excluded(rel) {
			return nil
		}
		src, err := os.ReadFile(abs)
		if err != nil {
			return err
		}
		raw = append(raw, extractText(rel, src)...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Bundles and the air-gapped examples: YAML, with plain text for other files.
	for _, dir := range []string{"bundles", "docs/install/examples/air-gapped"} {
		err := walkFiles(root, dir, func(rel, abs string) error {
			if excluded(rel) {
				return nil
			}
			src, err := os.ReadFile(abs)
			if err != nil {
				return err
			}
			switch filepath.Ext(rel) {
			case ".yaml", ".yml":
				hits, err := extractYAML(rel, src)
				if err != nil {
					return err
				}
				raw = append(raw, hits...)
			default:
				raw = append(raw, extractText(rel, src)...)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	// Plugin Java and Groovy sources.
	err = walkFiles(root, "plugin/src/main", func(rel, abs string) error {
		switch filepath.Ext(rel) {
		case ".java", ".groovy":
		default:
			return nil
		}
		if excluded(rel) {
			return nil
		}
		src, err := os.ReadFile(abs)
		if err != nil {
			return err
		}
		raw = append(raw, extractJava(rel, src)...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Frontend: TypeScript, TSX, and CSS under src, CSS under public, and the
	// entry page. Test files are skipped.
	err = walkFiles(root, "frontend/src", func(rel, abs string) error {
		var extract func(string, []byte) []hit
		switch filepath.Ext(rel) {
		case ".ts", ".tsx", ".js", ".jsx", ".mjs":
			extract = extractTS
		case ".css":
			extract = extractCSS
		default:
			return nil
		}
		if excluded(rel) || isFrontendTestFile(rel) {
			return nil
		}
		src, err := os.ReadFile(abs)
		if err != nil {
			return err
		}
		raw = append(raw, extract(rel, src)...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = walkFiles(root, "frontend/public", func(rel, abs string) error {
		if filepath.Ext(rel) != ".css" || excluded(rel) {
			return nil
		}
		src, err := os.ReadFile(abs)
		if err != nil {
			return err
		}
		raw = append(raw, extractCSS(rel, src)...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	indexRel := "frontend/index.html"
	src, err = os.ReadFile(filepath.Join(root, indexRel))
	switch {
	case err == nil:
		if !excluded(indexRel) {
			raw = append(raw, extractHTML(indexRel, src)...)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}

	var out []hit
	for _, h := range raw {
		if !isExcludedHost(h.Host) {
			out = append(out, h)
		}
	}
	return out, nil
}
