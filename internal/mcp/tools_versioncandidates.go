//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// =============================================================================
// Version candidate tools (3) — upgrade tracking
//
// ProfileCandidate is cluster-scoped and clusterless in the same sense the
// version-candidates BFF routes are: it is resolved directly against
// deps.Store, never through deps.ConfigBrood, so these tools take no
// "cluster" parameter unlike list_jenkins_version_profile/get_user's peers.
// =============================================================================

// versionCandidateSummary is the default projection for list_version_candidates.
type versionCandidateSummary struct {
	Name            string `json:"name"`
	ProfileRef      string `json:"profileRef"`
	ObservedVersion string `json:"observedVersion"`
	ResolveVersion  string `json:"resolveVersion"`
	Phase           string `json:"phase"`
	PromotedAt      string `json:"promotedAt,omitempty"`
}

// summarizeVersionCandidate projects a ProfileCandidate CR down to its summary.
func summarizeVersionCandidate(c *v1alpha1.ProfileCandidate) versionCandidateSummary {
	if c == nil {
		return versionCandidateSummary{}
	}
	promotedAt := ""
	if c.Status.PromotedAt != nil {
		promotedAt = c.Status.PromotedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return versionCandidateSummary{
		Name:            c.Name,
		ProfileRef:      c.Spec.ProfileRef,
		ObservedVersion: c.Spec.ObservedVersion,
		ResolveVersion:  c.Spec.ResolveVersion,
		Phase:           string(c.Status.Phase),
		PromotedAt:      promotedAt,
	}
}

var versionCandidateListOutputSchema = listOutputSchema("Default summary projection.", []schemaField{
	{Name: "name", Type: "string", Desc: "Candidate name."},
	{Name: "profileRef", Type: "string", Desc: "JenkinsVersionProfile this candidate would promote into."},
	{Name: "observedVersion", Type: "string", Desc: "Profile's resolveVersion at discovery time."},
	{Name: "resolveVersion", Type: "string", Desc: "Newly discovered patch this candidate would promote the profile to."},
	{Name: "phase", Type: "string", Desc: "Pending, Ready, Promoted, Failed, or Superseded."},
	{Name: "promotedAt", Type: "string", Desc: "RFC3339 UTC; set once Phase becomes Promoted."},
})

func registerVersionCandidateTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listVC := mcp.NewTool("list_version_candidates",
		mcp.WithDescription("List ProfileCandidate CRDs — discovered upstream patch/LTS-line "+
			"candidates for JenkinsVersionProfiles, pending validation and promotion. Returns a "+
			"compact summary per candidate (name, profileRef, observedVersion, resolveVersion, "+
			"phase, promotedAt). Use get_version_candidate for the full resource, including "+
			"conditions and the advisory pre-flight report."),
		mcp.WithBoolean("verbose", mcp.Description("Return full ProfileCandidate resources "+
			"instead of summaries. Prefer get_version_candidate for detail on a specific candidate.")),
		withListOutput(versionCandidateListOutputSchema),
	)
	addTool(mcpServer, kindRead, listVC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if deps.Authorizer == nil || !deps.Authorizer.CanUpdateVersionProfile(claims) {
			return mcp.NewToolResultError("access denied: missing version-profiles:update permission"), nil
		}
		items, err := crdstore.List[v1alpha1.ProfileCandidate](ctx, deps.Store, "", "")
		if err != nil {
			return mcp.NewToolResultError("failed to list version candidates: " + err.Error()), nil
		}
		if verbose, _ := getArgs(req)["verbose"].(bool); verbose {
			return resultJSON(items)
		}
		summaries := make([]versionCandidateSummary, 0, len(items))
		for _, c := range items {
			summaries = append(summaries, summarizeVersionCandidate(c))
		}
		return resultJSON(summaries)
	})

	getVC := mcp.NewTool("get_version_candidate",
		mcp.WithDescription("Get a specific ProfileCandidate CRD, including its conditions and "+
			"advisory pre-flight report"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Candidate name")),
	)
	addTool(mcpServer, kindRead, getVC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if deps.Authorizer == nil || !deps.Authorizer.CanUpdateVersionProfile(claims) {
			return mcp.NewToolResultError("access denied: missing version-profiles:update permission"), nil
		}
		name := strArg(getArgs(req), "name")
		candidate, err := crdstore.Get[v1alpha1.ProfileCandidate](ctx, deps.Store, name, "")
		if err != nil {
			return mcp.NewToolResultError("version candidate not found: " + err.Error()), nil
		}
		return resultJSON(candidate)
	})

	promoteVC := mcp.NewTool("promote_version_candidate",
		mcp.WithDescription("Promote a Ready ProfileCandidate: advances its JenkinsVersionProfile "+
			"to the candidate's resolved version and closure content, then supersedes every other "+
			"open candidate for the same profile. Every write in the sequence is resourceVersion-"+
			"scoped; a concurrent modification aborts the whole promotion, and Spec.ResolveVersion "+
			"is left advanced (not rolled back) if re-materialization fails, so the promotion can "+
			"be retried once the underlying issue is fixed."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Candidate name")),
	)
	addTool(mcpServer, kindAction, promoteVC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		if deps.Authorizer == nil || !deps.Authorizer.CanUpdateVersionProfile(claims) {
			return mcp.NewToolResultError("access denied: missing version-profiles:update permission"), nil
		}
		name := strArg(getArgs(req), "name")
		candidate, err := api.PromoteVersionCandidate(ctx, deps, name, api.ActorFrom(claims))
		if err != nil {
			if errors.Is(err, api.ErrCandidateNotReady) {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultError(fmt.Sprintf("failed to promote version candidate: %v", err)), nil
		}
		// PromoteVersionCandidate already publishes upgrade.candidate.promoted /
		// upgrade.candidate.superseded through deps.ActivityPublisher — emitting
		// again here would double the activity trail.
		return resultJSON(candidate)
	})
}
