package forgeurl

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeRepoURL_Accepts(t *testing.T) {
	cases := map[string]string{
		"https://git.example.org/team/app":     "https://git.example.org/team/app",
		"https://git.example.org/team/app.git": "https://git.example.org/team/app",
		"https://git.example.org/team/app/":    "https://git.example.org/team/app",
		"https://git.example.org:443/team/app": "https://git.example.org/team/app",
		"https://GIT.Example.Org/team/app":     "https://git.example.org/team/app",
		"https://github.com/acme/tool":         "https://github.com/acme/tool",
	}
	for in, want := range cases {
		got, err := NormalizeRepoURL(in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", in, err)
			continue
		}
		if got.String() != want {
			t.Errorf("%q: got %q want %q", in, got.String(), want)
		}
	}
}

func TestRepoURL_StringBracketsIPv6(t *testing.T) {
	got, err := NormalizeRepoURL("https://[fd00::10]/team/app")
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if s := got.String(); s != "https://[fd00::10]/team/app" {
		t.Fatalf("String() = %q", s)
	}
}

func TestNormalizeRepoURL_Refuses(t *testing.T) {
	cases := []string{
		"http://git.example.org/team/app",
		"https://user:pw@git.example.org/team/app",
		"https://git.example.org:8443/team/app",
		"https://git.example.org/team/app?x=1",
		"https://git.example.org/team/app?",
		"https://git.example.org/team/app#frag",
		"https://git.example.org/team//app",
		"https://git.example.org/team/app/extra",
		"https://git.example.org/team",
		"https://127.0.0.1/team/app",
		"https://[::1]/team/app",
		"https://169.254.1.1/team/app",
		"https://0.0.0.0/team/app",
		"https://git.example.org/team%2Fapp/x",
		"https://git.example.org/../app",
		"https://git.example.org/team/..",
		"https://git.example.org/team/.git",
		"",
	}
	for _, in := range cases {
		_, err := NormalizeRepoURL(in)
		if err == nil {
			t.Errorf("%q: accepted, want refused", in)
			continue
		}
		var re *RuleError
		if !errors.As(err, &re) || re.Arg != "repo" {
			t.Errorf("%q: error %v is not a repo RuleError", in, err)
		}
	}
}

func TestValidateBranch(t *testing.T) {
	ok := []string{"main", "feat/x-1", "varroa/propose-pipeline"}
	bad := []string{"", "a..b", "a//b", "-x", "/x", "x/", "x.lock", "a b", "a~b", "a^b", "a:b",
		"a?b", "a*b", "a[b", "a\\b", "a\x01b", "a\x7fb", strings.Repeat("a", 201)}
	for _, b := range ok {
		if err := ValidateBranch(b); err != nil {
			t.Errorf("%q: refused: %v", b, err)
		}
	}
	for _, b := range bad {
		if err := ValidateBranch(b); err == nil {
			t.Errorf("%q: accepted", b)
		}
	}
}

func TestValidateRevision(t *testing.T) {
	sha40 := strings.Repeat("a", 40)
	sha64 := strings.Repeat("0", 64)
	for _, r := range []string{sha40, sha64, "main"} {
		if err := ValidateRevision(r); err != nil {
			t.Errorf("%q: refused: %v", r, err)
		}
	}
	for _, r := range []string{"", "a..b", "-x", "x y"} {
		err := ValidateRevision(r)
		var re *RuleError
		if err == nil || !errors.As(err, &re) || re.Arg != "revision" {
			t.Errorf("%q: got %v, want revision RuleError", r, err)
		}
	}
}

func TestCleanReadPath(t *testing.T) {
	for in, want := range map[string]string{"": "", ".": "", "./": "", "a": "a", "a/": "a", "a/b.txt": "a/b.txt"} {
		got, err := CleanReadPath(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"..", "a/..", "/a", "a//b", "a/./b", "a\x00b", "a\nb"} {
		if _, err := CleanReadPath(in); err == nil {
			t.Errorf("%q: accepted", in)
		}
	}
}

func TestValidateFilePath(t *testing.T) {
	if err := ValidateFilePath("Jenkinsfile"); err != nil {
		t.Errorf("Jenkinsfile refused: %v", err)
	}
	for _, p := range []string{"", ".", "dir/", "../x"} {
		if err := ValidateFilePath(p); err == nil {
			t.Errorf("%q: accepted", p)
		}
	}
}

func TestValidateFiles(t *testing.T) {
	one := []File{{Path: "Jenkinsfile", Content: "x"}}
	if err := ValidateFiles(one); err != nil {
		t.Fatalf("valid set refused: %v", err)
	}
	if err := ValidateFiles(nil); err == nil {
		t.Error("empty set accepted")
	}
	eleven := make([]File, 11)
	for i := range eleven {
		eleven[i] = File{Path: strings.Repeat("p", i+1), Content: "x"}
	}
	if err := ValidateFiles(eleven); err == nil {
		t.Error("11 files accepted")
	}
	dup := []File{{Path: "a", Content: "x"}, {Path: "a", Content: "y"}}
	if err := ValidateFiles(dup); err == nil {
		t.Error("duplicate path accepted")
	}
	if err := ValidateFiles([]File{{Path: "a", Content: "x\x00"}}); err == nil {
		t.Error("NUL accepted")
	}
	if err := ValidateFiles([]File{{Path: "a", Content: "\xff"}}); err == nil {
		t.Error("invalid UTF-8 accepted")
	}
	if err := ValidateFiles([]File{{Path: "a", Content: strings.Repeat("x", 64<<10+1)}}); err == nil {
		t.Error("64 KiB+1 accepted")
	}
	if err := ValidateFiles([]File{{Path: "a", Content: strings.Repeat("x", 64<<10)}}); err != nil {
		t.Errorf("exact 64 KiB refused: %v", err)
	}
	var total []File
	for i := 0; i < 5; i++ {
		total = append(total, File{Path: "f" + string(rune('a'+i)), Content: strings.Repeat("x", 60<<10)})
	}
	if err := ValidateFiles(total); err == nil {
		t.Error("300 KiB total accepted")
	}
}

func TestValidateTitleBody(t *testing.T) {
	if err := ValidateTitle(""); err == nil {
		t.Error("empty title accepted")
	}
	if err := ValidateTitle(strings.Repeat("é", 200)); err != nil {
		t.Errorf("200 runes refused: %v", err)
	}
	if err := ValidateTitle(strings.Repeat("é", 201)); err == nil {
		t.Error("201 runes accepted")
	}
	if err := ValidateTitle("a\nb"); err == nil {
		t.Error("line break accepted")
	}
	if err := ValidateBody(strings.Repeat("x", 8<<10)); err != nil {
		t.Errorf("8 KiB body refused: %v", err)
	}
	if err := ValidateBody(strings.Repeat("x", 8<<10+1)); err == nil {
		t.Error("8 KiB+1 body accepted")
	}
}
