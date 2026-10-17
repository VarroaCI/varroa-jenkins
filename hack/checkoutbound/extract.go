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

	var out []hit
	for _, h := range raw {
		if !isExcludedHost(h.Host) {
			out = append(out, h)
		}
	}
	return out, nil
}
