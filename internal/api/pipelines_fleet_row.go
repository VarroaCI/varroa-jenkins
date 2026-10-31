package api

import (
	"encoding/json"
	"fmt"

	"github.com/varroaci/varroa-jenkins/internal/rbac"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// fleetTrigger maps the trigger source and the change request of a run onto
// one of the nine trigger kinds, with its label.
func fleetTrigger(run recordstore.Run, sum runSummaryJSON) FleetRunTrigger {
	cr := sum.ChangeRequest
	switch sum.TriggerSource {
	case triggerSourceUser:
		return FleetRunTrigger{Kind: triggerKindManual, Label: "Manual"}
	case triggerSourceWebhook:
		if cr != nil {
			return FleetRunTrigger{Kind: triggerKindPullRequest, Label: "Pull request #" + cr.ID}
		}
		return FleetRunTrigger{Kind: triggerKindWebhook, Label: "Webhook"}
	case triggerSourceSCM:
		if cr != nil {
			return FleetRunTrigger{Kind: triggerKindPullRequest, Label: "Pull request #" + cr.ID}
		}
		return FleetRunTrigger{Kind: triggerKindPush, Label: "Push"}
	case triggerSourceCron:
		return FleetRunTrigger{Kind: triggerKindCron, Label: "Cron"}
	case triggerSourceUpstream:
		return FleetRunTrigger{Kind: triggerKindUpstream, Label: upstreamLabel(run)}
	case triggerSourceReplay:
		return FleetRunTrigger{Kind: triggerKindReplay, Label: "Replay"}
	case triggerSourceRestart:
		return FleetRunTrigger{Kind: triggerKindRestart, Label: "Restart"}
	default:
		return FleetRunTrigger{Kind: triggerKindOther, Label: "Other"}
	}
}

// upstreamLabel names the upstream run of an upstream-triggered run. The
// stored job path is already tenant-qualified, so the label carries it once.
func upstreamLabel(run recordstore.Run) string {
	if run.UpstreamJob == nil {
		return "Upstream"
	}
	label := "Upstream · " + *run.UpstreamJob
	if run.UpstreamNumber != nil {
		label += fmt.Sprintf(" #%d", *run.UpstreamNumber)
	}
	return label
}

// fleetRunTitle answers the newest commit subject of a run. A run with no
// commit subject answers its job path.
func fleetRunTitle(run recordstore.Run) string {
	if subject := latestChangeSubject(run.Changes); subject != "" {
		return subject
	}
	return run.JobPath
}

// buildFleetRunRow maps one stored run onto a fleet row. railName is the
// rail and URL name of the tenant the run belongs to. canWrite is the tenant
// write check the caller passed for the run's job.
func buildFleetRunRow(run recordstore.Run, railName string, canWrite bool) FleetRunRow {
	sum := buildRunSummary(run)
	row := FleetRunRow{
		runSummaryJSON: sum,
		Cluster:        run.Cluster,
		Tenant:         railName,
		Title:          fleetRunTitle(run),
		Trigger:        fleetTrigger(run, sum),
		CanWrite:       canWrite,
		CreatedAt:      run.CreatedAt,
	}
	if run.Status == "RUNNING" {
		var inputs []recordstore.StoredRunInput
		if json.Unmarshal(run.Inputs, &inputs) == nil && len(inputs) > 0 {
			row.PendingInput = true
		}
	}
	if parsed, err := rbac.ParseTenant(run.Tenant); err == nil {
		row.TenantFolder = parsed.Folder
	}
	if run.QueuedAt != nil && run.StartedAt != nil {
		wait := run.StartedAt.Sub(*run.QueuedAt).Milliseconds()
		row.QueueWaitMs = &wait
	}
	if run.Result != nil && *run.Result == "INTERRUPTED" {
		var interrupted runInterruptedPayload
		if json.Unmarshal(run.Payload, &interrupted) == nil {
			row.InterruptedReason = interrupted.InterruptedReason
		}
	}
	if run.UpstreamTenant != nil {
		up := &FleetRunUpstream{Tenant: *run.UpstreamTenant}
		if run.UpstreamJob != nil {
			up.Job = *run.UpstreamJob
		}
		if run.UpstreamNumber != nil {
			up.BuildNumber = *run.UpstreamNumber
		}
		if run.UpstreamRunID != nil {
			up.RunID = run.UpstreamRunID.String()
		}
		row.Upstream = up
	}
	return row
}
