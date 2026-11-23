package recordstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// runOwnerSQL is the owner test of a write that a running build sends.
// sender is the placeholder of the envelope's sender, and owner is the
// placeholder of the intent's own owning instance. The stored runs row
// supplies the other two sources, so the test reads the same ownership
// expression the sweep reads: the intent's instance, else the accepted
// trigger's instance.
//
// The first two rows admit a write where the test has no two names to
// compare: an envelope that names no sender, and a run that no source
// names an owner for. A live install meets neither.
//
// The last row admits the target that a dispatched resume names. The
// resumed build sends every fact from that target, and the heartbeat
// that proves the resume comes from it too.
//
// The expression reads runs.payload. On a missing runs row the caller's
// join makes that value NULL. The expression then answers NULL, not
// false, for a sender that is not the intent's owner.
func runOwnerSQL(sender, owner string) string {
	return `
	` + sender + `::text = ''
	OR COALESCE(NULLIF(` + owner + `::text, ''), runs.payload->>'accepted_instance') IS NULL
	OR ` + sender + `::text = COALESCE(NULLIF(` + owner + `::text, ''), runs.payload->>'accepted_instance')
	OR ` + sender + `::text = runs.payload->'mesh'->'resume'->>'instance'`
}

// runningFactKind reports whether kind carries a fact of a running build
// that senderOwnsRun gates. A COMPLETED event also emits run.stage and
// run.changes, and the gate applies to those envelopes too: the recorder
// cannot tell them apart from a running build's own.
func runningFactKind(kind string) bool {
	switch kind {
	case KindRunStage, KindRunChanges, KindRunSource, KindRunInput:
		return true
	}
	return false
}

// senderOwnsRun reports whether env's sender may write a running-build
// fact onto the run that intent names. It applies the owner test of
// runOwnerSQL, the same test a heartbeat takes, inside the caller's
// transaction. loadIntent holds the intent row lock, and a resume
// dispatch moves the intent under that lock, so the answer cannot go
// stale before the write commits.
//
// A run that the claim granted under placement generation zero always
// passes. Such a run belongs to a controller with no cell role. That
// controller keeps its Jenkins home on a volume, so Jenkins on a
// replacement pod resumes the build from disk with no new claim. The
// intent then still names the old pod, and the test would drop every
// stage and input of the resumed build. A cell keeps its Jenkins home on
// ephemeral storage, so only a resume dispatch continues its build, and
// the dispatch moves the intent first.
func senderOwnsRun(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope) (bool, error) {
	if intent.placementGeneration == 0 {
		return true, nil
	}
	owner := intent.cellInstanceValue()
	if env.CellInstance == "" || env.CellInstance == owner {
		return true, nil
	}
	var owns bool
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE((`+runOwnerSQL("$3", "$4")+`), false)
		  FROM (SELECT 1) AS one
		  LEFT JOIN varroa.runs ON runs.run_id = $1 AND runs.created_at = $2
	`, intent.runID, intent.createdAt, env.CellInstance, owner).Scan(&owns); err != nil {
		return false, fmt.Errorf("read the run owner: %w", err)
	}
	return owns, nil
}
