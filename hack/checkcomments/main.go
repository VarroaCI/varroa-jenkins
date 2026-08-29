// Command checkcomments fails when a Go comment cites a GitHub issue or PR
// number. Such a citation records why a change was made rather than a
// constraint the code cannot show, and git history already holds that.
//
// Comments are read from the parsed AST, so a hash-number inside a string
// literal or test fixture is never mistaken for a citation.
//
// Usage: go run ./hack/checkcomments [dir...]
package main

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// citation matches a hash followed by a number, including the `PR#1234`
// spelling where the hash abuts a word.
// Two or more digits are required so ordinals like `Checkout #1` keep working,
// with no upper bound so a long number cannot slip past. A genuine exception
// can be marked with ignoreDirective.
var citation = regexp.MustCompile(`[^#]#\d{2,}\b|^#\d{2,}\b`)

// quoted spans are prose about a citation rather than a citation, so a comment
// may name one inside backticks or double quotes.
var quoted = regexp.MustCompile("`[^`]*`|\"[^\"]*\"")

// ignoreDirective exempts the one comment line it appears on. Use it only where
// a hash-number reads as an ordinal or as sample data, never to keep a real
// citation.
const ignoreDirective = "checkcomments:ignore"

// skipDirs holds trees that hold no Go source of ours. Hidden directories are
// skipped by name prefix, which covers caches, tool state, and version control
// without naming any of them.
var skipDirs = map[string]bool{
	"bin":          true,
	"frontend":     true,
	"node_modules": true,
	"testdata":     true,
}

// skipDir reports whether a directory should not be walked.
func skipDir(name string) bool {
	return skipDirs[name] || (len(name) > 1 && strings.HasPrefix(name, "."))
}

// skipFile reports whether a path holds generated code.
func skipFile(path string) bool {
	base := filepath.Base(path)
	switch {
	case strings.HasSuffix(base, "_gen.go"):
		return true
	case strings.HasPrefix(base, "zz_generated."):
		return true
	case strings.Contains(filepath.ToSlash(path), "/internal/mite/proto/"):
		return true
	case strings.Contains(filepath.ToSlash(path), "/pkg/client/"):
		return true
	}
	return false
}

type finding struct {
	pos  token.Position
	text string
}

func checkFile(fset *token.FileSet, path string) ([]finding, error) {
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var out []finding
	for _, group := range f.Comments {
		for _, c := range group.List {
			for offset, line := range strings.Split(c.Text, "\n") {
				if strings.Contains(line, ignoreDirective) {
					continue
				}
				if citation.MatchString(quoted.ReplaceAllString(line, "")) {
					pos := fset.Position(c.Slash)
					pos.Line += offset
					out = append(out, finding{
						pos:  pos,
						text: strings.TrimSpace(line),
					})
				}
			}
		}
	}
	return out, nil
}

func main() {
	roots := os.Args[1:]
	if len(roots) == 0 {
		roots = []string{"."}
	}

	fset := token.NewFileSet()
	var findings []finding

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if skipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".go" || skipFile(path) {
				return nil
			}
			got, err := checkFile(fset, path)
			if err != nil {
				return err
			}
			findings = append(findings, got...)
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "checkcomments: %v\n", err)
			os.Exit(2)
		}
	}

	if len(findings) > 0 {
		fmt.Fprintf(os.Stderr, "checkcomments: %d comment(s) cite an issue or PR number:\n\n", len(findings))
		for _, f := range findings {
			fmt.Fprintf(os.Stderr, "  %s:%d: %s\n", f.pos.Filename, f.pos.Line, f.text)
		}
		fmt.Fprint(os.Stderr, "\nA comment states a constraint the code cannot show. "+
			"Why a change was made belongs in git history, not beside the code.\n"+
			"Restate each as a present-tense invariant and drop the citation.\n")
		os.Exit(1)
	}

	fmt.Println("checkcomments: no issue or PR citations in Go comments")
}
