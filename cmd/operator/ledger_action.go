package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// ledgerActionMetrics holds the instruments of the ledger-action
// subscriber. rejected counts an action the operator refused, labelled by
// reason: "unknown_kind" for a kind the operator does not know, and
// "foreign_cluster" for an action that names another cluster.
type ledgerActionMetrics struct {
	rejected metric.Int64Counter
}

// newLedgerActionMetrics creates the ledger-action instruments on the
// "varroa-operator" meter of mp.
func newLedgerActionMetrics(mp metric.MeterProvider) (*ledgerActionMetrics, error) {
	rejected, err := mp.Meter("varroa-operator").Int64Counter("varroa_ledger_actions_rejected_total",
		metric.WithDescription("Ledger actions the operator refused, labelled by reason"))
	if err != nil {
		return nil, fmt.Errorf("create varroa_ledger_actions_rejected_total counter: %w", err)
	}
	return &ledgerActionMetrics{rejected: rejected}, nil
}

// subscribeLedgerActions queue-subscribes handler to the ledger-action
// subject of cluster only, through a Listener that subscribes again after
// NATS denies the subscription. The operators of every cluster share one core
// NATS and one queue-group name. NATS keeps a separate queue group for
// each subscription subject, so a subject that names one cluster gives
// that cluster's actions only to that cluster's operator replicas. A
// shared wildcard subject would join every cluster to one queue group,
// and NATS would give a share of one cluster's actions to the other
// clusters.
func subscribeLedgerActions(conn *bus.Conn, cluster, queue string, handler bus.SubjectHandler) (*bus.Listener, error) {
	return conn.Listen(bus.LedgerActionSubject(cluster), queue, handler)
}

// ledgerActionRunner is the narrow slice of *controller.Reconciler the
// ledger-action subscriber needs, kept as an interface so the mapping in
// handleLedgerAction is unit-testable without a full Reconciler (design
// D8a, task 9.1a).
type ledgerActionRunner interface {
	SendImperativeCommand(ctx context.Context, namespace, name string, cmd *mitev1.ImperativeCommand) error
	WakeHibernatedController(ctx context.Context, namespace, name string)
	ReleaseRunAgentPods(ctx context.Context, tenant, namespace string, names []string)
	ReleaseInstanceAgentPods(ctx context.Context, tenant, namespace string, keep []string, refreshedBefore time.Time)
}

// commitStatusEnder ends the pending commit statuses of one build. The
// forgeops answerer implements it.
type commitStatusEnder interface {
	EndPendingStatus(ctx context.Context, req forgeapi.CommitStatusRequest) (int, error)
}

// ledgerActionBuildNumber answers *bn, or 0 for a nil bn.
func ledgerActionBuildNumber(bn *int64) int64 {
	if bn == nil {
		return 0
	}
	return *bn
}

// ledgerActionParameters decodes a retry_run or trigger_run action's
// Parameters into the map TriggerBuildPayload carries. An empty or
// unparsable value answers nil, matching an ordinary triggered run with
// no parameters.
func ledgerActionParameters(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var params map[string]string
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil
	}
	return params
}

// ledgerActionDeadlineUnix renders an action's deadline as unix
// seconds, or 0 when the action carries none.
func ledgerActionDeadlineUnix(deadline *time.Time) int64 {
	if deadline == nil {
		return 0
	}
	return deadline.Unix()
}

// ledgerActionStateFiles renders an action's stored files as the command
// payload's own entries. Each one carries the path, the epoch, and the
// push generation of the object that holds it, because the run-state
// object key names all three.
func ledgerActionStateFiles(files []recordstore.ResumeFile) []*mitev1.ResumeStateFile {
	out := make([]*mitev1.ResumeStateFile, 0, len(files))
	for _, f := range files {
		out = append(out, &mitev1.ResumeStateFile{
			Relpath: f.RelPath, PusherEpoch: f.Epoch, StateGen: f.StateGen,
		})
	}
	return out
}

// resumeCommandDeadlineSec bounds one resume command. A command that
// reaches its target after the recorder gave up must not restore a run
// the recorder already marked, and this deadline is what ends it. The
// value leaves the resume's own six-minute deadline room for a retry.
const resumeCommandDeadlineSec = 120

// endStatusSlots bounds the forge writes that run at once on this replica.
// A full set drops the write, as a lost publish does.
var endStatusSlots = make(chan struct{}, 4)

// dispatchEndCommitStatus runs endCommitStatus off the subscriber callback,
// so a slow forge does not hold up the cancel, retry, and wake actions of the
// cluster. When endStatusSlots is full the write is dropped and logged at
// Info. A dropped write leaves the status pending.
//
// Constraint: the write races the retry's own pending status. If the retry's
// pending status lands first and the error write lands after it, the context
// shows error until the retry build completes and writes its own end state,
// which supersedes it. The target_url build-number match keeps the write off
// the retry's own status row.
func dispatchEndCommitStatus(ctx context.Context, logger *slog.Logger, forge commitStatusEnder, action recordstore.LedgerAction) {
	select {
	case endStatusSlots <- struct{}{}:
	default:
		logger.Info("end_commit_status dropped, forge writes at capacity",
			"run_id", action.RunID, "tenant", action.Tenant)
		return
	}
	go func() {
		defer func() { <-endStatusSlots }()
		endCommitStatus(ctx, logger, forge, action)
	}()
}

// endCommitStatus asks the forge answerer to end the pending commit
// statuses of a build that the recorder ended. It runs only when the
// action names the tenant, the repository, the commit, and the build
// number, and the operator has a forge answerer. A failure is logged and
// never retried: the status then stays pending.
func endCommitStatus(ctx context.Context, logger *slog.Logger, forge commitStatusEnder, action recordstore.LedgerAction) {
	if forge == nil || action.Tenant == "" || action.Repo == "" || action.Revision == "" || action.BuildNumber == nil {
		logger.Info("end_commit_status skipped, nothing to end",
			"run_id", action.RunID, "tenant", action.Tenant, "forge", forge != nil)
		return
	}
	fctx, cancel := context.WithTimeout(ctx, forgeRequestTimeout)
	defer cancel()
	written, err := forge.EndPendingStatus(fctx, forgeapi.CommitStatusRequest{
		Tenant:      action.Tenant,
		Repo:        action.Repo,
		SHA:         action.Revision,
		JobPath:     action.JobPath,
		BuildNumber: *action.BuildNumber,
		Reason:      action.Reason,
	})
	if err != nil {
		logger.Warn("end_commit_status failed", "run_id", action.RunID, "tenant", action.Tenant, "error", err)
		return
	}
	logger.Info("end_commit_status done", "run_id", action.RunID, "tenant", action.Tenant, "written", written)
}

// handleLedgerAction implements design D8a/D7 / the mite-command-transport
// spec's operator subscriber (task 9.1a, task 7.1): it decodes one action
// published on varroa.ledger.actions.<cluster>, ignores, logs, and counts
// an action naming another cluster, and maps cancel_run/retry_run/trigger_run to the
// matching imperative command or wake_cell to a direct
// WakeHibernatedController call. trigger_run and resume_run both wake
// the target through the same call before they send the command. An
// unknown kind takes no action and is counted. end_commit_status asks the
// forge answerer to end the pending commit statuses of the build.
// release_instance_agents deletes the idle agent pods of a confirmed gone
// cell instance, except the nodes the action names.
//
// The imperative command's subject comes from the action's own namespace
// and controller fields only — no other field ever names the target.
func handleLedgerAction(ctx context.Context, logger *slog.Logger, metrics *ledgerActionMetrics, subject, ownCluster string, rec ledgerActionRunner, forge commitStatusEnder, data []byte) {
	var action recordstore.LedgerAction
	if err := json.Unmarshal(data, &action); err != nil {
		logger.Warn("ledger action decode failed", "error", err)
		return
	}
	if action.Cluster != ownCluster {
		// The subscription names this cluster only, so this branch is a
		// second guard. An action that reaches it shows a publisher or a
		// subscription fault, and must be visible.
		logger.Warn("ledger action for another cluster rejected",
			"subject", subject, "actionCluster", action.Cluster, "ownCluster", ownCluster)
		metrics.rejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "foreign_cluster")))
		return
	}

	switch action.Kind {
	case recordstore.LedgerActionCancelRun:
		cmd := &mitev1.ImperativeCommand{
			CommandId: "cancel-build-" + action.RunID,
			Type:      mitev1.CommandTypeCancelBuild,
			CancelBuild: &mitev1.CancelBuildPayload{
				RunId:       action.RunID,
				JobPath:     action.JobPath,
				BuildNumber: ledgerActionBuildNumber(action.BuildNumber),
				Actor:       action.Actor,
			},
		}
		if err := rec.SendImperativeCommand(ctx, action.Namespace, action.Controller, cmd); err != nil {
			logger.Warn("ledger action cancel_run send failed",
				"controller", action.Namespace+"/"+action.Controller, "error", err)
		}
	case recordstore.LedgerActionRetryRun:
		cmd := &mitev1.ImperativeCommand{
			CommandId: "trigger-build-" + action.RunID,
			Type:      mitev1.CommandTypeTriggerBuild,
			TriggerBuild: &mitev1.TriggerBuildPayload{
				RunId:      action.RunID,
				IntentKey:  action.IntentKey,
				JobPath:    action.JobPath,
				Parameters: ledgerActionParameters(action.Parameters),
				Actor:      action.Actor,
			},
		}
		if err := rec.SendImperativeCommand(ctx, action.Namespace, action.Controller, cmd); err != nil {
			logger.Warn("ledger action retry_run send failed",
				"controller", action.Namespace+"/"+action.Controller, "error", err)
		}
	case recordstore.LedgerActionTriggerRun:
		// Design D7: the operator wakes a hibernated target through the
		// same in-process routine wake_cell uses, before it issues the
		// command. WakeHibernatedController is a no-op for a controller
		// that is not hibernated, so this call needs no separate power-
		// state read.
		rec.WakeHibernatedController(ctx, action.Namespace, action.Controller)
		cmd := &mitev1.ImperativeCommand{
			CommandId: "trigger-build-" + action.RunID,
			Type:      mitev1.CommandTypeTriggerBuild,
			TriggerBuild: &mitev1.TriggerBuildPayload{
				RunId:      action.RunID,
				IntentKey:  action.IntentKey,
				JobPath:    action.JobPath,
				Parameters: ledgerActionParameters(action.Parameters),
				Actor:      action.Actor,
			},
		}
		if err := rec.SendImperativeCommand(ctx, action.Namespace, action.Controller, cmd); err != nil {
			logger.Warn("ledger action trigger_run send failed",
				"controller", action.Namespace+"/"+action.Controller, "error", err)
		}
	case recordstore.LedgerActionResumeRun:
		// The target must run before it can adopt anything, so the
		// operator wakes it through the same routine trigger_run uses.
		// The namespace and the controller name the target cell, as they
		// do for every other kind.
		//
		// The action's Instance rides the command as a fence. The
		// recorder assigned the run to one pod, and that pod can roll
		// before the command arrives. The replacement then holds the
		// same controller name but a different instance, and it must
		// refuse the command rather than restore a run the recorder
		// gave to another pod.
		rec.WakeHibernatedController(ctx, action.Namespace, action.Controller)
		cmd := &mitev1.ImperativeCommand{
			// The identifier carries the target instance, because one run
			// can hold several resume attempts inside its arm window. The
			// gateway files one pending message for each command
			// identifier on a controller, so two attempts that shared an
			// identifier would file one entry: a late result from the
			// first attempt would then acknowledge the second attempt's
			// message and be recorded against the second target.
			CommandId: "resume-build-" + action.RunID + "-" + action.Instance,
			Type:      mitev1.CommandTypeResumeBuild,
			// The command carries its own deadline, so a delivery that
			// arrives late is abandoned instead of restoring a run the
			// recorder has already given up on. The value leaves the
			// resume's own six-minute deadline room for one retry.
			DeadlineSec: resumeCommandDeadlineSec,
			ResumeBuild: &mitev1.ResumeBuildPayload{
				RunId:       action.RunID,
				JobPath:     action.JobPath,
				BuildNumber: ledgerActionBuildNumber(action.BuildNumber),
				StateFiles:  ledgerActionStateFiles(action.Files),
				Instance:    action.Instance,
				// The absolute expiry, not a duration. DeadlineSec
				// below bounds the work once the mite starts it, and
				// that window opens when the worker picks the command
				// up. A command that waited in the queue needs the
				// moment the recorder gave up, which only this field
				// carries.
				DeadlineUnix: ledgerActionDeadlineUnix(action.Deadline),
				LogNextSeq:   action.LogNextSeq,
			},
		}
		logger.Info("resume_run dispatching",
			"controller", action.Namespace+"/"+action.Controller,
			"instance", action.Instance, "run_id", action.RunID,
			"job_path", action.JobPath, "files", len(action.Files),
			"log_next_seq", action.LogNextSeq)
		if err := rec.SendImperativeCommand(ctx, action.Namespace, action.Controller, cmd); err != nil {
			logger.Error("ledger action resume_run send failed",
				"controller", action.Namespace+"/"+action.Controller,
				"instance", action.Instance, "run_id", action.RunID, "error", err)
		}
	case recordstore.LedgerActionWakeCell:
		rec.WakeHibernatedController(ctx, action.Namespace, action.Controller)
	case recordstore.LedgerActionReleaseAgents:
		// The recorder ended the run. Its agent pods were kept for a
		// resume that can no longer adopt them, so they go now. The
		// operator deletes only the pods the action names, of the
		// action's tenant, in the tenant's agent namespace.
		if action.AgentNamespace == "" || action.Tenant == "" || len(action.AgentPods) == 0 {
			logger.Info("release_agents with no agent pods, nothing to do",
				"run_id", action.RunID, "tenant", action.Tenant)
			return
		}
		rec.ReleaseRunAgentPods(ctx, action.Tenant, action.AgentNamespace, action.AgentPods)
	case recordstore.LedgerActionReleaseInstanceAgents:
		// The operator confirmed the instance gone. Its idle pods have no node on
		// any JVM. The recorder names the nodes a resume can still adopt, and
		// those pods stay.
		if action.AgentNamespace == "" || action.Tenant == "" || action.RefreshedBefore == nil {
			logger.Info("release_instance_agents incomplete, nothing to do",
				"tenant", action.Tenant, "instance", action.Instance)
			return
		}
		rec.ReleaseInstanceAgentPods(ctx, action.Tenant, action.AgentNamespace, action.KeepPods, *action.RefreshedBefore)
	case recordstore.LedgerActionEndCommitStatus:
		dispatchEndCommitStatus(ctx, logger, forge, action)
	default:
		logger.Warn("ledger action unknown kind rejected", "kind", action.Kind)
		metrics.rejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "unknown_kind")))
	}
}
