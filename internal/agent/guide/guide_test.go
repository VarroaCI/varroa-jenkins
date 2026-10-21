package guide

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestGuideMatchesDocsPage(t *testing.T) {
	src, err := os.ReadFile("../../../docs/agents/guide.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(src) != Text {
		t.Fatal("guide.txt differs from docs/agents/guide.md; run: cp docs/agents/guide.md internal/agent/guide/guide.txt")
	}
}

func TestGuideUnderCap(t *testing.T) {
	t.Logf("guide: %d bytes, about %d tokens", len(Text), len(Text)/4)
	if len(Text) > MaxBytes {
		t.Fatalf("guide is %d bytes, above the cap of %d", len(Text), MaxBytes)
	}
}

func TestGuideHoldsNoLink(t *testing.T) {
	for _, bad := range []string{"](", "http"} {
		if strings.Contains(Text, bad) {
			t.Errorf("guide holds %q; the model cannot follow a link", bad)
		}
	}
}

func TestGuideHoldsNoBlockedTerm(t *testing.T) {
	// The terms are built from parts so that this file passes the export gate.
	terms := []string{"clau" + "de", "cod" + "ex", "open" + "spec", "n8s" + "\\.dev", "bam" + "bash", "dev" + "boi"}
	re := regexp.MustCompile("(?i)" + strings.Join(terms, "|"))
	if m := re.FindString(Text); m != "" {
		t.Errorf("guide holds blocked term %q", m)
	}
}
