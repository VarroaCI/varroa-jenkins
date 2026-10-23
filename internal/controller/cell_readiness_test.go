package controller

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// ---------------------------------------------------------------------------
// task 5.1: cellConfigGateReason
// ---------------------------------------------------------------------------

func TestCellConfigGateReason(t *testing.T) {
	external := &v1alpha1.ControllerCondition{Type: ConditionArtifactStorage, Status: metav1.ConditionTrue, Reason: "External"}
	localOnly := &v1alpha1.ControllerCondition{Type: ConditionArtifactStorage, Status: metav1.ConditionFalse, Reason: "LocalOnly"}

	cases := []struct {
		name            string
		claimGate       string
		blobstoreURL    string
		artifactStorage *v1alpha1.ControllerCondition
		want            string
	}{
		{"record store disabled wins first", "0", "s3://bucket", external, v1alpha1.ReasonRecordStoreUnsupported},
		{"record store disabled beats a bad blob tier too", "0", "pgblob:", localOnly, v1alpha1.ReasonRecordStoreUnsupported},
		{"blob tier not s3", "1", "pgblob:", external, v1alpha1.ReasonBlobTierUnsupported},
		{"blob tier unset", "1", "", external, v1alpha1.ReasonBlobTierUnsupported},
		{"artifact storage not external", "1", "s3://bucket", localOnly, v1alpha1.ReasonArtifactStorageLocal},
		{"artifact storage condition absent", "1", "s3://bucket", nil, v1alpha1.ReasonArtifactStorageLocal},
		{"every gate passes", "1", "s3://bucket", external, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cellConfigGateReason(tc.claimGate, tc.blobstoreURL, tc.artifactStorage)
			if got != tc.want {
				t.Errorf("cellConfigGateReason() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCellConfigGateRunsAfterBundleResolution asserts task 5.1's ordering
// requirement: the gate reads the ArtifactStorage condition
// resolveBundleForController computes THIS tick, not a stale or absent one
// left over from before. Before this reconcile, cr carries no
// ArtifactStorage condition at all — if the gate ran before resolution (or
// read only pre-tick status), it would see an absent condition and report
// ArtifactStorageLocal, blocking provisioning forever even though every
// external-storage input is configured to succeed.
func TestCellConfigGateRunsAfterBundleResolution(t *testing.T) {
	client := newTestClientWithBundle()
	client.existingSecrets = map[string]map[string][]byte{
		"abase-test": {"access-key-id": []byte("a"), "secret-access-key": []byte("b")},
	}
	rec := newTestReconciler(client)
	rec.SetClaimGate("1")
	rec.SetArtifactSecretBase("abase")
	rec.SetBlobstoreURL("s3://my-bucket?region=us-east-1")

	cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	pinnedProfile(client, cr)
	seedClientCRDs(client)

	if len(cr.Status.Conditions) != 0 {
		t.Fatalf("test setup: cr must start with no conditions, got %+v", cr.Status.Conditions)
	}

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcileController: %v", err)
	}

	artifactCond := findCondition(cr.Status.Conditions, ConditionArtifactStorage)
	if artifactCond == nil || artifactCond.Status != metav1.ConditionTrue || artifactCond.Reason != "External" {
		t.Fatalf("ArtifactStorage condition = %+v, want True/External", artifactCond)
	}
	cellCond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cellCond == nil || cellCond.Reason == v1alpha1.ReasonArtifactStorageLocal {
		t.Fatalf("CellReady = %+v; the gate read a stale/absent ArtifactStorage condition instead of this tick's fresh one", cellCond)
	}
	if cellCond.Reason != v1alpha1.ReasonNotProvisioned {
		t.Errorf("CellReady.Reason = %q, want %q (every config gate passed; phase is still Provisioning)", cellCond.Reason, v1alpha1.ReasonNotProvisioned)
	}
	if len(client.stsSpecs) == 0 {
		t.Error("expected a StatefulSet to be built once every config gate passes")
	}
}

// TestCellConfigGateBlocksProvisioning asserts the "provision no pod" half
// of task 5.1/cell-controllers: a cell whose config gate fails gets no
// StatefulSet, even though the ServiceAccount/RBAC/bootstrap-token steps
// that precede the gate check still ran.
func TestCellConfigGateBlocksProvisioning(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	// claimGate defaults to "" (not "1"): RecordStoreUnsupported.

	cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcileController: %v", err)
	}
	if len(client.stsSpecs) != 0 {
		t.Errorf("expected no StatefulSet for a cell that fails a config gate, got %d", len(client.stsSpecs))
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonRecordStoreUnsupported {
		t.Fatalf("CellReady = %+v, want False/RecordStoreUnsupported", cond)
	}
}

// ---------------------------------------------------------------------------
// task 5.2: evaluateCellReadiness's five outcomes
// ---------------------------------------------------------------------------

func TestEvaluateCellReadinessOutcomes(t *testing.T) {
	external := v1alpha1.ControllerCondition{Type: ConditionArtifactStorage, Status: metav1.ConditionTrue, Reason: "External"}
	localOnly := v1alpha1.ControllerCondition{Type: ConditionArtifactStorage, Status: metav1.ConditionFalse, Reason: "LocalOnly"}

	cases := []struct {
		name         string
		claimGate    string
		blobstoreURL string
		artifactCond *v1alpha1.ControllerCondition
		phase        v1alpha1.ControllerPhase
		wantStatus   metav1.ConditionStatus
		wantReason   string
	}{
		{"record store disabled", "0", "s3://bucket", &external, v1alpha1.ControllerPhaseConnected, metav1.ConditionFalse, v1alpha1.ReasonRecordStoreUnsupported},
		{"blob tier not s3", "1", "pgblob:", &external, v1alpha1.ControllerPhaseConnected, metav1.ConditionFalse, v1alpha1.ReasonBlobTierUnsupported},
		{"artifact storage local", "1", "s3://bucket", &localOnly, v1alpha1.ControllerPhaseConnected, metav1.ConditionFalse, v1alpha1.ReasonArtifactStorageLocal},
		{"not yet connected", "1", "s3://bucket", &external, v1alpha1.ControllerPhaseProvisioning, metav1.ConditionFalse, v1alpha1.ReasonNotProvisioned},
		{"connected", "1", "s3://bucket", &external, v1alpha1.ControllerPhaseConnected, metav1.ConditionTrue, v1alpha1.ReasonConnected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient()
			rec := newTestReconciler(client)
			rec.SetClaimGate(tc.claimGate)
			rec.SetBlobstoreURL(tc.blobstoreURL)

			cr := &v1alpha1.Controller{
				ObjectMeta: metav1.ObjectMeta{Name: "cell1", Namespace: "ns"},
				Spec:       v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
				Status:     v1alpha1.ControllerStatus{Phase: tc.phase},
			}
			if tc.artifactCond != nil {
				cr.Status.Conditions = setCondition(cr.Status.Conditions, *tc.artifactCond)
			}

			rec.evaluateCellReadiness(context.Background(), cr, time.Now())

			cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
			if cond == nil {
				t.Fatal("expected a CellReady condition")
			}
			if cond.Status != tc.wantStatus || cond.Reason != tc.wantReason {
				t.Errorf("CellReady = %s/%s, want %s/%s", cond.Status, cond.Reason, tc.wantStatus, tc.wantReason)
			}
			// A configuration gate never provisions and never rolls.
			if tc.wantReason == v1alpha1.ReasonRecordStoreUnsupported ||
				tc.wantReason == v1alpha1.ReasonBlobTierUnsupported ||
				tc.wantReason == v1alpha1.ReasonArtifactStorageLocal {
				if client.deleteControllerPodCalls != 0 {
					t.Errorf("expected no pod delete for a configuration gate, got %d", client.deleteControllerPodCalls)
				}
			}
		})
	}
}

// TestEvaluateCellReadinessNonCellNoop asserts a non-cell Controller is
// left untouched.
func TestEvaluateCellReadinessNonCellNoop(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "ns"},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	}
	rec.evaluateCellReadiness(context.Background(), cr, time.Now())
	if findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady) != nil {
		t.Error("expected no CellReady condition on a non-cell controller")
	}
	if cr.Status.Cell != nil {
		t.Error("expected status.cell to stay nil on a non-cell controller")
	}
}

// TestEvaluateCellReadinessWarmPool asserts task 5.2's status.cell.warm and
// WarmPool derivation: true only when the cell carries no tenant and
// CellReady is True.
func TestEvaluateCellReadinessWarmPool(t *testing.T) {
	newReadyCell := func(tenants []string) (*Reconciler, *v1alpha1.Controller) {
		client := newTestClient()
		rec := newTestReconciler(client)
		rec.SetClaimGate("1")
		rec.SetBlobstoreURL("s3://bucket")
		cr := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "cell1", Namespace: "ns"},
			Spec:       v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
			Status: v1alpha1.ControllerStatus{
				Phase: v1alpha1.ControllerPhaseConnected,
				Conditions: []v1alpha1.ControllerCondition{
					{Type: ConditionArtifactStorage, Status: metav1.ConditionTrue, Reason: "External"},
				},
				Cell: &v1alpha1.CellStatus{Tenants: tenants},
			},
		}
		return rec, cr
	}

	t.Run("free cell is warm", func(t *testing.T) {
		rec, cr := newReadyCell(nil)
		rec.evaluateCellReadiness(context.Background(), cr, time.Now())
		if !cr.Status.Cell.Warm {
			t.Error("expected status.cell.warm = true")
		}
		warm := findCondition(cr.Status.Conditions, v1alpha1.ConditionWarmPool)
		if warm == nil || warm.Status != metav1.ConditionTrue {
			t.Errorf("WarmPool = %+v, want True", warm)
		}
	})

	t.Run("tenanted cell is not warm", func(t *testing.T) {
		rec, cr := newReadyCell([]string{"payments"})
		rec.evaluateCellReadiness(context.Background(), cr, time.Now())
		if cr.Status.Cell.Warm {
			t.Error("expected status.cell.warm = false")
		}
		warm := findCondition(cr.Status.Conditions, v1alpha1.ConditionWarmPool)
		if warm == nil || warm.Status != metav1.ConditionFalse {
			t.Errorf("WarmPool = %+v, want False", warm)
		}
	})
}

// ---------------------------------------------------------------------------
// tasks 5.3, 5.4: the roll and the evacuate signal (fake clock)
// ---------------------------------------------------------------------------

// readyGateCell returns a Reconciler and cell Controller whose three
// configuration gates all pass, so evaluateCellReadiness's roll branch is
// exercised. now is injected explicitly through every call, matching the
// package's existing fake-clock convention (e.g. shouldHibernate).
func readyGateCell(t *testing.T, grace time.Duration) (*Reconciler, *testClient, *v1alpha1.Controller) {
	t.Helper()
	client := newTestClient()
	rec := newTestReconciler(client)
	rec.SetClaimGate("1")
	rec.SetBlobstoreURL("s3://bucket")
	rec.cellsConfig = CellsConfig{UnreadyGracePeriod: grace}
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell1", Namespace: "ns"},
		Spec:       v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
		Status: v1alpha1.ControllerStatus{
			Phase: v1alpha1.ControllerPhaseProvisioning,
			Conditions: []v1alpha1.ControllerCondition{
				{Type: ConditionArtifactStorage, Status: metav1.ConditionTrue, Reason: "External"},
			},
		},
	}
	return rec, client, cr
}

// TestCellRollAndEvacuate is the fake-clock table asked for by tasks 5.3
// and 5.4: one pod delete when the grace period passes, no second delete
// within the same unready period, CellRolled with the fault reason
// preserved in Message, evacuate after twice the grace period, and a
// clean recovery clearing all three signals together.
func TestCellRollAndEvacuate(t *testing.T) {
	grace := 10 * time.Minute
	rec, client, cr := readyGateCell(t, grace)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Tick 1: first observation, age 0 — no delete.
	rec.evaluateCellReadiness(context.Background(), cr, t0)
	if client.deleteControllerPodCalls != 0 {
		t.Fatalf("tick1: expected no delete, got %d", client.deleteControllerPodCalls)
	}
	if cr.Status.Cell.UnreadySince == nil || !cr.Status.Cell.UnreadySince.Time.Equal(t0) {
		t.Fatalf("tick1: unreadySince = %v, want %v", cr.Status.Cell.UnreadySince, t0)
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cond.Reason != v1alpha1.ReasonNotProvisioned {
		t.Fatalf("tick1: Reason = %q, want NotProvisioned", cond.Reason)
	}

	// Tick 2: age passes the grace period — exactly one delete, Reason
	// becomes CellRolled, Message carries the original fault reason.
	t1 := t0.Add(grace + time.Minute)
	rec.evaluateCellReadiness(context.Background(), cr, t1)
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("tick2: expected exactly 1 delete, got %d", client.deleteControllerPodCalls)
	}
	cond = findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cond.Reason != v1alpha1.ReasonCellRolled {
		t.Fatalf("tick2: Reason = %q, want CellRolled", cond.Reason)
	}
	if cond.Message != v1alpha1.ReasonNotProvisioned {
		t.Fatalf("tick2: Message = %q, want the original fault reason %q", cond.Message, v1alpha1.ReasonNotProvisioned)
	}
	if cr.Status.Cell.RolledAt == nil || cr.Status.Cell.RolledAt.Time.Before(cr.Status.Cell.UnreadySince.Time) {
		t.Fatalf("tick2: rolledAt = %v, want >= unreadySince %v", cr.Status.Cell.RolledAt, cr.Status.Cell.UnreadySince)
	}

	// Tick 3: a second tick within the same unready period — no second
	// delete, Reason and Message stay as they were.
	t2 := t0.Add(grace + 2*time.Minute)
	rec.evaluateCellReadiness(context.Background(), cr, t2)
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("tick3: expected still exactly 1 delete, got %d", client.deleteControllerPodCalls)
	}
	cond = findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cond.Reason != v1alpha1.ReasonCellRolled || cond.Message != v1alpha1.ReasonNotProvisioned {
		t.Fatalf("tick3: condition = %+v, want CellRolled/%q unchanged", cond, v1alpha1.ReasonNotProvisioned)
	}
	if cr.Status.Cell.Evacuate {
		t.Fatal("tick3: expected evacuate still false before twice the grace period")
	}

	// Tick 4: twice the grace period — evacuate sets, still no second
	// delete.
	t3 := t0.Add(2*grace + time.Minute)
	rec.evaluateCellReadiness(context.Background(), cr, t3)
	if client.deleteControllerPodCalls != 1 {
		t.Fatalf("tick4: expected still exactly 1 delete, got %d", client.deleteControllerPodCalls)
	}
	if !cr.Status.Cell.Evacuate {
		t.Fatal("tick4: expected status.cell.evacuate = true after twice the grace period")
	}

	// Tick 5: recovery. Phase reaches Connected; evacuate, unreadySince,
	// and rolledAt all clear together.
	cr.Status.Phase = v1alpha1.ControllerPhaseConnected
	rec.evaluateCellReadiness(context.Background(), cr, t3.Add(time.Minute))
	if cr.Status.Cell.Evacuate || cr.Status.Cell.UnreadySince != nil || cr.Status.Cell.RolledAt != nil {
		t.Fatalf("tick5: expected evacuate/unreadySince/rolledAt all cleared, got cell=%+v", cr.Status.Cell)
	}
	cond = findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.ReasonConnected {
		t.Fatalf("tick5: condition = %+v, want True/Connected", cond)
	}
}

// TestCellUnreadyGraceFollowsThePool asserts the pool-aware grace period: a
// cell that belongs to a pool setting unreadyGracePeriod uses that value,
// and a cell with no pool, or one naming a deleted pool, keeps the chart
// value.
func TestCellUnreadyGraceFollowsThePool(t *testing.T) {
	chart := 5 * time.Minute
	pool := 20 * time.Minute

	tests := []struct {
		name      string
		poolLabel string
		seedPool  bool
		want      time.Duration
	}{
		{name: "no pool label uses the chart value", want: chart},
		{name: "a pool cell uses its pool", poolLabel: "big", seedPool: true, want: pool},
		{name: "a deleted pool falls back to the chart value", poolLabel: "gone", want: chart},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestClient()
			rec := newTestReconciler(client)
			rec.cellsConfig = CellsConfig{UnreadyGracePeriod: chart}
			if tt.seedPool {
				crdstore.MustSeed(client.store, &v1alpha1.CellPool{
					ObjectMeta: metav1.ObjectMeta{Name: "big"},
					Spec: v1alpha1.CellPoolSpec{
						ProfileRef:         "lts-2-555",
						UnreadyGracePeriod: &metav1.Duration{Duration: pool},
					},
				})
			}
			cr := &v1alpha1.Controller{
				ObjectMeta: metav1.ObjectMeta{Name: "cell1", Namespace: "ns"},
				Spec:       v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
			}
			if tt.poolLabel != "" {
				cr.Labels = map[string]string{cells.LabelCellPool: tt.poolLabel}
			}
			if got := rec.cellUnreadyGrace(context.Background(), cr); got != tt.want {
				t.Errorf("cellUnreadyGrace() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCellRollNeverFiresForConfigGates asserts design D6: a cell that fails
// any of the three configuration gates never rolls, no matter how long it
// stays unready, and its reason is never replaced.
func TestCellRollNeverFiresForConfigGates(t *testing.T) {
	reasons := []struct {
		name         string
		claimGate    string
		blobstoreURL string
		artifactTrue bool
		want         string
	}{
		{"record store disabled", "0", "s3://bucket", true, v1alpha1.ReasonRecordStoreUnsupported},
		{"blob tier wrong", "1", "pgblob:", true, v1alpha1.ReasonBlobTierUnsupported},
		{"artifact storage local", "1", "s3://bucket", false, v1alpha1.ReasonArtifactStorageLocal},
	}
	for _, tc := range reasons {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient()
			rec := newTestReconciler(client)
			rec.SetClaimGate(tc.claimGate)
			rec.SetBlobstoreURL(tc.blobstoreURL)
			rec.cellsConfig = CellsConfig{UnreadyGracePeriod: time.Minute}

			artifactStatus := metav1.ConditionFalse
			artifactReason := "LocalOnly"
			if tc.artifactTrue {
				artifactStatus = metav1.ConditionTrue
				artifactReason = "External"
			}
			cr := &v1alpha1.Controller{
				ObjectMeta: metav1.ObjectMeta{Name: "cell1", Namespace: "ns"},
				Spec:       v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
				Status: v1alpha1.ControllerStatus{
					Phase: v1alpha1.ControllerPhaseProvisioning,
					Conditions: []v1alpha1.ControllerCondition{
						{Type: ConditionArtifactStorage, Status: artifactStatus, Reason: artifactReason},
					},
				},
			}

			t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			rec.evaluateCellReadiness(context.Background(), cr, t0)
			// A full hour, far past any grace period this test would use.
			rec.evaluateCellReadiness(context.Background(), cr, t0.Add(time.Hour))

			if client.deleteControllerPodCalls != 0 {
				t.Errorf("expected no pod delete for %s, got %d", tc.name, client.deleteControllerPodCalls)
			}
			cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
			if cond.Reason != tc.want {
				t.Errorf("Reason = %q, want %q unchanged", cond.Reason, tc.want)
			}
			if cr.Status.Cell.UnreadySince != nil || cr.Status.Cell.RolledAt != nil || cr.Status.Cell.Evacuate {
				t.Errorf("expected unreadySince/rolledAt/evacuate untouched for a configuration gate, got cell=%+v", cr.Status.Cell)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// task 5.5: the cell.rolled activity event
// ---------------------------------------------------------------------------

// TestCellRolledActivityEvent asserts task 5.5: a roll publishes exactly
// one activity event of type cell.rolled through the reconciler's ordinary
// activityPublisher. It follows the embedded-NATS pattern in
// controller_activity_test.go, gated the same way.
func TestCellRolledActivityEvent(t *testing.T) {
	if os.Getenv("NATS_TEST") == "" && os.Getenv("CI") == "" {
		t.Skip("skipping NATS integration test; set NATS_TEST=1 to run")
	}

	opts := &server.Options{Port: -1, StoreDir: t.TempDir()}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	defer s.Shutdown()

	conn, err := bus.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	pub := activity.NewPublisher(bus.DefaultCluster, conn)
	client := newTestClient()
	rec := newTestReconciler(client)
	rec.SetActivityPublisher(pub)
	rec.SetClaimGate("1")
	rec.SetBlobstoreURL("s3://bucket")
	rec.cellsConfig = CellsConfig{UnreadyGracePeriod: time.Minute}

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell1", Namespace: "ns"},
		Spec:       v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
		Status: v1alpha1.ControllerStatus{
			Phase: v1alpha1.ControllerPhaseProvisioning,
			Conditions: []v1alpha1.ControllerCondition{
				{Type: ConditionArtifactStorage, Status: metav1.ConditionTrue, Reason: "External"},
			},
		},
	}

	activitySubj := bus.ActivitySubject(bus.DefaultCluster, "ns", "cell1")
	received := make(chan activity.Event, 10)
	sub, err := conn.SubscribeData(activitySubj, func(data []byte) {
		var e activity.Event
		if jsonErr := json.Unmarshal(data, &e); jsonErr == nil {
			received <- e
		}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	t0 := time.Now()
	rec.evaluateCellReadiness(context.Background(), cr, t0)
	rec.evaluateCellReadiness(context.Background(), cr, t0.Add(2*time.Minute)) // rolls

	deadline := time.After(3 * time.Second)
	var got []activity.Event
	for {
		select {
		case e := <-received:
			got = append(got, e)
			if len(got) >= 1 {
				// Give a brief window for a spurious duplicate to arrive.
				select {
				case e2 := <-received:
					got = append(got, e2)
				case <-time.After(200 * time.Millisecond):
				}
				goto done
			}
		case <-deadline:
			goto done
		}
	}
done:
	var rolled []activity.Event
	for _, e := range got {
		if e.Type == "cell.rolled" {
			rolled = append(rolled, e)
		}
	}
	if len(rolled) != 1 {
		t.Fatalf("expected exactly 1 cell.rolled event, got %d (%+v)", len(rolled), got)
	}
}

// ---------------------------------------------------------------------------
// task 5.3b: status.cell.appliedTenants
// ---------------------------------------------------------------------------

// TestCellAppliedTenantsAdvancesOnSuccessfulApply asserts task 5.3b: the
// list advances only when a drained CommandResult reports
// DesiredState.ItemsSuccess with an AppliedHash matching
// status.desiredStateHash, and shrinks once a drain removes a tenant.
func TestCellAppliedTenantsAdvancesOnSuccessfulApply(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconcilerWithTokenSigner(client)
	transport := &captureTransport{}
	rec.miteTransport = transport

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(time.Now())},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"},
			Cell:              &v1alpha1.CellSpec{Enabled: true},
		},
		Status: v1alpha1.ControllerStatus{
			Phase:            v1alpha1.ControllerPhaseConnected,
			DesiredStateHash: "hash-1",
			Conditions:       []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
			Cell:             &v1alpha1.CellStatus{Tenants: []string{"payments", "search"}},
		},
	}

	// A failed apply must not advance the list.
	transport.drainResults = []*mitev1.CommandResult{
		mkDSR(true, true, true, false, "", "", "", "items failed", "hash-1"),
	}
	_ = rec.reconcileController(context.Background(), cr)
	if len(cr.Status.Cell.AppliedTenants) != 0 {
		t.Fatalf("failed apply: appliedTenants = %v, want empty", cr.Status.Cell.AppliedTenants)
	}

	// A successful apply whose hash matches the tracked desired-state hash
	// advances the list to the current (sorted) tenant set.
	cr.Status.DesiredStateHash = "hash-2"
	transport.drainResults = []*mitev1.CommandResult{
		mkDSR(true, true, true, true, "", "", "", "", "hash-2"),
	}
	_ = rec.reconcileController(context.Background(), cr)
	want := []string{"payments", "search"}
	if !stringSlicesEqual(cr.Status.Cell.AppliedTenants, want) {
		t.Fatalf("appliedTenants = %v, want %v", cr.Status.Cell.AppliedTenants, want)
	}

	// A drain removes "search"; the next successful apply's hash changes
	// (a real cell's items pipeline would reflect the smaller tenant set),
	// and appliedTenants shrinks to match.
	cr.Status.Cell.Tenants = []string{"payments"}
	cr.Status.DesiredStateHash = "hash-3"
	transport.drainResults = []*mitev1.CommandResult{
		mkDSR(true, true, true, true, "", "", "", "", "hash-3"),
	}
	_ = rec.reconcileController(context.Background(), cr)
	want = []string{"payments"}
	if !stringSlicesEqual(cr.Status.Cell.AppliedTenants, want) {
		t.Fatalf("after drain: appliedTenants = %v, want %v", cr.Status.Cell.AppliedTenants, want)
	}
}

// ---------------------------------------------------------------------------
// task 5.6: the auto-hibernate gate-6/tenantWarnings hand-off
// ---------------------------------------------------------------------------

// TestHibernationIgnoredOnCell asserts task 5.6: an idle cell with
// spec.hibernation.enabled=true is never parked, and status.cell.
// tenantWarnings carries one HibernationIgnored entry per placed tenant.
func TestHibernationIgnoredOnCell(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconcilerWithTokenSigner(client)
	rec.miteTransport = &captureTransport{}

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(time.Now())},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"},
			Cell:              &v1alpha1.CellSpec{Enabled: true},
			Hibernation:       &v1alpha1.HibernationSpec{Enabled: true, GracePeriodMinutes: 1},
		},
		Status: v1alpha1.ControllerStatus{
			Phase:      v1alpha1.ControllerPhaseConnected,
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
			Cell:       &v1alpha1.CellStatus{Tenants: []string{"payments", "search"}},
		},
	}

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcileController: %v", err)
	}

	if cr.Status.Hibernated {
		t.Error("expected the cell to stay unparked")
	}
	warnings := cr.Status.Cell.TenantWarnings
	if len(warnings) != 2 {
		t.Fatalf("tenantWarnings = %+v, want 2 entries", warnings)
	}
	byTenant := map[string]v1alpha1.TenantWarning{}
	for _, w := range warnings {
		byTenant[w.Tenant] = w
	}
	for _, tenant := range []string{"payments", "search"} {
		w, ok := byTenant[tenant]
		if !ok {
			t.Errorf("missing tenantWarnings entry for %q", tenant)
			continue
		}
		if w.Reason != v1alpha1.TenantReasonHibernationIgnored {
			t.Errorf("tenant %q reason = %q, want %q", tenant, w.Reason, v1alpha1.TenantReasonHibernationIgnored)
		}
	}
}
