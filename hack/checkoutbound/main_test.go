package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// hostsOf returns the sorted, distinct hosts of a hit list.
func hostsOf(hits []hit) []string {
	set := map[string]bool{}
	for _, h := range hits {
		set[h.Host] = true
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	slices.Sort(out)
	return out
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func TestExtractGo_SkipsImportsAndComments(t *testing.T) {
	hits, err := extractGo("sample.go", readFixture(t, "sample.go"))
	if err != nil {
		t.Fatal(err)
	}
	// The import path carries an image shape, but an import is never a destination.
	// The comment carries a URL, but comments are not in the AST walk.
	want := []string{"api.real-host.io", "docker.io"}
	if got := hostsOf(hits); !reflect.DeepEqual(got, want) {
		t.Fatalf("hosts = %v, want %v", got, want)
	}
}

func TestExtractGo_FindsURLAndImageLiterals(t *testing.T) {
	src := []byte(`package p

var a = "https://user@dial.example-host.io:8443/x"
var b = "ghcr.io/acme/tool:v1"
var c = "%s"
var d = "https://%s/format"
`)
	hits, err := extractGo("p.go", src)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"%s", "dial.example-host.io", "ghcr.io"}
	if got := hostsOf(hits); !reflect.DeepEqual(got, want) {
		// %s and the format string remain raw hits. isExcludedHost removes them later.
		t.Fatalf("hosts = %v, want %v", got, want)
	}
}

func TestExtractYAML_IgnoresComments(t *testing.T) {
	hits, err := extractYAML("sample.yaml", readFixture(t, "sample.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"docker.io", "real-host.io"}
	if got := hostsOf(hits); !reflect.DeepEqual(got, want) {
		t.Fatalf("hosts = %v, want %v", got, want)
	}
}

func TestExtractTemplate_StripsComments(t *testing.T) {
	hits := extractText("sample.tmpl", readFixture(t, "sample.tmpl"))
	want := []string{"ghcr.io", "real-host.io"}
	if got := hostsOf(hits); !reflect.DeepEqual(got, want) {
		t.Fatalf("hosts = %v, want %v", got, want)
	}
	for _, h := range hits {
		if h.Line < 1 || h.Line > 4 {
			t.Fatalf("hit %+v has a line outside the fixture", h)
		}
	}
}

func TestExtractJava_StringLiteralsOnly(t *testing.T) {
	hits := extractJava("Sample.java", readFixture(t, "Sample.java"))
	want := []string{"real-host.io", "second-host.io"}
	if got := hostsOf(hits); !reflect.DeepEqual(got, want) {
		t.Fatalf("hosts = %v, want %v", got, want)
	}
}

func TestIsExcludedHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"updates.jenkins.io", false},
		{"docker.io", false},
		{"localhost", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"2001:db8::1", false},
		{"0.0.0.0", true},
		{"10.1.2.3", false},
		{"varroa-bff.ns.svc", true},
		{"varroa-bff.ns.svc.cluster.local", true},
		{"nats.varroa-system.cluster.local", true},
		{"example.com", true},
		{"api.example.org", true},
		{"bad.test", true},
		{"thing.invalid", true},
		{"host.local", true},
		{"%s.host.io", true},
		{"{{ .Values.host }}", true},
		{"$HOST.io", true},
		{"nodot", true},
		{"host.io.", true},
		{"", true},
	}
	for _, c := range cases {
		if got := isExcludedHost(c.host); got != c.want {
			t.Errorf("isExcludedHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// writeTree builds a minimal repository under a temporary directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const minimalValues = "upstream: https://real-host.io/x\n"

func TestCheck_UnknownHostFails(t *testing.T) {
	root := writeTree(t, map[string]string{
		"charts/varroa/values.yaml": "upstream: https://unlisted-host.io/x\n",
		inventoryRel: `connections:
  - component: operator
    destination: real-host.io
    hosts: [real-host.io]
    purpose: p
    protocol: HTTPS 443
    control: None
    disable: "No."
`,
	})
	res, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	problems := res.problems()
	if !slices.ContainsFunc(problems, func(p string) bool {
		return strings.Contains(p, `"unlisted-host.io"`)
	}) {
		t.Fatalf("problems = %v, want an unknown-host finding", problems)
	}
}

func TestCheck_StaleRowFails(t *testing.T) {
	root := writeTree(t, map[string]string{
		"charts/varroa/values.yaml": minimalValues,
		inventoryRel: `connections:
  - component: operator
    destination: real-host.io
    hosts: [real-host.io]
    purpose: p
    protocol: HTTPS 443
    control: None
    disable: "No."
  - component: operator
    destination: gone-host.io
    hosts: [gone-host.io]
    purpose: p
    protocol: HTTPS 443
    control: None
    disable: "No."
`,
	})
	res, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	problems := res.problems()
	if len(problems) != 1 || !strings.Contains(problems[0], `"gone-host.io"`) {
		t.Fatalf("problems = %v, want one stale-row finding for gone-host.io", problems)
	}
}

func TestCheck_ValidInventoryHasNoProblems(t *testing.T) {
	root := writeTree(t, map[string]string{
		"charts/varroa/values.yaml": minimalValues,
		inventoryRel: `connections:
  - component: operator
    destination: real-host.io
    hosts: [real-host.io]
    purpose: p
    protocol: HTTPS 443
    control: None
    disable: "No."
`,
	})
	res, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if problems := res.problems(); len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
}

func TestRenderDoc_DeterministicAndMatchesCommitted(t *testing.T) {
	// The page depends only on the inventory. The source scan runs in
	// make check-outbound, so this test reads no source file.
	const repoRoot = "../.."
	inv, err := loadInventory(filepath.Join(repoRoot, inventoryRel))
	if err != nil {
		t.Fatal(err)
	}
	first, err := renderDoc(inv)
	if err != nil {
		t.Fatal(err)
	}
	second, err := renderDoc(inv)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("two renders of the same inventory differ")
	}
	committed, err := os.ReadFile(filepath.Join(repoRoot, pageRel))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, first) {
		t.Fatalf("%s differs from the inventory. Run make outbound-doc.", pageRel)
	}
}

func TestHostOf_IPv6Literal(t *testing.T) {
	cases := map[string]string{
		"[2001:db8::1]:8443": "2001:db8::1",
		"user@[2001:db8::1]": "2001:db8::1",
		"dial.host.io:8443":  "dial.host.io",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
	if isExcludedHost(hostOf("[2001:db8::1]:8443")) {
		t.Error("an external IPv6 literal must not be excluded")
	}
}

func TestInventory_AcceptsIPv6Host(t *testing.T) {
	inv := &Inventory{Connections: []Connection{{
		Component: "operator", Destination: "v6", Hosts: []string{"2001:db8::1"},
		Purpose: "p", Protocol: "HTTPS 443", Control: "None", Disable: "No.",
	}}}
	if err := inv.validate(); err != nil {
		t.Fatalf("validate rejected a bare IPv6 host: %v", err)
	}
	inv.Connections[0].Hosts = []string{"bad:port"}
	if err := inv.validate(); err == nil {
		t.Fatal("validate accepted a host with a port")
	}
}

func TestExtractTS_LiteralsOnly(t *testing.T) {
	hits := extractTS("sample.tsx", readFixture(t, "sample.tsx"))
	var kept []hit
	for _, h := range hits {
		if !isExcludedHost(h.Host) {
			kept = append(kept, h)
		}
	}
	want := []string{
		"after-div-host.io", "after-jsx-host.io", "after-regex-host.io",
		"deep-host.io", "double-host.io", "jsx-attr-host.io", "jsx-text-host.io",
		"nested-host.io", "single-host.io", "static-host.io",
	}
	if got := hostsOf(kept); !reflect.DeepEqual(got, want) {
		t.Fatalf("hosts = %v, want %v", got, want)
	}
}

func TestExtractTS_LineNumbers(t *testing.T) {
	src := []byte("const t = `line one\nline two\nline three`;\nconst s = \"https://line-host.io\";\n")
	hits := extractTS("line.ts", src)
	if len(hits) != 1 || hits[0].Host != "line-host.io" || hits[0].Line != 4 {
		t.Fatalf("hits = %+v, want one hit for line-host.io on line 4", hits)
	}
}

func TestExtractCSS_StripsComments(t *testing.T) {
	src := []byte("/* https://c.io */\n@import url(\"https://fonts-host.io/a\");\n.x { background: url(https://cdn-host.io/i.png); }\n")
	want := []string{"cdn-host.io", "fonts-host.io"}
	if got := hostsOf(extractCSS("s.css", src)); !reflect.DeepEqual(got, want) {
		t.Fatalf("hosts = %v, want %v", got, want)
	}
}

func TestExtractHTML_StripsComments(t *testing.T) {
	src := []byte("<!-- https://c.io -->\n<link href=\"https://html-host.io/x\">\n")
	want := []string{"html-host.io"}
	if got := hostsOf(extractHTML("index.html", src)); !reflect.DeepEqual(got, want) {
		t.Fatalf("hosts = %v, want %v", got, want)
	}
}

func TestExtractFrontend_ProtocolRelativeAndTemplateLines(t *testing.T) {
	css := hostsOf(extractCSS("s.css", []byte("@import url(//proto-css.io/a.css);\n")))
	if want := []string{"proto-css.io"}; !reflect.DeepEqual(css, want) {
		t.Fatalf("css hosts = %v, want %v", css, want)
	}
	html := hostsOf(extractHTML("index.html", []byte("<script src=\"//proto-html.io/x.js\"></script>\n")))
	if want := []string{"proto-html.io"}; !reflect.DeepEqual(html, want) {
		t.Fatalf("html hosts = %v, want %v", html, want)
	}
	ts := extractTS("p.ts", []byte("const s = \"//proto-ts.io/x\";\n"))
	if got := hostsOf(ts); !reflect.DeepEqual(got, []string{"proto-ts.io"}) {
		t.Fatalf("ts hosts = %v, want proto-ts.io", got)
	}
	if got := hostsOf(extractTS("u.ts", []byte("const t = `//proto-tpl.io/x`;\n"))); !reflect.DeepEqual(got, []string{"proto-tpl.io"}) {
		t.Fatalf("template protocol-relative hosts = %v, want proto-tpl.io", got)
	}
	if got := hostsOf(extractCSS("u.css", []byte("a { background: url(//proto-bare.io); }\n"))); !reflect.DeepEqual(got, []string{"proto-bare.io"}) {
		t.Fatalf("bare css hosts = %v, want proto-bare.io", got)
	}
	if got := hostsOf(extractTS("c.ts", []byte("const s = \"//PROTO-UP.io/x\";\n"))); !reflect.DeepEqual(got, []string{"proto-up.io"}) {
		t.Fatalf("uppercase hosts = %v, want proto-up.io", got)
	}
	expr := extractTS("e.ts", []byte("const t = `${\na\n}\nhttps://expr-host.io/x`;\n"))
	if len(expr) != 1 || expr[0].Line != 4 {
		t.Fatalf("expression template hit = %+v, want one hit on line 4", expr)
	}
	if got := hostsOf(extractCSS("u2.css", []byte("a { background: url(//first.last@userinfo-host.io:8443/x); }\n"))); !reflect.DeepEqual(got, []string{"userinfo-host.io"}) {
		t.Fatalf("userinfo hosts = %v, want userinfo-host.io", got)
	}
	if got := hostsOf(extractCSS("u3.css", []byte("a { background: url(//build_bot@underscore-host.io/x); }\n"))); !reflect.DeepEqual(got, []string{"underscore-host.io"}) {
		t.Fatalf("underscore userinfo hosts = %v, want underscore-host.io", got)
	}
	multi := extractTS("m.ts", []byte("const t = `a\nb\nhttps://tpl-host.io/x`;\n"))
	if len(multi) != 1 || multi[0].Line != 3 {
		t.Fatalf("template hit = %+v, want one hit on line 3", multi)
	}
}

func TestCollectHits_FrontendScope(t *testing.T) {
	root := writeTree(t, map[string]string{
		"charts/varroa/values.yaml":     minimalValues,
		"frontend/src/a.ts":             "export const u = \"https://fe-host.io/x\";\n",
		"frontend/src/a.test.ts":        "export const u = \"https://test-host.io/x\";\n",
		"frontend/src/test/handlers.ts": "export const u = \"https://mock-host.io/x\";\n",
		"frontend/src/s.css":            "a { background: url(https://css-host.io/f); }\n",
		"frontend/src/j.js":             "export const u = \"https://js-host.io/x\";\n",
		"frontend/src/j.test.js":        "export const u = \"https://jstest-host.io/x\";\n",
		"frontend/public/t.css":         "a { background: url(https://pub-host.io/f); }\n",
		"frontend/index.html":           "<script src=\"https://html-host.io/x.js\"></script>\n",
	})
	hits, err := collectHits(root, &Inventory{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"css-host.io", "fe-host.io", "html-host.io", "js-host.io", "pub-host.io", "real-host.io"}
	if got := hostsOf(hits); !reflect.DeepEqual(got, want) {
		t.Fatalf("hosts = %v, want %v", got, want)
	}
}
