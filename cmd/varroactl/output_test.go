package main

import (
	"bytes"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// printTable
// ---------------------------------------------------------------------------

func TestPrintTable_WithHeaders(t *testing.T) {
	var buf bytes.Buffer
	headers := []string{"NAME", "PHASE"}
	rows := [][]string{
		{"ctrl-1", "Running"},
		{"ctrl-2", "Stopped"},
	}
	printTable(&buf, headers, rows, false)
	out := buf.String()

	if !strings.Contains(out, "NAME") {
		t.Errorf("expected header NAME in output:\n%s", out)
	}
	if !strings.Contains(out, "PHASE") {
		t.Errorf("expected header PHASE in output:\n%s", out)
	}
	if !strings.Contains(out, "ctrl-1") {
		t.Errorf("expected ctrl-1 in output:\n%s", out)
	}
	if !strings.Contains(out, "Running") {
		t.Errorf("expected Running in output:\n%s", out)
	}
}

func TestPrintTable_NoHeaders(t *testing.T) {
	var buf bytes.Buffer
	headers := []string{"NAME", "PHASE"}
	rows := [][]string{
		{"ctrl-1", "Running"},
	}
	printTable(&buf, headers, rows, true)
	out := buf.String()

	if strings.Contains(out, "NAME") {
		t.Errorf("expected no header NAME when noHeaders=true:\n%s", out)
	}
	if !strings.Contains(out, "ctrl-1") {
		t.Errorf("expected ctrl-1 in output:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// printJSON
// ---------------------------------------------------------------------------

func TestPrintJSON(t *testing.T) {
	var buf bytes.Buffer
	v := map[string]string{"name": "test", "phase": "Running"}
	if err := printJSON(&buf, v); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"name"`) {
		t.Errorf("expected name in JSON output:\n%s", out)
	}
	if !strings.Contains(out, `"test"`) {
		t.Errorf("expected test in JSON output:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// printYAML
// ---------------------------------------------------------------------------

func TestPrintYAML(t *testing.T) {
	var buf bytes.Buffer
	v := map[string]string{"name": "test", "phase": "Running"}
	if err := printYAML(&buf, v); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "name: test") {
		t.Errorf("expected name: test in YAML output:\n%s", out)
	}
	if !strings.Contains(out, "phase: Running") {
		t.Errorf("expected phase: Running in YAML output:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// printName
// ---------------------------------------------------------------------------

type namedItem struct {
	ns   string
	name string
}

func (n namedItem) GetNamespace() string { return n.ns }
func (n namedItem) GetName() string      { return n.name }

func TestPrintName(t *testing.T) {
	var buf bytes.Buffer
	item := namedItem{ns: "team-a", name: "ctrl-1"}
	printName(&buf, item)
	out := buf.String()
	expected := "team-a/ctrl-1\n"
	if out != expected {
		t.Errorf("expected %q, got %q", expected, out)
	}
}

func TestPrintNameMulti(t *testing.T) {
	var buf bytes.Buffer
	items := []namedItem{
		{ns: "team-a", name: "ctrl-1"},
		{ns: "team-b", name: "ctrl-2"},
	}
	if err := printNameMulti(&buf, items); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "team-a/ctrl-1") {
		t.Errorf("expected team-a/ctrl-1:\n%s", out)
	}
	if !strings.Contains(out, "team-b/ctrl-2") {
		t.Errorf("expected team-b/ctrl-2:\n%s", out)
	}
}
