package recordstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// crossTenantArtifactURLTTL is the signed URL lifetime design.md D12
// fixes for a cross-tenant artifact read.
const crossTenantArtifactURLTTL = 300 * time.Second

// crossTenantBlobModeMessage names the chart value a caller must set to
// an object-storage tier before a cross-tenant artifact read can work,
// matching the blob-storage capability's rule.
const crossTenantBlobModeMessage = "cross-tenant artifact reads need recordStore.blobs.mode set to an object-storage tier"

// handleCrossTenantArtifactList implements the artifact_list kind. It
// reads the run's tenant and job path from the record, then runs the
// request-path grant against them, exactly as the status kind does.
func handleCrossTenantArtifactList(ctx context.Context, deps CrossTenantDeps, req CrossTenantRequest) (CrossTenantResponse, error) {
	allowed, resp, err := checkCrossTenantArtifactGrant(ctx, deps, req)
	if err != nil || !allowed {
		return resp, err
	}

	artifacts, err := deps.Reader.ListArtifacts(ctx, req.RunID)
	if err != nil {
		return CrossTenantResponse{}, err
	}

	out := make([]CrossTenantArtifact, 0, len(artifacts))
	for _, a := range artifacts {
		out = append(out, CrossTenantArtifact{
			Path:        a.Path,
			SizeBytes:   a.SizeBytes,
			Digest:      derefString(a.Digest),
			ContentType: derefString(a.ContentType),
		})
	}
	return CrossTenantResponse{Artifacts: out}, nil
}

// handleCrossTenantArtifactURL implements the artifact_url kind. It
// signs the exact recorded blob_key for the named path, for
// crossTenantArtifactURLTTL, and derives no key: artifact-manager-s3
// owns the key layout.
func handleCrossTenantArtifactURL(ctx context.Context, deps CrossTenantDeps, req CrossTenantRequest) (CrossTenantResponse, error) {
	allowed, resp, err := checkCrossTenantArtifactGrant(ctx, deps, req)
	if err != nil || !allowed {
		return resp, err
	}

	artifacts, err := deps.Reader.ListArtifacts(ctx, req.RunID)
	if err != nil {
		return CrossTenantResponse{}, err
	}
	var blobKey string
	found := false
	for _, a := range artifacts {
		if a.Path == req.Path {
			blobKey = a.BlobKey
			found = true
			break
		}
	}
	if !found {
		return CrossTenantResponse{Code: CodeInternal, Error: fmt.Sprintf("no artifact at path %q", req.Path)}, nil
	}

	signed, err := deps.Blob.SignedURL(ctx, blobKey, crossTenantArtifactURLTTL)
	if err != nil {
		if errors.Is(err, blob.ErrUnsupported) {
			return CrossTenantResponse{Code: CodeInternal, Error: crossTenantBlobModeMessage}, nil
		}
		return CrossTenantResponse{}, err
	}

	expiresAt := time.Now().UTC().Add(crossTenantArtifactURLTTL)
	return CrossTenantResponse{URL: signed, ExpiresAt: &expiresAt}, nil
}

// checkCrossTenantArtifactGrant reads req.RunID's tenant and job path
// from the record and runs the request-path grant against them, shared
// by both artifact kinds. allowed is false whenever resp is the answer
// to send back as-is: a veto, an unknown run_id, or (with a non-nil
// err) a system fault.
func checkCrossTenantArtifactGrant(ctx context.Context, deps CrossTenantDeps, req CrossTenantRequest) (bool, CrossTenantResponse, error) {
	rec, found, err := readCrossTenantRunRecord(ctx, deps.Pool, req.RunID)
	if err != nil {
		return false, CrossTenantResponse{}, err
	}
	if !found {
		return false, CrossTenantResponse{Code: CodeInternal,
			Error: fmt.Sprintf("unknown run_id %q", req.RunID)}, nil
	}

	grant, err := deps.Evaluator.Check(ctx, GrantInput{
		PlacementInput: PlacementInput{
			Path:                GrantPathRequest,
			Cluster:             req.Cluster,
			Namespace:           req.Namespace,
			Cell:                req.Cell,
			Tenant:              req.CallerTenant,
			PlacementGeneration: req.PlacementGeneration,
		},
		CallerTenant:    req.CallerTenant,
		TargetTenant:    rec.tenant,
		ResourceKind:    GrantResourceJob,
		Resource:        rec.jobPath,
		IntentKeyScheme: IntentSchemeUpstream,
	})
	if err != nil {
		return false, CrossTenantResponse{}, err
	}
	if !grant.Allow {
		return false, CrossTenantResponse{ReasonCode: grant.ReasonCode, Message: grant.Message}, nil
	}
	return true, CrossTenantResponse{}, nil
}
