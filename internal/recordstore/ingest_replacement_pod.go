package recordstore

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	return `(` + controllerSenderSQL(intent, sender, cluster, namespace, cell) + `
		AND EXISTS (SELECT 1 FROM varroa.tenant_placements p
		             WHERE p.cluster = ` + intent + `.cluster AND p.tenant = ` + intent + `.tenant
		               AND p.generation = 0 AND p.namespace = ` + intent + `.namespace
		               AND p.cell = ` + intent + `.cell AND p.cell_instance = ` + sender + `::text))`
}

// controllerSenderSQL holds the conditions of replacementPodSQL that do
// not read the placement row: a generation-zero intent, a sender of the
// intent's own controller, and an intent that names another owner.
func controllerSenderSQL(intent, sender, cluster, namespace, cell string) string {
	return intent + `.placement_generation = 0
		AND ` + sender + `::text <> ''
		AND COALESCE(` + intent + `.cell_instance, '') NOT IN ('', ` + sender + `::text)
		AND ` + intent + `.cluster = ` + cluster + `::text
		AND ` + intent + `.namespace = ` + namespace + `::text
		AND ` + intent + `.cell = ` + cell + `::text`
}

// awaitingAnnounceSQL is the test for a sender that can be the
// replacement pod before the operator announces it. The operator writes
// the pod's identifier into the placement row on its next reconcile tick,
// so the sender's events can arrive first. The test holds the conditions
// of controllerSenderSQL, and the placement row of the controller still
// names the intent's owner or no pod. The row must not list the sender as
// a dead instance, so a late event of a replaced pod never waits.
//
// The placement row stays the only proof of the takeover. This test
// moves nothing: it only lets an event wait for that proof.
func awaitingAnnounceSQL(intent, sender, cluster, namespace, cell string) string {
	return `(` + controllerSenderSQL(intent, sender, cluster, namespace, cell) + `
		AND EXISTS (SELECT 1 FROM varroa.tenant_placements p
		             WHERE p.cluster = ` + intent + `.cluster AND p.tenant = ` + intent + `.tenant
		               AND p.generation = 0 AND p.namespace = ` + intent + `.namespace
		               AND p.cell = ` + intent + `.cell AND p.cell_instance IN ('', ` + intent + `.cell_instance)
		               AND NOT p.dead_instances @> jsonb_build_array(jsonb_build_object('instance', ` + sender + `::text))))`
}

// ErrAwaitingAnnounce is the answer of ApplyDelivery for an event that
// waits for the operator's announce of its sender (awaitingAnnounceSQL).
// The write rolled back. The consumer delivers the event again after
// announceRetryDelay.
var ErrAwaitingAnnounce = errors.New("the sender can be a replacement pod that the operator has not announced")

// announceWait bounds how long an event of an unannounced replacement pod
// waits for the announce. The operator announces the placement row of a
// controller with no cell role on each reconcile tick, 30 s apart by
// default, so the wait spans two ticks. After the wait the event takes
// the rule of any other sender. announceRetryDelay is the delay between
// two deliveries of a waiting event.
const (
	announceWait       = 60 * time.Second
	announceRetryDelay = 5 * time.Second
)

// waitsForAnnounce reports whether an event of kind waits for the
// announce of its sender. A heartbeat does not wait, because the next
// heartbeat after the announce moves the run.
func waitsForAnnounce(kind string) bool {
	return kind == KindRunCompleted || runningFactKind(kind)
}

// awaitsAnnounce reports whether env must wait for the operator's
// announce of its sender (awaitingAnnounceSQL). It reads the placement
// row with no lock, in the write's own transaction.
func awaitsAnnounce(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope) (bool, error) {
	if !waitsForAnnounce(env.Kind) || intent.placementGeneration != 0 ||
		env.CellInstance == "" || intent.cellInstanceValue() == "" || env.CellInstance == intent.cellInstanceValue() {
		return false, nil
	}
	var waits bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM varroa.run_intents ri
		                WHERE ri.run_id = $1 AND `+awaitingAnnounceSQL("ri", "$2", "$3", "$4", "$5")+`)
	`, intent.runID, env.CellInstance, env.Cluster, env.Namespace, env.Cell).Scan(&waits); err != nil {
		return false, fmt.Errorf("read the announce of the sender: %w", err)
	}
	return waits, nil
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
// with no lock. On a move it also sets the owner in intent, so each later
// rule of this write compares with the new owner, and it moves the run's
// fleet locks (moveReplacementPodLocks). It reports whether it moved the
// owner. The caller publishes the owner move signal after the commit.
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
	if env.Kind != KindRunCompleted {
		if err := moveReplacementPodLocks(ctx, tx, intent.runID.String(), sender); err != nil {
			return false, err
		}
	}
	return true, nil
}

// moveReplacementPodLocks sets the cell_instance of each fleet_locks row
// of the run to sender, the replacement pod. The release confirmation and
// the force release read that column. A row that still named the replaced
// pod would read a confirmed-gone holder while the build holds the lock on
// the next pod.
//
// The caller skips a run.completed. Its owner write deletes every lock row
// of the run in the same transaction. The skip also keeps the lock order
// of lockFleetLockRun: the terminal delete takes the run's advisory lock,
// and an acquire takes that lock before it locks a lock row. Every other
// adopting kind takes no advisory lock, so it locks the intent row and
// then the lock rows, the order of the mesh owner move.
func moveReplacementPodLocks(ctx context.Context, tx pgx.Tx, runID, sender string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE varroa.fleet_locks SET cell_instance = $2
		 WHERE run_id = $1 AND cell_instance <> $2
	`, runID, sender); err != nil {
		return fmt.Errorf("move the fleet locks to the replacement pod: %w", err)
	}
	return nil
}
