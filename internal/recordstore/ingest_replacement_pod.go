package recordstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// replacementPodSQL is the one test for a sender that takes over a run
// as the replacement pod of a controller with no cell role. intent is the
// alias of the run_intents row. sender, cluster, namespace, and cell are
// the placeholders of the sender's identity.
//
// Each condition is necessary:
//
//   - The claim granted the run under placement generation zero. Only a
//     controller with no cell role claims under zero. Such a controller
//     keeps its Jenkins home on a volume, and a StatefulSet runs one pod
//     of it at a time, so Jenkins on the next pod resumes the build from
//     disk with no new claim. A cell keeps its home on ephemeral storage.
//     Only a resume dispatch or a claim moves a cell run.
//   - The sender names the controller that the intent names. The gateway
//     takes the controller from the verified certificate of the stream.
//   - The intent names another owner. A run with no owner admits every
//     sender, so it needs no move.
//   - The placement row of the controller names the sender as its current
//     pod. Only the operator's pod read writes that value. A late event
//     from the replaced pod therefore never moves the run back.
func replacementPodSQL(intent, sender, cluster, namespace, cell string) string {
	return `(` + intent + `.placement_generation = 0
		AND ` + sender + `::text <> ''
		AND COALESCE(` + intent + `.cell_instance, '') NOT IN ('', ` + sender + `::text)
		AND ` + intent + `.cluster = ` + cluster + `::text
		AND ` + intent + `.namespace = ` + namespace + `::text
		AND ` + intent + `.cell = ` + cell + `::text
		AND EXISTS (SELECT 1 FROM varroa.tenant_placements p
		             WHERE p.cluster = ` + intent + `.cluster AND p.tenant = ` + intent + `.tenant
		               AND p.generation = 0 AND p.namespace = ` + intent + `.namespace
		               AND p.cell = ` + intent + `.cell AND p.cell_instance = ` + sender + `::text))`
}

// adoptingKind reports whether kind compares its sender with the run's
// owner, so that a replacement pod must own the run before the write. A
// run.started keeps its own owner rule: a superseded start must stay
// visible. The other kinds hold no owner test.
func adoptingKind(kind string) bool {
	return kind == KindRunHeartbeat || kind == KindRunCompleted || runningFactKind(kind)
}

// adoptReplacementPod moves the run's owner to env's sender when
// replacementPodSQL holds. It runs in the write's own transaction, under
// the intent row lock that loadIntent holds. It reads the placement row
// with no lock, so it adds no lock order. On a move it also sets the
// owner in intent, so each later rule of this write compares with the
// new owner. It reports whether it moved the owner. The caller publishes
// the owner move signal after the commit.
func adoptReplacementPod(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope) (bool, error) {
	if !adoptingKind(env.Kind) || intent.placementGeneration != 0 ||
		env.CellInstance == "" || intent.cellInstanceValue() == "" || env.CellInstance == intent.cellInstanceValue() {
		return false, nil
	}
	tag, err := tx.Exec(ctx, `
		UPDATE varroa.run_intents ri
		   SET cell_instance = $2
		 WHERE ri.run_id = $1 AND `+replacementPodSQL("ri", "$2", "$3", "$4", "$5"),
		intent.runID, env.CellInstance, env.Cluster, env.Namespace, env.Cell)
	if err != nil {
		return false, fmt.Errorf("move the run to the replacement pod: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	sender := env.CellInstance
	intent.cellInstance = &sender
	return true, nil
}
