// Package recordstore: the pipeline mesh's ownership takeover. A run
// whose cell is confirmed gone and whose state is replicated is claimed
// in two steps, armed first and dispatched once a seeded replacement
// reports itself.
package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// resumeArmWindow bounds the wait for a live, seeded target, measured
// from mesh.resume.armed_at. It must exceed a replacement cell pod's
// own recovery, which is the longest step of the whole path. Its value
// is the recorder's existing silent-run window, so the recorder holds
// one long window and not two. It is a constant, not a setting: a
// setting could only be set wrong.
const resumeArmWindow = 15 * time.Minute

// resumeDeadline bounds the dispatched resume, measured from
// mesh.resume.requested_at. It must exceed the five minutes a pipeline
// step waits for a node of the same name before it aborts the build.
// That wait is a constant inside the pipeline plugin, so a setting
// could only be set wrong.
const resumeDeadline = 6 * time.Minute

// ResumeAdoptBound is the longest time from a run's arm to the moment its
// resume adopts the run's agent nodes: the arm window, then the deadline
// of the dispatched resume. An agent pod that a resume can still adopt
// must live at least this long after the arm.
const ResumeAdoptBound = resumeArmWindow + resumeDeadline

// buildRecordRelPath is the file Jenkins reads to rebuild a run. The
// action lists it last, so a reload never sees a run whose flow program
// is still missing.
const buildRecordRelPath = "build.xml"

// Outcome and reason labels recorder_run_resume_total carries.
const (
	resumeOutcomeArmed     = "armed"
	resumeOutcomePublished = "published"
	resumeOutcomeSkipped   = "skipped"
	// resumeOutcomeFailed counts a run the recorder returned to the armed
	// state after its target failed the resume command.
	resumeOutcomeFailed = "failed"
	// resumeOutcomeHeld counts a failure the recorder does not re-arm,
	// because its reason code proves the target reached the build load.
	resumeOutcomeHeld    = "held"
	resumeReasonNone     = ""
	resumeReasonNoState  = "no-state"
	resumeReasonNoTarget = "no-target"
	// resumeReasonTargetFailed names a home instance that already failed
	// this run's resume, which the helper never picks again.
	resumeReasonTargetFailed = "target-failed"
	// resumeReasonBuildLoaded names a failure at or after the build load.
	// A second target must never adopt a build the first may hold.
	resumeReasonBuildLoaded = "build-loaded"
)

// runMeshResume is the resume half of a run's mesh record. An armed run
// holds this object with no Instance: every reader therefore tests the
// named instance, never the presence of the object, to ask whether a
// resume was dispatched.
type runMeshResume struct {
	ArmedAt     *time.Time `json:"armed_at,omitempty"`
	ArmExpires  *time.Time `json:"arm_expires,omitempty"`
	Instance    string     `json:"instance,omitempty"`
	RequestedAt *time.Time `json:"requested_at,omitempty"`
	Deadline    *time.Time `json:"deadline,omitempty"`

	// Failed lists the instances that already failed this run's resume.
	// resumeTargetForRun never picks an instance on the list, which is
	// the bound on re-dispatch: a further dispatch needs an instance that
	// is new, so a pod must roll or a job home must move. The list lives
	// as long as the resume record, which the target's own heartbeat
	// removes.
	Failed []string `json:"failed,omitempty"`
}

// hasFailed reports whether instance already failed this run's resume.
func (r *runMeshResume) hasFailed(instance string) bool {
	if r == nil || instance == "" {
		return false
	}
	for _, failed := range r.Failed {
		if failed == instance {
			return true
		}
	}
	return false
}

// runMeshRecord is runs.payload->'mesh'. StateAt is the freshness
// anchor and the resume gate: a null value means no usable state.
type runMeshRecord struct {
	StateAt        *time.Time     `json:"state_at,omitempty"`
	Files          []runStateFile `json:"files,omitempty"`
	RejectedReason string         `json:"rejected_reason,omitempty"`
	CapWarnedAt    *time.Time     `json:"cap_warned_at,omitempty"`
	Resume         *runMeshResume `json:"resume,omitempty"`
}

// decodeMeshRecord decodes runs.payload->'mesh'. An absent key, a JSON
// null, and an unreadable value all answer a nil record, which every
// test then treats as "no mesh".
func decodeMeshRecord(raw json.RawMessage) *runMeshRecord {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var rec runMeshRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil
	}
	return &rec
}

// hasState reports whether the record holds usable replicated state.
//
// A file entry whose epoch or generation is not positive names no
// object: the object key carries both numbers. An empty list names
// nothing at all. Either way the run has no state a resume can read, so
// it takes the ordinary confirmed-loss path.
func (r *runMeshRecord) hasState() bool {
	if r == nil || r.StateAt == nil || len(r.Files) == 0 {
		return false
	}
	for _, f := range r.Files {
		if f.Epoch <= 0 || f.StateGen <= 0 {
			return false
		}
	}
	return true
}

// decodeRunStateFiles decodes one run's mesh.files array.
func decodeRunStateFiles(raw []byte) ([]runStateFile, error) {
	var files []runStateFile
	if err := json.Unmarshal(raw, &files); err != nil {
		return nil, fmt.Errorf("decode run-state file list: %w", err)
	}
	return files, nil
}

// meshCandidate is one RUNNING run the takeover reads.
type meshCandidate struct {
	runID        string
	createdAt    time.Time
	cluster      string
	namespace    string
	cell         string
	tenant       string
	jobPath      string
	buildNumber  *int64
	cellInstance string
	mesh         *runMeshRecord
}

// TakeOverLostRuns claims every RUNNING run of cluster/tenant whose
// cell is confirmed gone and whose state is replicated, then dispatches
// each armed run whose tenant now reports a seeded replacement. It
// answers how many resume actions it published.
//
// It runs before the interrupted marking at both call sites, which
// removes one round of latency. No rule depends on that order: the
// dispatch tests its own window, and the marking write tests the same
// window against the committed record.
func TakeOverLostRuns(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn,
	metrics *InterruptedMetrics, actionMetrics *ActionMetrics, cluster, tenant string,
	grace time.Duration, now time.Time, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.Default()
	}

	candidates, err := selectMeshCandidates(ctx, pool, cluster, tenant)
	if err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	row, exists, err := readTenantPlacementLivenessRow(ctx, pool, cluster, tenant)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}

	armMeshCandidates(ctx, pool, metrics, candidates, row, grace, now, logger)
	return dispatchMeshResumes(ctx, pool, conn, metrics, actionMetrics, candidates, now, logger), nil
}

// armMeshCandidates runs step A over every candidate. The arm needs no
// target, and it shields the run from the marking at once.
func armMeshCandidates(ctx context.Context, pool *pgxpool.Pool, metrics *InterruptedMetrics,
	candidates []meshCandidate, row tenantPlacementLivenessRow, grace time.Duration,
	now time.Time, logger *slog.Logger) {
	for i := range candidates {
		c := &candidates[i]
		// A1: only a pod-read confirmation claims a run. A stream loss
		// alone is never a claim.
		if !confirmedGoneInstance(row.deadInstances, c.cellInstance, now, grace) {
			continue
		}
		// A2: a run with no replicated state can never resume.
		if !c.mesh.hasState() {
			metrics.addResume(ctx, resumeOutcomeSkipped, resumeReasonNoState)
			continue
		}
		// A3: the arm happens at most once.
		if c.mesh.Resume != nil {
			continue
		}
		armed, err := armMeshRun(ctx, pool, *c, now)
		if err != nil {
			logger.Warn("arm lost run failed", "run_id", c.runID, "error", err)
			continue
		}
		if !armed {
			continue
		}
		expires := now.Add(resumeArmWindow)
		c.mesh.Resume = &runMeshResume{ArmedAt: &now, ArmExpires: &expires}
		metrics.addResume(ctx, resumeOutcomeArmed, resumeReasonNone)
	}
}

// armMeshRun writes the arm. The mesh.resume IS NULL guard is the
// at-most-once marker: two rounds reading the same unarmed candidate
// write one arm between them.
func armMeshRun(ctx context.Context, pool *pgxpool.Pool, c meshCandidate, now time.Time) (bool, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE varroa.runs
		   SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('mesh',
		         COALESCE(payload->'mesh', '{}'::jsonb) || jsonb_build_object('resume',
		           jsonb_build_object('armed_at', $3::timestamptz, 'arm_expires', $4::timestamptz)))
		 WHERE run_id = $1 AND created_at = $2 AND status = 'RUNNING'
		   AND payload->'mesh'->'resume' IS NULL
	`, c.runID, c.createdAt, now, now.Add(resumeArmWindow))
	if err != nil {
		return false, fmt.Errorf("arm lost run: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// dispatchMeshResumes runs step B over every armed candidate.
func dispatchMeshResumes(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn,
	metrics *InterruptedMetrics, actionMetrics *ActionMetrics, candidates []meshCandidate,
	now time.Time, logger *slog.Logger) int {
	published := 0
	for _, c := range candidates {
		// B2: the cap path can clear the anchor and delete the state
		// after the arm. Such a run keeps its arm and times out.
		if !c.mesh.hasState() {
			continue
		}
		// B3: an armed run holds a resume that names no instance.
		if c.mesh.Resume == nil || c.mesh.Resume.Instance != "" {
			continue
		}
		// B4: the marking may already be claiming the run. The two run
		// on different goroutines, so each tests the window itself.
		if c.mesh.Resume.ArmExpires == nil || !c.mesh.Resume.ArmExpires.After(now) {
			continue
		}
		action, ok, err := dispatchMeshResume(ctx, pool, metrics, c, now)
		if err != nil {
			logger.Warn("dispatch resume failed", "run_id", c.runID, "error", err)
			continue
		}
		if !ok {
			continue
		}
		// The publish is outside the transaction. A lost action
		// produces no resume, and the recorder never republishes: the
		// run then reaches its deadline and takes the ordinary path.
		if err := PublishLedgerAction(conn, action); err != nil {
			logger.Warn("publish resume_run action failed", "run_id", c.runID, "error", err)
			continue
		}
		metrics.addResume(ctx, resumeOutcomePublished, resumeReasonNone)
		actionMetrics.addPublished(ctx, action.Kind)
		published++
	}
	return published
}

// dispatchMeshResume moves one run's ownership in a single transaction
// and builds the action the caller publishes. It answers ok=false when
// the transaction changed no run row, which is the whole claim: B1 and
// the at-most-once dispatch guard both live in that statement's WHERE
// clause.
func dispatchMeshResume(ctx context.Context, pool *pgxpool.Pool, metrics *InterruptedMetrics,
	c meshCandidate, now time.Time) (LedgerAction, bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return LedgerAction{}, false, fmt.Errorf("begin resume takeover: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// B5: only a seeded replacement may adopt the build. A missing
	// target defers the dispatch and never drops it; the round retries
	// every minute until the arm window passes. The helper names its own
	// refusal, so a tenant with no replacement and a tenant whose only
	// candidate already failed are counted apart.
	target, refusal, err := resumeTargetForRun(ctx, tx, c)
	if err != nil {
		return LedgerAction{}, false, err
	}
	if refusal != resumeRefusalNone {
		metrics.addResume(ctx, resumeOutcomeSkipped, string(refusal))
		return LedgerAction{}, false, nil
	}

	if _, err := tx.Exec(ctx, `
		UPDATE varroa.run_intents SET cell = $2, cell_instance = $3
		 WHERE run_id = $1 AND cell_instance = $4
	`, c.runID, target.cell, target.instance, c.cellInstance); err != nil {
		return LedgerAction{}, false, fmt.Errorf("move run intent owner: %w", err)
	}

	// The move also raises the lock's stored generation to the tenant's
	// current value, so the epoch follows its holder. The guard names
	// the dead instance, so a repeat of the move writes nothing.
	if _, err := tx.Exec(ctx, `
		UPDATE varroa.fleet_locks l SET cell_instance = $2,
		       placement_generation = GREATEST(l.placement_generation, COALESCE((
		           SELECT p.generation FROM varroa.tenant_placements p
		            WHERE p.cluster = l.cluster AND p.tenant = l.tenant
		       ), l.placement_generation))
		 WHERE l.run_id = $1::text AND l.cell_instance = $3
	`, c.runID, target.instance, c.cellInstance); err != nil {
		return LedgerAction{}, false, fmt.Errorf("move fleet lock owner: %w", err)
	}

	deadline := now.Add(resumeDeadline)
	// The statement returns the run's own file list, so the action names
	// the epochs the record holds at this moment. The candidate's copy was
	// read before the arm, and an ingest that supersedes an epoch deletes
	// the older object: an action built from that copy could name an
	// object that is already gone.
	var files []byte
	err = tx.QueryRow(ctx, `
		UPDATE varroa.runs
		   SET cell = $3,
		       payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('mesh',
		         COALESCE(payload->'mesh', '{}'::jsonb) || jsonb_build_object('resume',
		           COALESCE(payload->'mesh'->'resume', '{}'::jsonb) || jsonb_build_object(
		             'instance', $4::text, 'requested_at', $5::timestamptz, 'deadline', $6::timestamptz)))
		 WHERE run_id = $1 AND created_at = $2 AND status = 'RUNNING'
		   AND payload->'mesh'->'resume' IS NOT NULL
		   AND payload->'mesh'->'resume'->'instance' IS NULL
		RETURNING COALESCE(payload->'mesh'->'files', '[]'::jsonb)
	`, c.runID, c.createdAt, target.cell, target.instance, now, deadline).Scan(&files)
	switch {
	// No row matched. A real completion can commit while this transaction
	// is open, and the first two statements would then leave a finished
	// run owning an intent row and a lock row that name the target, so the
	// whole transaction rolls back.
	case errors.Is(err, pgx.ErrNoRows):
		metrics.addResume(ctx, resumeOutcomeSkipped, resumeReasonNone)
		return LedgerAction{}, false, nil
	case err != nil:
		return LedgerAction{}, false, fmt.Errorf("move run owner: %w", err)
	}

	fresh, err := decodeRunStateFiles(files)
	if err != nil {
		return LedgerAction{}, false, err
	}
	// A file entry with no epoch names no object, and an empty list names
	// nothing at all. Either answers the no-state refusal, so the run
	// keeps its arm and takes the ordinary path at its deadline.
	if !(&runMeshRecord{StateAt: c.mesh.StateAt, Files: fresh}).hasState() {
		metrics.addResume(ctx, resumeOutcomeSkipped, resumeReasonNoState)
		return LedgerAction{}, false, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return LedgerAction{}, false, fmt.Errorf("commit resume takeover: %w", err)
	}

	return LedgerAction{
		Kind:        LedgerActionResumeRun,
		Cluster:     c.cluster,
		Namespace:   target.namespace,
		Controller:  target.cell,
		Actor:       LedgerActionRecorder,
		RunID:       c.runID,
		JobPath:     c.jobPath,
		BuildNumber: c.buildNumber,
		Instance:    target.instance,
		Files:       resumeFileList(fresh),
		Deadline:    &deadline,
	}, true, nil
}

// resumeFileList orders the run's stored files with the build record
// last. Jenkins reads that file to rebuild the run, so writing it last
// means a reload never sees a run whose flow program is still missing.
//
// Each entry carries the epoch and the generation of the object that
// holds the file. The target reads that exact object, so a path alone
// would name nothing.
func resumeFileList(files []runStateFile) []ResumeFile {
	out := make([]ResumeFile, 0, len(files))
	for _, f := range files {
		if f.RelPath != "" {
			out = append(out, ResumeFile{RelPath: f.RelPath, Epoch: f.Epoch, StateGen: f.StateGen})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return !isBuildRecordPath(out[i].RelPath) && isBuildRecordPath(out[j].RelPath)
	})
	return out
}

// isBuildRecordPath reports whether relpath names a build record.
func isBuildRecordPath(relpath string) bool {
	return len(relpath) >= len(buildRecordRelPath) &&
		relpath[len(relpath)-len(buildRecordRelPath):] == buildRecordRelPath
}

// selectMeshCandidates reads every RUNNING run of cluster/tenant with
// the columns the takeover's tests and its transaction need.
func selectMeshCandidates(ctx context.Context, pool *pgxpool.Pool, cluster, tenant string) ([]meshCandidate, error) {
	rows, err := pool.Query(ctx, `
		SELECT r.run_id, r.created_at, r.cluster, r.namespace, r.cell, r.tenant, r.job_path,
		       r.build_number, r.payload->'mesh',
		       COALESCE(NULLIF(ri.cell_instance, ''), r.payload->>'accepted_instance')
		  FROM varroa.runs r
		  LEFT JOIN varroa.run_intents ri ON ri.run_id = r.run_id
		 WHERE r.status = 'RUNNING' AND r.cluster = $1 AND r.tenant = $2
	`, cluster, tenant)
	if err != nil {
		return nil, fmt.Errorf("select mesh candidates: %w", err)
	}
	defer rows.Close()

	var out []meshCandidate
	for rows.Next() {
		var (
			c              meshCandidate
			meshRaw        json.RawMessage
			owningInstance *string
		)
		if err := rows.Scan(&c.runID, &c.createdAt, &c.cluster, &c.namespace, &c.cell, &c.tenant,
			&c.jobPath, &c.buildNumber, &meshRaw, &owningInstance); err != nil {
			return nil, fmt.Errorf("scan mesh candidate: %w", err)
		}
		c.mesh = decodeMeshRecord(meshRaw)
		c.cellInstance = derefString(owningInstance)
		if c.cellInstance == "" {
			continue
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select mesh candidates: %w", err)
	}
	return out, nil
}

// runTakeoverRound runs the takeover for every (cluster, tenant) that
// holds a RUNNING run naming an owning instance.
func runTakeoverRound(ctx context.Context, pool *pgxpool.Pool, conn *bus.Conn, metrics *InterruptedMetrics,
	actionMetrics *ActionMetrics, now time.Time, grace time.Duration, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	pairs, err := listMeshPairs(ctx, pool)
	if err != nil {
		return err
	}
	for _, p := range pairs {
		if _, err := TakeOverLostRuns(ctx, pool, conn, metrics, actionMetrics,
			p.cluster, p.tenant, grace, now, logger); err != nil {
			logger.Warn("resume takeover failed", "cluster", p.cluster, "tenant", p.tenant, "error", err)
		}
	}
	return nil
}

// listMeshPairs answers every distinct (cluster, tenant) that holds a
// RUNNING run.
func listMeshPairs(ctx context.Context, pool *pgxpool.Pool) ([]refirePair, error) {
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT cluster, tenant FROM varroa.runs WHERE status = 'RUNNING'
	`)
	if err != nil {
		return nil, fmt.Errorf("list mesh pairs: %w", err)
	}
	defer rows.Close()

	var out []refirePair
	for rows.Next() {
		var p refirePair
		if err := rows.Scan(&p.cluster, &p.tenant); err != nil {
			return nil, fmt.Errorf("scan mesh pair: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list mesh pairs: %w", err)
	}
	return out, nil
}
