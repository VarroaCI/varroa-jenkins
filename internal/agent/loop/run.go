package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/budget"
	"github.com/varroaci/varroa-jenkins/internal/agent/datashare"
	"github.com/varroaci/varroa-jenkins/internal/agent/policy"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
)

// flushEvery is how often the loop writes the text of a streaming turn.
const flushEvery = 500 * time.Millisecond

// defaultApprovalExpiry applies when the config leaves the expiry at zero.
const defaultApprovalExpiry = 24 * time.Hour

// errStop ends a run quietly. The session is no longer this run's: the lease
// is lost, the session is ended by another writer, or the context is done.
var errStop = errors.New("loop: session no longer held by this run")

type run struct {
	cfg   RunConfig
	st    store.Store
	id    uuid.UUID
	owner string
	guard store.Guard
	log   *slog.Logger
}

// Run executes the session until it ends or parks. It writes the transcript
// and the session state, and it never writes after its lease is lost.
func Run(ctx context.Context, cfg RunConfig, trigger Trigger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r := &run{
		cfg:   cfg,
		st:    cfg.Store,
		id:    cfg.SessionID,
		owner: cfg.Owner,
		guard: store.Guard{States: []agent.SessionState{agent.StateRunning}, LeaseOwner: cfg.Owner},
		log:   cfg.logger().With("session", cfg.SessionID.String(), "trigger", trigger.String()),
	}
	go r.renewLease(ctx, cancel)

	err := r.loop(ctx)
	if errors.Is(err, errStop) {
		r.log.Info("assistant run stopped", "reason", "lease_or_state")
		return nil
	}
	if err != nil && ctx.Err() != nil {
		return nil
	}
	return err
}

// renewLease extends the lease every store.LeaseRenewEvery. A failed renewal
// cancels the run, so the run writes nothing after its lease is gone.
func (r *run) renewLease(ctx context.Context, cancel context.CancelFunc) {
	t := time.NewTicker(store.LeaseRenewEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.st.RenewLease(ctx, r.id, r.owner); err != nil {
				if ctx.Err() == nil {
					r.log.Info("assistant lease lost", "error", err.Error())
				}
				cancel()
				return
			}
		}
	}
}

// mapErr turns a write refused by the guard into errStop. Other errors pass.
func mapErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrConflict) || ctx.Err() != nil {
		return errStop
	}
	return err
}

// still re-reads the session. It answers errStop when the session is no
// longer running under this run's lease.
func (r *run) still(ctx context.Context) error {
	if ctx.Err() != nil {
		return errStop
	}
	cur, err := r.st.GetSession(ctx, r.id)
	if err != nil {
		return mapErr(ctx, err)
	}
	if cur.State != agent.StateRunning || cur.LeaseOwner != r.owner {
		return errStop
	}
	return nil
}

func (r *run) appendEvents(ctx context.Context, evs ...agent.EventInput) error {
	if ctx.Err() != nil {
		return errStop
	}
	_, err := r.st.AppendEvent(ctx, r.id, r.guard, evs...)
	return mapErr(ctx, err)
}

// end ends the session in a final state with at most one error event.
func (r *run) end(ctx context.Context, state agent.SessionState, code agent.ErrorCode, msg, stop string, estimated bool) error {
	var evs []agent.EventInput
	if code != "" {
		e, err := agent.NewEvent(agent.EventError, agent.ErrorPayload{Code: code, Message: msg})
		if err != nil {
			return err
		}
		evs = append(evs, e)
	}
	done, err := agent.NewEvent(agent.EventDone, agent.DonePayload{State: state, Stop: stop, UsageEstimated: estimated})
	if err != nil {
		return err
	}
	evs = append(evs, done)
	if ctx.Err() != nil {
		return errStop
	}
	_, err = r.st.EndSession(ctx, r.id, store.EndInput{
		From: []agent.SessionState{agent.StateRunning}, LeaseOwner: r.owner, State: state, Events: evs,
	})
	return mapErr(ctx, err)
}

// park parks the session for approvals. It writes the approval requests and
// clears the lease in that order.
func (r *run) park(ctx context.Context, approvals []agent.Approval, evs []agent.EventInput) error {
	if ctx.Err() != nil {
		return errStop
	}
	if _, _, err := r.st.CreateApprovals(ctx, r.id, r.owner, approvals, evs); err != nil {
		return mapErr(ctx, err)
	}
	done, err := agent.NewEvent(agent.EventDone, agent.DonePayload{State: agent.StateWaitingApproval})
	if err != nil {
		return err
	}
	_, err = r.st.ClearLease(ctx, r.id, r.owner, []agent.EventInput{done})
	return mapErr(ctx, err)
}

// loop runs provider calls until the model answers without a tool call, a
// budget or a stop ends the session, or a write parks it.
func (r *run) loop(ctx context.Context) error {
	sess, err := r.st.GetSession(ctx, r.id)
	if err != nil {
		return mapErr(ctx, err)
	}
	if sess.State != agent.StateRunning || sess.LeaseOwner != r.owner {
		return errStop
	}
	all, err := r.cfg.Host.List(ctx)
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}
	visible := datashare.Visible(sess.DataSharing, filterVisible(all))

	events, err := r.allEvents(ctx)
	if err != nil {
		return err
	}
	msgs, err := Rebuild(events)
	if err != nil {
		return err
	}
	system := SystemText(r.cfg.SystemBase, sess.Context, sess.DataSharing)

	for {
		cur, err := r.st.GetSession(ctx, r.id)
		if err != nil {
			return mapErr(ctx, err)
		}
		if cur.State != agent.StateRunning || cur.LeaseOwner != r.owner {
			return errStop
		}
		proceed, err := r.preflight(ctx, cur)
		if err != nil || !proceed {
			return err
		}
		usage := budget.Usage{PromptTokens: cur.PromptTokens, CompletionTokens: cur.CompletionTokens, Steps: cur.Steps}
		maxTokens := budget.MaxTokens(usage, r.cfg.SessionBudget)

		turn := cur.Steps + 1
		ch, err := r.cfg.Provider.Stream(ctx, r.cfg.Key, provider.Request{
			Model: r.cfg.Model, System: system, Messages: msgs, Tools: visible, MaxTokens: maxTokens,
		})
		if err != nil {
			if ctx.Err() != nil {
				return errStop
			}
			r.log.Info("assistant provider call failed", "error", err.Error())
			return r.end(ctx, agent.StateFailed, agent.CodeProviderError, "provider call failed", "error", false)
		}
		out, err := r.consume(ctx, ch, turn)
		if err != nil {
			return err
		}
		usedEstimate, err := r.recordUsage(ctx, out, msgs, system)
		if err != nil {
			return err
		}
		if out.err != nil || out.stop == provider.StopError {
			r.log.Info("assistant provider call failed", "error", errText(out.err))
			return r.end(ctx, agent.StateFailed, agent.CodeProviderError, "provider call failed", "error", usedEstimate)
		}
		if out.partial {
			// A tool call with arguments that do not parse cannot run, whatever the stop reason.
			return r.end(ctx, agent.StateFailed, agent.CodeProviderError, "provider sent a tool call with invalid arguments", "error", usedEstimate)
		}
		if len(out.calls) == 0 {
			return r.end(ctx, agent.StateCompleted, "", "", string(out.stop), usedEstimate)
		}

		assistant := provider.Message{Role: provider.RoleAssistant, Text: out.text, ToolCalls: out.calls}
		results, approvals, approvalEvs, err := r.runCalls(ctx, sess, visible, out.calls)
		if err != nil {
			return err
		}
		if len(approvals) > 0 {
			return r.park(ctx, approvals, approvalEvs)
		}
		msgs = append(msgs, assistant)
		msgs = append(msgs, results...)
	}
}

// preflight checks the kill switch and the budgets before a provider call.
// It ends the session and answers false when one of them stops the call.
func (r *run) preflight(ctx context.Context, cur agent.Session) (bool, error) {
	settings, err := r.st.GetSettings(ctx, agent.ScopeGlobal)
	if err != nil {
		return false, mapErr(ctx, err)
	}
	if settings.Paused {
		return false, r.end(ctx, agent.StateCancelled, agent.CodeKillSwitch, "the assistant is paused", "", false)
	}
	total, limit, err := r.cfg.DaySpend(ctx)
	if err != nil {
		return false, fmt.Errorf("day spend: %w", err)
	}
	usage := budget.Usage{PromptTokens: cur.PromptTokens, CompletionTokens: cur.CompletionTokens, Steps: cur.Steps}
	code, spent := budget.Check(usage, budget.Limits{Session: r.cfg.SessionBudget, Steps: r.cfg.StepBudget}, total, limit)
	if !spent {
		return true, nil
	}
	return false, r.end(ctx, agent.StateFailed, code, budgetMessage(code), "", false)
}

func budgetMessage(code agent.ErrorCode) string {
	switch code {
	case agent.CodeBudgetSession:
		return "the session token budget is spent"
	case agent.CodeBudgetDay:
		return "the day token budget is spent"
	case agent.CodeSteps:
		return "the step limit is reached"
	}
	return "a budget is spent"
}

// streamed is the joined output of one provider call.
type streamed struct {
	text  string
	calls []provider.ToolCall
	// partial marks a call whose arguments do not parse.
	partial bool
	usage   provider.Usage
	hasUsed bool
	stop    provider.StopReason
	err     error
}

// consume reads one stream. It writes the text of the turn in chunks, and it
// writes one final chunk when the stream ends.
func (r *run) consume(ctx context.Context, ch <-chan provider.Delta, turn int) (streamed, error) {
	var out streamed
	var pending, all strings.Builder
	type partialCall struct {
		call provider.ToolCall
		args strings.Builder
	}
	partials := map[int]*partialCall{}
	var order []int

	flush := func(final bool) error {
		text := pending.String()
		pending.Reset()
		if text == "" && !final {
			return nil
		}
		p, err := agent.NewEvent(agent.EventAssistant, agent.AssistantPayload{Turn: turn, Text: text, Final: final})
		if err != nil {
			return err
		}
		return r.appendEvents(ctx, p)
	}

	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()
	open := true
	for open {
		select {
		case <-ctx.Done():
			return out, errStop
		case <-ticker.C:
			if err := flush(false); err != nil {
				return out, err
			}
		case d, ok := <-ch:
			if !ok {
				open = false
				break
			}
			if d.Text != "" {
				pending.WriteString(d.Text)
				all.WriteString(d.Text)
			}
			if d.ToolCall != nil {
				idx := d.ToolCall.Index
				p, seen := partials[idx]
				if !seen {
					p = &partialCall{}
					partials[idx] = p
					order = append(order, idx)
				}
				if d.ToolCall.ID != "" {
					p.call.ID = d.ToolCall.ID
				}
				if d.ToolCall.Name != "" {
					p.call.Name = d.ToolCall.Name
				}
				p.args.WriteString(d.ToolCall.ArgumentsFragment)
			}
			if d.Usage != nil {
				out.hasUsed = true
				out.usage.Estimated = d.Usage.Estimated
				if d.Usage.PromptTokens > 0 {
					out.usage.PromptTokens = d.Usage.PromptTokens
				}
				if d.Usage.CompletionTokens > 0 {
					out.usage.CompletionTokens = d.Usage.CompletionTokens
				}
				if d.Usage.CacheCreationTokens > 0 {
					out.usage.CacheCreationTokens = d.Usage.CacheCreationTokens
				}
				if d.Usage.CacheReadTokens > 0 {
					out.usage.CacheReadTokens = d.Usage.CacheReadTokens
				}
			}
			if d.Stop != "" {
				out.stop = d.Stop
			}
			if d.Err != nil {
				out.err = d.Err
			}
		}
	}
	if ctx.Err() != nil {
		return out, errStop
	}
	if err := flush(true); err != nil {
		return out, err
	}
	out.text = all.String()
	for _, idx := range order {
		p := partials[idx]
		args := argsOrEmpty(json.RawMessage(p.args.String()))
		if !json.Valid(args) {
			out.partial = true
		}
		c := p.call
		c.Arguments = args
		out.calls = append(out.calls, c)
	}
	return out, nil
}

// recordUsage adds one call to the session usage. A provider that sends no
// usage gets a size estimate, and the done event says so.
func (r *run) recordUsage(ctx context.Context, out streamed, msgs []provider.Message, system string) (bool, error) {
	prompt, completion := out.usage.PromptTokens, out.usage.CompletionTokens
	estimated := out.usage.Estimated
	if !out.hasUsed {
		prompt, completion, estimated = estimateTokens(msgs, system), int64(len(out.text)/4), true
	}
	if _, err := r.st.RecordStep(ctx, r.id, r.owner, prompt, completion); err != nil {
		return estimated, mapErr(ctx, err)
	}
	r.log.Info("assistant step", "prompt_tokens", prompt, "completion_tokens", completion,
		"cache_read_tokens", out.usage.CacheReadTokens, "cache_creation_tokens", out.usage.CacheCreationTokens,
		"estimated", estimated)
	return estimated, nil
}

func estimateTokens(msgs []provider.Message, system string) int64 {
	n := len(system)
	for _, m := range msgs {
		n += len(m.Text)
	}
	return int64(n / 4)
}

func errText(err error) string {
	if err == nil {
		return "stop error"
	}
	return err.Error()
}

// runCalls runs the tool calls of one turn in order. A read call runs now, a
// write call is queued for approval when the decision says so. It returns the
// result messages of the calls that ran and the approvals to park for.
func (r *run) runCalls(ctx context.Context, sess agent.Session, visible []agent.ToolDef, calls []provider.ToolCall) ([]provider.Message, []agent.Approval, []agent.EventInput, error) {
	var results []provider.Message
	var approvals []agent.Approval
	var approvalEvs []agent.EventInput
	for _, call := range calls {
		if err := r.still(ctx); err != nil {
			return nil, nil, nil, err
		}
		def, known := findTool(visible, call.Name)
		class := agent.ClassRead
		if known {
			class = policy.Class(def)
		}
		tc, err := agent.NewEvent(agent.EventToolCall, agent.ToolCallPayload{
			CallID: call.ID, Name: call.Name, Arguments: call.Arguments, Class: class,
		})
		if err != nil {
			return nil, nil, nil, err
		}
		if err := r.appendEvents(ctx, tc); err != nil {
			return nil, nil, nil, err
		}

		if !known {
			msg, err := r.result(ctx, call, "unknown tool", true, sess.DataSharing)
			if err != nil {
				return nil, nil, nil, err
			}
			results = append(results, msg)
			continue
		}
		if class == agent.ClassWrite && r.cfg.DecideWrite(def, call.Arguments) == WriteApprove {
			ap, ev, err := r.queueApproval(sess, call)
			if err != nil {
				return nil, nil, nil, err
			}
			approvals = append(approvals, ap)
			approvalEvs = append(approvalEvs, ev)
			continue
		}
		msg, err := r.runRead(ctx, sess, def, call)
		if err != nil {
			return nil, nil, nil, err
		}
		results = append(results, msg)
	}
	return results, approvals, approvalEvs, nil
}

// runRead runs one tool call under the data level of its tenant and writes
// the result.
func (r *run) runRead(ctx context.Context, sess agent.Session, def agent.ToolDef, call provider.ToolCall) (provider.Message, error) {
	levels, err := r.cfg.TenantLevels(ctx)
	if err != nil {
		return r.result(ctx, call, "tenant levels are unavailable", true, sess.DataSharing)
	}
	cl, err := datashare.Check(ctx, r.cfg.Tenants, levels, sess.DataSharing, call.Name, call.Arguments)
	if err != nil {
		return r.result(ctx, call, "tenant lookup failed", true, sess.DataSharing)
	}
	if len(datashare.Visible(cl.Level, []agent.ToolDef{def})) == 0 {
		return r.result(ctx, call, datashare.Refusal(cl.Tenant), true, cl.Level)
	}
	callCtx := agent.WithSessionContext(ctx, agent.SessionContext{SessionID: r.id, Values: sess.Context})
	res, err := r.cfg.Host.Call(callCtx, call.Name, call.Arguments)
	if err != nil {
		r.log.Info("assistant tool call failed", "tool", call.Name, "error", err.Error())
		return r.result(ctx, call, "tool call failed", true, cl.Level)
	}
	content := datashare.Filter(call.Name, cl.Level, res.Content)
	return r.result(ctx, call, content, res.IsError, cl.Level)
}

// result writes one tool_result event and returns its message. The content
// is cut to MaxResultBytes before it is stored or sent.
func (r *run) result(ctx context.Context, call provider.ToolCall, content string, isError bool, level agent.DataSharing) (provider.Message, error) {
	content = CutResult(content)
	ev, err := agent.NewEvent(agent.EventToolResult, agent.ToolResultPayload{
		CallID: call.ID, Name: call.Name, Content: content, IsError: isError, DataLevel: level,
	})
	if err != nil {
		return provider.Message{}, err
	}
	if err := r.appendEvents(ctx, ev); err != nil {
		return provider.Message{}, err
	}
	return provider.Message{Role: provider.RoleTool, ToolCallID: call.ID, ToolName: call.Name, Text: content, IsError: isError}, nil
}

// queueApproval builds the approval request of one write call and its event.
func (r *run) queueApproval(sess agent.Session, call provider.ToolCall) (agent.Approval, agent.EventInput, error) {
	id, err := agent.NewID()
	if err != nil {
		return agent.Approval{}, agent.EventInput{}, err
	}
	now := r.cfg.now()
	expiry := r.cfg.ApprovalExpiry
	if expiry <= 0 {
		expiry = defaultApprovalExpiry
	}
	kind, ref := policy.TargetIn(call.Name, call.Arguments, sess.Cluster)
	ap := agent.Approval{
		ID: id, SessionID: r.id, CallID: call.ID, Tool: call.Name, Arguments: call.Arguments,
		TargetKind: kind, TargetRef: ref, State: agent.ApprovalPending,
		RequestedAt: now, ExpiresAt: now.Add(expiry),
	}
	ev, err := agent.NewEvent(agent.EventApprovalRequested, agent.ApprovalRequestedPayload{
		ApprovalID: id.String(), CallID: call.ID, Tool: call.Name,
		TargetKind: kind, TargetRef: ref, ExpiresAt: ap.ExpiresAt,
	})
	if err != nil {
		return agent.Approval{}, agent.EventInput{}, err
	}
	return ap, ev, nil
}

// allEvents reads the whole transcript in pages.
func (r *run) allEvents(ctx context.Context) ([]agent.Event, error) {
	var out []agent.Event
	after := 0
	for {
		page, err := r.st.Events(ctx, r.id, after, 500)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return out, nil
		}
		out = append(out, page...)
		after = page[len(page)-1].Seq
	}
}

// filterVisible keeps the tools that the model may see.
func filterVisible(all []agent.ToolDef) []agent.ToolDef {
	out := make([]agent.ToolDef, 0, len(all))
	for _, t := range all {
		if policy.Visible(t) {
			out = append(out, t)
		}
	}
	return out
}

func findTool(tools []agent.ToolDef, name string) (agent.ToolDef, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return agent.ToolDef{}, false
}
