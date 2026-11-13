package triggers

import (
	"context"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider/factories"
	agentstore "github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/assistantconfig"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	tenantstore "github.com/varroaci/varroa-jenkins/internal/tenantagent/store"
)

// freshDB creates a migrated database that only this test uses. It returns the
// pool and the DSN, and skips when VARROA_TEST_DSN is not set.
func freshDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("VARROA_TEST_DSN")
	if dsn == "" {
		t.Skip("VARROA_TEST_DSN is not set; skipping trigger database test")
	}
	ctx := context.Background()
	name := "trig_" + strings.ReplaceAll(uuid.Must(uuid.NewV7()).String(), "-", "")[:20]
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	_ = admin.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	if err := recordstore.RunMigrations(ctx, u.String()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, u.String()
}

// testCreator builds a Creator over a fresh database. The provider is ready,
// the chart day budget is large, and the kill switch is off unless paused.
func testCreator(t *testing.T, paused bool) *Creator {
	t.Helper()
	pool, dsn := freshDB(t)
	ctx := context.Background()
	st, err := agentstore.Open(ctx, dsn, agentstore.Options{Now: time.Now})
	if err != nil {
		t.Fatalf("agent store: %v", err)
	}
	t.Cleanup(st.Close)
	snap := assistantconfig.Build(&v1alpha1.AssistantConfig{Spec: v1alpha1.AssistantConfigSpec{
		Providers: []v1alpha1.AssistantProvider{{
			Name: "local", Kind: v1alpha1.AssistantProviderKindOpenAICompatible, BaseURL: "http://127.0.0.1:1/v1",
			Models: []string{"m1"}, Trust: v1alpha1.AssistantTrustInternal, Auth: v1alpha1.AssistantAuthNone,
		}},
		DefaultProvider:     "local",
		ExternalDataSharing: "logs",
		Budgets:             v1alpha1.AssistantBudgets{Session: 1000, UserDay: 1000, TenantDay: 1000000, Steps: 4},
		ApprovalExpiry:      "24h",
	}}, factories.DefaultFactories(), t.TempDir())
	if snap.Err != nil {
		t.Fatalf("snapshot: %v", snap.Err)
	}
	return &Creator{
		Store:   st,
		Tenants: tenantstore.New(pool, nil),
		Live:    assistantconfig.NewLiveWith(snap),
		Settings: func(context.Context) (agent.Settings, error) {
			return agent.Settings{Paused: paused}, nil
		},
		Owner: "pod-a",
		Image: "sha256:test",
		Now:   func() time.Time { return testNow },
	}
}

func testFor(eventKey string, _ *v1alpha1.TenantAgent) Match {
	return Match{
		TA:       testTA("run.failed", "manual"),
		Trigger:  "run.failed",
		Tenant:   "obj",
		ItemKey:  "obj/build",
		EventKey: eventKey,
		Event:    rowOf("build.failed", "obj/build", "core"),
	}
}

// TestCreatorUsesConfigDefaultModel proves that a session with no model of its
// own takes the default model of the snapshot, not the first listed model.
func TestCreatorUsesConfigDefaultModel(t *testing.T) {
	c := testCreator(t, false)
	snap := assistantconfig.Build(&v1alpha1.AssistantConfig{Spec: v1alpha1.AssistantConfigSpec{
		Providers: []v1alpha1.AssistantProvider{{
			Name: "local", Kind: v1alpha1.AssistantProviderKindOpenAICompatible, BaseURL: "http://127.0.0.1:1/v1",
			Models: []string{"m1", "m2"}, Trust: v1alpha1.AssistantTrustInternal, Auth: v1alpha1.AssistantAuthNone,
		}},
		DefaultProvider:     "local",
		DefaultModel:        "m2",
		ExternalDataSharing: "logs",
		Budgets:             v1alpha1.AssistantBudgets{Session: 1000, UserDay: 1000, TenantDay: 1000000, Steps: 4},
		ApprovalExpiry:      "24h",
	}}, factories.DefaultFactories(), t.TempDir())
	if snap.Err != nil {
		t.Fatalf("snapshot: %v", snap.Err)
	}
	c.Live = assistantconfig.NewLiveWith(snap)
	s, out, err := c.Create(context.Background(), testFor("model-default", nil))
	if err != nil || out != Created {
		t.Fatalf("Create = %v, %v", out, err)
	}
	if s.Model != "m2" {
		t.Fatalf("session model = %q, want the default m2", s.Model)
	}
}

func TestCreate_TwoGoroutinesOneSession(t *testing.T) {
	c := testCreator(t, false)
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[CreateOutcome]int{}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Two failures of one job, so two event keys on one item key.
			_, out, err := c.Create(ctx, testFor(string(rune('a'+i)), nil))
			if err != nil {
				t.Errorf("Create: %v", err)
				return
			}
			mu.Lock()
			counts[out]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if counts[Created] != 1 || counts[Debounced] != 1 {
		t.Fatalf("outcomes = %v, want one created and one debounced", counts)
	}
}

func TestCreate_DedupeOnTriggerEvent(t *testing.T) {
	c := testCreator(t, false)
	ctx := context.Background()
	if _, out, err := c.Create(ctx, testFor("evt-1", nil)); err != nil || out != Created {
		t.Fatalf("first create = %v, %v", out, err)
	}
	if _, out, err := c.Create(ctx, testFor("evt-1", nil)); err != nil || out != Deduped {
		t.Fatalf("repeat of one event = %v, %v; want deduped", out, err)
	}
}

func TestCreate_ManualIsNotDebounced(t *testing.T) {
	c := testCreator(t, false)
	ctx := context.Background()
	m1 := testFor("evt-1", nil)
	m1.Trigger = ManualTrigger
	m1.ItemKey = "evt-1"
	m2 := testFor("evt-2", nil)
	m2.Trigger = ManualTrigger
	m2.ItemKey = "evt-2"
	for _, m := range []Match{m1, m2} {
		if _, out, err := c.Create(ctx, m); err != nil || out != Created {
			t.Fatalf("manual create = %v, %v; want created", out, err)
		}
	}
}

func TestCreate_KillSwitchWritesSkippedRow(t *testing.T) {
	c := testCreator(t, true)
	sess, out, err := c.Create(context.Background(), testFor("evt-k", nil))
	if err != nil || out != Skipped {
		t.Fatalf("paused create = %v, %v; want skipped", out, err)
	}
	if sess.State != agent.StateSkipped || sess.SkipReason != agent.SkipKillSwitch {
		t.Fatalf("skipped row = state %s reason %s", sess.State, sess.SkipReason)
	}
	if sess.EndedAt == nil || !sess.EndedAt.Equal(sess.StartedAt) {
		t.Fatal("a skipped row ends at its start")
	}
	evs, err := c.Store.Events(context.Background(), sess.ID, 0, 10)
	if err != nil || len(evs) != 2 {
		t.Fatalf("skipped events = %d, %v; want user and done", len(evs), err)
	}
}
