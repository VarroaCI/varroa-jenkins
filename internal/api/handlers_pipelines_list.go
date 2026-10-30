package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// pipelineTenantJSON is one row of GET /pipelines. Cell and CellNamespace
// are set only on a placed row.
type pipelineTenantJSON struct {
	Name          string `json:"name"`
	Cluster       string `json:"cluster"`
	Cell          string `json:"cell,omitempty"`
	CellNamespace string `json:"cellNamespace,omitempty"`
	Placed        bool   `json:"placed"`
}

// pipelineTenantListJSON is the body of GET /pipelines. RecordStore is
// false when the answer carries no member-placed row because the record
// store is off or its schema is too old.
type pipelineTenantListJSON struct {
	Items       []pipelineTenantJSON `json:"items"`
	RecordStore bool                 `json:"recordStore"`
}

// handleListPipelineTenants serves GET /pipelines, the landing-page read.
// It answers every pipelines Tenant the caller may read: each local Tenant
// that carries spec.pipelines, each live record-store placement row of
// another cluster, and each local Tenant without spec.pipelines whose
// namespaces hold a controller the caller may read. It has no gate of
// its own; the tenant read check drops each row the caller may not read.
func (s *Server) handleListPipelineTenants(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	body, err := s.listPipelineTenants(r.Context(), claims)
	if err != nil {
		s.deps.Logger.Error("list pipelines tenants", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list pipelines tenants failed")
		return
	}
	s.writeJSON(w, http.StatusOK, body)
}

// listPipelineTenants builds the GET /pipelines body. It projects the rows of
// readablePipelineTenants, so the list and the fleet reads share one walk. A
// local Tenant hides a placement row of the same name, because the tenant
// resolver opens the local Tenant first for that name. One name placed on two
// other clusters answers one row for each cluster.
func (s *Server) listPipelineTenants(ctx context.Context, claims *auth.Claims) (pipelineTenantListJSON, error) {
	readable, recordStore, err := s.readablePipelineTenants(ctx, claims)
	if err != nil {
		return pipelineTenantListJSON{}, err
	}
	items := make([]pipelineTenantJSON, 0, len(readable))
	for _, t := range readable {
		items = append(items, t.Row)
	}
	return pipelineTenantListJSON{Items: items, RecordStore: recordStore}, nil
}

// allTenantPlacements reads every placement row. It reports false, with
// no error, when the record store is off or its schema is too old: the
// landing page then lists the local Tenants alone.
func (s *Server) allTenantPlacements(ctx context.Context) ([]recordstore.TenantPlacement, bool, error) {
	if s.deps.RecordStore == nil {
		return nil, false, nil
	}
	rows, err := s.deps.RecordStore.ListAllTenantPlacements(ctx)
	if errors.Is(err, recordstore.ErrSchemaTooOld) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return rows, true, nil
}

// localPipelineTenantRow maps a local pipelines Tenant onto its row. A Tenant
// with no placed cell answers the local cluster and placed false.
func localPipelineTenantRow(tenant *v1alpha1.Tenant, local string) pipelineTenantJSON {
	row := pipelineTenantJSON{Name: tenant.Name, Cluster: local}
	placement := tenant.Status.Placement
	if placement == nil || placement.Cell == "" {
		return row
	}
	if placement.Cluster != "" {
		row.Cluster = placement.Cluster
	}
	row.Cell = placement.Cell
	row.CellNamespace = placement.CellNamespace
	row.Placed = true
	return row
}
