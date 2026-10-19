package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/accept"
)

// Limits of the Team agent tools. A read returns at most one page of
// teamAgentReadMaxLength bytes.
const (
	teamAgentNoteMaxBytes      = 16384
	teamAgentIssueTitleMax     = 256
	teamAgentIssueBodyMax      = 65536
	teamAgentWebhookMaxBytes   = 16 * 1024
	teamAgentReadMaxLength     = 32768
	teamAgentReadDefaultLength = 32768
)

// TeamAgentSource is the cached view of the TeamAgent, Team, and CatalogSource
// objects. The cache in internal/teamagent/cache implements it.
type TeamAgentSource interface {
	List() []v1alpha1.TeamAgent
	ByUID(uid string) (*v1alpha1.TeamAgent, bool)
	AcceptInput() accept.Input
	CatalogSource(ns, name string) (*v1alpha1.CatalogSource, bool)
	Synced() bool
}

// TeamAgentForge sends a forge request to the operator of a cluster. The
// forgerelay package implements it over the bus.
type TeamAgentForge interface {
	Read(ctx context.Context, cluster string, req forgeapi.ReadRequest) (forgeapi.ReadReply, error)
	Issue(ctx context.Context, cluster string, req forgeapi.IssueRequest) (forgeapi.IssueReply, error)
}

// WebhookEnqueuer asks the recorder to queue one signed tenant webhook.
type WebhookEnqueuer interface {
	Enqueue(ctx context.Context, req recordstore.TeamAgentWebhookRequest) (recordstore.TeamAgentWebhookResponse, error)
}

// busWebhookEnqueuer is the WebhookEnqueuer over the core NATS request-reply
// subject bus.WebhookEnqueueSubject.
type busWebhookEnqueuer struct {
	conn *bus.Conn
}

// NewBusWebhookEnqueuer answers a WebhookEnqueuer over conn.
func NewBusWebhookEnqueuer(conn *bus.Conn) WebhookEnqueuer {
	return &busWebhookEnqueuer{conn: conn}
}

// Enqueue sends one request and decodes the reply.
func (b *busWebhookEnqueuer) Enqueue(ctx context.Context, req recordstore.TeamAgentWebhookRequest) (recordstore.TeamAgentWebhookResponse, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return recordstore.TeamAgentWebhookResponse{}, fmt.Errorf("encode webhook request: %w", err)
	}
	msg, err := b.conn.RequestWithContext(ctx, bus.WebhookEnqueueSubject, data, runNotePostWait)
	if err != nil {
		return recordstore.TeamAgentWebhookResponse{}, fmt.Errorf("request webhook enqueue: %w", err)
	}
	var resp recordstore.TeamAgentWebhookResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return recordstore.TeamAgentWebhookResponse{}, fmt.Errorf("decode webhook reply: %w", err)
	}
	return resp, nil
}

// teamAgentSessionID returns the session of the call, from the assistant loop
// or from a Team agent token. It returns uuid.Nil when neither is set.
func teamAgentSessionID(ctx context.Context) uuid.UUID {
	if sc, ok := agent.SessionContextFrom(ctx); ok && sc.SessionID != uuid.Nil {
		return sc.SessionID
	}
	if sid, ok := auth.AgentSessionFrom(ctx); ok {
		if id, err := uuid.Parse(sid); err == nil {
			return id
		}
	}
	return uuid.Nil
}

// teamAgentSession reads the session row of the call. It answers false when
// the call has no session or the assistant is off. The error is set when the
// call names a session that the store cannot return. A reader that pins its
// input to the session snapshot must refuse the call on that error.
func (s *Server) teamAgentSession(ctx context.Context) (agent.Session, bool, error) {
	id := teamAgentSessionID(ctx)
	if id == uuid.Nil || s.deps.Assistant == nil || s.deps.Assistant.Store == nil {
		return agent.Session{}, false, nil
	}
	sess, err := s.deps.Assistant.Store.GetSession(ctx, id)
	if err != nil {
		return agent.Session{}, false, err
	}
	return sess, true, nil
}

// errSessionUnavailable is the answer of a Team agent tool whose call names
// a session that the store cannot return.
var errSessionUnavailable = &ServiceError{Status: http.StatusServiceUnavailable, Message: "session snapshot is unavailable"}

// callerAgent returns the TeamAgent whose principal is the caller. It answers
// false when the caller is not a Team agent.
func callerAgent(claims *auth.Claims, in accept.Input) (*v1alpha1.TeamAgent, bool) {
	if claims == nil || !accept.IsPrincipal(claims.Subject) {
		return nil, false
	}
	for i := range in.TeamAgents {
		if accept.Principal(&in.TeamAgents[i]) == claims.Subject {
			return &in.TeamAgents[i], true
		}
	}
	return nil, false
}

// teamAgentFor returns the accepted, enabled TeamAgent of team.
func (s *Server) teamAgentFor(team string) (*v1alpha1.TeamAgent, bool) {
	if s.deps.TeamAgents == nil {
		return nil, false
	}
	return accept.AcceptedFor(s.deps.TeamAgents.AcceptInput(), team)
}

// teamAgentsReady answers the 503 for a call made before the cache synced, or
// on a BFF with no Team agent wiring.
func (s *Server) teamAgentsReady() *ServiceError {
	if s.deps.TeamAgents == nil || !s.deps.TeamAgents.Synced() {
		return &ServiceError{Status: http.StatusServiceUnavailable, Message: "team agents are not ready"}
	}
	return nil
}

// ReadRepoFileArgs names one read of a repository file or directory.
type ReadRepoFileArgs struct {
	Team   string
	Path   string
	Repo   string
	Offset int64
	Length int64
}

// ReadRepoFile serves the read_repo_file tool. Inside a session the repository
// is the session's own repo unless the caller names another one, which needs
// the tenant write check. Outside a session the repository is the Team agent's
// instructions source.
func (s *Server) ReadRepoFile(ctx context.Context, claims *auth.Claims, a ReadRepoFileArgs) (any, *ServiceError) {
	if serr := s.teamAgentsReady(); serr != nil {
		return nil, serr
	}
	if s.deps.ForgeRelay == nil {
		return nil, &ServiceError{Status: http.StatusServiceUnavailable, Message: "forge relay not configured"}
	}
	refs, serr := s.resolveTeamTenants(ctx, claims, a.Team)
	if serr != nil || len(refs) == 0 || !s.canReadPipeline(claims, refs[0], "") {
		return nil, &ServiceError{Status: http.StatusNotFound, Message: "team not found"}
	}
	sess, inSession, err := s.teamAgentSession(ctx)
	if err != nil {
		// A session read never falls through to the live CatalogSource.
		return nil, errSessionUnavailable
	}
	revision := ""
	repo := a.Repo
	cluster := s.teamAgentCluster()
	switch {
	case inSession && repo != "":
		if sessRepo, _ := sess.Context["repo"].(string); repo != sessRepo {
			return nil, &ServiceError{Status: http.StatusBadRequest, Message: "repo outside the session"}
		}
		revision, _ = sess.Context["revision"].(string)
		cluster = placementClusterOf(refs, cluster)
	case repo != "":
		if !s.canWritePipeline(claims, refs[0], "") {
			return nil, &ServiceError{Status: http.StatusForbidden, Message: "repo needs the tenant write check outside a session"}
		}
		cluster = placementClusterOf(refs, cluster)
	default:
		if inSession {
			// A session reads only the repository and revision it pinned at
			// creation. The live CatalogSource is never consulted for it.
			repo, _ = sess.Context["repo"].(string)
			revision, _ = sess.Context["revision"].(string)
			if repo == "" {
				return nil, &ServiceError{Status: http.StatusNotFound, Message: "no tracked repo for team " + a.Team}
			}
			break
		}
		ta, ok := s.teamAgentFor(a.Team)
		if !ok || ta.Spec.Instructions == nil {
			return nil, &ServiceError{Status: http.StatusNotFound, Message: "no tracked repo for team " + a.Team}
		}
		src, ok := s.deps.TeamAgents.CatalogSource(ta.Namespace, ta.Spec.Instructions.Source)
		if !ok {
			return nil, &ServiceError{Status: http.StatusNotFound, Message: "no tracked repo for team " + a.Team}
		}
		repo = src.Spec.RepoURL
		revision = src.Status.ObservedRevision
	}
	reply, err := s.deps.ForgeRelay.Read(ctx, cluster, forgeapi.ReadRequest{
		Team: a.Team, Repo: repo, Path: a.Path, Revision: revision, Limit: forgeapi.MaxRead,
	})
	if err != nil {
		return nil, &ServiceError{Status: http.StatusServiceUnavailable, Message: "forge relay: " + err.Error()}
	}
	if !reply.OK {
		return nil, &ServiceError{Status: http.StatusBadGateway, Message: reply.Code + ": " + reply.Error}
	}
	if reply.ReadDir != nil {
		return map[string]any{"revision": reply.Revision, "path": reply.Path, "entries": reply.Entries}, nil
	}
	if reply.ReadFile == nil {
		return nil, &ServiceError{Status: http.StatusBadGateway, Message: "forge returned no content"}
	}
	length := a.Length
	if length <= 0 || length > teamAgentReadMaxLength {
		length = teamAgentReadDefaultLength
	}
	offset := a.Offset
	if offset < 0 {
		offset = 0
	}
	content, sliced := sliceAtRune(reply.Content, offset, length)
	truncated := reply.Truncated || sliced
	return map[string]any{
		"revision":  reply.Revision,
		"path":      reply.Path,
		"size":      reply.Size,
		"offset":    offset,
		"length":    len(content),
		"truncated": truncated,
		"content":   content,
		"encoding":  reply.Encoding,
	}, nil
}

// placementClusterOf returns the cluster of the first tenant ref when it names
// one, and fallback otherwise. An explicit repository reads with the credentials
// of the cluster that hosts the tenant.
func placementClusterOf(refs []TenantRef, fallback string) string {
	if len(refs) > 0 && refs[0].Cluster != "" {
		return refs[0].Cluster
	}
	return fallback
}

// teamAgentCluster is the cluster that answers forge requests: the core.
func (s *Server) teamAgentCluster() string { return s.deps.CoreCluster }

// sliceAtRune returns the bytes from offset with at most length bytes. Both
// ends move to a rune boundary, so the result is valid UTF-8. The second
// result reports whether text was cut.
func sliceAtRune(text string, offset, length int64) (string, bool) {
	n := int64(len(text))
	if offset >= n {
		return "", n > 0
	}
	start := int(offset)
	for start > 0 && start < len(text) && !utf8.RuneStart(text[start]) {
		start--
	}
	end := int(min(int64(start)+length, n))
	for end < len(text) && end > start && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[start:end], int64(start) > 0 || int64(end) < n
}

// PostNoteArgs names the target of one Team agent note.
type PostNoteArgs struct {
	Run  string
	Team string
	Job  string
	Body string
}

// PostNote serves the post_note tool. A run note takes the tenant write check
// for the run's job. A job note takes the tenant read check. An agent caller
// needs the notes output on its own TeamAgent and gets one note per session,
// which the recorder enforces.
func (s *Server) PostNote(ctx context.Context, claims *auth.Claims, a PostNoteArgs) (recordstore.RunNotePostResponse, *ServiceError) {
	if serr := s.teamAgentsReady(); serr != nil {
		return recordstore.RunNotePostResponse{}, serr
	}
	if s.deps.RunNotePoster == nil {
		return recordstore.RunNotePostResponse{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "the record store is not reachable"}
	}
	if len(a.Body) == 0 || len(a.Body) > teamAgentNoteMaxBytes {
		return recordstore.RunNotePostResponse{}, &ServiceError{Status: http.StatusBadRequest, Message: "body must be 1 to 16384 bytes"}
	}
	sub := claims.Subject
	sess, inSession, err := s.teamAgentSession(ctx)
	if err != nil {
		// The recorder keys one note per session; a lost session id
		// would let one session post more.
		return recordstore.RunNotePostResponse{}, errSessionUnavailable
	}
	sessionID := ""
	if inSession {
		sessionID = sess.ID.String()
	}
	in := s.deps.TeamAgents.AcceptInput()
	if self, isAgent := callerAgent(claims, in); isAgent {
		if !self.Spec.Outputs.Notes {
			return recordstore.RunNotePostResponse{}, &ServiceError{Status: http.StatusForbidden, Message: "notes are off for this team agent"}
		}
		if a.Team != "" && a.Team != self.Spec.TeamRef.Name {
			return recordstore.RunNotePostResponse{}, &ServiceError{Status: http.StatusForbidden, Message: "a team agent writes notes for its own team only"}
		}
	}

	post := recordstore.RunNotePostRequest{Principal: sub, SessionID: sessionID, Body: a.Body}
	if a.Run != "" {
		run, serr := s.GetRun(ctx, claims, a.Run)
		if serr != nil {
			return recordstore.RunNotePostResponse{}, serr
		}
		refs, serr := s.resolveTeamTenants(ctx, claims, run.Tenant)
		if serr != nil || len(refs) == 0 || !s.canWritePipeline(claims, refs[0], run.JobPath) {
			return recordstore.RunNotePostResponse{}, &ServiceError{Status: http.StatusForbidden, Message: "the tenant write check refused the note"}
		}
		post.RunID = run.RunID
	} else {
		if a.Team == "" || a.Job == "" {
			return recordstore.RunNotePostResponse{}, &ServiceError{Status: http.StatusBadRequest, Message: "a note names a run, or a team and a job"}
		}
		refs, serr := s.resolveTeamTenants(ctx, claims, a.Team)
		if serr != nil || len(refs) == 0 {
			return recordstore.RunNotePostResponse{}, &ServiceError{Status: http.StatusNotFound, Message: "team not found"}
		}
		ref, ok := s.tenantForJob(claims, refs, a.Job)
		if !ok {
			return recordstore.RunNotePostResponse{}, &ServiceError{Status: http.StatusNotFound, Message: "team not found"}
		}
		post.Team = a.Team
		post.JobPath = a.Job
		post.Cluster = ref.Cluster
		if inSession && sess.Cluster != "" {
			post.Cluster = sess.Cluster
		}
	}
	resp, err := s.deps.RunNotePoster.PostRunNote(ctx, post)
	if err != nil {
		return recordstore.RunNotePostResponse{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "the record store is not reachable"}
	}
	if resp.Error != "" {
		return recordstore.RunNotePostResponse{}, &ServiceError{Status: http.StatusBadRequest, Message: resp.Error}
	}
	return resp, nil
}

// OpenIssueArgs names one issue that a Team agent opens.
type OpenIssueArgs struct {
	Team   string
	Title  string
	Body   string
	Labels []string
}

// OpenIssue serves the open_issue tool. The caller is the team's own agent, or
// a tenant writer. The team's TeamAgent must enable the issues output.
func (s *Server) OpenIssue(ctx context.Context, claims *auth.Claims, a OpenIssueArgs) (forgeapi.IssueReply, *ServiceError) {
	if serr := s.teamAgentsReady(); serr != nil {
		return forgeapi.IssueReply{}, serr
	}
	if s.deps.ForgeRelay == nil {
		return forgeapi.IssueReply{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "forge relay not configured"}
	}
	if a.Team == "" || a.Title == "" || len(a.Title) > teamAgentIssueTitleMax || len(a.Body) > teamAgentIssueBodyMax {
		return forgeapi.IssueReply{}, &ServiceError{Status: http.StatusBadRequest, Message: "team, title, and body must be within their limits"}
	}
	ta, ok := s.teamAgentFor(a.Team)
	if !ok || ta.Spec.Outputs.Issues == nil {
		return forgeapi.IssueReply{}, &ServiceError{Status: http.StatusForbidden, Message: "issues are off for this team"}
	}
	in := s.deps.TeamAgents.AcceptInput()
	if self, isAgent := callerAgent(claims, in); isAgent {
		if self.Spec.TeamRef.Name != a.Team {
			return forgeapi.IssueReply{}, &ServiceError{Status: http.StatusForbidden, Message: "a team agent opens issues for its own team only"}
		}
	} else {
		refs, serr := s.resolveTeamTenants(ctx, claims, a.Team)
		if serr != nil || len(refs) == 0 || !s.canWritePipeline(claims, refs[0], "") {
			return forgeapi.IssueReply{}, &ServiceError{Status: http.StatusForbidden, Message: "the tenant write check refused the issue"}
		}
	}
	reply, err := s.deps.ForgeRelay.Issue(ctx, s.teamAgentCluster(), forgeapi.IssueRequest{
		Team:      a.Team,
		TeamAgent: forgeapi.TeamAgentRef{Namespace: ta.Namespace, Name: ta.Name, UID: string(ta.UID)},
		Title:     a.Title,
		Body:      a.Body,
		Labels:    a.Labels,
		Actor:     ActorFrom(claims),
		SessionID: teamAgentSessionID(ctx).String(),
	})
	if err != nil {
		return forgeapi.IssueReply{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "forge relay: " + err.Error()}
	}
	if !reply.OK {
		return forgeapi.IssueReply{}, &ServiceError{Status: http.StatusBadGateway, Message: reply.Code + ": " + reply.Error}
	}
	return reply, nil
}

// SendWebhookArgs holds the JSON object that one agent webhook carries.
type SendWebhookArgs struct {
	Team string
	Body json.RawMessage
}

// SendWebhook serves the send_webhook tool. It needs the webhook output on the
// team's TeamAgent and a webhook on the team, and queues one signed delivery.
func (s *Server) SendWebhook(ctx context.Context, claims *auth.Claims, a SendWebhookArgs) (string, *ServiceError) {
	if serr := s.teamAgentsReady(); serr != nil {
		return "", serr
	}
	if s.deps.WebhookEnqueuer == nil {
		return "", &ServiceError{Status: http.StatusServiceUnavailable, Message: "the record store is not reachable"}
	}
	if a.Team == "" || len(a.Body) == 0 || len(a.Body) > teamAgentWebhookMaxBytes || !isJSONObjectBody(a.Body) {
		return "", &ServiceError{Status: http.StatusBadRequest, Message: "team is required and body must be a JSON object of at most 16 KiB"}
	}
	ta, ok := s.teamAgentFor(a.Team)
	if !ok || !ta.Spec.Outputs.Webhook {
		return "", &ServiceError{Status: http.StatusForbidden, Message: "the webhook output is off for this team"}
	}
	in := s.deps.TeamAgents.AcceptInput()
	if self, isAgent := callerAgent(claims, in); isAgent && self.Spec.TeamRef.Name != a.Team {
		return "", &ServiceError{Status: http.StatusForbidden, Message: "a team agent sends webhooks for its own team only"}
	}
	if team := in.Teams[a.Team]; team != nil && (team.Spec.Pipelines == nil || team.Spec.Pipelines.Notifications == nil || team.Spec.Pipelines.Notifications.Webhook == nil) {
		return "", &ServiceError{Status: http.StatusBadRequest, Message: "no webhook is set for this team"}
	}
	refs, serr := s.resolveTeamTenants(ctx, claims, a.Team)
	if serr != nil || len(refs) == 0 {
		return "", &ServiceError{Status: http.StatusNotFound, Message: "team not found"}
	}
	if _, isAgent := callerAgent(claims, in); !isAgent && !s.canWritePipeline(claims, refs[0], "") {
		return "", &ServiceError{Status: http.StatusForbidden, Message: "the tenant write check refused the webhook"}
	}
	sess, inSession, err := s.teamAgentSession(ctx)
	if err != nil {
		// A delivery of a session carries that session, never a synthetic one.
		return "", errSessionUnavailable
	}
	req := recordstore.TeamAgentWebhookRequest{
		Cluster:   refs[0].Cluster,
		Tenant:    a.Team,
		Principal: claims.Subject,
		Body:      a.Body,
	}
	if inSession {
		req.SessionID = sess.ID.String()
		if run, ok := sess.Context["run"].(string); ok {
			req.RunID = run
		}
	}
	if req.SessionID == "" {
		req.SessionID = uuid.Must(uuid.NewV7()).String()
	}
	req.CallID = uuid.Must(uuid.NewV7()).String()
	resp, err := s.deps.WebhookEnqueuer.Enqueue(ctx, req)
	if err != nil {
		return "", &ServiceError{Status: http.StatusServiceUnavailable, Message: "the record store is not reachable"}
	}
	if !resp.OK {
		return "", &ServiceError{Status: http.StatusBadRequest, Message: resp.Error}
	}
	return resp.DeliveryID, nil
}

// isJSONObjectBody reports whether raw is one JSON object.
func isJSONObjectBody(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&m); err != nil || m == nil {
		return false
	}
	return errors.Is(dec.Decode(&struct{}{}), io.EOF)
}
