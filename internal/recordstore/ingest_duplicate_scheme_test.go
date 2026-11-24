package recordstore

import (
	"context"
	"testing"
)

// TestIsBackstopKey covers the scheme read of the duplicate backstop. A key
// with no colon names no scheme, so it is never compared, also when the
// whole key equals a scheme name.
func TestIsBackstopKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		key  string
		want bool
	}{
		{"scm:acme/app:0123abc", true},
		{"cron:acme/app:2026-11-23T10:00:00Z", true},
		{"api:0193f5d2-0000-7000-8000-000000000000", true},
		{"manual:0193f5d2-0000-7000-8000-000000000000", false},
		{"upstream:0193f5d2-0000-7000-8000-000000000000:1", false},
		{"api", false},
		{"scm", false},
		{"cron", false},
		{"apiscm", false},
		{"", false},
		{":api", false},
	}
	for _, tc := range cases {
		if got := isBackstopKey(tc.key); got != tc.want {
			t.Errorf("isBackstopKey(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

// TestDuplicateBackstopSkipsAStoredKeyWithNoColon asserts that the
// candidate query applies the same scheme read as isBackstopKey. A stored
// live run whose key is the bare word api never makes a newer api: run of
// the same revision a duplicate.
func TestDuplicateBackstopSkipsAStoredKeyWithNoColon(t *testing.T) {
	t.Parallel()

	r := newBackstopTestRig(t)
	const revision = "rev-no-colon"
	stored := newTestUUIDv7String()
	if _, err := r.pool.Exec(context.Background(), `
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, scm_revision, status
		) VALUES ($1::uuid, now() - interval '1 minute', 'api', $2, $3, 'cell-a', $4, $5, $6, 'RUNNING')
	`, stored, testCluster, testNamespace, testTenant, testJobPath, revision); err != nil {
		t.Fatalf("seed run with a bare key: %v", err)
	}

	key := "api:" + newTestUUIDv7String()
	run := seedGrantedIntent(t, r.pool, key)
	r.start(t, run, key, revision, nil)

	r.wantStatus(t, "arriving api: run", run, StatusRunning)
	r.wantNoAction(t, "stored run with a bare key")
}
