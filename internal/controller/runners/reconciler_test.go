package runners

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// stubSigner is a fake OperatorTokenSigner for tests that never touch a
// real RSA key.
type stubSigner struct {
	err error
}

func (s *stubSigner) GenerateOperatorJenkinsToken(_, _ string, _ time.Duration) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return "stub-token", nil
}

func seedCell(t *testing.T, backend crdstore.Backend, name, namespace string) {
	t.Helper()
	cell := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: "abcdef12-0000-0000-0000-000000000000"},
	}
	if err := crdstore.Create[v1alpha1.Controller](context.Background(), backend, cell); err != nil {
		t.Fatalf("seed cell %s/%s: %v", namespace, name, err)
	}
}

// newTestReconciler builds a Reconciler with a stub signer over a fresh
// Fake backend, with liveness probing disabled by default (no
// ServiceURL/HTTPClient override needed) since most tests below do not
// exercise the probe. Secrets defaults to sshSecrets(), which answers
// sshRunner's fixture credential, so a test that does not care about tick
// step 7 still delivers an ssh runner the way it did before step 7 existed.
func newTestReconciler(backend crdstore.Backend) *Reconciler {
	return &Reconciler{
		Backend:     backend,
		Secrets:     sshSecrets(),
		HTTPClient:  http.DefaultClient,
		TokenSigner: &stubSigner{},
		Logger:      testLogger(),
	}
}

// runnerWithCredentialRef returns an ssh Runner on the "payments" team
// naming credentialRef, overriding sshRunner's fixture "ssh-key" reference
// for a test that needs a specific credential fault.
func runnerWithCredentialRef(name, credentialRef string) *v1alpha1.Runner {
	r := sshRunner(name, "payments")
	r.Spec.CredentialRef = credentialRef
	return r
}

// malformedSSHSecrets answers sshRunner's fixture credentialRef with a
// Secret missing the required privateKey key, for a CredentialMalformed
// case.
func malformedSSHSecrets() *fakeSecretReader {
	return &fakeSecretReader{
		namespace: "agents",
		records: []map[string][]byte{
			secretRecord("ssh-key", map[string]string{"username": "jenkins"}),
		},
	}
}

func getRunner(t *testing.T, backend crdstore.Backend, name string) *v1alpha1.Runner {
	t.Helper()
	r, err := crdstore.Get[v1alpha1.Runner](context.Background(), backend, name, "")
	if err != nil {
		t.Fatalf("get runner %s: %v", name, err)
	}
	return r
}

// --- Task 3.2a: status.cell / status.cellNamespace writes -----------------

func TestTick_CellWrites_FirstPlacement(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, sshRunner("win-1", "payments"))

	rec := newTestReconciler(f)
	rec.probeOffline(t)
	rec.Tick(context.Background())

	got := getRunner(t, f, "win-1")
	if got.Status.Cell != "cell-a" || got.Status.CellNamespace != "varroa-system" {
		t.Fatalf("status = %+v, want cell-a/varroa-system", got.Status)
	}
}

func TestTick_CellWrites_MovedPlacement(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, sshRunner("win-1", "payments"))

	rec := newTestReconciler(f)
	rec.probeOffline(t)
	rec.Tick(context.Background())

	// Move the team to a new cell.
	moved := placedTeam("payments", "cell-b", "varroa-system-2")
	moved.ResourceVersion = ""
	if err := crdstore.Update[v1alpha1.Team](context.Background(), f, moved); err != nil {
		t.Fatalf("move team: %v", err)
	}
	rec.Tick(context.Background())

	got := getRunner(t, f, "win-1")
	if got.Status.Cell != "cell-b" || got.Status.CellNamespace != "varroa-system-2" {
		t.Fatalf("status after move = %+v, want cell-b/varroa-system-2", got.Status)
	}
}

// TestTick_CellWrites_PlacedToUnplaced proves the reconciler computes an
// empty cell/cellNamespace once a Team's placement clears. The in-package
// Fake does not itself apply crdstore.ApplyStatusClears (only the real
// ClientsetClient.PatchObjectStatus force-nulls an absent clearable key), so
// this asserts against the recorded StatusPatch's Status value — what the
// reconciler actually computed and sent — rather than a re-Get, mirroring
// the pattern crdstore's own TestClearStatusFields_RunnerClearsCellFields
// uses for the same reason.
func TestTick_CellWrites_PlacedToUnplaced(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, sshRunner("win-1", "payments"))

	rec := newTestReconciler(f)
	rec.probeOffline(t)
	rec.Tick(context.Background())

	unplaced := &v1alpha1.Team{
		ObjectMeta: metaObj("payments"),
		Spec:       v1alpha1.TeamSpec{Pipelines: &v1alpha1.TeamPipelinesSpec{AgentNamespace: "agents"}},
	}
	if err := crdstore.Update[v1alpha1.Team](context.Background(), f, unplaced); err != nil {
		t.Fatalf("unplace team: %v", err)
	}
	rec.Tick(context.Background())

	gvr, err := crdstore.GVRFor[v1alpha1.Runner]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	patches := f.StatusPatches(gvr)
	if len(patches) == 0 {
		t.Fatal("expected at least one status patch")
	}
	last := patches[len(patches)-1].Status.(*v1alpha1.RunnerStatus)
	if last.Cell != "" || last.CellNamespace != "" {
		t.Fatalf("computed status = %+v, want cell and cellNamespace cleared", last)
	}
}

// --- Task 3.2b: the delivery decision and observedGeneration --------------

func TestTick_DeliveredRunner_ObservedGenerationAdvances(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, sshRunner("win-1", "payments"))

	rec := newTestReconciler(f)
	rec.probeOffline(t)
	rec.Tick(context.Background())

	got := getRunner(t, f, "win-1")
	if got.Status.ObservedGeneration != 1 {
		t.Fatalf("ObservedGeneration = %d, want 1", got.Status.ObservedGeneration)
	}
	ready := findReady(got.Status.Conditions)
	if ready == nil {
		t.Fatal("no Ready condition")
	}
}

// TestTick_UndeliveredReasons_ObservedGenerationStillAdvances covers every
// undelivered reason the ticker produces across steps 4 to 7: each one
// still reaches the delivery decision, so observedGeneration advances even
// though the runner is not delivered.
func TestTick_UndeliveredReasons_ObservedGenerationStillAdvances(t *testing.T) {
	tests := []struct {
		name       string
		runner     *v1alpha1.Runner
		team       *v1alpha1.Team // nil: seed no Team
		secrets    *fakeSecretReader
		wantReason string
	}{
		{
			name:       "team not found",
			runner:     sshRunner("orphan", "ghost"),
			wantReason: ReasonTeamNotFound,
		},
		{
			name:   "team not pipelines",
			runner: sshRunner("orphan", "search"),
			team: &v1alpha1.Team{
				ObjectMeta: metaObj("search"),
				Spec:       v1alpha1.TeamSpec{},
			},
			wantReason: ReasonTeamNotPipelines,
		},
		{
			name: "launcher unsupported",
			runner: &v1alpha1.Runner{
				ObjectMeta: metaObj("orphan"),
				Spec:       v1alpha1.RunnerSpec{Team: "payments", Launcher: "winrm", RemoteFS: "/x"},
			},
			team:       placedTeam("payments", "cell-a", "varroa-system"),
			wantReason: ReasonLauncherUnsupported,
		},
		{
			name:       "team name unsupported",
			runner:     sshRunner("orphan", "pay.roll"),
			team:       placedTeam("pay.roll", "cell-a", "varroa-system"),
			wantReason: ReasonTeamNameUnsupported,
		},
		{
			name:       "runner name unsupported",
			runner:     sshRunner("win.1", "payments"),
			team:       placedTeam("payments", "cell-a", "varroa-system"),
			wantReason: ReasonRunnerNameUnsupported,
		},
		{
			name:       "awaiting placement",
			runner:     sshRunner("orphan", "payments"),
			team:       &v1alpha1.Team{ObjectMeta: metaObj("payments"), Spec: v1alpha1.TeamSpec{Pipelines: &v1alpha1.TeamPipelinesSpec{}}},
			wantReason: ReasonAwaitingPlacement,
		},
		{
			name:       "credential missing",
			runner:     runnerWithCredentialRef("orphan", "no-such-key"),
			team:       placedTeam("payments", "cell-a", "varroa-system"),
			wantReason: ReasonCredentialMissing,
		},
		{
			name:       "credential malformed",
			runner:     runnerWithCredentialRef("orphan", "ssh-key"),
			team:       placedTeam("payments", "cell-a", "varroa-system"),
			secrets:    malformedSSHSecrets(),
			wantReason: ReasonCredentialMalformed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := crdstore.NewFake()
			if tt.team != nil {
				crdstore.MustSeed(f, tt.team)
			}
			crdstore.MustSeed(f, tt.runner)

			rec := newTestReconciler(f)
			if tt.secrets != nil {
				rec.Secrets = tt.secrets
			}
			rec.Tick(context.Background())

			got := getRunner(t, f, tt.runner.Name)
			if got.Status.ObservedGeneration != 1 {
				t.Fatalf("ObservedGeneration = %d, want 1 (an undelivered runner still reaches the delivery decision)", got.Status.ObservedGeneration)
			}
			ready := findReady(got.Status.Conditions)
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != tt.wantReason {
				t.Fatalf("Ready condition = %+v, want False/%s", ready, tt.wantReason)
			}
		})
	}
}

// TestTick_EarlyFault_ObservedGenerationUnchanged proves a tick that cannot
// even list Runners never reaches a delivery decision for any of them, so
// no status patch happens and observedGeneration is left exactly as it was.
func TestTick_EarlyFault_ObservedGenerationUnchanged(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, sshRunner("win-1", "payments"))

	rec := newTestReconciler(f)
	rec.probeOffline(t)
	rec.Tick(context.Background()) // establish a baseline ObservedGeneration
	before := getRunner(t, f, "win-1").Status.ObservedGeneration

	runnerGVR, err := crdstore.GVRFor[v1alpha1.Runner]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	f.FailNext("list", runnerGVR, errors.New("injected transport fault"))

	result := rec.Tick(context.Background())
	if len(result.TouchedCells) != 0 {
		t.Fatalf("TouchedCells = %v, want none on an early-fault tick", result.TouchedCells)
	}

	after := getRunner(t, f, "win-1").Status.ObservedGeneration
	if after != before {
		t.Fatalf("ObservedGeneration changed on an early-fault tick: before=%d after=%d", before, after)
	}
}

// --- Task 3.3: the token mint ----------------------------------------------

func TestTick_TokenMint_InboundOnce(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, inboundRunner())

	rec := newTestReconciler(f)
	rec.probeOffline(t)
	rec.Tick(context.Background())

	got := getRunner(t, f, "agent-1")
	if got.Status.Token == "" {
		t.Fatal("expected a minted token for an inbound runner")
	}
	if len(got.Status.Token) != 32 { // 16 bytes as hex
		t.Fatalf("token length = %d, want 32 hex characters", len(got.Status.Token))
	}
}

func TestTick_TokenMint_SSHNever(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, sshRunner("win-1", "payments"))

	rec := newTestReconciler(f)
	rec.probeOffline(t)
	rec.Tick(context.Background())

	got := getRunner(t, f, "win-1")
	if got.Status.Token != "" {
		t.Fatalf("Token = %q, want empty for an ssh runner", got.Status.Token)
	}
}

func TestTick_TokenMint_NeverReplaced(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, inboundRunner())

	rec := newTestReconciler(f)
	rec.probeOffline(t)
	rec.Tick(context.Background())
	first := getRunner(t, f, "agent-1").Status.Token

	rec.Tick(context.Background())
	second := getRunner(t, f, "agent-1").Status.Token

	if first != second {
		t.Fatalf("token replaced across ticks: %q -> %q", first, second)
	}
}

// --- Task 3.4: the liveness probe ------------------------------------------

func newProbeServer(t *testing.T, handler http.HandlerFunc) func(cr *v1alpha1.Controller) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return func(*v1alpha1.Controller) string { return srv.URL }
}

func TestTick_Liveness_Online(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, sshRunner("win-1", "payments"))
	seedCell(t, f, "cell-a", "varroa-system")

	urlFn := newProbeServer(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer stub-token" {
			t.Errorf("missing operator bearer token, got %q", req.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"offline": false})
	})

	rec := newTestReconciler(f)
	rec.ServiceURL = urlFn
	rec.Tick(context.Background())

	got := getRunner(t, f, "win-1")
	if !got.Status.Connected {
		t.Fatal("Connected = false, want true for an online probe")
	}
	if got.Status.LastSeen == nil {
		t.Fatal("LastSeen not set after an online probe")
	}
	ready := findReady(got.Status.Conditions)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != ReasonConnected {
		t.Fatalf("Ready condition = %+v, want True/Connected", ready)
	}
}

func TestTick_Liveness_Offline(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, sshRunner("win-1", "payments"))
	seedCell(t, f, "cell-a", "varroa-system")

	urlFn := newProbeServer(t, func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]bool{"offline": true})
	})

	rec := newTestReconciler(f)
	rec.ServiceURL = urlFn
	rec.Tick(context.Background())

	got := getRunner(t, f, "win-1")
	if got.Status.Connected {
		t.Fatal("Connected = true, want false for an offline probe")
	}
	if got.Status.LastSeen != nil {
		t.Fatal("LastSeen set on an offline probe, want unchanged (nil)")
	}
	ready := findReady(got.Status.Conditions)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != ReasonOffline {
		t.Fatalf("Ready condition = %+v, want False/Offline", ready)
	}
}

func TestTick_Liveness_NonOKLeavesLastSeenUnchanged(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, sshRunner("win-1", "payments"))
	seedCell(t, f, "cell-a", "varroa-system")

	urlFn := newProbeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	rec := newTestReconciler(f)
	rec.ServiceURL = urlFn
	rec.Tick(context.Background())

	got := getRunner(t, f, "win-1")
	if got.Status.Connected {
		t.Fatal("Connected = true, want false for a non-200 probe answer")
	}
	if got.Status.LastSeen != nil {
		t.Fatal("LastSeen set on a non-200 probe answer, want unchanged")
	}
	ready := findReady(got.Status.Conditions)
	if ready == nil || ready.Reason != ReasonOffline {
		t.Fatalf("Ready condition = %+v, want reason Offline", ready)
	}
}

func TestTick_Liveness_TransportErrorLeavesLastSeenUnchanged(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, sshRunner("win-1", "payments"))
	seedCell(t, f, "cell-a", "varroa-system")

	rec := newTestReconciler(f)
	// A URL nothing listens on: connection refused, a transport error.
	rec.ServiceURL = func(*v1alpha1.Controller) string { return "http://127.0.0.1:1" }
	rec.Tick(context.Background())

	got := getRunner(t, f, "win-1")
	if got.Status.Connected {
		t.Fatal("Connected = true, want false for a transport error")
	}
	if got.Status.LastSeen != nil {
		t.Fatal("LastSeen set on a transport error, want unchanged")
	}
	ready := findReady(got.Status.Conditions)
	if ready == nil || ready.Reason != ReasonOffline {
		t.Fatalf("Ready condition = %+v, want reason Offline", ready)
	}
}

// TestTick_PatchOnlyWhenChanged proves a second tick with an identical
// offline answer sends no further status patch.
func TestTick_PatchOnlyWhenChanged(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, sshRunner("orphan", "ghost")) // stable TeamNotFound fault, no probe

	rec := newTestReconciler(f)
	rec.Tick(context.Background())

	gvr, err := crdstore.GVRFor[v1alpha1.Runner]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	first := len(f.StatusPatches(gvr))
	if first == 0 {
		t.Fatal("expected a patch on the first tick")
	}

	rec.Tick(context.Background())
	second := len(f.StatusPatches(gvr))
	if second != first {
		t.Fatalf("patch count changed on an unchanged tick: %d -> %d", first, second)
	}
}

// --- Task 3.7: the enqueue-hint map ----------------------------------------

func TestTick_RunnerDeletionEnqueuesTheCellItLeft(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, sshRunner("win-1", "payments"))

	rec := newTestReconciler(f)
	rec.probeOffline(t)
	result := rec.Tick(context.Background())
	if !containsCell(result.TouchedCells, CellKey{Cluster: "core", Namespace: "varroa-system", Name: "cell-a"}) {
		t.Fatalf("first tick TouchedCells = %v, want cell-a present", result.TouchedCells)
	}

	if err := crdstore.Delete[v1alpha1.Runner](context.Background(), f, "win-1", ""); err != nil {
		t.Fatalf("delete runner: %v", err)
	}

	result = rec.Tick(context.Background())
	if !containsCell(result.TouchedCells, CellKey{Cluster: "core", Namespace: "varroa-system", Name: "cell-a"}) {
		t.Fatalf("tick after deletion TouchedCells = %v, want the left cell present", result.TouchedCells)
	}
}

func containsCell(cells []CellKey, want CellKey) bool {
	for _, c := range cells {
		if c == want {
			return true
		}
	}
	return false
}

func findReady(conds []v1alpha1.RunnerCondition) *v1alpha1.RunnerCondition {
	for i := range conds {
		if conds[i].Type == ConditionTypeReady {
			return &conds[i]
		}
	}
	return nil
}

// probeOffline points rec at a server that always reports offline, for
// tests that need a delivered runner but do not care about the probe
// outcome itself.
func (r *Reconciler) probeOffline(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]bool{"offline": true})
	}))
	t.Cleanup(srv.Close)
	r.ServiceURL = func(*v1alpha1.Controller) string { return srv.URL }
	seedCell(t, r.Backend, "cell-a", "varroa-system")
	seedCell(t, r.Backend, "cell-b", "varroa-system-2")
}

// --- Tick step 7: the credential read ---------------------------------

// TestTick_CredentialFault_SkipsProbe proves a credential-fault runner
// never reaches the liveness probe: the probe would report an ssh runner
// with no working key as falsely online or offline instead of naming the
// real fault.
func TestTick_CredentialFault_SkipsProbe(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, runnerWithCredentialRef("win-1", "no-such-key"))
	seedCell(t, f, "cell-a", "varroa-system")

	probed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probed = true
		_ = json.NewEncoder(w).Encode(map[string]bool{"offline": false})
	}))
	t.Cleanup(srv.Close)

	rec := newTestReconciler(f)
	rec.ServiceURL = func(*v1alpha1.Controller) string { return srv.URL }
	rec.Tick(context.Background())

	if probed {
		t.Fatal("liveness probe called for a runner whose credential read faulted")
	}

	got := getRunner(t, f, "win-1")
	if got.Status.Connected {
		t.Fatal("Connected = true for a credential fault")
	}
	ready := findReady(got.Status.Conditions)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != ReasonCredentialMissing {
		t.Fatalf("Ready condition = %+v, want False/%s", ready, ReasonCredentialMissing)
	}
}

// TestTick_CredentialFault_AgreesWithContribution proves the ticker's
// delivery decision and BuildContribution's separate read of the same
// Secret never disagree: a runner the ticker reports CredentialMissing for
// gets no node from the same tick's desired-state read either.
func TestTick_CredentialFault_AgreesWithContribution(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, runnerWithCredentialRef("win-1", "no-such-key"))

	rec := newTestReconciler(f)
	rec.probeOffline(t)
	rec.Tick(context.Background())

	got := getRunner(t, f, "win-1")
	ready := findReady(got.Status.Conditions)
	if ready == nil || ready.Reason != ReasonCredentialMissing {
		t.Fatalf("Ready condition = %+v, want reason %s", ready, ReasonCredentialMissing)
	}

	contrib, err := BuildContribution(context.Background(), f, rec.Secrets, cellFor("cell-a"))
	if err != nil {
		t.Fatalf("BuildContribution: %v", err)
	}
	if len(contrib.Nodes) != 0 {
		t.Fatalf("Nodes = %+v, want none for a runner the ticker reports undelivered", contrib.Nodes)
	}
}

// TestTick_OfflineRunner_StillInContribution is task 3.4's combined proof:
// a runner the ticker marks Offline (delivered, but the probe reports it
// unreachable) still has its node in the same tick's BuildContribution
// read. Removing the node of an offline runner would remove the node it
// needs to reconnect.
func TestTick_OfflineRunner_StillInContribution(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	crdstore.MustSeed(f, sshRunner("win-1", "payments"))
	seedCell(t, f, "cell-a", "varroa-system")

	urlFn := newProbeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]bool{"offline": true})
	})

	rec := newTestReconciler(f)
	rec.ServiceURL = urlFn
	rec.Tick(context.Background())

	got := getRunner(t, f, "win-1")
	ready := findReady(got.Status.Conditions)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != ReasonOffline {
		t.Fatalf("Ready condition = %+v, want False/%s", ready, ReasonOffline)
	}

	contrib, err := BuildContribution(context.Background(), f, rec.Secrets, cellFor("cell-a"))
	if err != nil {
		t.Fatalf("BuildContribution: %v", err)
	}
	if len(contrib.Nodes) != 1 {
		t.Fatalf("Nodes = %+v, want the offline runner's node still rendered", contrib.Nodes)
	}
}

// TestTick_CredentialFault_ClearsConnected proves a runner that was
// connected reports connected=false once its credential faults: an
// undelivered runner never carries a connected value forward (design D10).
func TestTick_CredentialFault_ClearsConnected(t *testing.T) {
	f := crdstore.NewFake()
	crdstore.MustSeed(f, placedTeam("payments", "cell-a", "varroa-system"))
	runner := runnerWithCredentialRef("win-1", "no-such-key")
	runner.Status.Connected = true
	crdstore.MustSeed(f, runner)
	seedCell(t, f, "cell-a", "varroa-system")

	rec := newTestReconciler(f)
	rec.Tick(context.Background())

	got := getRunner(t, f, "win-1")
	if got.Status.Connected {
		t.Fatal("Connected = true after a credential fault")
	}
	ready := findReady(got.Status.Conditions)
	if ready == nil || ready.Reason != ReasonCredentialMissing {
		t.Fatalf("Ready condition = %+v, want %s", ready, ReasonCredentialMissing)
	}
}
