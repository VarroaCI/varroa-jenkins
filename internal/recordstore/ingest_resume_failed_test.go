package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// resumeFailedTarget is the instance every resume in this file was
// dispatched to, and the value the write's own guard reads.
const resumeFailedTarget = "pod-live"

// newResumeFailedEnvelope returns a run.resume_failed envelope naming
// instance and code.
func newResumeFailedEnvelope(t *testing.T, runID, instance, code string) IngestEnvelope {
	t.Helper()
	raw, err := json.Marshal(RunResumeFailedPayload{
		Instance:   instance,
		Error:      "resume_build: fetch build.xml: stream closed",
		ReasonCode: code,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return IngestEnvelope{
		SchemaVersion: 1,
		Kind:          KindRunResumeFailed,
		RunID:         runID,
		Cluster:       testCluster,
		Namespace:     testNamespace,
		Cell:          meshTargetCell,
		CellInstance:  instance,
		Tenant:        testTenant,
		JobPath:       testJobPath,
		OccurredAt:    time.Now().UTC().Truncate(time.Microsecond),
		Payload:       raw,
	}
}

// stampDispatchedResume stamps one run's mesh record as a dispatched
// resume naming instance, and returns the arm fields the re-arm must
// leave untouched.
func stampDispatchedResume(t *testing.T, db *sql.DB, runID uuid.UUID, instance string) (armedAt, armExpires string) {
	t.Helper()
	if _, err := db.Exec(`
		UPDATE varroa.runs
		   SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('mesh',
		         COALESCE(payload->'mesh', '{}'::jsonb) || jsonb_build_object('resume',
		           jsonb_build_object(
		             'armed_at', now() - interval '2 minutes',
		             'arm_expires', now() + interval '13 minutes',
		             'instance', $2::text,
		             'requested_at', now() - interval '1 minute',
		             'deadline', now() + interval '5 minutes')))
		 WHERE run_id = $1
	`, runID, instance); err != nil {
		t.Fatalf("seed dispatched resume: %v", err)
	}
	armedAt, armExpires, _ = readMeshResume(t, db, runID)
	return armedAt, armExpires
}

// readResumeFailedList reads one run's mesh.resume.failed list.
func readResumeFailedList(t *testing.T, db *sql.DB, runID uuid.UUID) []string {
	t.Helper()
	var raw []byte
	if err := db.QueryRow(`
		SELECT COALESCE(payload->'mesh'->'resume'->'failed', '[]'::jsonb)
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&raw); err != nil {
		t.Fatalf("read failed list: %v", err)
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode failed list: %v", err)
	}
	return out
}

// readResumeTimes reads the request time and the deadline of one run's
// resume record, as text, answering empty strings for an absent key.
func readResumeTimes(t *testing.T, db *sql.DB, runID uuid.UUID) (requestedAt, deadline string) {
	t.Helper()
	if err := db.QueryRow(`
		SELECT COALESCE(payload->'mesh'->'resume'->>'requested_at', ''),
		       COALESCE(payload->'mesh'->'resume'->>'deadline', '')
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&requestedAt, &deadline); err != nil {
		t.Fatalf("read resume times: %v", err)
	}
	return requestedAt, deadline
}

// applyResumeFailedEnvelope applies one envelope and fails the test on
// an error.
func applyResumeFailedEnvelope(t *testing.T, g *Ingester, env IngestEnvelope) ApplyResult {
	t.Helper()
	res, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return res
}

// seedRunWithDispatchedResume seeds one RUNNING run owned by the dead
// instance, stamps a dispatched resume on it, and answers the run
// identifier with its arm fields.
func seedRunWithDispatchedResume(t *testing.T, pool *pgxpool.Pool, db *sql.DB) (uuid.UUID, string, string) {
	t.Helper()
	runID := seedMeshRunningRun(t, pool, db)
	armedAt, armExpires := stampDispatchedResume(t, db, runID, resumeFailedTarget)
	return runID, armedAt, armExpires
}

// TestApplyResumeFailedRearmsRun asserts the write clears the dispatch,
// lists the failed instance, and leaves the arm untouched. The arm
// window is never extended: a failure buys the run no time.
func TestApplyResumeFailedRearmsRun(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	runID, armedAt, armExpires := seedRunWithDispatchedResume(t, pool, db)

	res := applyResumeFailedEnvelope(t, g,
		newResumeFailedEnvelope(t, runID.String(), resumeFailedTarget, "RESUME_FAILED"))
	if !res.ResumeFailedCleared {
		t.Fatal("ResumeFailedCleared = false, want true")
	}
	if res.ResumeFailedReason != "RESUME_FAILED" {
		t.Fatalf("ResumeFailedReason = %q", res.ResumeFailedReason)
	}

	gotArmed, gotExpires, gotInstance := readMeshResume(t, db, runID)
	if gotInstance != "" {
		t.Fatalf("instance = %q, want cleared", gotInstance)
	}
	if gotArmed != armedAt || gotExpires != armExpires {
		t.Fatalf("arm moved from %q/%q to %q/%q, want it unchanged",
			armedAt, armExpires, gotArmed, gotExpires)
	}
	requestedAt, deadline := readResumeTimes(t, db, runID)
	if requestedAt != "" || deadline != "" {
		t.Fatalf("requested_at=%q deadline=%q, want both cleared", requestedAt, deadline)
	}
	if got := readResumeFailedList(t, db, runID); len(got) != 1 || got[0] != resumeFailedTarget {
		t.Fatalf("failed list = %v, want [%s]", got, resumeFailedTarget)
	}
}

// TestApplyResumeFailedHeldCodeWritesNothing asserts a reason code that
// proves the target reached the build load leaves the record alone. A
// second target must never adopt a build the first may already hold.
func TestApplyResumeFailedHeldCodeWritesNothing(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"RESUME_LOAD_FAILED", "RESUME_NOT_BUILDING"} {
		t.Run(code, func(t *testing.T) {
			pool, db := setupLedgerPool(t)
			g := NewIngester(pool, nil, nil)

			runID, _, _ := seedRunWithDispatchedResume(t, pool, db)

			res := applyResumeFailedEnvelope(t, g,
				newResumeFailedEnvelope(t, runID.String(), resumeFailedTarget, code))
			if res.ResumeFailedCleared {
				t.Fatal("ResumeFailedCleared = true, want false for a held code")
			}

			_, _, gotInstance := readMeshResume(t, db, runID)
			if gotInstance != resumeFailedTarget {
				t.Fatalf("instance = %q, want the dispatch kept", gotInstance)
			}
			if got := readResumeFailedList(t, db, runID); len(got) != 0 {
				t.Fatalf("failed list = %v, want empty", got)
			}
		})
	}
}

// TestApplyResumeFailedGuardsOnInstance asserts the three ways the write
// must change no row: a repeat of the same record, a record from an
// earlier attempt whose instance the record no longer names, and a run
// that is no longer RUNNING.
func TestApplyResumeFailedGuardsOnInstance(t *testing.T) {
	t.Parallel()

	t.Run("a repeat changes nothing", func(t *testing.T) {
		pool, db := setupLedgerPool(t)
		g := NewIngester(pool, nil, nil)
		runID, _, _ := seedRunWithDispatchedResume(t, pool, db)

		env := newResumeFailedEnvelope(t, runID.String(), resumeFailedTarget, "RESUME_FAILED")
		if res := applyResumeFailedEnvelope(t, g, env); !res.ResumeFailedCleared {
			t.Fatal("the first record must re-arm the run")
		}
		if res := applyResumeFailedEnvelope(t, g, env); res.ResumeFailedCleared {
			t.Fatal("the second record must change no row")
		}
		if got := readResumeFailedList(t, db, runID); len(got) != 1 {
			t.Fatalf("failed list = %v, want one entry", got)
		}
	})

	t.Run("a stale instance changes nothing", func(t *testing.T) {
		pool, db := setupLedgerPool(t)
		g := NewIngester(pool, nil, nil)
		runID, _, _ := seedRunWithDispatchedResume(t, pool, db)

		res := applyResumeFailedEnvelope(t, g,
			newResumeFailedEnvelope(t, runID.String(), "pod-earlier", "RESUME_FAILED"))
		if res.ResumeFailedCleared {
			t.Fatal("a record naming another instance must change no row")
		}
		_, _, gotInstance := readMeshResume(t, db, runID)
		if gotInstance != resumeFailedTarget {
			t.Fatalf("instance = %q, want the dispatch kept", gotInstance)
		}
	})

	t.Run("a run that ended changes nothing", func(t *testing.T) {
		pool, db := setupLedgerPool(t)
		g := NewIngester(pool, nil, nil)
		runID, _, _ := seedRunWithDispatchedResume(t, pool, db)
		if _, err := db.Exec(`
			UPDATE varroa.runs SET status = 'COMPLETED', result = 'SUCCESS', ended_at = now()
			 WHERE run_id = $1
		`, runID); err != nil {
			t.Fatalf("end the run: %v", err)
		}

		res := applyResumeFailedEnvelope(t, g,
			newResumeFailedEnvelope(t, runID.String(), resumeFailedTarget, "RESUME_FAILED"))
		if res.ResumeFailedCleared {
			t.Fatal("a terminal run must change no row")
		}
	})
}
