package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// ledgerActionsRejected counts an action the operator refused (design D15,
// task 9.1a): an unknown kind. Registered once at package init.
var ledgerActionsRejected metric.Int64Counter

func init() { bindLedgerActionMetrics() }

// bindLedgerActionMetrics (re)binds the package-level instrument against
// the current global MeterProvider. It runs once at package init; a test
// re-invokes it after swapping in a test MeterProvider so it exercises
// the real package instrument, not a throwaway local counter.
func bindLedgerActionMetrics() {
	meter := otel.Meter("varroa-operator")
	var err error
	ledgerActionsRejected, err = meter.Int64Counter("varroa_ledger_actions_rejected_total",
		metric.WithDescription("Ledger actions the operator refused, labelled by reason"))
	if err != nil {
		panic("failed to register varroa_ledger_actions_rejected_total counter: " + err.Error())
	}
}

// ledgerActionRunner is the narrow slice of *controller.Reconciler the
// ledger-action subscriber needs, kept as an interface so the mapping in
// handleLedgerAction is unit-testable without a full Reconciler (design
// D8a, task 9.1a).
type ledgerActionRunner interface {
	SendImperativeCommand(ctx context.Context, namespace, name string, cmd *mitev1.ImperativeCommand) error
	WakeHibernatedController(ctx context.Context, namespace, name string)
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
// payload's own entries. Each one carries the path and the epoch of the
// object that holds it, because the run-state object key names both.
func ledgerActionStateFiles(files []recordstore.ResumeFile) []*mitev1.ResumeStateFile {
	out := make([]*mitev1.ResumeStateFile, 0, len(files))
	for _, f := range files {
		out = append(out, &mitev1.ResumeStateFile{Relpath: f.RelPath, PusherEpoch: f.Epoch})
	}
	return out
}

// resumeCommandDeadlineSec bounds one resume command. A command that
// reaches its target after the recorder gave up must not restore a run
// the recorder already marked, and this deadline is what ends it. The
// value leaves the resume's own six-minute deadline room for a retry.
const resumeCommandDeadlineSec = 120

// handleLedgerAction implements design D8a/D7 / the mite-command-transport
// spec's operator subscriber (task 9.1a, task 7.1): it decodes one action
// published on varroa.ledger.actions.<cluster>, ignores an action naming
// another cluster, and maps cancel_run/retry_run/trigger_run to the
// matching imperative command or wake_cell to a direct
// WakeHibernatedController call. trigger_run and resume_run both wake
// the target through the same call before they send the command. An
// unknown kind takes no action and is counted.
//
// The imperative command's subject comes from the action's own namespace
// and controller fields only — no other field ever names the target.
func handleLedgerAction(ctx context.Context, logger *slog.Logger, ownCluster string, rec ledgerActionRunner, data []byte) {
	var action recordstore.LedgerAction
	if err := json.Unmarshal(data, &action); err != nil {
		logger.Warn("ledger action decode failed", "error", err)
		return
	}
	if action.Cluster != ownCluster {
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
			},
		}
		logger.Info("resume_run dispatching",
			"controller", action.Namespace+"/"+action.Controller,
			"instance", action.Instance, "run_id", action.RunID,
			"job_path", action.JobPath, "files", len(action.Files))
		if err := rec.SendImperativeCommand(ctx, action.Namespace, action.Controller, cmd); err != nil {
			logger.Error("ledger action resume_run send failed",
				"controller", action.Namespace+"/"+action.Controller,
				"instance", action.Instance, "run_id", action.RunID, "error", err)
		}
	case recordstore.LedgerActionWakeCell:
		rec.WakeHibernatedController(ctx, action.Namespace, action.Controller)
	default:
		logger.Warn("ledger action unknown kind rejected", "kind", action.Kind)
		ledgerActionsRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "unknown_kind")))
	}
}
