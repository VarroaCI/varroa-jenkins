package recordstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// resumeMember is the cell that may adopt a lost run's build.
type resumeMember struct {
	cell      string
	namespace string
	instance  string
}

// resumeTargetForRun answers the member that may resume run's build,
// and ok=false while no member may.
//
// This is the only reader of the placement row for a takeover. C3
// answers slot 0's current instance once its reported seed matches it.
// C4 extends this one body to members[job_homes[job_path]], and every
// caller follows with no change.
//
// Three tests must hold. The placement must name a live instance,
// because a cell with no pod can resume nothing. That instance must
// differ from the run's stored owner, because the dead instance is
// never its own target. The reported seed must equal the live instance,
// which is the field the claim fence reads: a resume that landed before
// the seed applied would reload a job whose own configuration is not
// written yet.
func resumeTargetForRun(ctx context.Context, tx pgx.Tx, run meshCandidate) (resumeMember, bool, error) {
	var (
		namespace          string
		cell               string
		cellInstance       string
		rehydratedInstance *string
	)
	err := tx.QueryRow(ctx, `
		SELECT namespace, cell, cell_instance, rehydrated_instance
		  FROM varroa.tenant_placements
		 WHERE cluster = $1 AND tenant = $2
	`, run.cluster, run.tenant).Scan(&namespace, &cell, &cellInstance, &rehydratedInstance)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return resumeMember{}, false, nil
	case err != nil:
		return resumeMember{}, false, fmt.Errorf("read resume target placement: %w", err)
	}

	if cell == "" || cellInstance == "" {
		return resumeMember{}, false, nil
	}
	if cellInstance == run.cellInstance {
		return resumeMember{}, false, nil
	}
	if derefString(rehydratedInstance) != cellInstance {
		return resumeMember{}, false, nil
	}
	return resumeMember{cell: cell, namespace: namespace, instance: cellInstance}, true, nil
}
