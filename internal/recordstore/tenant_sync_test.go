package recordstore

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats-server/v2/server"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// newTestSyncConn starts an embedded NATS server and connects, for the
// announce-triggered interrupted-run marking's cancel_run/retry_run
// publishes. A test that never reaches a live cell instance or an
// automatic-retry policy never dereferences it, but every sendTenantSync
// caller gets a real connection regardless.
func newTestSyncConn(t *testing.T) *bus.Conn {
	t.Helper()
	s, err := server.NewServer(&server.Options{Port: -1, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(s.Shutdown)

	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect to test bus: %v", err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// readTenantPlacement reads one tenant_placements row's generation, cell
// instance, seed fields, dead_instances, incarnation, and retired
// incarnations for assertions.
type storedTenantPlacement struct {
	generation         int64
	cellInstance       string
	rehydratedInstance string
	pendingJobs        []string
	deadInstances      []TenantSyncDeadInstance
	incarnation        string
	retired            []string
}

func readTenantPlacement(t *testing.T, db *sql.DB, tenant string) storedTenantPlacement {
	t.Helper()
	var (
		generation   int64
		cellInstance string
		rehydrated   string
		pendingRaw   []byte
		deadRaw      []byte
		incarnation  string
		retiredRaw   []byte
	)
	if err := db.QueryRow(`
		SELECT generation, cell_instance, rehydrated_instance, pending_jobs, dead_instances, incarnation,
		       retired_incarnations
		FROM varroa.tenant_placements WHERE cluster = 'core' AND tenant = $1
	`, tenant).Scan(&generation, &cellInstance, &rehydrated, &pendingRaw, &deadRaw, &incarnation,
		&retiredRaw); err != nil {
		t.Fatalf("read tenant_placements row: %v", err)
	}
	var pending []string
	if err := json.Unmarshal(pendingRaw, &pending); err != nil {
		t.Fatalf("decode pending_jobs: %v", err)
	}
	var dead []TenantSyncDeadInstance
	if err := json.Unmarshal(deadRaw, &dead); err != nil {
		t.Fatalf("decode dead_instances: %v", err)
	}
	var retired []string
	if err := json.Unmarshal(retiredRaw, &retired); err != nil {
		t.Fatalf("decode retired_incarnations: %v", err)
	}
	return storedTenantPlacement{
		generation: generation, cellInstance: cellInstance, rehydratedInstance: rehydrated,
		pendingJobs: pending, deadInstances: dead, incarnation: incarnation, retired: retired,
	}
}

// TestTenantSyncAnnounceStoresRow is the table test for kind "announce":
// it asserts a fresh placement's tenant, namespace, cell, and generation
// land exactly as announced.
func TestTenantSyncAnnounceStoresRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	resp := sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: "core", Tenant: "payments",
		Namespace: "cells-a", Cell: "cell-b", Generation: 5, CellInstance: "pod-1",
	})
	if resp.Error != "" || !resp.Ok {
		t.Fatalf("want ok, got %+v", resp)
	}

	var namespace, cell string
	if err := db.QueryRow(`
		SELECT namespace, cell FROM varroa.tenant_placements WHERE cluster = 'core' AND tenant = 'payments'
	`).Scan(&namespace, &cell); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if namespace != "cells-a" || cell != "cell-b" {
		t.Fatalf("want namespace cells-a and cell cell-b, got %q/%q", namespace, cell)
	}
	stored := readTenantPlacement(t, db, "payments")
	if stored.generation != 5 {
		t.Fatalf("want generation 5, got %d", stored.generation)
	}
}

// TestTenantSyncRecordsAnswersFromListRehydrationRecords is the table
// test for kind "records": it seeds one completed run and asserts the
// responder's reply matches ListRehydrationRecords.
func TestTenantSyncRecordsAnswersFromListRehydrationRecords(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)
	n := int64(1)
	seedRehydrationRun(t, db, "acme/app", &n, time.Now().UTC().Add(-time.Hour), "SUCCESS")

	resp := sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindRecords, Cluster: testCluster, Tenant: testTenant,
		JobPaths: []string{"acme/app"}, PerJob: 10,
	})
	if resp.Error != "" {
		t.Fatalf("want no error, got %q", resp.Error)
	}
	if len(resp.Jobs) != 1 || resp.Jobs[0].JobPath != "acme/app" {
		t.Fatalf("want one job acme/app, got %+v", resp.Jobs)
	}
	if len(resp.Jobs[0].Records) != 1 || resp.Jobs[0].Records[0].BuildNumber != 1 {
		t.Fatalf("want one record with build number 1, got %+v", resp.Jobs[0].Records)
	}
}

// TestTenantSyncUnknownKindIsError asserts a request naming a kind other
// than announce or records answers an error and writes nothing.
func TestTenantSyncUnknownKindIsError(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	resp := sendTenantSync(t, pool, conn, TenantSyncRequest{Kind: "purge-everything", Cluster: "core", Tenant: "acme"})
	if resp.Error == "" {
		t.Fatal("want a non-empty error for an unknown kind")
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.tenant_placements`).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("want no row written for an unknown kind, got %d", count)
	}
}

// TestTenantSyncAnnounceClearsSeedOnGenerationRise asserts a stored row
// above generation 0 loses its seed state when a later announce raises
// the generation, even though the request still names the old seed.
func TestTenantSyncAnnounceClearsSeedOnGenerationRise(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	// The first announce at generation 3 raises from the implicit
	// generation 0 of a row that does not exist yet, so it starts with
	// no seed. A second announce at the same generation and the same
	// cell instance reports the seed applying, which lands in the
	// stored row: this is the baseline the clearing assertion below
	// builds on.
	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: "core", Tenant: "acme",
		Generation: 3, CellInstance: "pod-a",
	})
	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: "core", Tenant: "acme",
		Generation: 3, CellInstance: "pod-a", RehydratedInstance: "pod-a",
	})
	stored := readTenantPlacement(t, db, "acme")
	if stored.rehydratedInstance != "pod-a" {
		t.Fatalf("seed setup: want rehydrated_instance pod-a, got %q", stored.rehydratedInstance)
	}

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: "core", Tenant: "acme",
		Generation: 4, CellInstance: "pod-b", RehydratedInstance: "pod-a", PendingJobs: []string{"acme/app"},
	})
	stored = readTenantPlacement(t, db, "acme")
	if stored.generation != 4 {
		t.Fatalf("want generation 4, got %d", stored.generation)
	}
	if stored.rehydratedInstance != "" {
		t.Fatalf("want rehydrated_instance cleared, got %q", stored.rehydratedInstance)
	}
	if len(stored.pendingJobs) != 0 {
		t.Fatalf("want pending_jobs cleared, got %v", stored.pendingJobs)
	}
}

// TestTenantSyncAnnounceClearsSeedOnCellInstanceChange asserts a stored
// row above generation 0 loses its seed state when a later announce at
// the same generation names a new cell instance, matching a pod roll
// with no generation change.
func TestTenantSyncAnnounceClearsSeedOnCellInstanceChange(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	// Establish a real seed at generation 2 on pod-a first, the same
	// two-announce baseline the generation-rise test uses.
	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: "core", Tenant: "acme",
		Generation: 2, CellInstance: "pod-a",
	})
	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: "core", Tenant: "acme",
		Generation: 2, CellInstance: "pod-a", RehydratedInstance: "pod-a",
	})
	if stored := readTenantPlacement(t, db, "acme"); stored.rehydratedInstance != "pod-a" {
		t.Fatalf("seed setup: want rehydrated_instance pod-a, got %q", stored.rehydratedInstance)
	}

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: "core", Tenant: "acme",
		Generation: 2, CellInstance: "pod-c", RehydratedInstance: "pod-a",
	})
	stored := readTenantPlacement(t, db, "acme")
	if stored.generation != 2 {
		t.Fatalf("want generation unchanged at 2, got %d", stored.generation)
	}
	if stored.cellInstance != "pod-c" {
		t.Fatalf("want cell_instance pod-c, got %q", stored.cellInstance)
	}
	if stored.rehydratedInstance != "" {
		t.Fatalf("want rehydrated_instance cleared on a cell-instance change, got %q", stored.rehydratedInstance)
	}
}

// TestTenantSyncAnnounceGenerationZeroSeedsImmediately asserts a
// generation-zero announce, matching a controller with no cell role,
// seeds itself from the announced cell instance on the very first
// announce.
func TestTenantSyncAnnounceGenerationZeroSeedsImmediately(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: "core", Tenant: "solo",
		Namespace: "jenkins", Cell: "solo", Generation: 0, CellInstance: "pod-solo",
	})
	stored := readTenantPlacement(t, db, "solo")
	if stored.generation != 0 {
		t.Fatalf("want generation 0, got %d", stored.generation)
	}
	if stored.rehydratedInstance != "pod-solo" {
		t.Fatalf("want rehydrated_instance pod-solo, got %q", stored.rehydratedInstance)
	}
	if len(stored.pendingJobs) != 0 {
		t.Fatalf("want no pending job, got %v", stored.pendingJobs)
	}
}

// TestTenantSyncAnnounceGenerationZeroRestartRepeatsSeed asserts that
// after a single-tenant controller's pod restarts, the very next
// generation-zero announce (still generation 0, a new cell instance)
// seeds itself again at once, with no held build. Without this branch a
// single-tenant controller would lose its seed state on every pod
// restart and the fence would hold its builds forever.
func TestTenantSyncAnnounceGenerationZeroRestartRepeatsSeed(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: "core", Tenant: "solo",
		Generation: 0, CellInstance: "pod-solo-1",
	})
	stored := readTenantPlacement(t, db, "solo")
	if stored.rehydratedInstance != "pod-solo-1" {
		t.Fatalf("first announce: want rehydrated_instance pod-solo-1, got %q", stored.rehydratedInstance)
	}

	// The pod restarts: Kubernetes assigns a new pod, so the controller
	// reconciler announces the same generation-zero row with a new
	// cell instance.
	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: "core", Tenant: "solo",
		Generation: 0, CellInstance: "pod-solo-2",
	})
	stored = readTenantPlacement(t, db, "solo")
	if stored.generation != 0 {
		t.Fatalf("want generation to stay 0, got %d", stored.generation)
	}
	if stored.cellInstance != "pod-solo-2" {
		t.Fatalf("want cell_instance pod-solo-2, got %q", stored.cellInstance)
	}
	// The seed follows the restart with no gap: rehydrated_instance
	// names the new pod at once. The fence's order 6 and order 8 (D7)
	// both read this row and both pass for the new pod, so a claim from
	// it is granted with no wait.
	if stored.rehydratedInstance != "pod-solo-2" {
		t.Fatalf("want rehydrated_instance to follow the restart to pod-solo-2, got %q", stored.rehydratedInstance)
	}
}

// TestTenantSyncAnnounceMergesAndTrimsDeadInstances asserts a later
// announce merges its dead_instances into the stored list rather than
// replacing it, keeps the entry a RUNNING run names, and trims the rest
// to ten.
func TestTenantSyncAnnounceMergesAndTrimsDeadInstances(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)
	insertStubRunsRow(t, db, newTestUUIDv7String())
	runID := newTestUUIDv7String()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs (run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status)
		VALUES ($1, now(), $2, $3, $4, 'cell-a', $5, $6, 'RUNNING')
	`, runID, "manual:running-"+runID, testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("insert running run: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.run_intents (intent_key, run_id, created_at, cluster, namespace, tenant, job_path, state, cell_instance)
		VALUES ($1, $2, now(), $3, $4, $5, $6, 'CONSUMED', 'pod-referenced')
	`, "manual:running-"+runID, runID, testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("insert run_intents: %v", err)
	}

	base := time.Now().UTC()
	first := TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Generation: 1, CellInstance: "pod-current",
		DeadInstances: []TenantSyncDeadInstance{{Instance: "pod-referenced", LastSeenAt: base.Add(-time.Hour)}},
	}
	sendTenantSync(t, pool, conn, first)

	entries := make([]TenantSyncDeadInstance, 0, 12)
	for i := 0; i < 12; i++ {
		entries = append(entries, TenantSyncDeadInstance{
			Instance: "pod-extra-" + string(rune('a'+i)), LastSeenAt: base.Add(-time.Duration(i) * time.Minute),
		})
	}
	second := TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Generation: 1, CellInstance: "pod-current", DeadInstances: entries,
	}
	sendTenantSync(t, pool, conn, second)

	stored := readTenantPlacement(t, db, testTenant)
	var referencedKept bool
	for _, d := range stored.deadInstances {
		if d.Instance == "pod-referenced" {
			referencedKept = true
		}
	}
	if !referencedKept {
		t.Fatalf("want pod-referenced kept because a RUNNING run names it, got %v", stored.deadInstances)
	}
	if len(stored.deadInstances) != 11 {
		t.Fatalf("want 1 kept entry plus 10 trimmed entries (11 total), got %d: %v", len(stored.deadInstances), stored.deadInstances)
	}
}

// TestDeadInstancesTrimKeepsPendingRefireEntry asserts the trim keeps
// the dead_instances entry a pending re-fire needs. A GRANTED intent
// with a null refired_at holds no runs row, so the first two arms of the
// keep set would miss it.
func TestDeadInstancesTrimKeepsPendingRefireEntry(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	base := time.Now().UTC()
	intentKey := "manual:" + newTestUUIDv7String()
	seedRefireIntent(t, db, intentKey, "pod-referenced", "")

	first := TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Generation: 1, CellInstance: "pod-current",
		DeadInstances: []TenantSyncDeadInstance{{Instance: "pod-referenced", LastSeenAt: base.Add(-time.Hour)}},
	}
	sendTenantSync(t, pool, conn, first)

	entries := make([]TenantSyncDeadInstance, 0, 12)
	for i := 0; i < 12; i++ {
		entries = append(entries, TenantSyncDeadInstance{
			Instance: "pod-extra-" + string(rune('a'+i)), LastSeenAt: base.Add(-time.Duration(i) * time.Minute),
		})
	}
	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Generation: 1, CellInstance: "pod-current", DeadInstances: entries,
	})

	stored := readTenantPlacement(t, db, testTenant)
	var kept bool
	for _, d := range stored.deadInstances {
		if d.Instance == "pod-referenced" {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("want pod-referenced kept for the pending re-fire, got %v", stored.deadInstances)
	}

	// An intent the recorder already re-fired stops protecting its
	// instance at once.
	if _, err := db.Exec(`UPDATE varroa.run_intents SET refired_at = now() WHERE intent_key = $1`,
		intentKey); err != nil {
		t.Fatalf("mark the intent refired: %v", err)
	}
	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Generation: 1, CellInstance: "pod-current", DeadInstances: entries,
	})
	stored = readTenantPlacement(t, db, testTenant)
	if len(stored.deadInstances) != maxDeadInstances {
		t.Fatalf("dead instances = %d, want %d once the re-fire marker is set",
			len(stored.deadInstances), maxDeadInstances)
	}
}

// TestMergeDeadInstancesConfirmation asserts the confirmation merge
// rule: the EARLIER non-nil ConfirmedGoneAt wins, a stored nil takes the
// incoming value, an incoming nil keeps the stored value, and LastSeenAt
// still takes the newer value in every case.
func TestMergeDeadInstancesConfirmation(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	early := base.Add(-2 * time.Minute)
	late := base.Add(-1 * time.Minute)
	oldSeen := base.Add(-10 * time.Minute)
	newSeen := base.Add(-5 * time.Minute)

	cases := []struct {
		name          string
		stored        TenantSyncDeadInstance
		incoming      TenantSyncDeadInstance
		wantConfirmed *time.Time
	}{
		{
			name:          "earlier incoming wins",
			stored:        TenantSyncDeadInstance{Instance: "pod-a", LastSeenAt: oldSeen, ConfirmedGoneAt: &late},
			incoming:      TenantSyncDeadInstance{Instance: "pod-a", LastSeenAt: newSeen, ConfirmedGoneAt: &early},
			wantConfirmed: &early,
		},
		{
			name:          "earlier stored wins",
			stored:        TenantSyncDeadInstance{Instance: "pod-a", LastSeenAt: oldSeen, ConfirmedGoneAt: &early},
			incoming:      TenantSyncDeadInstance{Instance: "pod-a", LastSeenAt: newSeen, ConfirmedGoneAt: &late},
			wantConfirmed: &early,
		},
		{
			name:          "stored nil takes the incoming value",
			stored:        TenantSyncDeadInstance{Instance: "pod-a", LastSeenAt: oldSeen},
			incoming:      TenantSyncDeadInstance{Instance: "pod-a", LastSeenAt: newSeen, ConfirmedGoneAt: &late},
			wantConfirmed: &late,
		},
		{
			name:          "incoming nil keeps the stored value",
			stored:        TenantSyncDeadInstance{Instance: "pod-a", LastSeenAt: oldSeen, ConfirmedGoneAt: &early},
			incoming:      TenantSyncDeadInstance{Instance: "pod-a", LastSeenAt: newSeen},
			wantConfirmed: &early,
		},
		{
			name:          "both nil stay nil",
			stored:        TenantSyncDeadInstance{Instance: "pod-a", LastSeenAt: oldSeen},
			incoming:      TenantSyncDeadInstance{Instance: "pod-a", LastSeenAt: newSeen},
			wantConfirmed: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged := mergeDeadInstances([]TenantSyncDeadInstance{tc.stored}, []TenantSyncDeadInstance{tc.incoming})
			if len(merged) != 1 {
				t.Fatalf("merged = %d entries, want 1", len(merged))
			}
			got := merged[0]
			if !got.LastSeenAt.Equal(newSeen) {
				t.Fatalf("LastSeenAt = %v, want the newer value %v", got.LastSeenAt, newSeen)
			}
			switch {
			case tc.wantConfirmed == nil && got.ConfirmedGoneAt != nil:
				t.Fatalf("ConfirmedGoneAt = %v, want nil", got.ConfirmedGoneAt)
			case tc.wantConfirmed != nil && got.ConfirmedGoneAt == nil:
				t.Fatalf("ConfirmedGoneAt = nil, want %v", tc.wantConfirmed)
			case tc.wantConfirmed != nil && !got.ConfirmedGoneAt.Equal(*tc.wantConfirmed):
				t.Fatalf("ConfirmedGoneAt = %v, want %v", got.ConfirmedGoneAt, tc.wantConfirmed)
			}
		})
	}
}

// sendTenantSync marshals req, runs it through handleTenantSync, and
// decodes the reply. conn feeds the announce-triggered interrupted-run
// marking's action publishes.
func sendTenantSync(t *testing.T, pool *pgxpool.Pool, conn *bus.Conn, req TenantSyncRequest) TenantSyncResponse {
	t.Helper()
	return sendTenantSyncWithMetrics(t, pool, conn, nil, req)
}

// sendTenantSyncWithMetrics is sendTenantSync with a caller-supplied
// InterruptedMetrics, for a test that asserts on a counter the announce
// hook's own marking increments.
func sendTenantSyncWithMetrics(t *testing.T, pool *pgxpool.Pool, conn *bus.Conn,
	metrics *InterruptedMetrics, req TenantSyncRequest) TenantSyncResponse {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out := handleTenantSync(conn, slog.Default(), pool, metrics, nil, nil, nil, testGrace, nil, body)
	var resp TenantSyncResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return resp
}

// TestTenantSyncAnnounceStoresAgentNamespace asserts an announce stores
// its agent namespace, and a later announce replaces the stored value
// with the announced one.
func TestTenantSyncAnnounceStoresAgentNamespace(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	readAgentNamespace := func() string {
		t.Helper()
		var ns string
		if err := db.QueryRow(`
			SELECT agent_namespace FROM varroa.tenant_placements WHERE cluster = 'core' AND tenant = 'payments'
		`).Scan(&ns); err != nil {
			t.Fatalf("read agent_namespace: %v", err)
		}
		return ns
	}

	for _, want := range []string{"payments-agents", "payments-agents-2"} {
		resp := sendTenantSync(t, pool, conn, TenantSyncRequest{
			Kind: TenantSyncKindAnnounce, Cluster: "core", Tenant: "payments",
			Namespace: "cells-a", Cell: "cell-b", Generation: 5, CellInstance: "pod-1",
			AgentNamespace: want,
		})
		if resp.Error != "" || !resp.Ok {
			t.Fatalf("want ok, got %+v", resp)
		}
		if got := readAgentNamespace(); got != want {
			t.Fatalf("agent_namespace = %q, want %q", got, want)
		}
	}
}

// derivedEntry answers the stored dead_instances entry for instance.
func derivedEntry(t *testing.T, db *sql.DB, instance string) (TenantSyncDeadInstance, bool) {
	t.Helper()
	for _, d := range readTenantPlacement(t, db, testTenant).deadInstances {
		if d.Instance == instance {
			return d, true
		}
	}
	return TenantSyncDeadInstance{}, false
}

// TestTenantSyncAnnounceDerivesARolledMemberInstance asserts a member whose
// cell reports a new instance leaves its old instance in dead_instances,
// tagged rolled and with no confirmation, while the operator reports none.
func TestTenantSyncAnnounceDerivesARolledMemberInstance(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, announceMembers(1, []member{
		activeMember("cell-a", 0, "pod-a1"), activeMember("cell-b", 1, "pod-b1"),
	}))
	sendTenantSync(t, pool, conn, announceMembers(1, []member{
		activeMember("cell-a", 0, "pod-a1"), activeMember("cell-b", 1, "pod-b2"),
	}))

	got, ok := derivedEntry(t, db, "pod-b1")
	if !ok || got.Derived != DeadInstanceDerivedRolled || got.ConfirmedGoneAt != nil {
		t.Fatalf("want an unconfirmed rolled entry for pod-b1, got %+v (found %v)", got, ok)
	}
	if _, ok := derivedEntry(t, db, "pod-a1"); ok {
		t.Fatal("the unchanged member pod-a1 was listed dead")
	}
}

// TestTenantSyncAnnounceDerivesADroppedMemberInstance asserts a member
// whose cell the request drops leaves its instance in dead_instances,
// tagged dropped.
func TestTenantSyncAnnounceDerivesADroppedMemberInstance(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, announceMembers(1, []member{
		activeMember("cell-a", 0, "pod-a1"), activeMember("cell-b", 1, "pod-b1"),
	}))
	sendTenantSync(t, pool, conn, announceMembers(2, []member{activeMember("cell-a", 0, "pod-a1")}))

	got, ok := derivedEntry(t, db, "pod-b1")
	if !ok || got.Derived != DeadInstanceDerivedDropped || got.ConfirmedGoneAt != nil {
		t.Fatalf("want an unconfirmed dropped entry for pod-b1, got %+v (found %v)", got, ok)
	}
}

// TestTenantSyncAnnounceUnconfirmedLeftMemberKeepsAReleasingLock asserts
// an operator entry with no confirmation keeps a releasing lock, even past
// the moved window. The operator sends such an entry for a member that
// leaves a live cell. A later entry with a confirmation releases the lock.
func TestTenantSyncAnnounceUnconfirmedLeftMemberKeepsAReleasingLock(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)
	now := time.Now().UTC()

	sendTenantSync(t, pool, conn, announceMembers(1, []member{
		activeMember("cell-a", 0, "pod-a1"), activeMember("cell-b", 1, "pod-b1"),
	}))
	insertLockRowForTenant(t, db, "acme/deploy-prod", "run-1", testTenant, "acme/release",
		FleetLockStateReleasing, now.Add(-10*time.Minute), now.Add(5*time.Minute))
	if _, err := db.Exec(`UPDATE varroa.fleet_locks SET cell_instance = 'pod-b1' WHERE name = $1 AND run_id = $2`,
		"acme/deploy-prod", "run-1"); err != nil {
		t.Fatalf("set cell_instance: %v", err)
	}

	left := announceMembers(2, []member{activeMember("cell-a", 0, "pod-a1")})
	left.DeadInstances = []TenantSyncDeadInstance{{Instance: "pod-b1", LastSeenAt: now.Add(-6 * time.Minute)}}
	if resp := sendTenantSync(t, pool, conn, left); !resp.Ok {
		t.Fatalf("announce: %+v", resp)
	}
	if _, _, found := fleetLockRowState(t, db, "run-1"); !found {
		t.Fatal("an unconfirmed entry for a member that left its cell released the lock")
	}

	confirmed := now.Add(-time.Minute)
	left.DeadInstances[0].ConfirmedGoneAt = &confirmed
	if resp := sendTenantSync(t, pool, conn, left); !resp.Ok {
		t.Fatalf("announce: %+v", resp)
	}
	if _, _, found := fleetLockRowState(t, db, "run-1"); found {
		t.Fatal("a confirmed entry past the grace kept the lock")
	}
}

// TestTenantSyncAnnounceDerivationIsIdempotent asserts a repeated announce
// keeps the derived entry as it is and adds no second one.
func TestTenantSyncAnnounceDerivationIsIdempotent(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, announceMembers(1, []member{activeMember("cell-a", 0, "pod-a1")}))
	rolled := announceMembers(1, []member{activeMember("cell-a", 0, "pod-a2")})
	sendTenantSync(t, pool, conn, rolled)
	first := readTenantPlacement(t, db, testTenant).deadInstances

	sendTenantSync(t, pool, conn, rolled)
	second := readTenantPlacement(t, db, testTenant).deadInstances
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("want one entry after each announce, got %+v and %+v", first, second)
	}
	if !first[0].LastSeenAt.Equal(second[0].LastSeenAt) || second[0].Derived != DeadInstanceDerivedRolled {
		t.Fatalf("the repeat changed the entry: %+v then %+v", first[0], second[0])
	}
}

// TestTenantSyncAnnounceStaleAnnounceDerivesNothing asserts an announce
// that names an instance the row already lists dead derives nothing. A
// stale announce from an old shard owner therefore never lists the live
// pod dead.
func TestTenantSyncAnnounceStaleAnnounceDerivesNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	older := announceMembers(1, []member{activeMember("cell-a", 0, "pod-a1")})
	sendTenantSync(t, pool, conn, older)
	sendTenantSync(t, pool, conn, announceMembers(1, []member{activeMember("cell-a", 0, "pod-a2")}))
	assertStaleReply(t, sendTenantSync(t, pool, conn, older))

	if got, ok := derivedEntry(t, db, "pod-a2"); ok {
		t.Fatalf("the stale announce listed the live pod dead: %+v", got)
	}
}

// TestTenantSyncAnnounceLowerGenerationDerivesNothing asserts an announce
// below the stored generation derives nothing from the member list it
// names.
func TestTenantSyncAnnounceLowerGenerationDerivesNothing(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, announceMembers(3, []member{activeMember("cell-b", 0, "pod-b1")}))
	assertStaleReply(t, sendTenantSync(t, pool, conn, announceMembers(2, []member{activeMember("cell-a", 0, "pod-a1")})))

	if got, ok := derivedEntry(t, db, "pod-b1"); ok {
		t.Fatalf("a lower generation listed the stored pod dead: %+v", got)
	}
}

// placementSnapshot reads every column an announce writes, except
// announced_at, as one comparable string.
func placementSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var snapshot string
	if err := db.QueryRow(`
		SELECT concat_ws('|', namespace, cell, generation, cell_instance, policy::text,
		       rehydrated_instance, pending_jobs::text, cell_alive, cell_alive_at, cell_last_seen_at,
		       dead_instances::text, members::text, job_homes::text, incarnation, agent_namespace)
		FROM varroa.tenant_placements WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant).Scan(&snapshot); err != nil {
		t.Fatalf("read the placement snapshot: %v", err)
	}
	return snapshot
}

// assertStaleReply asserts resp is the refusal of a stale announce.
func assertStaleReply(t *testing.T, resp TenantSyncResponse) {
	t.Helper()
	if !resp.Stale || resp.Ok || resp.Error != "" {
		t.Fatalf("want a stale refusal, got %+v", resp)
	}
}

// TestTenantSyncAnnounceStaleAnnounceKeepsTheStoredRow asserts an old
// shard owner's announce, which names a replaced pod, changes no column.
func TestTenantSyncAnnounceStaleAnnounceKeepsTheStoredRow(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)
	seedJobHomePaths(t, db, "acme/api", "acme/web")

	older := announceMembers(1, []member{activeMember("cell-a", 0, "pod-a1"), activeMember("cell-b", 1, "pod-b1")})
	sendTenantSync(t, pool, conn, older)
	sendTenantSync(t, pool, conn, announceMembers(1, []member{
		activeMember("cell-a", 0, "pod-a2"), activeMember("cell-b", 1, "pod-b1"),
	}))
	before := placementSnapshot(t, db)

	older.AgentNamespace = "stale-agents"
	assertStaleReply(t, sendTenantSync(t, pool, conn, older))
	if after := placementSnapshot(t, db); after != before {
		t.Fatalf("the stale announce changed the row:\nbefore %s\nafter  %s", before, after)
	}
}

// TestTenantSyncAnnounceStaleAnnounceKeepsTheSeed asserts the seed of the
// live pod survives a stale announce and the fresh announce after it.
func TestTenantSyncAnnounceStaleAnnounceKeepsTheSeed(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, announceMembers(1, []member{activeMember("cell-a", 0, "pod-a1")}))
	fresh := announceMembers(1, []member{activeMember("cell-a", 0, "pod-a2")})
	fresh.RehydratedInstance = "pod-a2"
	confirmed := time.Now()
	fresh.DeadInstances = []TenantSyncDeadInstance{{Instance: "pod-a1", LastSeenAt: confirmed, ConfirmedGoneAt: &confirmed}}
	sendTenantSync(t, pool, conn, fresh)
	sendTenantSync(t, pool, conn, fresh)
	if got := readTenantPlacement(t, db, testTenant).rehydratedInstance; got != "pod-a2" {
		t.Fatalf("seed setup: rehydrated_instance = %q, want pod-a2", got)
	}

	stale := announceMembers(1, []member{activeMember("cell-a", 0, "pod-a1")})
	stale.RehydratedInstance = "pod-a1"
	assertStaleReply(t, sendTenantSync(t, pool, conn, stale))
	if resp := sendTenantSync(t, pool, conn, fresh); !resp.Ok {
		t.Fatalf("the fresh announce after a stale one: %+v", resp)
	}

	stored := readTenantPlacement(t, db, testTenant)
	if stored.cellInstance != "pod-a2" || stored.rehydratedInstance != "pod-a2" {
		t.Fatalf("want pod-a2 with its seed, got cell_instance %q, rehydrated_instance %q",
			stored.cellInstance, stored.rehydratedInstance)
	}
	if members := readStoredMembers(t, db); len(members) != 1 || members[0].RehydratedInstance != "pod-a2" {
		t.Fatalf("the member seed did not survive: %+v", members)
	}
}

// TestTenantSyncAnnounceLowerGenerationKeepsTheCell asserts an announce
// below the stored generation keeps the stored cell and members.
func TestTenantSyncAnnounceLowerGenerationKeepsTheCell(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, announceMembers(3, []member{activeMember("cell-b", 0, "pod-b1")}))
	before := placementSnapshot(t, db)

	assertStaleReply(t, sendTenantSync(t, pool, conn, announceMembers(2, []member{activeMember("cell-a", 0, "pod-a1")})))
	if after := placementSnapshot(t, db); after != before {
		t.Fatalf("the lower generation changed the row:\nbefore %s\nafter  %s", before, after)
	}
}

// TestTenantSyncAnnounceTakeOverBelowTheStoredGenerationIsAccepted asserts
// a new incarnation takes the row over at a lower generation, even when it
// names an instance the old tenant listed as confirmed gone.
func TestTenantSyncAnnounceTakeOverBelowTheStoredGenerationIsAccepted(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	old := announceMembers(3, []member{activeMember("cell-a", 0, "pod-a2")})
	old.Incarnation = "uid-old"
	confirmed := time.Now()
	old.DeadInstances = []TenantSyncDeadInstance{{Instance: "pod-a1", LastSeenAt: confirmed, ConfirmedGoneAt: &confirmed}}
	sendTenantSync(t, pool, conn, old)

	next := announceMembers(1, []member{activeMember("cell-a", 0, "pod-a1")})
	next.Incarnation = "uid-new"
	if resp := sendTenantSync(t, pool, conn, next); !resp.Ok || resp.Stale {
		t.Fatalf("the take-over was refused: %+v", resp)
	}
	stored := readTenantPlacement(t, db, testTenant)
	if stored.generation != 1 || stored.cellInstance != "pod-a1" || stored.incarnation != "uid-new" {
		t.Fatalf("the take-over did not land: %+v", stored)
	}
}

// TestTenantSyncAnnounceReturnToALeftPodIsAccepted asserts a tenant that
// returns to a live pod it left is not refused. The recorder lists that
// pod as dropped, and a dropped entry proves no replacement.
func TestTenantSyncAnnounceReturnToALeftPodIsAccepted(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, announceMembers(1, []member{
		activeMember("cell-a", 0, "pod-a1"), activeMember("cell-b", 1, "pod-b1"),
	}))
	sendTenantSync(t, pool, conn, announceMembers(2, []member{activeMember("cell-a", 0, "pod-a1")}))
	if _, ok := derivedEntry(t, db, "pod-b1"); !ok {
		t.Fatal("setup: pod-b1 is not listed dropped")
	}

	back := announceMembers(3, []member{activeMember("cell-a", 0, "pod-a1"), activeMember("cell-b", 1, "pod-b1")})
	if resp := sendTenantSync(t, pool, conn, back); !resp.Ok || resp.Stale {
		t.Fatalf("the return was refused: %+v", resp)
	}
	if members := readStoredMembers(t, db); len(members) != 2 {
		t.Fatalf("want two stored members after the return, got %+v", members)
	}
	if d, ok := derivedEntry(t, db, "pod-b1"); ok {
		t.Fatalf("the returned live pod stays in dead_instances: %+v", d)
	}
}

// TestTenantSyncAnnounceReturnThenRollDerivesRolled asserts a returned pod
// that later rolls is derived again, tagged rolled and with a fresh
// last_seen_at, because the return removed its old dropped entry.
func TestTenantSyncAnnounceReturnThenRollDerivesRolled(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	both := []member{activeMember("cell-a", 0, "pod-a1"), activeMember("cell-b", 1, "pod-b1")}
	sendTenantSync(t, pool, conn, announceMembers(1, both))
	sendTenantSync(t, pool, conn, announceMembers(2, []member{activeMember("cell-a", 0, "pod-a1")}))
	dropped, ok := derivedEntry(t, db, "pod-b1")
	if !ok {
		t.Fatal("setup: pod-b1 is not listed dropped")
	}
	sendTenantSync(t, pool, conn, announceMembers(3, both))

	rolled := announceMembers(3, []member{activeMember("cell-a", 0, "pod-a1"), activeMember("cell-b", 1, "pod-b2")})
	if resp := sendTenantSync(t, pool, conn, rolled); !resp.Ok {
		t.Fatalf("the roll was refused: %+v", resp)
	}
	got, ok := derivedEntry(t, db, "pod-b1")
	if !ok || got.Derived != DeadInstanceDerivedRolled {
		t.Fatalf("want pod-b1 listed rolled after the roll, got %+v (listed=%v)", got, ok)
	}
	if !got.LastSeenAt.After(dropped.LastSeenAt) {
		t.Fatalf("want a fresh last_seen_at, got %v (dropped at %v)", got.LastSeenAt, dropped.LastSeenAt)
	}
}

// TestTenantSyncAnnounceNamedPodKeepsItsLock asserts an accepted
// announcement that names a pod removes that pod's unconfirmed rolled
// entry before the fleet-lock release reads the row. A slot-1 owner is
// not the scalar cell_instance, so without the removal the rolled entry
// would release the lock of a live pod.
func TestTenantSyncAnnounceNamedPodKeepsItsLock(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	live := []member{activeMember("cell-a", 0, "pod-a1"), activeMember("cell-b", 1, "pod-b2")}
	sendTenantSync(t, pool, conn, announceMembers(1, live))
	if _, err := db.Exec(`
		UPDATE varroa.tenant_placements
		SET dead_instances = jsonb_build_array(jsonb_build_object(
			'instance', 'pod-b2', 'last_seen_at', to_jsonb(now() - interval '10 minutes'), 'derived', 'rolled'))
		WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant); err != nil {
		t.Fatalf("seed the rolled entry: %v", err)
	}
	now := time.Now()
	insertLockRowForTenant(t, db, testTenant+"/deploy", "run-b2", testTenant, testTenant+"/release",
		FleetLockStateReleasing, now.Add(-10*time.Minute), now.Add(5*time.Minute))
	if _, err := db.Exec(`UPDATE varroa.fleet_locks SET cell_instance = 'pod-b2' WHERE run_id = 'run-b2'`); err != nil {
		t.Fatalf("set cell_instance: %v", err)
	}

	if resp := sendTenantSync(t, pool, conn, announceMembers(1, live)); !resp.Ok {
		t.Fatalf("the announcement was refused: %+v", resp)
	}
	if d, ok := derivedEntry(t, db, "pod-b2"); ok {
		t.Fatalf("the named live pod stays in dead_instances: %+v", d)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.fleet_locks WHERE run_id = 'run-b2'`).Scan(&n); err != nil {
		t.Fatalf("count locks: %v", err)
	}
	if n != 1 {
		t.Fatalf("the live pod's lock was released: %d rows left", n)
	}
}

// TestTenantSyncAnnounceStaleAnnounceMarksNoRun asserts a stale announce
// runs no interrupted-run marking against the live pod's run.
func TestTenantSyncAnnounceStaleAnnounceMarksNoRun(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)
	runID := seedStaleRunOn(t, db, "cell-a", "pod-a2")

	sendTenantSync(t, pool, conn, announceMembers(1, []member{activeMember("cell-a", 0, "pod-a1")}))
	fresh := announceMembers(1, []member{activeMember("cell-a", 0, "pod-a2")})
	confirmed := time.Now().Add(-time.Hour)
	fresh.DeadInstances = []TenantSyncDeadInstance{{Instance: "pod-a1", LastSeenAt: confirmed, ConfirmedGoneAt: &confirmed}}
	sendTenantSync(t, pool, conn, fresh)

	stale := announceMembers(1, []member{activeMember("cell-a", 0, "pod-a1")})
	resp := sendTenantSync(t, pool, conn, stale)
	assertStaleReply(t, resp)
	if resp.InterruptedMarked != 0 {
		t.Fatalf("the stale announce marked %d runs", resp.InterruptedMarked)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status); err != nil {
		t.Fatalf("read the run: %v", err)
	}
	if status != StatusRunning {
		t.Fatalf("run status = %s, want RUNNING", status)
	}
}

// TestTenantSyncAnnounceReleasesLockOfRunItMarks asserts an announce that
// marks a run cell-lost also moves that run's held lock to releasing and
// releases it in the same announce, because the old pod is confirmed gone
// past the grace.
func TestTenantSyncAnnounceReleasesLockOfRunItMarks(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)
	runID := seedStaleRunOn(t, db, "cell-a", "pod-a1")

	sendTenantSync(t, pool, conn, announceMembers(1, []member{activeMember("cell-a", 0, "pod-a1")}))
	now := time.Now()
	insertLockRowForTenant(t, db, testTenant+"/deploy", runID, testTenant, testJobPath,
		FleetLockStateHeld, now.Add(-10*time.Minute), now.Add(5*time.Minute))
	if _, err := db.Exec(`UPDATE varroa.fleet_locks SET cell_instance = 'pod-a1' WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("set cell_instance: %v", err)
	}

	fresh := announceMembers(1, []member{activeMember("cell-a", 0, "pod-a2")})
	confirmed := now.Add(-time.Hour)
	fresh.DeadInstances = []TenantSyncDeadInstance{{Instance: "pod-a1", LastSeenAt: confirmed, ConfirmedGoneAt: &confirmed}}
	resp := sendTenantSync(t, pool, conn, fresh)
	if !resp.Ok || resp.InterruptedMarked != 1 {
		t.Fatalf("announce reply = %+v, want ok with one marked run", resp)
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.fleet_locks WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("count locks: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d lock rows left, want the marked run's lock released in the same announce", n)
	}
}

// TestWriteTenantPlacement_StoresAndClearsNotificationWebhook asserts an
// announce stores the completion webhook in notification_webhook, and that a
// later announce with no webhook clears it. The policy column never holds it.
func TestWriteTenantPlacement_StoresAndClearsNotificationWebhook(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	ctx := t.Context()
	write := func(hook *TenantSyncWebhook) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		req := TenantSyncRequest{
			Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
			Namespace: testNamespace, Cell: "cell-a", CellInstance: "pod-a",
			NotificationWebhook: hook,
		}
		if err := writeTenantPlacement(ctx, tx, req, 1, "", nil, nil, nil, nil, placementIdentity{}); err != nil {
			t.Fatalf("write tenant placement: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	read := func() (bool, string, string) {
		t.Helper()
		var set bool
		var url, policy string
		if err := pool.QueryRow(ctx, `
			SELECT notification_webhook IS NOT NULL,
			       COALESCE(notification_webhook->>'url', ''),
			       policy::text
			FROM varroa.tenant_placements WHERE cluster = $1 AND tenant = $2
		`, testCluster, testTenant).Scan(&set, &url, &policy); err != nil {
			t.Fatalf("read tenant placement: %v", err)
		}
		return set, url, policy
	}

	write(&TenantSyncWebhook{URL: "https://hooks.example.test/varroa", Key: "whsec-test"})
	set, url, policy := read()
	if !set || url != "https://hooks.example.test/varroa" {
		t.Fatalf("stored webhook = (set %v, url %q), want the announced URL", set, url)
	}
	if policy != "{}" {
		t.Fatalf("policy = %s, want the empty policy with no webhook", policy)
	}

	write(nil)
	if set, _, _ := read(); set {
		t.Fatalf("an announce with no webhook left notification_webhook set")
	}
}
