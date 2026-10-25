package api

import (
	"bytes"
	"fmt"
	"html"
	"unicode/utf8"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// Badge colours. The grey covers every state that is not a build result.
const (
	badgeColorPassing  = "#4c1"
	badgeColorFailing  = "#e05d44"
	badgeColorUnstable = "#dfb317"
	badgeColorGrey     = "#9f9f9f"
	badgeColorRunning  = "#007ec6"
	badgeColorLabel    = "#555"

	// badgeMaxLabelRunes bounds the job path in the label. A longer path
	// is cut and ends with an ellipsis.
	badgeMaxLabelRunes = 48
)

// renderBadgeSVG draws a flat two-part badge. Every string is escaped, and
// the SVG holds no external reference: no href, no xlink, no image.
func renderBadgeSVG(label, message, color string) []byte {
	labelW := badgePartWidth(label)
	msgW := badgePartWidth(message)
	total := labelW + msgW
	var b bytes.Buffer
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="%s: %s">`,
		total, html.EscapeString(label), html.EscapeString(message))
	fmt.Fprintf(&b, `<title>%s: %s</title>`, html.EscapeString(label), html.EscapeString(message))
	b.WriteString(`<linearGradient id="s" x2="0" y2="100%"><stop offset="0" stop-color="#bbb" stop-opacity=".1"/><stop offset="1" stop-opacity=".1"/></linearGradient>`)
	fmt.Fprintf(&b, `<clipPath id="r"><rect width="%d" height="20" rx="3" fill="#fff"/></clipPath>`, total)
	fmt.Fprintf(&b, `<g clip-path="url(#r)"><rect width="%d" height="20" fill="%s"/><rect x="%d" width="%d" height="20" fill="%s"/><rect width="%d" height="20" fill="url(#s)"/></g>`,
		labelW, badgeColorLabel, labelW, msgW, html.EscapeString(color), total)
	b.WriteString(`<g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">`)
	badgeText(&b, labelW/2, label)
	badgeText(&b, labelW+msgW/2, message)
	b.WriteString(`</g></svg>`)
	return b.Bytes()
}

// badgeText writes one text element with a shadow copy, centred on x.
func badgeText(b *bytes.Buffer, x int, text string) {
	n := utf8.RuneCountInString(text)
	escaped := html.EscapeString(text)
	fmt.Fprintf(b, `<text x="%d" y="15" fill="#010101" fill-opacity=".3" textLength="%d">%s</text>`, x, 7*n, escaped)
	fmt.Fprintf(b, `<text x="%d" y="14" textLength="%d">%s</text>`, x, 7*n, escaped)
}

// badgePartWidth is the pixel width of one badge part. It is an estimate:
// each rune takes seven pixels, plus a margin of five on each side.
func badgePartWidth(text string) int {
	return 7*utf8.RuneCountInString(text) + 10
}

// badgeStateForRuns maps the runs of a job to the badge message and colour.
// completed holds the completed runs of the job, newest first. The first run
// whose result is not TRIGGER_FAILED decides. A TRIGGER_FAILED run never
// reached Jenkins, so it does not count as a build.
//
// newest holds the newest run of the job, of any status. It is consulted
// only when no completed run decides: a running or queued build then shows
// blue. With neither, the badge reads "no runs".
func badgeStateForRuns(completed, newest []recordstore.Run) (message, color string) {
	for _, run := range completed {
		if !badgeRunDecides(run) {
			continue
		}
		return badgeStateForResult(run.Result)
	}
	for _, run := range newest {
		switch run.Status {
		case "RUNNING":
			return "running", badgeColorRunning
		case "QUEUED":
			return "queued", badgeColorRunning
		}
	}
	return "no runs", badgeColorGrey
}

// badgeRunDecides reports whether a completed run decides the badge. Only a
// TRIGGER_FAILED run does not decide it. A completed run with no result
// decides as unknown, so the badge shows that state and does not hide it
// behind an older run.
func badgeRunDecides(run recordstore.Run) bool {
	return run.Result == nil || *run.Result != "TRIGGER_FAILED"
}

// badgeStateForResult maps one completed result to its badge state. A missing
// or unknown result shows "unknown".
func badgeStateForResult(result *string) (message, color string) {
	if result == nil {
		return "unknown", badgeColorGrey
	}
	switch *result {
	case "SUCCESS":
		return "passing", badgeColorPassing
	case "FAILURE":
		return "failing", badgeColorFailing
	case "UNSTABLE":
		return "unstable", badgeColorUnstable
	case "ABORTED":
		return "aborted", badgeColorGrey
	case "NOT_BUILT":
		return "not built", badgeColorGrey
	case "INTERRUPTED":
		return "interrupted", badgeColorGrey
	default:
		return "unknown", badgeColorGrey
	}
}

// badgeLabel returns the badge label of a folder-relative job path. The path
// is already decoded: the mux decodes the {file} segment once. The label is cut
// to badgeMaxLabelRunes runes.
func badgeLabel(relPath string) string {
	label := relPath
	runes := []rune(label)
	if len(runes) > badgeMaxLabelRunes {
		label = string(runes[:badgeMaxLabelRunes-1]) + "…"
	}
	return label
}
