package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// maxDoorRunsPerEvent caps the runs that one re-run event starts. A pull
// request or a check suite that names more branch jobs starts the first
// this many.
const maxDoorRunsPerEvent = 8

// rerunWorkTimeout bounds the work of one accepted re-run event. The work
// runs after the door answers 202, so it has its own clock.
const rerunWorkTimeout = 60 * time.Second

// ghIssueComment is the subset of a GitHub issue_comment delivery that a
// re-run reads.
type ghIssueComment struct {
	Action  string `json:"action"`
	Comment struct {
		Body              string `json:"body"`
		AuthorAssociation string `json:"author_association"`
		User              struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
	Issue struct {
		Number      int `json:"number"`
		PullRequest *struct {
			HTMLURL string `json:"html_url"`
		} `json:"pull_request"`
	} `json:"issue"`
	Repository ghRepository `json:"repository"`
}

// ghRepository names the repository of a delivery.
type ghRepository struct {
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
}

// ghCheckRun is the subset of a GitHub check_run delivery that a re-run reads.
type ghCheckRun struct {
	Action   string `json:"action"`
	CheckRun struct {
		ExternalID string `json:"external_id"`
	} `json:"check_run"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// ghCheckSuite is the subset of a GitHub check_suite delivery that a re-run
// reads.
type ghCheckSuite struct {
	Action     string `json:"action"`
	CheckSuite struct {
		HeadSHA      string `json:"head_sha"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"check_suite"`
	Repository ghRepository `json:"repository"`
	Sender     struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// isRerunEvent reports whether the door consumes the GitHub event itself.
// The door never buffers or replays these events to Jenkins.
func isRerunEvent(event string) bool {
	switch event {
	case "issue_comment", "check_run", "check_suite":
		return true
	}
	return false
}

// tenantTriggers reads the re-run settings of a pipelines spec. A spec with
// no triggers block has no comment pattern and keeps check re-runs on. An
// invalid pattern answers nil, so no comment starts a run.
func tenantTriggers(p *v1alpha1.TenantPipelinesSpec) (*regexp.Regexp, bool) {
	if p == nil || p.Triggers == nil {
		return nil, true
	}
	checksRerun := true
	if p.Triggers.ChecksRerun != nil {
		checksRerun = *p.Triggers.ChecksRerun
	}
	if p.Triggers.CommentPattern == "" {
		return nil, checksRerun
	}
	pattern, err := regexp.Compile(p.Triggers.CommentPattern)
	if err != nil {
		slog.Warn("tenant comment trigger pattern does not compile; comment triggers are off", "error", err)
		return nil, checksRerun
	}
	return pattern, checksRerun
}

// doorTenantRef names the cell that holds a placed tenant. It reads the
// placement the way placedTenant does. An empty placement cluster means the
// local cluster. The bool is false when the tenant holds no cell.
func doorTenantRef(tm *v1alpha1.Tenant, local string) (TenantRef, bool) {
	if tm == nil || tm.Status.Placement == nil || tm.Status.Placement.Cell == "" {
		return TenantRef{}, false
	}
	cluster := tm.Status.Placement.Cluster
	if cluster == "" {
		cluster = local
	}
	return TenantRef{
		Cluster:    cluster,
		Namespace:  tm.Status.Placement.CellNamespace,
		Controller: tm.Status.Placement.Cell,
		Tenant:     tm.Name,
		TenantName: tm.Name,
		Folder:     tm.Name,
	}, true
}

// jobPathFromExternalID reads the job path out of a GitHub check run's
// external id. The id has the form "<job fullName>#<build number>". The
// job must sit under the tenant folder, and the build number must be
// decimal digits.
func jobPathFromExternalID(externalID, tenant string) (string, bool) {
	i := strings.LastIndex(externalID, "#")
	if i < 0 {
		return "", false
	}
	job, number := externalID[:i], externalID[i+1:]
	if !strings.HasPrefix(job, tenant+"/") || len(job) <= len(tenant)+1 {
		return "", false
	}
	if number == "" {
		return "", false
	}
	if _, err := strconv.ParseUint(number, 10, 64); err != nil {
		return "", false
	}
	return job, true
}

// triggerDoorRun publishes one TRIGGER_BUILD for jobPath to the job's home
// member. The run carries the forge actor and the cause text. It uses the
// same intent key shape as a manual run.
func (s *Server) triggerDoorRun(ctx context.Context, ref TenantRef, jobPath, actor, cause string) error {
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("mint run id: %w", err)
	}
	runID := id.String()
	cmd := &mitev1.ImperativeCommand{
		CommandId: "trigger-build-" + runID,
		Type:      mitev1.CommandTypeTriggerBuild,
		TriggerBuild: &mitev1.TriggerBuildPayload{
			RunId:     runID,
			IntentKey: "manual:" + runID,
			JobPath:   jobPath,
			Actor:     actor,
			Cause:     cause,
		},
	}
	target, serr := s.homeRef(ctx, ref, jobPath)
	if serr != nil {
		return fmt.Errorf("resolve the job home: %s", serr.Message)
	}
	return s.publishImperative(ctx, target, cmd)
}

// acceptRerunEvent starts the work of one re-run event in the background.
// The caller answers 202 at once. The work reads the tenant as it stood
// when the door accepted the event.
func (s *Server) acceptRerunEvent(tm *v1alpha1.Tenant, tenant, event, deliveryID string, body []byte) {
	snapshot := tm.DeepCopy()
	work := func() {
		ctx, cancel := context.WithTimeout(context.Background(), rerunWorkTimeout)
		defer cancel()
		s.handleRerunEvent(ctx, snapshot, tenant, event, deliveryID, body)
	}
	if s.rerunSpawn != nil {
		s.rerunSpawn(work)
		return
	}
	go work()
}

// handleRerunEvent applies the trigger rules to one re-run event. Every
// refusal is logged at Info with the tenant, the event, and the delivery id.
// The payload is never logged.
func (s *Server) handleRerunEvent(ctx context.Context, tm *v1alpha1.Tenant, tenant, event, deliveryID string, body []byte) {
	pattern, checksRerun := tenantTriggers(tm.Spec.Pipelines)
	local := ""
	if s.deps.Brood != nil {
		local = s.deps.Brood.LocalCluster()
	}
	ref, ok := doorTenantRef(tm, local)
	if !ok {
		s.refuseRerun(tenant, event, deliveryID, "the tenant holds no cell")
		return
	}
	switch event {
	case "issue_comment":
		s.rerunIssueComment(ctx, ref, tenant, pattern, deliveryID, body)
	case "check_run":
		if !checksRerun {
			s.refuseRerun(tenant, event, deliveryID, "check re-runs are off")
			return
		}
		s.rerunCheckRun(ctx, ref, tenant, deliveryID, body)
	case "check_suite":
		if !checksRerun {
			s.refuseRerun(tenant, event, deliveryID, "check re-runs are off")
			return
		}
		s.rerunCheckSuite(ctx, ref, tenant, deliveryID, body)
	}
}

// rerunIssueComment starts the pull-request job of a writer's new comment
// that matches the tenant's pattern. The association test is a pre-filter.
// The forge permission read decides, and any failure refuses.
func (s *Server) rerunIssueComment(ctx context.Context, ref TenantRef, tenant string, pattern *regexp.Regexp, deliveryID string, body []byte) {
	const event = "issue_comment"
	var p ghIssueComment
	if err := json.Unmarshal(body, &p); err != nil {
		s.refuseRerun(tenant, event, deliveryID, "payload does not decode")
		return
	}
	if p.Action != "created" || p.Issue.PullRequest == nil || p.Issue.PullRequest.HTMLURL == "" {
		return
	}
	if pattern == nil || !pattern.MatchString(strings.TrimSpace(p.Comment.Body)) {
		return
	}
	switch p.Comment.AuthorAssociation {
	case "OWNER", "MEMBER", "COLLABORATOR":
	default:
		s.refuseRerun(tenant, event, deliveryID, "the commenter is not an owner, member, or collaborator")
		return
	}
	if s.deps.ForgePermission == nil || s.deps.RecordStore == nil {
		s.refuseRerun(tenant, event, deliveryID, "the forge permission check is not configured")
		return
	}
	login := p.Comment.User.Login
	reply, err := s.deps.ForgePermission.Permission(ctx, s.deps.CoreCluster, forgeapi.PermissionRequest{
		Tenant: tenant,
		Repo:   p.Repository.HTMLURL,
		Login:  login,
	})
	if err != nil {
		s.refuseRerun(tenant, event, deliveryID, "the permission read failed")
		return
	}
	if !reply.OK {
		s.refuseRerun(tenant, event, deliveryID, "the permission read refused: "+reply.Code)
		return
	}
	if reply.Permission != "admin" && reply.Permission != "write" {
		s.refuseRerun(tenant, event, deliveryID, "the commenter's permission is "+reply.Permission)
		return
	}
	jobs, err := s.deps.RecordStore.ChangeRequestJobs(ctx, ref.Cluster, tenant, p.Issue.PullRequest.HTMLURL)
	if err != nil {
		s.refuseRerun(tenant, event, deliveryID, "the job lookup failed")
		return
	}
	cause := fmt.Sprintf("Started by a pull request comment of %s on %s#%d", login, p.Repository.FullName, p.Issue.Number)
	s.startDoorRuns(ctx, ref, tenant, event, deliveryID, jobs, "github:"+login, cause)
}

// rerunCheckRun starts the job of a check re-run request. The job comes from
// the check's external id, and the id must name a job under the tenant folder.
func (s *Server) rerunCheckRun(ctx context.Context, ref TenantRef, tenant, deliveryID string, body []byte) {
	const event = "check_run"
	var p ghCheckRun
	if err := json.Unmarshal(body, &p); err != nil {
		s.refuseRerun(tenant, event, deliveryID, "payload does not decode")
		return
	}
	if p.Action != "rerequested" {
		return
	}
	jobPath, ok := jobPathFromExternalID(p.CheckRun.ExternalID, tenant)
	if !ok {
		s.refuseRerun(tenant, event, deliveryID, "the external id names no job of this tenant")
		return
	}
	login := p.Sender.Login
	cause := "Started by a GitHub check re-run request of " + login
	s.startDoorRuns(ctx, ref, tenant, event, deliveryID, []string{jobPath}, "github:"+login, cause)
}

// rerunCheckSuite starts the change-request jobs of the suite's pull requests
// and the branch jobs that hold the suite's head commit.
func (s *Server) rerunCheckSuite(ctx context.Context, ref TenantRef, tenant, deliveryID string, body []byte) {
	const event = "check_suite"
	var p ghCheckSuite
	if err := json.Unmarshal(body, &p); err != nil {
		s.refuseRerun(tenant, event, deliveryID, "payload does not decode")
		return
	}
	if p.Action != "rerequested" {
		return
	}
	if s.deps.RecordStore == nil {
		s.refuseRerun(tenant, event, deliveryID, "the job lookup is not configured")
		return
	}
	var jobs []string
	seen := map[string]bool{}
	add := func(list []string) {
		for _, j := range list {
			if !seen[j] {
				seen[j] = true
				jobs = append(jobs, j)
			}
		}
	}
	for _, pr := range p.CheckSuite.PullRequests {
		url := p.Repository.HTMLURL + "/pull/" + strconv.Itoa(pr.Number)
		found, err := s.deps.RecordStore.ChangeRequestJobs(ctx, ref.Cluster, tenant, url)
		if err != nil {
			s.refuseRerun(tenant, event, deliveryID, "the job lookup failed")
			return
		}
		add(found)
	}
	if p.CheckSuite.HeadSHA != "" {
		found, err := s.deps.RecordStore.RevisionJobs(ctx, ref.Cluster, tenant, p.CheckSuite.HeadSHA)
		if err != nil {
			// The pull-request jobs above still run. Only the branch lookup is lost.
			s.refuseRerun(tenant, event, deliveryID, "the branch job lookup failed")
		} else {
			add(found)
		}
	}
	login := p.Sender.Login
	cause := "Started by a GitHub check suite re-run request of " + login
	s.startDoorRuns(ctx, ref, tenant, event, deliveryID, jobs, "github:"+login, cause)
}

// startDoorRuns triggers each job, up to maxDoorRunsPerEvent. A job that
// fails to trigger is logged and skipped, so the others still run.
func (s *Server) startDoorRuns(ctx context.Context, ref TenantRef, tenant, event, deliveryID string, jobs []string, actor, cause string) {
	if len(jobs) == 0 {
		s.refuseRerun(tenant, event, deliveryID, "no job matches the event")
		return
	}
	if len(jobs) > maxDoorRunsPerEvent {
		s.deps.Logger.Info("tenant re-run capped",
			"tenant", tenant, "event", event, "delivery_id", deliveryID,
			"jobs", len(jobs), "limit", maxDoorRunsPerEvent)
		jobs = jobs[:maxDoorRunsPerEvent]
	}
	for _, job := range jobs {
		if err := s.triggerDoorRun(ctx, ref, job, actor, cause); err != nil {
			s.deps.Logger.Warn("tenant re-run trigger failed",
				"tenant", tenant, "event", event, "delivery_id", deliveryID,
				"job_path", job, "error", err)
		}
	}
}

// refuseRerun logs one refused trigger at Info. The reason names the rule
// that refused, and never the payload.
func (s *Server) refuseRerun(tenant, event, deliveryID, reason string) {
	s.deps.Logger.Info("tenant re-run refused",
		"tenant", tenant, "event", event, "delivery_id", deliveryID, "reason", reason)
}
