package triggers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/datashare"
	"github.com/varroaci/varroa-jenkins/internal/agent/policy"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	agentstore "github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/tenantagent/accept"
	"github.com/varroaci/varroa-jenkins/internal/tenantagent/config"
	tenantstore "github.com/varroaci/varroa-jenkins/internal/tenantagent/store"
)

// CreateOutcome reports what Create did with one match.
type CreateOutcome string

const (
	// Created means a session row runs and needs a worker.
	Created CreateOutcome = "created"
	// Skipped means a session row exists in the skipped state.
	Skipped CreateOutcome = "skipped"
	// Deduped means a session already exists for this trigger event.
	Deduped CreateOutcome = "deduped"
	// Debounced means a session for this item started inside the debounce window.
	Debounced CreateOutcome = "debounced"
	// Capped means the principal already runs its maximum of sessions.
	Capped CreateOutcome = "capped"
)

// defaultDebounce is the debounce window when a trigger names none.
const defaultDebounce = 10 * time.Minute

// dedupeWindow bounds the dedupe check. A trigger event older than this is not
// deduplicated against an earlier session.
const dedupeWindow = 2 * time.Hour

// Creator writes the session row of a match. It holds the advisory lock of the
// match key for the whole check, so two replicas cannot both start one session.
type Creator struct {
	Store    *agentstore.Postgres
	Tenants  *tenantstore.Store
	Cfg      config.Config
	Registry *provider.Registry
	// Settings reads the global settings: the kill switch and the tenant levels.
	Settings func(ctx context.Context) (agent.Settings, error)
	// Owner is this replica's lease owner, used for the rows it starts.
	Owner string
	// Image is the digest of the running image, recorded on each session.
	Image string
	// Source returns the repository and the pinned revision of the TenantAgent's
	// instructions source. Nil leaves both empty.
	Source func(ta *v1alpha1.TenantAgent) (repo, revision string)
	Now    func() time.Time
}

// Create runs the create path of one match. It returns the session row when
// one is written, and the outcome.
func (c *Creator) Create(ctx context.Context, m Match) (agent.Session, CreateOutcome, error) {
	now := c.Now()
	key := fmt.Sprintf("tenantagent:%s/%s:%s:%s", m.TA.Namespace, m.TA.Name, m.Trigger, m.ItemKey)
	var outcome CreateOutcome
	settings, err := c.Settings(ctx)
	if err != nil {
		return agent.Session{}, "", fmt.Errorf("read settings: %w", err)
	}
	sess, _, created, err := c.Store.CreateUnderLock(ctx, key,
		func(ctx context.Context, tx pgx.Tx) (agent.Session, []agent.EventInput, bool, error) {
			out, s, in, ok, err := c.decide(ctx, tx, m, settings, now)
			outcome = out
			return s, in, ok, err
		})
	if err != nil {
		return agent.Session{}, "", err
	}
	if !created {
		return agent.Session{}, outcome, nil
	}
	if sess.State == agent.StateSkipped {
		return sess, Skipped, nil
	}
	return sess, Created, nil
}

// decide runs the checks under the lock and builds the row. It returns ok false
// with an outcome when no row should be written.
func (c *Creator) decide(ctx context.Context, tx pgx.Tx, m Match, settings agent.Settings, now time.Time) (CreateOutcome, agent.Session, []agent.EventInput, bool, error) {
	tenant := m.Tenant
	principal := accept.Principal(m.TA)
	if n, err := countWhere(ctx, tx, `tenant = $1 AND started_at >= $2 AND context->>'trigger_event' = $3`,
		tenant, now.Add(-dedupeWindow), m.EventKey); err != nil {
		return "", agent.Session{}, nil, false, err
	} else if n > 0 {
		return Deduped, agent.Session{}, nil, false, nil
	}
	if m.Trigger != ManualTrigger {
		window := debounceOf(m.TA, m.Trigger)
		if n, err := countWhere(ctx, tx,
			`principal = $1 AND trigger = $2 AND context->>'item_key' = $3 AND started_at >= $4`,
			principal, m.Trigger, m.ItemKey, now.Add(-window)); err != nil {
			return "", agent.Session{}, nil, false, err
		} else if n > 0 {
			return Debounced, agent.Session{}, nil, false, nil
		}
	}
	running, err := countWhere(ctx, tx, `tenant = $1 AND principal = $2 AND state = 'running'`, tenant, principal)
	if err != nil {
		return "", agent.Session{}, nil, false, err
	}
	if running >= agent.MaxRunningSessions {
		return Capped, agent.Session{}, nil, false, nil
	}

	provName, endpoint, reg := c.endpoint(m.TA)
	level := dataLevel(m.TA, endpoint, settings, cfgCeiling(c.Cfg), m.Tenant)
	dayTotal, err := spendToday(ctx, tx, tenant, now)
	if err != nil {
		return "", agent.Session{}, nil, false, err
	}
	skip, start := agent.DecideCreate(agent.CreateInput{
		Paused:            settings.Paused,
		DayTotal:          dayTotal,
		DayLimit:          dayLimit(c.Cfg, m.TA),
		DataLevel:         level,
		ProviderAvailable: reg && provAvailable(c.Registry, provName),
	})

	runID, err := c.Tenants.RunID(ctx, m.Tenant, m.Event.ItemPath, m.Event.BuildNumber)
	if err != nil {
		return "", agent.Session{}, nil, false, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", agent.Session{}, nil, false, fmt.Errorf("new session id: %w", err)
	}
	gen := m.TA.Generation
	repo, revision := "", ""
	if c.Source != nil {
		repo, revision = c.Source(m.TA)
	}
	s := agent.Session{
		ID:                    id,
		Kind:                  agent.KindTenantAgent,
		Principal:             principal,
		Tenant:                tenant,
		Cluster:               m.Event.Cluster,
		Trigger:               m.Trigger,
		Provider:              provName,
		Model:                 modelOf(m.TA, endpoint),
		KeySource:             agent.KeySourceChart,
		DataSharing:           level,
		TenantAgentGeneration: &gen,
		Context: map[string]any{
			"run":           runID,
			"job":           jobOf(m),
			"cell":          m.Event.Controller,
			"repo":          repo,
			"revision":      revision,
			"item_key":      m.ItemKey,
			"message":       m.Event.Message,
			"build":         m.Event.BuildNumber,
			"trigger_event": m.EventKey,
			"unattended":    unattendedFor(m.TA, settings),
			"outputs":       accept.OutputKinds(m.TA.Spec.Outputs),
			"runner_image":  c.Image,
			"tenantagent":   map[string]any{"namespace": m.TA.Namespace, "name": m.TA.Name, "uid": string(m.TA.UID)},
		},
		StartedAt: now,
	}
	if !start {
		s.State = agent.StateSkipped
		s.SkipReason = skip
		s.EndedAt = &now
		user, _ := agent.NewEvent(agent.EventUser, agent.UserPayload{Text: "Trigger " + m.Trigger + " on " + m.ItemKey})
		done, _ := agent.NewEvent(agent.EventDone, agent.DonePayload{State: agent.StateSkipped})
		return Skipped, s, []agent.EventInput{user, done}, true, nil
	}
	s.State = agent.StateRunning
	s.LeaseOwner = c.Owner
	expires := now.Add(agentstore.LeaseTTL)
	s.LeaseExpiresAt = &expires
	return Created, s, nil, true, nil
}

// endpoint returns the provider name, its endpoint, and whether the name is in
// the registry. The TenantAgent's provider wins over the registry default.
func (c *Creator) endpoint(ta *v1alpha1.TenantAgent) (string, provider.Endpoint, bool) {
	if c.Registry == nil {
		return ta.Spec.Provider, provider.Endpoint{}, false
	}
	name := ta.Spec.Provider
	if name == "" {
		if def, ok := c.Registry.Default(); ok {
			name = def.Endpoint.Name
		}
	}
	e, ok := c.Registry.Get(name)
	return name, e.Endpoint, ok
}

func provAvailable(reg *provider.Registry, name string) bool {
	return reg != nil && reg.Available(name)
}

// debounceOf returns the debounce window of a trigger on a TenantAgent.
func debounceOf(ta *v1alpha1.TenantAgent, trigger string) time.Duration {
	for _, t := range ta.Spec.Triggers {
		if t.Type == trigger && t.Debounce != nil {
			return t.Debounce.Duration
		}
	}
	return defaultDebounce
}

// dayLimit returns the smaller of the chart budget and the TenantAgent budget.
func dayLimit(cfg config.Config, ta *v1alpha1.TenantAgent) int64 {
	limit := cfg.Assistant.Budgets.TenantDay
	if ta.Spec.Budgets != nil && ta.Spec.Budgets.Day != nil && *ta.Spec.Budgets.Day < limit {
		limit = *ta.Spec.Budgets.Day
	}
	return limit
}

// cfgCeiling returns the external data-sharing ceiling of the chart.
func cfgCeiling(cfg config.Config) agent.DataSharing {
	return cfg.Assistant.ExternalDataSharing
}

// dataLevel returns the effective data level of a session for the TenantAgent.
func dataLevel(ta *v1alpha1.TenantAgent, e provider.Endpoint, settings agent.Settings, ceiling agent.DataSharing, tenant string) agent.DataSharing {
	return datashare.Effective(datashare.Inputs{
		User:            agent.DataSharing(ta.Spec.DataSharing),
		Tenant:          tenant,
		TenantLevels:    settings.TenantDataSharing,
		Trust:           e.Trust,
		ExternalCeiling: ceiling,
	})
}

// modelOf returns the model the TenantAgent names, or the endpoint default.
func modelOf(ta *v1alpha1.TenantAgent, e provider.Endpoint) string {
	if ta.Spec.Model != "" {
		return ta.Spec.Model
	}
	return e.DefaultModel()
}

// jobOf returns the job path when the row names a job, else "".
func jobOf(m Match) string {
	if m.Event.ItemPath != m.Tenant {
		return m.Event.ItemPath
	}
	return ""
}

// unattendedFor returns the write tools that may run without an approval: the
// TenantAgent's list, cut to its global grant with the same UID and to the
// eligible set.
func unattendedFor(ta *v1alpha1.TenantAgent, settings agent.Settings) []string {
	grant, ok := settings.TenantAgentUnattended[ta.Namespace+"/"+ta.Name]
	if !ok || grant.UID != string(ta.UID) {
		return []string{}
	}
	out := []string{}
	for _, t := range ta.Spec.UnattendedWrites {
		if slices.Contains(grant.Tools, t) && slices.Contains(policy.UnattendedEligible, t) {
			out = append(out, t)
		}
	}
	return out
}

// countWhere counts the rows of agent_sessions that match where.
func countWhere(ctx context.Context, tx pgx.Tx, where string, args ...any) (int, error) {
	var n int
	err := tx.QueryRow(ctx, "SELECT count(*) FROM varroa.agent_sessions WHERE "+where, args...).Scan(&n)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("count sessions: %w", err)
	}
	return n, nil
}

// spendToday sums the Tenant's sessions for the UTC day of now, inside tx. The
// sum is live, so a session that ended a moment ago counts against the budget.
func spendToday(ctx context.Context, tx pgx.Tx, tenant string, now time.Time) (int64, error) {
	u := now.UTC()
	start := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	var tokens int64
	err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(prompt_tokens + completion_tokens), 0) FROM varroa.agent_sessions
		 WHERE kind = 'tenant_agent' AND tenant = $1 AND started_at >= $2 AND started_at < $3`,
		tenant, start, start.AddDate(0, 0, 1)).Scan(&tokens)
	if err != nil {
		return 0, fmt.Errorf("read tenant spend: %w", err)
	}
	return tokens, nil
}
