package recordstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// adoptMarkedIntent answers a repeat claim for the stored run of a marked,
// unstarted intent from an instance other than the stored owner. It
// answers duplicate and writes nothing, unless replacementPodSQL holds for
// the sender and the sender's job counter proves that the build did not
// start.
//
// A controller with no cell role keeps its Jenkins home on a volume.
// Jenkins on its next pod reloads a queued item from that volume with the
// same run id, and it resumes a started build from that volume with no
// claim. Jenkins saves the job's next build number before it creates a
// build, and it raises that number past every build on the volume when it
// loads the job. A counter at or below the stored number therefore proves
// that the volume holds no build of the run, and the sender is the only
// pod that can build it. A null stored number or a zero counter proves
// nothing.
//
// The move applies replacementPodSQL, the rule of the ingest takeover, and
// moves the run's fleet locks as the takeover does. The answer is the
// owner's own repeat: it keeps the stored number, the start mark, and the
// placement generation, and it reserves no number. The move stamps the
// grant time and the claiming incarnation, as the move of reanswerSameRun
// does. The caller signals the move after the commit.
func adoptMarkedIntent(ctx context.Context, tx pgx.Tx, req ClaimRequest, row storedIntent,
	incarnation string) (ClaimResponse, claimEffects, error) {
	duplicate := ClaimResponse{Decision: DecisionDuplicate, RunID: row.runID.String()}
	if row.buildNumber == nil || req.JenkinsNextBuildNumber <= 0 || req.JenkinsNextBuildNumber > *row.buildNumber {
		return duplicate, claimEffects{}, nil
	}
	var cluster string
	err := tx.QueryRow(ctx, `
		UPDATE varroa.run_intents ri
		   SET cell_instance = $2, granted_at = now(), incarnation = $6
		 WHERE ri.run_id = $1 AND `+replacementPodSQL("ri", "$2", "$3", "$4", "$5")+`
		RETURNING ri.cluster
	`, row.runID, req.CellInstance, req.Cluster, req.Namespace, req.Cell, incarnation).Scan(&cluster)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return duplicate, claimEffects{}, nil
	case err != nil:
		return ClaimResponse{}, claimEffects{}, fmt.Errorf("move a marked run to the replacement pod: %w", err)
	}
	if err := moveReplacementPodLocks(ctx, tx, row.runID.String(), req.CellInstance); err != nil {
		return ClaimResponse{}, claimEffects{}, err
	}
	return ClaimResponse{Decision: DecisionGranted, RunID: row.runID.String(), BuildNumber: *row.buildNumber},
		claimEffects{moved: row.runID.String(), movedCluster: cluster}, nil
}
