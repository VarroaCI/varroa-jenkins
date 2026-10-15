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

// resumeRefusal names why resumeTargetForRun answers no member. The
// empty value means it answered one. Every value is a reason label
// recorder_run_resume_total carries, so the caller passes it straight
// through.
type resumeRefusal string

const (
	resumeRefusalNone       resumeRefusal = resumeReasonNone
	resumeRefusalNoTarget   resumeRefusal = resumeReasonNoTarget
	resumeRefusalTargetFail resumeRefusal = resumeReasonTargetFailed
)

// resumeTargetForRun answers the member that may resume run's build, and
// a refusal reason while no member may.
//
// This is the only reader of the placement row for a takeover. It
// answers the member that holds the run's job's home slot, resolved with
// memberForJob over the run's own job path. A tenant with one active
// slot therefore resolves to slot 0, exactly as it did before slots
// existed.
//
// Four tests must hold. The home member must name a live instance,
// because a cell with no pod can resume nothing. That instance must
// differ from the run's stored owner, because the dead instance is
// never its own target. The reported seed must equal the live instance,
// which is the field the claim fence reads: a resume that landed before
// the seed applied would reload a job whose own configuration is not
// written yet. The instance must also carry no earlier failure of this
// run's own resume.
//
// The owner test alone would already block a second dispatch to an
// unchanged target, because a failed dispatch leaves that target owning
// the run. It would not block a job home that moves to a second member,
// fails there, and moves back. The failure test blocks that case, and it
// runs first so the refusal carries the reason a person can act on.
func resumeTargetForRun(ctx context.Context, tx pgx.Tx, run meshCandidate) (resumeMember, resumeRefusal, error) {
	row, exists, err := readTenantPlacementMembers(ctx, tx, run.cluster, run.tenant)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return resumeMember{}, resumeRefusalNoTarget, nil
	case err != nil:
		return resumeMember{}, resumeRefusalNoTarget, fmt.Errorf("read resume target placement: %w", err)
	case !exists:
		return resumeMember{}, resumeRefusalNoTarget, nil
	}

	home := memberForJob(row, run.jobPath)
	if home.Cell == "" || home.Instance == "" {
		return resumeMember{}, resumeRefusalNoTarget, nil
	}
	// The failure test runs before the owner test, and only because of
	// the label. A failed dispatch leaves its target owning the run, so
	// both tests refuse the usual case and the owner test would report
	// the wrong reason. The list is empty for every run that never
	// failed a resume, so this order changes no existing answer.
	if run.mesh != nil && run.mesh.Resume.hasFailed(home.Instance) {
		return resumeMember{}, resumeRefusalTargetFail, nil
	}
	if home.Instance == run.cellInstance {
		return resumeMember{}, resumeRefusalNoTarget, nil
	}
	if home.RehydratedInstance != home.Instance {
		return resumeMember{}, resumeRefusalNoTarget, nil
	}
	return resumeMember{cell: home.Cell, namespace: home.Namespace, instance: home.Instance},
		resumeRefusalNone, nil
}
