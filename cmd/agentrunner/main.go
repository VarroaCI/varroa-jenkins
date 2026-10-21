// Command agentrunner runs the Team agent sessions. It consumes the audit feed,
// starts a session for each trigger that a TeamAgent enables, and runs the
// sessions through the BFF MCP endpoint. It serves /healthz, /readyz, and
// /metrics on port 8080, and it answers no NATS request.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/datashare"
	"github.com/varroaci/varroa-jenkins/internal/agent/loop"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider/factories"
	agentstore "github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/forgerelay"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/accept"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/cache"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/config"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/sessions"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/spend"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/status"
	teamstore "github.com/varroaci/varroa-jenkins/internal/teamagent/store"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/token"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/triggers"
	"github.com/varroaci/varroa-jenkins/internal/telemetry"
)

// minSchemaVersion is the first record-store schema that holds the Team agent
// spend table.
const minSchemaVersion = 40

// consumerPending is the number of rows the trigger consumer has not read yet.
var consumerPending = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "varroa_agent_consumer_pending",
	Help: "Activity rows that the Team agent trigger consumer has not read yet.",
})

// triggerCounts counts each trigger by its outcome.
var triggerCounts = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "varroa_agent_triggers_total",
	Help: "Activity rows handled by the Team agent trigger path, by trigger and outcome.",
}, []string{"trigger", "outcome"})

func main() {
	busURL := flag.String("bus-url", os.Getenv("VARROA_BUS_URL"), "NATS bus URL")
	busCAFile := flag.String("bus-ca-file", "", "Path to the NATS CA certificate file")
	busPassFile := flag.String("bus-pass-file", "", "Path to the NATS password file for the agentrunner user")
	port := flag.Int("port", 8080, "HTTP port for health and metrics")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger, *busURL, *busCAFile, *busPassFile, *port); err != nil {
		logger.Error("varroa-agent stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger, busURL, caFile, passFile string, port int) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.FromEnv(os.Getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if busURL == "" {
		return errors.New("-bus-url or VARROA_BUS_URL is required")
	}

	pool, err := pgxpool.New(ctx, cfg.DSN)
	if err != nil {
		return fmt.Errorf("open record store pool: %w", err)
	}
	defer pool.Close()
	if err := waitForSchema(ctx, pool, logger); err != nil {
		return err
	}
	st, err := agentstore.FromPool(ctx, pool, agentstore.Options{})
	if err != nil {
		return fmt.Errorf("agent store: %w", err)
	}

	busCfg := bus.Config{Username: "agentrunner", CAFile: caFile, InboxPrefix: "_INBOX_agentrunner", Logger: logger}
	if passFile != "" {
		busCfg.PasswordFile = passFile
	}
	conn, err := bus.Connect(busURL, busCfg)
	if err != nil {
		return fmt.Errorf("connect bus: %w", err)
	}
	defer conn.Close()

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("cluster config: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}
	releaseNS := os.Getenv("POD_NAMESPACE")
	objs := cache.New(dyn, releaseNS, logger)
	go objs.Run(ctx)
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("kube client: %w", err)
	}

	key, err := token.ReadKey(cfg.KeyPath)
	if err != nil {
		return fmt.Errorf("read signing key: %w", err)
	}

	var reg *provider.Registry
	if cfg.AssistantErr == nil {
		reg, err = provider.NewRegistry(cfg.Assistant, factories.DefaultFactories(), provider.DefaultKeyDir)
		if err != nil {
			return fmt.Errorf("provider registry: %w", err)
		}
	}

	var ready atomic.Bool
	var consumerReady atomic.Bool
	deps := &runnerDeps{
		cfg: cfg, st: st, reg: reg, objs: objs, key: key, conn: conn, logger: logger,
		tenants: teamstore.New(pool, nil),
		relay:   forgerelay.Relay{Conn: conn},
	}
	go serveHTTP(ctx, port, pool, conn, objs, &ready, &consumerReady, logger)

	deps.startConsumer(ctx, &consumerReady)
	changed := make(chan struct{}, 1)
	wake := func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	objs.OnChange(wake)
	// Every replica hears the kill switch. The settings are read per call, so
	// the signal wakes the status ticker, which shows the new state at once.
	ks, err := sessions.SubscribeKillSwitch(conn, wake)
	if err != nil {
		return fmt.Errorf("subscribe kill switch: %w", err)
	}
	defer func() { _ = ks.Unsubscribe() }()
	ticker := &status.Ticker{
		Src:       objs,
		Out:       dynWriter{dyn: dyn},
		Sessions:  deps.tenants,
		Spend:     deps.tenants,
		Rollover:  &spend.Rollover{Store: deps.tenants},
		Settings:  func(c context.Context) (agent.Settings, error) { return st.GetSettings(c, agent.ScopeGlobal) },
		Placement: deps.placement,
		Registry:  reg,
		Cfg:       cfg,
		Relay:     forgerelay.Relay{Conn: conn},
		Cluster:   cfg.Cluster,
		Now:       time.Now,
	}
	go func() {
		if err := status.RunElected(ctx, kube, releaseNS, cfg.PodName, ticker, changed); err != nil {
			logger.Error("status election", "error", err.Error())
		}
	}()
	go deps.reportConsumerPending(ctx)
	ready.Store(true)
	logger.Info("varroa-agent running", "cluster", cfg.Cluster, "pod", cfg.PodName)
	<-ctx.Done()
	return nil
}

// waitForSchema polls until the record store holds the Team agent tables.
func waitForSchema(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	for {
		var v int64
		err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM varroa.goose_db_version WHERE is_applied`).Scan(&v)
		if err == nil && v >= minSchemaVersion {
			return nil
		}
		logger.Info("waiting for the record store schema", "need", minSchemaVersion, "have", v)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// runnerDeps holds what the trigger path and the session runner share.
type runnerDeps struct {
	cfg     config.Config
	st      *agentstore.Postgres
	reg     *provider.Registry
	objs    *cache.Cache
	key     []byte
	conn    *bus.Conn
	logger  *slog.Logger
	tenants *teamstore.Store
	relay   forgerelay.Relay
	pool    *sessions.Pool
}

// startConsumer wires the trigger path and runs it until ctx ends.
func (d *runnerDeps) startConsumer(ctx context.Context, consumerReady *atomic.Bool) {
	writer := agentstore.WithNotify(d.st, d.conn.Publish, d.logger)
	pool := sessions.NewPool(ctx, sessions.DefaultPoolSize, func(runCtx context.Context, sess agent.Session, trigger loop.Trigger) {
		d.runSession(runCtx, writer, sess, trigger)
	})
	d.pool = pool
	matcher := &triggers.Matcher{
		Accepted:  d.objs.AcceptInput,
		Placement: d.placement,
	}
	creator := &triggers.Creator{
		Store:    d.st,
		Tenants:  d.tenants,
		Cfg:      d.cfg,
		Registry: d.reg,
		Settings: func(c context.Context) (agent.Settings, error) {
			return d.st.GetSettings(c, agent.ScopeGlobal)
		},
		Owner: d.cfg.PodName,
		Image: os.Getenv("VARROA_IMAGE_DIGEST"),
		Source: func(ta *v1alpha1.TeamAgent) (string, string) {
			if ta.Spec.Instructions == nil {
				return "", ""
			}
			src, ok := d.objs.CatalogSource(ta.Namespace, ta.Spec.Instructions.Source)
			if !ok || src.Spec.Revision != "" {
				// A source that pins a revision is not read: the session has no
				// repository to pin, so its instructions block stays out.
				return "", ""
			}
			return src.Spec.RepoURL, src.Status.ObservedRevision
		},
		Now: time.Now,
	}
	resumer := &sessions.Resumer{
		Store:   d.st,
		Tenants: d.tenants,
		Owner:   d.cfg.PodName,
		Teams: func() []string {
			return acceptedTeams(d.objs.AcceptInput())
		},
		// The slot is reserved before the lease is taken, and the pool frees it
		// when the resumed run ends.
		Reserve: pool.TryReserve,
		Release: pool.Release,
		Start: func(_ context.Context, sess agent.Session, trigger loop.Trigger) {
			pool.Start(sess, trigger)
		},
		Window: d.cfg.Assistant.ApprovalExpiry + time.Hour,
		Now:    time.Now,
	}
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := resumer.Scan(ctx); err != nil {
					d.logger.Warn("team agent resume scan", "error", err.Error())
				}
			}
		}
	}()
	handoff := &triggers.Handoff{
		Matcher: matcher,
		Creator: creator,
		Pool:    pool,
		Publish: d.conn.Publish,
		Cluster: d.cfg.Cluster,
		Now:     time.Now,
		Logger:  d.logger,
		OnOutcome: func(trigger, outcome string) {
			triggerCounts.WithLabelValues(trigger, outcome).Inc()
		},
	}
	go func() {
		// The readiness flag is set only once the consumer is bound.
		err := triggers.Run(ctx, d.conn, d.logger, func(bound bool) { consumerReady.Store(bound) },
			func(c context.Context, m *nats.Msg) {
				handoff.Handle(c, natsMsg{m}, m.Data)
			})
		if err != nil {
			d.logger.Error("trigger consumer stopped", "error", err.Error())
		}
	}()
}

// dynWriter writes a TeamAgent's status through the status subresource.
type dynWriter struct{ dyn dynamic.Interface }

var teamAgentGVR = schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "teamagents"}

// WriteStatus merge-patches the status of ta.
func (w dynWriter) WriteStatus(ctx context.Context, ta *v1alpha1.TeamAgent, st v1alpha1.TeamAgentStatus) error {
	body, err := json.Marshal(map[string]any{"status": st})
	if err != nil {
		return err
	}
	_, err = w.dyn.Resource(teamAgentGVR).Namespace(ta.Namespace).Patch(ctx, ta.Name,
		types.MergePatchType, body, metav1.PatchOptions{}, "status")
	return err
}

// reportConsumerPending sets the pending gauge of the trigger consumer.
func (d *runnerDeps) reportConsumerPending(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if info, err := d.conn.ConsumerInfo(triggers.StreamName, triggers.ConsumerName); err == nil {
				consumerPending.Set(float64(info.NumPending))
			}
		}
	}
}

// acceptedTeams returns the names of the Teams that have an accepted TeamAgent.
func acceptedTeams(in accept.Input) []string {
	seen := map[string]bool{}
	var out []string
	for i := range in.TeamAgents {
		team := in.TeamAgents[i].Spec.TeamRef.Name
		if seen[team] {
			continue
		}
		if ta, ok := accept.AcceptedFor(in, team); ok && ta != nil {
			seen[team] = true
			out = append(out, team)
		}
	}
	return out
}

// natsMsg adapts a JetStream message to the trigger path's acknowledgement.
type natsMsg struct{ m *nats.Msg }

func (n natsMsg) Ack() error { return n.m.Ack() }

func (n natsMsg) NakWithDelay(delay time.Duration) error { return n.m.NakWithDelay(delay) }

// placement answers the cluster that holds a tenant's live cell. A core Team
// reports it on its status. A member-placed tenant reads the record store.
func (d *runnerDeps) placement(tenant string) (string, bool) {
	in := d.objs.AcceptInput()
	if team := in.Teams[tenant]; team != nil {
		if team.Status.Placement != nil && team.Status.Placement.Cluster != "" {
			return team.Status.Placement.Cluster, true
		}
		return d.cfg.Cluster, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cluster, ok, err := d.tenants.LivePlacement(ctx, tenant)
	if err != nil {
		d.logger.Warn("placement lookup", "tenant", tenant, "error", err.Error())
		return "", false
	}
	return cluster, ok
}

// runSession runs one session to its end with the loop.
func (d *runnerDeps) runSession(ctx context.Context, writer agentstore.Store, sess agent.Session, trigger loop.Trigger) {
	defer func() {
		st, err := writer.GetSession(ctx, sess.ID)
		if err == nil {
			sessions.SessionsTotal.WithLabelValues(string(st.State)).Inc()
		}
	}()
	ta, ok := d.objs.ByUID(teamUIDOf(sess))
	if !ok {
		d.logger.Warn("team agent session has no object", "session", sess.ID.String())
		endFailed(ctx, writer, sess)
		return
	}
	if d.reg == nil {
		// The assistant configuration did not load, so no session can run.
		endFailed(ctx, writer, sess)
		return
	}
	entry, ok := d.reg.Get(sess.Provider)
	if !ok {
		endFailed(ctx, writer, sess)
		return
	}
	pkey, err := d.reg.LoadKey(sess.Provider)
	if err != nil {
		d.logger.Warn("provider key", "provider", sess.Provider, "error", err.Error())
	}
	if trigger == loop.TriggerStart {
		if err := sessions.Prepare(ctx, writer, d.relay, d.cfg.Cluster, sess, ta); err != nil {
			d.logger.Warn("team agent first turn", "session", sess.ID.String(), "error", err.Error())
			endFailed(ctx, writer, sess)
			return
		}
	}
	// The outputs and the unattended list are the ones the session was created
	// with, so an edit to the TeamAgent does not change a queued session.
	outputs := stringsOf(sess.Context["outputs"])
	host := sessions.NewHTTPHost(d.cfg.BFFURL, d.key, sess.Principal, ta.UID, sess.ID, outputs, time.Now)
	unattended := stringsOf(sess.Context["unattended"])
	sessionBudget, dayBudget := d.budgetsOf(ta)
	rc := loop.RunConfig{
		Host:           host,
		DecideWrite:    sessions.NewDecision(unattended, outputs),
		DaySpend:       d.daySpend(sess.Team, dayBudget),
		Provider:       entry.Provider,
		Key:            pkey,
		Model:          sess.Model,
		Store:          writer,
		SessionID:      sess.ID,
		Owner:          sess.LeaseOwner,
		SessionBudget:  sessionBudget,
		SystemBase:     sessions.SystemBase(ta),
		StepBudget:     d.cfg.Assistant.Budgets.Steps,
		ApprovalExpiry: d.cfg.Assistant.ApprovalExpiry,
		Tenants:        tenantLookup{d.tenants},
		// Read per call, so a tenant restriction applies to the next read.
		TenantLevels: func(c context.Context) (map[string]agent.DataSharing, error) {
			st, err := d.st.GetSettings(c, agent.ScopeGlobal)
			if err != nil {
				return nil, err
			}
			return st.TenantDataSharing, nil
		},
		Logger: d.logger,
	}
	if err := loop.Run(ctx, rc, trigger); err != nil {
		d.logger.Warn("team agent run", "session", sess.ID.String(), "error", err.Error())
	}
}

// endFailed ends a session that cannot run, under its lease owner.
func endFailed(ctx context.Context, writer agentstore.Store, sess agent.Session) {
	done, _ := agent.NewEvent(agent.EventDone, agent.DonePayload{State: agent.StateFailed, Stop: "error"})
	_, _ = writer.EndSession(ctx, sess.ID, agentstore.EndInput{
		From:       []agent.SessionState{agent.StateRunning},
		LeaseOwner: sess.LeaseOwner,
		State:      agent.StateFailed,
		Events:     []agent.EventInput{done},
	})
}

// stringsOf reads a string list from a session context. A fresh session holds
// it as strings, and a row read back from the store holds it as any.
func stringsOf(v any) []string {
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		out := []string{}
		for _, item := range list {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return []string{}
}

// budgetsOf returns the session and day budgets of one TeamAgent. A TeamAgent
// may lower the chart values; it never raises them.
func (d *runnerDeps) budgetsOf(ta *v1alpha1.TeamAgent) (int64, int64) {
	session := d.cfg.Assistant.Budgets.Session
	day := d.cfg.Assistant.Budgets.TeamDay
	if ta.Spec.Budgets != nil {
		if b := ta.Spec.Budgets.Session; b != nil && *b < session {
			session = *b
		}
		if b := ta.Spec.Budgets.Day; b != nil && *b < day {
			day = *b
		}
	}
	return session, day
}

// daySpend reads the Team's day total and returns it with its limit for the loop.
func (d *runnerDeps) daySpend(team string, limit int64) func(ctx context.Context) (int64, int64, error) {
	return func(ctx context.Context) (int64, int64, error) {
		total, err := d.tenants.TeamDaySpend(ctx, team, time.Now())
		return total, limit, err
	}
}

// teamUIDOf returns the UID of the TeamAgent that a session names in its
// context.
func teamUIDOf(sess agent.Session) string {
	ta, _ := sess.Context["teamagent"].(map[string]any)
	uid, _ := ta["uid"].(string)
	return uid
}

// tenantLookup maps runs and Teams to their tenants for the data-sharing check.
type tenantLookup struct{ s *teamstore.Store }

var _ datashare.TenantResolver = tenantLookup{}

func (l tenantLookup) RunTenant(ctx context.Context, runID string) (string, error) {
	return l.s.RunTenant(ctx, runID)
}

// TeamTenant returns the tenant of a Team: the Team's own name.
func (l tenantLookup) TeamTenant(_ context.Context, team string) (string, error) {
	return team, nil
}

// serveHTTP serves the probes and the metrics.
func serveHTTP(ctx context.Context, port int, pool *pgxpool.Pool, conn *bus.Conn, objs *cache.Cache, ready, consumer *atomic.Bool, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() || !consumer.Load() || !objs.Synced() || !conn.Connected() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		if err := pool.Ping(r.Context()); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/metrics", telemetry.MetricsAuthMiddleware(promhttp.Handler()))
	srv := &http.Server{Addr: ":" + strconv.Itoa(port), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("http server", "error", err.Error())
	}
}

func init() {
	prometheus.MustRegister(triggerCounts, consumerPending, sessions.SessionsTotal, sessions.ToolCalls)
}
