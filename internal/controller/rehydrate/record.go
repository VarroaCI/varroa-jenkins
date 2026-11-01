// Package rehydrate renders build-record rehydration content for the
// operator: the thin build.xml and changelog0.xml documents of
// design.md D1/D1a/D2, the per-job seed selection of D4, the 256 KiB
// bundle budget, and the wire RehydrationBundle the operator sends to
// a cell. See specs/build-record-rehydration/spec.md for the binding
// contract.
package rehydrate

import "time"

// resultInterrupted is the run ledger's terminal result with no
// Jenkins hudson.model.Result counterpart. jenkinsResult maps it to
// ABORTED.
const resultInterrupted = "INTERRUPTED"

// resultSuccess is the Jenkins result value the job-seed builder
// treats as "the newest successful record."
const resultSuccess = "SUCCESS"

// Record is one thin build record the operator renders into a
// build.xml document. It is this package's own shape, decoupled from
// the record store's wire and storage types
// (recordstore.RehydrationRecord, recordstore.TenantSyncRecord): a
// caller adapts whichever transport type it holds into a Record. The
// field names mirror recordstore.RehydrationRecord.
type Record struct {
	// BuildNumber is the build's directory name. The renderer never
	// writes it into build.xml: Run.number is transient, and Jenkins
	// reads the build number from the directory name.
	BuildNumber int64
	QueuedAt    *time.Time
	StartedAt   *time.Time
	DurationMs  *int64
	// Result is one of SUCCESS, UNSTABLE, FAILURE, ABORTED, NOT_BUILT,
	// or INTERRUPTED. RenderBuildXML maps INTERRUPTED to ABORTED,
	// because the Jenkins Result type has no INTERRUPTED value.
	Result string
	// CauseDescription is the recorded cause description. It renders
	// as <description>.
	CauseDescription string
	// Params holds the run's recorded parameters, already decoded from
	// the record store's JSON. A name listed in MaskedParameterNames
	// is skipped even when Params still carries a value for it.
	Params map[string]string
	// MaskedParameterNames lists the parameter names the renderer
	// SHALL skip, both name and value, so a password parameter never
	// reaches a file. The record store already replaces a masked
	// value with a placeholder; this is the second line.
	MaskedParameterNames []string
	// Changes holds the run's recorded changeset, oldest first, the
	// plugin order, already capped by the caller (the record store
	// caps at 100 and keeps the newest). A
	// non-empty Changes gets a changelog0.xml file and a <checkouts>
	// element.
	Changes []Change
}

// Change is one recorded commit of a Record's changeset.
//
// The changelog document format (design.md D1a) also names an
// authorEmail element and zero-or-more path elements per entry. No
// component in the build-event pipeline records a commit author's
// email or the affected file paths — internal/mite/build_events.go's
// changeEntry and the plugin's BuildChange.java carry commitId,
// authorId, authorDisplayName, message, timestamp, and only an
// affected-path COUNT, never the paths themselves. RenderChangelog
// therefore writes an empty authorEmail element and zero path
// elements for every entry. Recorded in review-log.md.
type Change struct {
	CommitID string
	// AuthorID is the Jenkins user id the mite recorded. The plugin's
	// VarroaChangeLogSet.Entry resolves it with
	// User.getById(id, false) and falls back to User.getUnknown().
	AuthorID  string
	Message   string
	Timestamp time.Time
}

// jenkinsResult maps a Record's stored result to the Jenkins Result
// value RenderBuildXML writes into <result>.
func jenkinsResult(result string) string {
	if result == resultInterrupted {
		return "ABORTED"
	}
	return result
}

// millis answers t's Unix time in milliseconds, or 0 for a nil t.
func millis(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.UnixMilli()
}

// durationMillis answers *d, or 0 for a nil d.
func durationMillis(d *int64) int64 {
	if d == nil {
		return 0
	}
	return *d
}
