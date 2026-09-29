package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// pipelineTeamJSON is one row of GET /pipelines. Cell and CellNamespace
// are set only on a placed row.
type pipelineTeamJSON struct {
	Name          string `json:"name"`
	Cluster       string `json:"cluster"`
	Cell          string `json:"cell,omitempty"`
	CellNamespace string `json:"cellNamespace,omitempty"`
	Placed        bool   `json:"placed"`
}

// pipelineTeamListJSON is the body of GET /pipelines. RecordStore is
// false when the answer carries no member-placed row because the record
// store is off or its schema is too old.
type pipelineTeamListJSON struct {
	Items       []pipelineTeamJSON `json:"items"`
	RecordStore bool               `json:"recordStore"`
}

// handleListPipelineTeams serves GET /pipelines, the landing-page read.
// It answers every pipelines Team the caller may read: each local Team
// that carries spec.pipelines, and each live record-store placement row
// of another cluster. It has no gate of its own; the tenant read check
// drops each row the caller may not read.
func (s *Server) handleListPipelineTeams(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	body, err := s.listPipelineTeams(r.Context(), claims)
	if err != nil {
		s.deps.Logger.Error("list pipelines teams", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list pipelines teams failed")
		return
	}
	s.writeJSON(w, http.StatusOK, body)
}

// listPipelineTeams builds the GET /pipelines body. A local Team hides a
// placement row of the same name, because the tenant resolver opens the
// local Team first for that name. One name placed on two other clusters
// answers one row for each cluster.
func (s *Server) listPipelineTeams(ctx context.Context, claims *auth.Claims) (pipelineTeamListJSON, error) {
	local := ""
	if s.deps.Brood != nil {
		local = s.deps.Brood.LocalCluster()
	}

	var rows []pipelineTeamJSON
	localNames := make(map[string]bool)
	if s.deps.Store != nil {
		teams, err := crdstore.List[v1alpha1.Team](ctx, s.deps.Store, "", "")
		if err != nil {
			return pipelineTeamListJSON{}, err
		}
		for _, team := range teams {
			localNames[team.Name] = true
			if team.Spec.Pipelines == nil {
				continue
			}
			rows = append(rows, localPipelineTeamRow(team, local))
		}
	}

	placements, recordStore, err := s.allTenantPlacements(ctx)
	if err != nil {
		return pipelineTeamListJSON{}, err
	}
	for _, row := range placements {
		if localNames[row.Tenant] || strings.Contains(row.Tenant, "/") || !livePlacementRow(row, local) {
			continue
		}
		rows = append(rows, pipelineTeamJSON{
			Name:          row.Tenant,
			Cluster:       row.Cluster,
			Cell:          row.Cell,
			CellNamespace: row.Namespace,
			Placed:        true,
		})
	}

	readable := make(map[string]bool)
	items := make([]pipelineTeamJSON, 0, len(rows))
	for _, row := range rows {
		ok, seen := readable[row.Name]
		if !seen {
			ref := TenantRef{Tenant: row.Name, Team: row.Name, Folder: row.Name}
			ok = s.deps.Authorizer.CanReadPipeline(claims, ref, "")
			readable[row.Name] = ok
		}
		if ok {
			items = append(items, row)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].Cluster < items[j].Cluster
	})
	return pipelineTeamListJSON{Items: items, RecordStore: recordStore}, nil
}

// allTenantPlacements reads every placement row. It reports false, with
// no error, when the record store is off or its schema is too old: the
// landing page then lists the local Teams alone.
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

// localPipelineTeamRow maps a local pipelines Team onto its row. A Team
// with no placed cell answers the local cluster and placed false.
func localPipelineTeamRow(team *v1alpha1.Team, local string) pipelineTeamJSON {
	row := pipelineTeamJSON{Name: team.Name, Cluster: local}
	placement := team.Status.Placement
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
