// Package status computes the conditions of each TeamAgent and writes them to
// its status. Only the elected replica writes, so the conditions have one
// author.
package status

import (
	"context"
	"fmt"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/policy"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/accept"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/config"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/instructions"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/spend"
	teamstore "github.com/varroaci/varroa-jenkins/internal/teamagent/store"
)

// reasonActive is the Paused reason of a TeamAgent that may run.
const reasonActive = "Active"

// fetchRetry is how long a failed instructions fetch waits before the ticker
// reads again at the same revision.
const fetchRetry = 10 * time.Minute

// Source is the cached view that the ticker reads.
type Source interface {
	List() []v1alpha1.TeamAgent
	AcceptInput() accept.Input
	CatalogSource(ns, name string) (*v1alpha1.CatalogSource, bool)
}

// Writer patches the status of one TeamAgent.
type Writer interface {
	WriteStatus(ctx context.Context, ta *v1alpha1.TeamAgent, st v1alpha1.TeamAgentStatus) error
}

// Sessions reads the newest session of a principal.
type Sessions interface {
	LastSession(ctx context.Context, principal string) (teamstore.LastSession, bool, error)
}

// Spend reads one Team's total for one UTC day.
type Spend interface {
	TeamDaySpend(ctx context.Context, team string, day time.Time) (int64, error)
}

// Ticker holds the state of one status pass. Tick is not safe for concurrent
// use; RunElected calls it from one goroutine.
type Ticker struct {
	Src       Source
	Out       Writer
	Sessions  Sessions
	Spend     Spend
	Rollover  *spend.Rollover
	Settings  func(ctx context.Context) (agent.Settings, error)
	Placement func(team string) (string, bool)
	Registry  *provider.Registry
	Cfg       config.Config
	Relay     instructions.Reader
	Cluster   string
	Now       func() time.Time

	attempts map[types.UID]attempt
}

// attempt is the last instructions read of one TeamAgent. The ticker reads
// again only when the source revision or the object generation changes, or
// when a fetch failure is older than fetchRetry.
type attempt struct {
	revision   string
	generation int64
	result     instructions.Result
	at         time.Time
}

// Tick runs one pass over the cache. It refreshes the spend rollup first, so
// the day totals it reads include the sessions that just ended.
func (t *Ticker) Tick(ctx context.Context) error {
	if t.attempts == nil {
		t.attempts = map[types.UID]attempt{}
	}
	now := t.Now()
	settings, err := t.Settings(ctx)
	if err != nil {
		return fmt.Errorf("read settings: %w", err)
	}
	in := t.Src.AcceptInput()
	verdicts := accept.Decide(in)
	list := t.Src.List()

	var accepted []string
	for i := range list {
		if v := verdicts[nsName(&list[i])]; v.Accepted {
			accepted = append(accepted, list[i].Spec.TeamRef.Name)
		}
	}
	if t.Rollover != nil {
		if err := t.Rollover.Tick(ctx, accepted, now); err != nil {
			return fmt.Errorf("refresh spend: %w", err)
		}
	}

	for i := range list {
		ta := &list[i]
		st, err := t.statusOf(ctx, ta, verdicts[nsName(ta)], in, settings, now)
		if err != nil {
			return fmt.Errorf("status of %s/%s: %w", ta.Namespace, ta.Name, err)
		}
		if sameStatus(ta.Status, st) {
			continue
		}
		if err := t.Out.WriteStatus(ctx, ta, st); err != nil {
			return fmt.Errorf("write status of %s/%s: %w", ta.Namespace, ta.Name, err)
		}
	}
	return nil
}

// statusOf computes the full status of one TeamAgent.
func (t *Ticker) statusOf(ctx context.Context, ta *v1alpha1.TeamAgent, v accept.Verdict, in accept.Input,
	settings agent.Settings, now time.Time) (v1alpha1.TeamAgentStatus, error) {
	st := v1alpha1.TeamAgentStatus{ObservedGeneration: ta.Generation, InstructionsSHA: ta.Status.InstructionsSHA}
	var conds []metav1.Condition

	pStatus, pReason, pMsg := config.ProviderReady(t.Cfg, t.Registry, ta)
	conds = append(conds, condition(ta, v1alpha1.TeamAgentCondProviderReady, pStatus == metav1.ConditionTrue, pReason, pMsg, now))

	iCond, sha := t.instructionsOf(ctx, ta, now)
	if sha != "" {
		st.InstructionsSHA = sha
	}
	conds = append(conds, iCond)

	conds = append(conds, unattendedOf(ta, settings, now))

	spendToday, err := t.Spend.TeamDaySpend(ctx, ta.Spec.TeamRef.Name, now)
	if err != nil {
		return st, fmt.Errorf("read spend: %w", err)
	}
	pausedReason, pausedMsg := t.pausedReason(ta, v, in, settings, spendToday)
	paused := pausedReason != reasonActive
	conds = append(conds, condition(ta, v1alpha1.TeamAgentCondPaused, paused, pausedReason, pausedMsg, now))

	for i := range conds {
		if prev := findCondition(ta.Status.Conditions, conds[i].Type); prev != nil && prev.Status == conds[i].Status {
			conds[i].LastTransitionTime = prev.LastTransitionTime
		}
	}
	st.Conditions = conds
	st.SpendToday = spendToday
	last, ok, err := t.Sessions.LastSession(ctx, accept.Principal(ta))
	if err != nil {
		return st, fmt.Errorf("read last session: %w", err)
	}
	if ok {
		st.LastSession = &v1alpha1.TeamAgentLastSession{
			ID: last.ID.String(), Trigger: last.Trigger, State: last.State, At: metav1.NewTime(last.StartedAt),
		}
	}
	return st, nil
}

// instructionsOf returns the InstructionsResolved condition and the pinned
// revision. It reads the file only when the attempt memory says a read is due.
func (t *Ticker) instructionsOf(ctx context.Context, ta *v1alpha1.TeamAgent, now time.Time) (metav1.Condition, string) {
	if ta.Spec.Instructions == nil {
		return condition(ta, v1alpha1.TeamAgentCondInstructionsResolved, false, v1alpha1.TeamAgentReasonNotConfigured,
			"the team agent names no instructions source", now), ""
	}
	var src *v1alpha1.CatalogSource
	revision := ""
	if cs, ok := t.Src.CatalogSource(ta.Namespace, ta.Spec.Instructions.Source); ok {
		src = cs
		revision = cs.Status.ObservedRevision
	}
	prev, have := t.attempts[ta.UID]
	due := !have || prev.revision != revision || prev.generation != ta.Generation ||
		(prev.result.Reason == v1alpha1.TeamAgentReasonFetchFailed && now.Sub(prev.at) >= fetchRetry)
	if due {
		res := instructions.Read(context.WithoutCancel(ctx), t.Relay, t.Cluster, ta, src, revision)
		prev = attempt{revision: revision, generation: ta.Generation, result: res, at: now}
		t.attempts[ta.UID] = prev
	}
	res := prev.result
	if res.Reason == v1alpha1.TeamAgentReasonResolved {
		return condition(ta, v1alpha1.TeamAgentCondInstructionsResolved, true, res.Reason,
			"instructions pinned at revision "+res.Revision, now), res.Revision
	}
	return condition(ta, v1alpha1.TeamAgentCondInstructionsResolved, false, res.Reason, res.Message, now), ""
}

// unattendedOf returns the UnattendedAllowed condition. A grant counts only for
// the same object UID, and only for the eligible write tools.
func unattendedOf(ta *v1alpha1.TeamAgent, settings agent.Settings, now time.Time) metav1.Condition {
	var requested []string
	for _, w := range ta.Spec.UnattendedWrites {
		if slices.Contains(policy.UnattendedEligible, w) {
			requested = append(requested, w)
		}
	}
	if len(ta.Spec.UnattendedWrites) == 0 {
		return condition(ta, v1alpha1.TeamAgentCondUnattendedAllowed, true, v1alpha1.TeamAgentReasonNoneRequested,
			"no unattended write is requested", now)
	}
	grant, ok := settings.TeamAgentUnattended[ta.Namespace+"/"+ta.Name]
	granted := 0
	if ok && grant.UID == string(ta.UID) {
		for _, w := range requested {
			if slices.Contains(grant.Tools, w) {
				granted++
			}
		}
	}
	msg := fmt.Sprintf("%d of %d requested tools granted", granted, len(ta.Spec.UnattendedWrites))
	switch {
	case granted == len(ta.Spec.UnattendedWrites) && len(requested) == len(ta.Spec.UnattendedWrites):
		return condition(ta, v1alpha1.TeamAgentCondUnattendedAllowed, true, v1alpha1.TeamAgentReasonGranted, msg, now)
	case granted == 0:
		return condition(ta, v1alpha1.TeamAgentCondUnattendedAllowed, false, v1alpha1.TeamAgentReasonNotGranted, msg, now)
	default:
		return condition(ta, v1alpha1.TeamAgentCondUnattendedAllowed, false, v1alpha1.TeamAgentReasonPartiallyGranted, msg, now)
	}
}

// pausedReason returns the first reason that holds the TeamAgent paused, in
// the order of the design: Duplicate, NamespaceNotOwned, TeamNotFound,
// Disabled, KillSwitch, BudgetDay. It returns reasonActive when none holds.
func (t *Ticker) pausedReason(ta *v1alpha1.TeamAgent, v accept.Verdict, in accept.Input,
	settings agent.Settings, spendToday int64) (string, string) {
	if !v.Accepted && v.Reason == accept.ReasonDuplicate {
		return v1alpha1.TeamAgentReasonDuplicate, "another TeamAgent holds this team"
	}
	if !v.Accepted && v.Reason == accept.ReasonNamespaceNotOwned {
		return v1alpha1.TeamAgentReasonNamespaceNotOwned, "the namespace does not own this team"
	}
	team := ta.Spec.TeamRef.Name
	if in.Teams[team] == nil {
		if _, placed := t.Placement(team); !placed {
			return v1alpha1.TeamAgentReasonTeamNotFound, "the team does not exist"
		}
	}
	if !accept.Enabled(ta) {
		return v1alpha1.TeamAgentReasonDisabled, "the team agent is disabled"
	}
	if settings.Paused {
		return v1alpha1.TeamAgentReasonKillSwitch, "the assistant kill switch is on"
	}
	if spendToday >= dayLimit(t.Cfg, ta) {
		return v1alpha1.TeamAgentReasonBudgetDay, "the team's day budget is spent"
	}
	return reasonActive, "the team agent may run"
}

// dayLimit returns the smaller of the chart day budget and the TeamAgent's own.
func dayLimit(cfg config.Config, ta *v1alpha1.TeamAgent) int64 {
	limit := cfg.Assistant.Budgets.TeamDay
	if ta.Spec.Budgets != nil && ta.Spec.Budgets.Day != nil && *ta.Spec.Budgets.Day < limit {
		limit = *ta.Spec.Budgets.Day
	}
	return limit
}

// findCondition returns the condition of typ, or nil.
func findCondition(conds []metav1.Condition, typ string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == typ {
			return &conds[i]
		}
	}
	return nil
}

// condition builds one condition. Its transition time stays the same while its
// status holds, so an unchanged pass writes nothing.
func condition(ta *v1alpha1.TeamAgent, typ string, ok bool, reason, msg string, now time.Time) metav1.Condition {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	return metav1.Condition{
		Type:               typ,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: ta.Generation,
		LastTransitionTime: metav1.NewTime(now),
	}
}

// sameStatus reports whether two statuses hold the same facts. Transition times
// are not compared.
func sameStatus(a, b v1alpha1.TeamAgentStatus) bool {
	if a.InstructionsSHA != b.InstructionsSHA || a.SpendToday != b.SpendToday || a.ObservedGeneration != b.ObservedGeneration {
		return false
	}
	if len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		x, y := a.Conditions[i], b.Conditions[i]
		if x.Type != y.Type || x.Status != y.Status || x.Reason != y.Reason || x.Message != y.Message {
			return false
		}
	}
	switch {
	case a.LastSession == nil && b.LastSession == nil:
		return true
	case a.LastSession == nil || b.LastSession == nil:
		return false
	}
	return *a.LastSession == *b.LastSession
}

func nsName(ta *v1alpha1.TeamAgent) types.NamespacedName {
	return types.NamespacedName{Namespace: ta.Namespace, Name: ta.Name}
}
