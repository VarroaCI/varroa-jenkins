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
	const repoRoot = "../.."
	res, err := scan(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if problems := res.problems(); len(problems) != 0 {
		t.Fatalf("inventory has problems: %v", problems)
	}
	first, err := renderDoc(res.inv)
	if err != nil {
		t.Fatal(err)
	}
	second, err := renderDoc(res.inv)
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
