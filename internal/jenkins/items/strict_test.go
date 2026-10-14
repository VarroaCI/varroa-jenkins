package items

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const unknownBuilderKeyYAML = `items:
  - kind: freeStyle
    name: build
    builders:
      - jobDsl:
          targets: x
`

func unknownKeysOf(t *testing.T, err error) []UnknownKey {
	t.Helper()
	var uk *UnknownKeysError
	if !errors.As(err, &uk) {
		t.Fatalf("error = %v (%T), want *UnknownKeysError", err, err)
	}
	return uk.Keys
}

func TestParseStrict_UnknownBuilderKeyNamesKeyAndPath(t *testing.T) {
	_, err := ParseStrict(unknownBuilderKeyYAML)
	keys := unknownKeysOf(t, err)
	want := UnknownKey{ItemPath: "build", Field: "items[0].builders[0].jobDsl", Key: "jobDsl"}
	if len(keys) != 1 || keys[0] != want {
		t.Fatalf("keys = %+v, want [%+v]", keys, want)
	}
	msg := err.Error()
	if !strings.Contains(msg, "jobDsl") || !strings.Contains(msg, `"build"`) {
		t.Fatalf("Error() = %q, want key and item name", msg)
	}
}

func TestParseStrict_NestedItemPath(t *testing.T) {
	input := `items:
  - kind: folder
    name: a
    items:
      - kind: pipeline
        name: b
        notAField:
          script: echo hi
`
	_, err := ParseStrict(input)
	keys := unknownKeysOf(t, err)
	if len(keys) != 1 || keys[0].ItemPath != "a/b" || keys[0].Field != "items[0].items[0].notAField" {
		t.Fatalf("keys = %+v, want ItemPath a/b and Field items[0].items[0].notAField", keys)
	}
}

func TestParseStrict_TopLevelUnknownKey(t *testing.T) {
	input := "item: []\n"
	_, err := ParseStrict(input)
	keys := unknownKeysOf(t, err)
	if len(keys) != 1 || keys[0].ItemPath != "" || keys[0].Field != "item" {
		t.Fatalf("keys = %+v, want top-level item key", keys)
	}
	if !strings.Contains(err.Error(), "manifest top level") {
		t.Fatalf("Error() = %q, want top-level wording", err.Error())
	}
}

func TestParseStrict_ReportsEveryUnknownKey(t *testing.T) {
	input := `items:
  - kind: freeStyle
    name: first
    builders:
      - jobDsl:
          targets: x
  - kind: freeStyle
    name: second
    bogus: true
`
	_, err := ParseStrict(input)
	keys := unknownKeysOf(t, err)
	if len(keys) != 2 {
		t.Fatalf("keys = %+v, want 2", keys)
	}
	if keys[0].Key != "jobDsl" || keys[0].ItemPath != "first" {
		t.Errorf("keys[0] = %+v, want jobDsl at first", keys[0])
	}
	if keys[1].Key != "bogus" || keys[1].ItemPath != "second" {
		t.Errorf("keys[1] = %+v, want bogus at second", keys[1])
	}
}

func TestParseStrict_FreeFormTemplateValuesAccepted(t *testing.T) {
	input := `items:
  - kind: pipeline
    name: templated
    definition:
      varroaTemplate:
        template: build
        values:
          anything:
            goes:
              here: 1
`
	if _, err := ParseStrict(input); err != nil {
		t.Fatalf("ParseStrict() error = %v, want nil", err)
	}
}

func TestParseStrict_DuplicateKeyRejected(t *testing.T) {
	input := `items:
  - kind: freeStyle
    name: one
    name: two
`
	_, err := ParseStrict(input)
	if err == nil || !strings.Contains(err.Error(), "already defined") {
		t.Fatalf("error = %v, want duplicate key error", err)
	}
}

func TestParseStrict_EmptyAndCommentOnly(t *testing.T) {
	for _, input := range []string{"", "# c\n"} {
		m, err := ParseStrict(input)
		if err != nil {
			t.Errorf("ParseStrict(%q) error = %v, want nil", input, err)
		}
		if m == nil || len(m.Items) != 0 {
			t.Errorf("ParseStrict(%q) = %+v, want empty manifest", input, m)
		}
	}
}

func TestParseStrict_StillValidates(t *testing.T) {
	input := `items:
  - kind: matrix
    name: m
`
	_, err := ParseStrict(input)
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("error = %v, want unknown kind", err)
	}
}

func TestParse_IgnoresUnknownKey(t *testing.T) {
	if _, err := Parse(unknownBuilderKeyYAML); err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
}

func TestParseStrict_AcceptsCorpus(t *testing.T) {
	patterns := []string{
		"testdata/challenge/cases/breadth/*/items.yaml",
		"testdata/challenge/cases/depth/*/items.yaml",
		"testdata/challenge/bundle-challenge/items.yaml",
		"../../../bundles/starter/items.yaml",
	}
	checked := 0
	for _, pattern := range patterns {
		files, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			if _, err := Parse(string(data)); err != nil {
				continue
			}
			checked++
			if _, err := ParseStrict(string(data)); err != nil {
				t.Errorf("%s: ParseStrict() error = %v, want nil", f, err)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no corpus file parsed; the glob paths are wrong")
	}
}

func TestParseStrict_NegativeCorpusUnknownBuilder(t *testing.T) {
	data, err := os.ReadFile("testdata/challenge/cases/negative/freestyle-unknown-builder/items.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, err = ParseStrict(string(data))
	keys := unknownKeysOf(t, err)
	if len(keys) != 1 || keys[0].Key != "unknownTool" {
		t.Fatalf("keys = %+v, want unknownTool", keys)
	}
}

func TestParseStrict_UnknownKeyInMergedAliasReportedPerItem(t *testing.T) {
	input := `items:
  - kind: freeStyle
    name: first
    <<: &common
      bogusMerged: 1
  - kind: freeStyle
    name: second
    <<: *common
`
	_, err := ParseStrict(input)
	keys := unknownKeysOf(t, err)
	if len(keys) != 2 {
		t.Fatalf("keys = %+v, want one per item", keys)
	}
	if keys[0].ItemPath != "first" || keys[0].Key != "bogusMerged" {
		t.Errorf("keys[0] = %+v, want bogusMerged at first", keys[0])
	}
	if keys[1].ItemPath != "second" || keys[1].Field != "items[1].bogusMerged" {
		t.Errorf("keys[1] = %+v, want bogusMerged at second", keys[1])
	}
}

func TestParseStrict_NonSpecificTagMergeKeyExpands(t *testing.T) {
	input := `items:
  - kind: freeStyle
    name: tagged
    ! <<: {bogusTagged: 1}
`
	_, err := ParseStrict(input)
	keys := unknownKeysOf(t, err)
	if len(keys) != 1 || keys[0].Key != "bogusTagged" || keys[0].ItemPath != "tagged" {
		t.Fatalf("keys = %+v, want bogusTagged at item tagged", keys)
	}
}

func TestParseStrict_QuotedMergeKeyIsAnOrdinaryKey(t *testing.T) {
	input := `items:
  - kind: freeStyle
    name: quoted
    "<<": 1
`
	_, err := ParseStrict(input)
	keys := unknownKeysOf(t, err)
	if len(keys) != 1 || keys[0].Key != "<<" || keys[0].ItemPath != "quoted" {
		t.Fatalf("keys = %+v, want quoted << key at item quoted", keys)
	}
}

func TestIsManifestKey(t *testing.T) {
	for _, key := range []string{"removeStrategy", "root", "retain", "items"} {
		if !IsManifestKey(key) {
			t.Errorf("IsManifestKey(%q) = false, want true", key)
		}
	}
	for _, key := range []string{"item", "Items", "", "rbac"} {
		if IsManifestKey(key) {
			t.Errorf("IsManifestKey(%q) = true, want false", key)
		}
	}
}
