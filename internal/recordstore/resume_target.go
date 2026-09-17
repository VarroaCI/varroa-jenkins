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
// This is the only reader of the placement row for a takeover. It
// answers the member that holds the run's job's home slot, resolved with
// memberForJob over the run's own job path. A tenant with one active
// slot therefore resolves to slot 0, exactly as it did before slots
// existed.
//
// Three tests must hold. The home member must name a live instance,
// because a cell with no pod can resume nothing. That instance must
// differ from the run's stored owner, because the dead instance is
// never its own target. The reported seed must equal the live instance,
// which is the field the claim fence reads: a resume that landed before
// the seed applied would reload a job whose own configuration is not
// written yet.
func resumeTargetForRun(ctx context.Context, tx pgx.Tx, run meshCandidate) (resumeMember, bool, error) {
	row, exists, err := readTenantPlacementMembers(ctx, tx, run.cluster, run.tenant)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return resumeMember{}, false, nil
	case err != nil:
		return resumeMember{}, false, fmt.Errorf("read resume target placement: %w", err)
	case !exists:
		return resumeMember{}, false, nil
	}

	home := memberForJob(row, run.jobPath)
	if home.Cell == "" || home.Instance == "" {
		return resumeMember{}, false, nil
	}
	if home.Instance == run.cellInstance {
		return resumeMember{}, false, nil
	}
	if home.RehydratedInstance != home.Instance {
		return resumeMember{}, false, nil
	}
	return resumeMember{cell: home.Cell, namespace: home.Namespace, instance: home.Instance}, true, nil
}
