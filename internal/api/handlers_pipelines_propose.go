package api

import (
	"encoding/json"
	"net/http"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/datashare"
	"github.com/varroaci/varroa-jenkins/internal/agent/prompts"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	"github.com/varroaci/varroa-jenkins/internal/forgeurl"
)

// proposeErrDataSharingTooLow is the code of a proposal that the data level
// of the tenant and the provider hides from the assistant.
const proposeErrDataSharingTooLow = "data_sharing_too_low"

// proposeMaxBody bounds the body of a proposal request.
const proposeMaxBody = 4 << 10

// proposePipelineRequest is the body of POST /pipelines/{tenant}/propose.
type proposePipelineRequest struct {
	Repo string `json:"repo"`
}

// proposePipelineResponse answers a started proposal.
type proposePipelineResponse struct {
	SessionID    string `json:"sessionId"`
	State        string `json:"state"`
	Repo         string `json:"repo"`
	Forge        string `json:"forge"`
	BaseBranch   string `json:"baseBranch"`
	CredentialID string `json:"credentialId"`
}

// tenantPipelinesTenant returns the Tenant pipelines tenant of tenant among refs.
func tenantPipelinesTenant(refs []TenantRef, tenant string) (TenantRef, bool) {
	for _, ref := range refs {
		if ref.Tenant == tenant {
			return ref, true
		}
	}
	return TenantRef{}, false
}

// forgeCodeStatus maps a forge reply code onto an HTTP status. The propose route,
// open_pull_request, and open_issue share it.
func forgeCodeStatus(code string) int {
	switch code {
	case forgeapi.CodeRepoURLInvalid, forgeapi.CodeInvalidRequest:
		return http.StatusBadRequest
	case forgeapi.CodeNoMatchingCredential, forgeapi.CodeForgeUnsupported,
		forgeapi.CodeForgeAuthFailed, forgeapi.CodeForgeWriteDenied:
		return http.StatusUnprocessableEntity
	case forgeapi.CodeJenkinsfileExists, forgeapi.CodeProposalBranchExists, forgeapi.CodeProposalBaseMoved:
		return http.StatusConflict
	case forgeapi.CodeNotFound:
		return http.StatusNotFound
	case forgeapi.CodeForgeUnreachable:
		return http.StatusBadGateway
	case forgeapi.CodeRequestExpired:
		return http.StatusGatewayTimeout
	case forgeapi.CodePullRequestBusy:
		return http.StatusTooManyRequests
	}
	return http.StatusInternalServerError
}

// readRepoFileVisible reports whether the level shows read_repo_file. The
// tool's data class decides the level: a metadata class needs metadata, and
// any other class needs logs.
func readRepoFileVisible(level agent.DataSharing) bool {
	class, known := datashare.ClassOf("read_repo_file")
	if known && (class == datashare.ClassMetadata || class == datashare.ClassMetadataFiltered) {
		return datashare.Rank(level) >= 1
	}
	return datashare.Rank(level) == 2
}

// handleProposePipeline serves POST /pipelines/{tenant}/propose. It runs the
// refusals of proposal section 5.5 in order. Nothing is written before the
// session starts, so a refusal creates no session row.
func (s *Server) handleProposePipeline(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if !s.assistantCanUse(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	var body proposePipelineRequest
	r.Body = http.MaxBytesReader(w, r.Body, proposeMaxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeAssistantError(s, w, http.StatusBadRequest, forgeapi.CodeInvalidRequest, "invalid request body")
		return
	}
	ctx := r.Context()
	user, err := rt.Store.GetSettings(ctx, agent.UserScope(claims.Subject))
	if err != nil {
		s.assistantStoreError(w, err)
		return
	}
	if !user.Enabled {
		writeAssistantError(s, w, http.StatusConflict, assistantErrOff, "the assistant is off for this user")
		return
	}

	tenant := r.PathValue("tenant")
	refs, serr := s.resolveTenantTenants(ctx, claims, tenant)
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	ref, ok := tenantPipelinesTenant(refs, tenant)
	if !ok || !s.canReadPipeline(claims, ref, "") {
		s.writeJSONError(w, http.StatusNotFound, "tenant not found")
		return
	}
	if !s.canWritePipeline(claims, ref, "") {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	u, err := forgeurl.NormalizeRepoURL(body.Repo)
	if err != nil {
		writeAssistantError(s, w, http.StatusBadRequest, forgeapi.CodeRepoURLInvalid, err.Error())
		return
	}

	global, err := rt.Store.GetSettings(ctx, agent.ScopeGlobal)
	if err != nil {
		s.assistantStoreError(w, err)
		return
	}
	_, _, entry, err := rt.chooseProvider("", "", user, global)
	if err != nil {
		s.writeAssistantStartError(w, err)
		return
	}
	if !readRepoFileVisible(rt.sessionLevel(user, global, ref.Tenant, entry)) {
		writeAssistantError(s, w, http.StatusConflict, proposeErrDataSharingTooLow,
			"the data level of this tenant and provider hides read_repo_file")
		return
	}

	if s.deps.ForgeRelay == nil {
		writeAssistantError(s, w, http.StatusBadGateway, forgeapi.CodeForgeUnreachable, "the forge relay is not configured")
		return
	}
	cluster := ref.Cluster
	if cluster == "" {
		cluster = s.deps.CoreCluster
	}
	check, err := s.deps.ForgeRelay.Check(ctx, cluster, forgeapi.CheckRequest{Tenant: tenant, Repo: u.String()})
	if err != nil && isOperatorUnavailable(err) {
		writeAssistantError(s, w, http.StatusServiceUnavailable, codeOperatorUnavailable, "no operator received the forge check")
		return
	}
	if err != nil && isOperatorTimeout(err) {
		writeAssistantError(s, w, http.StatusGatewayTimeout, codeOperatorTimeout, "the operator did not answer in time")
		return
	}
	if err != nil {
		writeAssistantError(s, w, http.StatusBadGateway, forgeapi.CodeForgeUnreachable, "the forge relay failed")
		return
	}
	if !check.OK {
		writeAssistantError(s, w, forgeCodeStatus(check.Code), check.Code, check.Error)
		return
	}

	sealed := ""
	if c, err := r.Cookie(AssistantKeyCookie); err == nil {
		sealed = c.Value
	}
	prompt, _ := prompts.Get(prompts.ProposePipeline)
	sess, err := s.createAssistantSession(ctx, rt, claims, sessionStart{
		Values: map[string]any{
			agent.CtxTenant: tenant,
			agent.CtxRoute:  "/pipelines/" + tenant,
			agent.CtxRepo:   u.String(),
			agent.CtxPrompt: prompts.ProposePipeline,
			// The session reads and proposes against the commit that the check read.
			"revision":     check.Revision,
			"baseRevision": check.Revision,
		},
		First: prompt.Text,
	}, sealed)
	if err != nil {
		s.writeAssistantStartError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, proposePipelineResponse{
		SessionID:    sess.ID.String(),
		State:        string(sess.State),
		Repo:         u.String(),
		Forge:        check.Forge,
		BaseBranch:   check.BaseBranch,
		CredentialID: check.CredentialID,
	})
}
