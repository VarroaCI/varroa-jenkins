package status

import (
	"context"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	"github.com/varroaci/varroa-jenkins/internal/tenantagent/accept"
	"github.com/varroaci/varroa-jenkins/internal/tenantagent/config"
	tenantstore "github.com/varroaci/varroa-jenkins/internal/tenantagent/store"
)

var testNow = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// fakeSource is the cache view: one TenantAgent, one CatalogSource, and the
// Tenants that exist in the core.
type fakeSource struct {
	ta      v1alpha1.TenantAgent
	cs      *v1alpha1.CatalogSource
	tenants map[string]*v1alpha1.Tenant
}

func (f *fakeSource) List() []v1alpha1.TenantAgent { return []v1alpha1.TenantAgent{f.ta} }

func (f *fakeSource) AcceptInput() accept.Input {
	return accept.Input{TenantAgents: f.List(), Tenants: f.tenants, ReleaseNamespace: "team-a"}
}

func (f *fakeSource) CatalogSource(string, string) (*v1alpha1.CatalogSource, bool) {
	return f.cs, f.cs != nil
}

// fakeWriter counts the status writes and stores each one on the source, as the
// API server does.
type fakeWriter struct {
	mu     sync.Mutex
	writes int
	src    *fakeSource
}

func (w *fakeWriter) WriteStatus(_ context.Context, _ *v1alpha1.TenantAgent, st v1alpha1.TenantAgentStatus) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	if w.src != nil {
		w.src.ta.Status = st
	}
	return nil
}

func (w *fakeWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

type fakeSessions struct{}

func (fakeSessions) LastSession(context.Context, string) (tenantstore.LastSession, bool, error) {
	return tenantstore.LastSession{}, false, nil
}

type fakeSpend struct{ total int64 }

func (f fakeSpend) TenantDaySpend(context.Context, string, time.Time) (int64, error) {
	return f.total, nil
}

// fakeRelay counts the reads and answers with a fixed reply.
type fakeRelay struct {
	mu    sync.Mutex
	reads int
	reply forgeapi.ReadReply
	err   error
}

func (r *fakeRelay) Read(context.Context, string, forgeapi.ReadRequest) (forgeapi.ReadReply, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	return r.reply, r.err
}

func (r *fakeRelay) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

func baseTA() v1alpha1.TenantAgent {
	ta := v1alpha1.TenantAgent{}
	ta.Name = "triage"
	ta.Namespace = "team-a"
	ta.UID = "uid-triage"
	ta.Generation = 1
	ta.Spec.TenantRef.Name = "obj"
	ta.Spec.Instructions = &v1alpha1.TenantAgentInstructions{Source: "obj-src"}
	return ta
}

func baseSource() *fakeSource {
	cs := &v1alpha1.CatalogSource{}
	cs.Spec.RepoURL = "https://git.example.org/x/y"
	cs.Status.ObservedRevision = "rev-1"
	return &fakeSource{
		ta:      baseTA(),
		cs:      cs,
		tenants: map[string]*v1alpha1.Tenant{"obj": {}},
	}
}

func baseTicker(src *fakeSource, relay *fakeRelay, out *fakeWriter, now *time.Time) *Ticker {
	return &Ticker{
		Src:       src,
		Out:       out,
		Sessions:  fakeSessions{},
		Spend:     fakeSpend{},
		Settings:  func(context.Context) (agent.Settings, error) { return agent.Settings{}, nil },
		Placement: func(string) (string, bool) { return "core", true },
		Cfg:       testCfg(),
		Relay:     relay,
		Cluster:   "core",
		Now:       func() time.Time { return *now },
	}
}

// TestTicker_NonLeaderWritesNothing proves that a replica without the status
// Lease never writes a status, while the holder of the Lease is another replica.
func TestTicker_NonLeaderWritesNothing(t *testing.T) {
	now := testNow
	src := baseSource()
	out := &fakeWriter{}
	tk := baseTicker(src, &fakeRelay{reply: forgeapi.ReadReply{Status: forgeapi.Status{OK: true}}}, out, &now)

	kube := fake.NewSimpleClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: LeaseName, Namespace: "team-a"},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       ptr("other"),
			LeaseDurationSeconds: ptr(int32(15)),
			RenewTime:            &metav1.MicroTime{Time: time.Now()},
			AcquireTime:          &metav1.MicroTime{Time: time.Now()},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// The other replica holds a fresh Lease. This replica never takes it, so
	// its ticker does not run.
	_ = RunElected(ctx, kube, "team-a", "me", tk, nil)
	if got := out.count(); got != 0 {
		t.Fatalf("a non-leader wrote %d statuses", got)
	}
}

func ptr[T any](v T) *T { return &v }

// TestConditions_Reasons runs one case per reason of the Paused and the
// UnattendedAllowed conditions.
func TestConditions_Reasons(t *testing.T) {
	now := testNow
	src := baseSource()
	tk := baseTicker(src, &fakeRelay{}, &fakeWriter{}, &now)
	okVerdict := accept.Verdict{Accepted: true}

	cases := []struct {
		name   string
		mut    func(*v1alpha1.TenantAgent, *accept.Input, *agent.Settings)
		v      accept.Verdict
		spend  int64
		reason string
	}{
		{"duplicate", nil, accept.Verdict{Reason: accept.ReasonDuplicate}, 0, v1alpha1.TenantAgentReasonDuplicate},
		{"namespace not owned", nil, accept.Verdict{Reason: accept.ReasonNamespaceNotOwned}, 0, v1alpha1.TenantAgentReasonNamespaceNotOwned},
		{"tenant not found", func(_ *v1alpha1.TenantAgent, in *accept.Input, _ *agent.Settings) {
			in.Tenants = map[string]*v1alpha1.Tenant{}
			tk.Placement = func(string) (string, bool) { return "", false }
		}, okVerdict, 0, v1alpha1.TenantAgentReasonTenantNotFound},
		{"disabled", func(t *v1alpha1.TenantAgent, _ *accept.Input, _ *agent.Settings) {
			off := false
			t.Spec.Enabled = &off
		}, okVerdict, 0, v1alpha1.TenantAgentReasonDisabled},
		{"kill switch", func(_ *v1alpha1.TenantAgent, _ *accept.Input, s *agent.Settings) { s.Paused = true }, okVerdict, 0, v1alpha1.TenantAgentReasonKillSwitch},
		{"budget day", nil, okVerdict, 2000000, v1alpha1.TenantAgentReasonBudgetDay},
		{"active", nil, okVerdict, 0, reasonActive},
	}
	for _, c := range cases {
		t.Run("paused "+c.name, func(t *testing.T) {
			cur := baseTA()
			in := src.AcceptInput()
			settings := agent.Settings{}
			tk.Placement = func(string) (string, bool) { return "core", true }
			if c.mut != nil {
				c.mut(&cur, &in, &settings)
			}
			reason, _ := tk.pausedReason(&cur, c.v, in, settings, c.spend)
			if reason != c.reason {
				t.Fatalf("paused reason = %s, want %s", reason, c.reason)
			}
		})
	}

	grants := []struct {
		name   string
		grant  *agent.UnattendedGrant
		reason string
	}{
		{"none requested", nil, v1alpha1.TenantAgentReasonNoneRequested},
		{"not granted", nil, v1alpha1.TenantAgentReasonNotGranted},
		{"granted", &agent.UnattendedGrant{UID: "uid-triage", Tools: []string{"run_pipeline"}}, v1alpha1.TenantAgentReasonGranted},
		{"partially granted", &agent.UnattendedGrant{UID: "uid-triage", Tools: []string{"run_pipeline"}}, v1alpha1.TenantAgentReasonPartiallyGranted},
		{"uid mismatch", &agent.UnattendedGrant{UID: "other", Tools: []string{"run_pipeline"}}, v1alpha1.TenantAgentReasonNotGranted},
	}
	for _, g := range grants {
		t.Run("unattended "+g.name, func(t *testing.T) {
			cur := baseTA()
			switch g.name {
			case "none requested":
			case "partially granted":
				cur.Spec.UnattendedWrites = []string{"run_pipeline", "restart_run"}
			default:
				cur.Spec.UnattendedWrites = []string{"run_pipeline"}
			}
			settings := agent.Settings{TenantAgentUnattended: map[string]agent.UnattendedGrant{}}
			if g.grant != nil {
				settings.TenantAgentUnattended[cur.Namespace+"/"+cur.Name] = *g.grant
			}
			cond := unattendedOf(&cur, settings, now)
			if cond.Reason != g.reason {
				t.Fatalf("unattended reason = %s (%s), want %s", cond.Reason, cond.Message, g.reason)
			}
		})
	}
}

// TestInstructionsAttemptMemory_NotFoundNotRetriedUntilRevision proves that a
// NotFound answer is not read again until the source revision changes.
func TestInstructionsAttemptMemory_NotFoundNotRetriedUntilRevision(t *testing.T) {
	now := testNow
	src := baseSource()
	relay := &fakeRelay{reply: forgeapi.ReadReply{Status: forgeapi.Status{Code: "not_found", Error: "missing"}}}
	tk := baseTicker(src, relay, &fakeWriter{}, &now)
	for i := 0; i < 3; i++ {
		if err := tk.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := relay.count(); got != 1 {
		t.Fatalf("reads at one revision = %d, want 1", got)
	}
	src.cs.Status.ObservedRevision = "rev-2"
	if err := tk.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := relay.count(); got != 2 {
		t.Fatalf("reads after a new revision = %d, want 2", got)
	}
}

// TestFetchFailedRetriesAfter10m proves that a fetch failure waits ten minutes
// before the next read at the same revision.
func TestFetchFailedRetriesAfter10m(t *testing.T) {
	now := testNow
	src := baseSource()
	relay := &fakeRelay{err: context.DeadlineExceeded}
	tk := baseTicker(src, relay, &fakeWriter{}, &now)
	if err := tk.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	if err := tk.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := relay.count(); got != 1 {
		t.Fatalf("reads at +5m = %d, want 1", got)
	}
	now = now.Add(6 * time.Minute)
	if err := tk.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := relay.count(); got != 2 {
		t.Fatalf("reads at +11m = %d, want 2", got)
	}
}

// TestTicker_WritesOnlyOnChange proves that an unchanged pass writes nothing.
func TestTicker_WritesOnlyOnChange(t *testing.T) {
	now := testNow
	src := baseSource()
	out := &fakeWriter{}
	out.src = src
	tk := baseTicker(src, &fakeRelay{reply: forgeapi.ReadReply{Status: forgeapi.Status{OK: true}, Revision: "sha-1",
		ReadFile: &forgeapi.ReadFile{Size: 4, Content: "text"}}}, out, &now)
	if err := tk.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := out.count()
	if first != 1 {
		t.Fatalf("first pass writes = %d, want 1", first)
	}
	if err := tk.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out.count() != first {
		t.Fatalf("an unchanged pass wrote again")
	}
}

func testCfg() config.Config {
	var cfg config.Config
	cfg.Assistant.Budgets.TenantDay = 1000000
	return cfg
}
