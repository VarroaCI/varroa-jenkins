package api

import (
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

func badgeStrPtr(s string) *string { return &s }

func TestRenderBadgeSVG_EscapesLabelAndMessage(t *testing.T) {
	svg := string(renderBadgeSVG(`a<b>&"`, `x"<y`, badgeColorPassing))
	for _, bad := range []string{"<b>", `"<y`} {
		if strings.Contains(svg, bad) {
			t.Errorf("svg holds unescaped %q", bad)
		}
	}
	if !strings.Contains(svg, "a&lt;b&gt;&amp;&#34;") {
		t.Errorf("label not escaped in svg: %s", svg)
	}
}

func TestRenderBadgeSVG_NoExternalReference(t *testing.T) {
	svg := string(renderBadgeSVG("checkout-api", "passing", badgeColorPassing))
	for _, bad := range []string{"href", "xlink", "src=", "url(http"} {
		if strings.Contains(svg, bad) {
			t.Errorf("svg holds external reference marker %q", bad)
		}
	}
}

func TestBadgeStateForRuns(t *testing.T) {
	run := func(status string, result *string) recordstore.Run {
		return recordstore.Run{Status: status, Result: result}
	}
	cases := []struct {
		name      string
		completed []recordstore.Run
		newest    []recordstore.Run
		message   string
		color     string
	}{
		{"success", []recordstore.Run{run("COMPLETED", badgeStrPtr("SUCCESS"))}, nil, "passing", badgeColorPassing},
		{"failure", []recordstore.Run{run("COMPLETED", badgeStrPtr("FAILURE"))}, nil, "failing", badgeColorFailing},
		{"unstable", []recordstore.Run{run("COMPLETED", badgeStrPtr("UNSTABLE"))}, nil, "unstable", badgeColorUnstable},
		{"aborted", []recordstore.Run{run("COMPLETED", badgeStrPtr("ABORTED"))}, nil, "aborted", badgeColorGrey},
		{"not built", []recordstore.Run{run("COMPLETED", badgeStrPtr("NOT_BUILT"))}, nil, "not built", badgeColorGrey},
		{"interrupted", []recordstore.Run{run("COMPLETED", badgeStrPtr("INTERRUPTED"))}, nil, "interrupted", badgeColorGrey},
		{"unknown result", []recordstore.Run{run("COMPLETED", badgeStrPtr("WEIRD"))}, nil, "unknown", badgeColorGrey},
		{"nil result shows unknown", []recordstore.Run{run("COMPLETED", nil)}, nil, "unknown", badgeColorGrey},
		{
			"nil result decides over an older definite result",
			[]recordstore.Run{run("COMPLETED", nil), run("COMPLETED", badgeStrPtr("FAILURE"))},
			nil, "unknown", badgeColorGrey,
		},
		{
			"trigger failure skipped",
			[]recordstore.Run{run("COMPLETED", badgeStrPtr("TRIGGER_FAILED")), run("COMPLETED", badgeStrPtr("FAILURE"))},
			nil, "failing", badgeColorFailing,
		},
		{
			"running newest, completed older decides",
			[]recordstore.Run{run("COMPLETED", badgeStrPtr("FAILURE"))},
			[]recordstore.Run{run("RUNNING", nil)}, "failing", badgeColorFailing,
		},
		{"only running", nil, []recordstore.Run{run("RUNNING", nil)}, "running", badgeColorRunning},
		{"only queued", nil, []recordstore.Run{run("QUEUED", nil)}, "queued", badgeColorRunning},
		{"no runs", nil, nil, "no runs", badgeColorGrey},
		{
			"only trigger failures",
			[]recordstore.Run{run("COMPLETED", badgeStrPtr("TRIGGER_FAILED"))},
			[]recordstore.Run{run("COMPLETED", badgeStrPtr("TRIGGER_FAILED"))},
			"no runs", badgeColorGrey,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, color := badgeStateForRuns(tc.completed, tc.newest)
			if msg != tc.message || color != tc.color {
				t.Errorf("got (%q, %q), want (%q, %q)", msg, color, tc.message, tc.color)
			}
		})
	}
}

// TestBadgeLabel_KeepsDecodedSegments asserts the label does not decode a
// second time. The mux decodes the {file} segment once before the handler runs,
// so a job named a%41b keeps that name.
func TestBadgeLabel_KeepsDecodedSegments(t *testing.T) {
	if got := badgeLabel("app/main"); got != "app/main" {
		t.Errorf("label = %q, want app/main", got)
	}
	if got := badgeLabel("a%41b"); got != "a%41b" {
		t.Errorf("label = %q, want a%%41b kept as written", got)
	}
}

func TestBadgeLabel_Truncates(t *testing.T) {
	long := strings.Repeat("x", 60)
	got := badgeLabel(long)
	if n := len([]rune(got)); n != badgeMaxLabelRunes {
		t.Errorf("truncated label has %d runes, want %d", n, badgeMaxLabelRunes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated label %q does not end in an ellipsis", got)
	}
}
