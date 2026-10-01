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
// instance, seed fields, dead_instances, and incarnation for assertions.
type storedTenantPlacement struct {
	generation         int64
	cellInstance       string
	rehydratedInstance string
	pendingJobs        []string
	deadInstances      []TenantSyncDeadInstance
	incarnation        string
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
	)
	if err := db.QueryRow(`
		SELECT generation, cell_instance, rehydrated_instance, pending_jobs, dead_instances, incarnation
		FROM varroa.tenant_placements WHERE cluster = 'core' AND tenant = $1
	`, tenant).Scan(&generation, &cellInstance, &rehydrated, &pendingRaw, &deadRaw, &incarnation); err != nil {
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
	return storedTenantPlacement{
		generation: generation, cellInstance: cellInstance, rehydratedInstance: rehydrated,
		pendingJobs: pending, deadInstances: dead, incarnation: incarnation,
	}
}

// TestTenantSyncAnnounceStoresRow is the table test for kind "announce":
// it asserts a fresh placement's tenant, namespace, cell, and generation
// land exactly as announced.
func TestTenantSyncAnnounceStoresRow(t *testing.T) {
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
	out := handleTenantSync(conn, slog.Default(), pool, metrics, nil, nil, testGrace, nil, body)
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

// TestTenantSyncAnnounceDerivationIsIdempotent asserts a repeated announce
// keeps the derived entry as it is and adds no second one.
func TestTenantSyncAnnounceDerivationIsIdempotent(t *testing.T) {
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
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	older := announceMembers(1, []member{activeMember("cell-a", 0, "pod-a1")})
	sendTenantSync(t, pool, conn, older)
	sendTenantSync(t, pool, conn, announceMembers(1, []member{activeMember("cell-a", 0, "pod-a2")}))
	sendTenantSync(t, pool, conn, older)

	if got, ok := derivedEntry(t, db, "pod-a2"); ok {
		t.Fatalf("the stale announce listed the live pod dead: %+v", got)
	}
}

// TestTenantSyncAnnounceLowerGenerationDerivesNothing asserts an announce
// below the stored generation derives nothing from the member list it
// names.
func TestTenantSyncAnnounceLowerGenerationDerivesNothing(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, announceMembers(3, []member{activeMember("cell-b", 0, "pod-b1")}))
	sendTenantSync(t, pool, conn, announceMembers(2, []member{activeMember("cell-a", 0, "pod-a1")}))

	if got, ok := derivedEntry(t, db, "pod-b1"); ok {
		t.Fatalf("a lower generation listed the stored pod dead: %+v", got)
	}
}
