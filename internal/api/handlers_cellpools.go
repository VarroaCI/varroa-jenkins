package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
)

// cellPoolEntry is the JSON shape of GET /cellpools entries and of
// GET /cellpools/{name}. Cluster names the cluster that holds the pool.
// Field names match v1alpha1.CellPoolSpec and CellPoolStatus's own JSON
// tags.
type cellPoolEntry struct {
	Cluster string              `json:"cluster"`
	Name    string              `json:"name"`
	Spec    cellPoolSpecEntry   `json:"spec"`
	Status  cellPoolStatusEntry `json:"status"`
}

// cellPoolSpecEntry mirrors v1alpha1.CellPoolSpec.
type cellPoolSpecEntry struct {
	ProfileRef         string                 `json:"profileRef"`
	Warm               int32                  `json:"warm"`
	MaxCells           int32                  `json:"maxCells,omitempty"`
	MaxTenantsPerCell  int32                  `json:"maxTenantsPerCell,omitempty"`
	UnreadyGracePeriod *metav1.Duration       `json:"unreadyGracePeriod,omitempty"`
	CellTemplate       *cellTemplate          `json:"cellTemplate,omitempty"`
	Libraries          []cellPoolLibraryEntry `json:"libraries,omitempty"`
}

// cellPoolLibraryEntry mirrors v1alpha1.CellPoolLibrary. It carries the
// credential Secret name, never the Secret value.
type cellPoolLibraryEntry struct {
	Name                 string `json:"name"`
	Remote               string `json:"remote"`
	DefaultVersion       string `json:"defaultVersion"`
	CredentialRef        string `json:"credentialRef,omitempty"`
	Implicit             bool   `json:"implicit"`
	AllowVersionOverride bool   `json:"allowVersionOverride"`
}

// cellTemplate mirrors v1alpha1.CellTemplate. The BFF carries it as raw JSON
// on the read path: the surface is read and scale only, so no handler ever
// parses its three keys.
type cellTemplate struct {
	Resources     json.RawMessage `json:"resources,omitempty"`
	MiteResources json.RawMessage `json:"miteResources,omitempty"`
	PodOverrides  json.RawMessage `json:"podOverrides,omitempty"`
}

// cellPoolStatusEntry mirrors v1alpha1.CellPoolStatus. The four counters
// carry no omitempty, so a count that falls to zero reaches the caller.
type cellPoolStatusEntry struct {
	ObservedGeneration int64                    `json:"observedGeneration,omitempty"`
	WarmCells          int32                    `json:"warmCells"`
	PlacedCells        int32                    `json:"placedCells"`
	UnreadyCells       int32                    `json:"unreadyCells"`
	FreeTenantSlots    int32                    `json:"freeTenantSlots"`
	Tenants            []string                 `json:"tenants,omitempty"`
	Conditions         []cellPoolConditionEntry `json:"conditions,omitempty"`
}

// cellPoolConditionEntry mirrors one metav1.Condition on a CellPool.
type cellPoolConditionEntry struct {
	Type               string       `json:"type"`
	Status             string       `json:"status"`
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	Reason             string       `json:"reason,omitempty"`
	Message            string       `json:"message,omitempty"`
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
}

// cellPoolScaleRequest is the PATCH body. Each dial is a pointer, so the
// handler applies only the dials the caller actually named.
type cellPoolScaleRequest struct {
	Warm               *int32  `json:"warm,omitempty"`
	MaxCells           *int32  `json:"maxCells,omitempty"`
	MaxTenantsPerCell  *int32  `json:"maxTenantsPerCell,omitempty"`
	UnreadyGracePeriod *string `json:"unreadyGracePeriod,omitempty"`
}

// cellPoolEntryFrom maps a stored pool onto the wire shape.
func cellPoolEntryFrom(pool *v1alpha1.CellPool) cellPoolEntry {
	e := cellPoolEntry{
		Name: pool.Name,
		Spec: cellPoolSpecEntry{
			ProfileRef:         pool.Spec.ProfileRef,
			Warm:               pool.Spec.Warm,
			MaxCells:           pool.Spec.MaxCells,
			MaxTenantsPerCell:  pool.Spec.MaxTenantsPerCell,
			UnreadyGracePeriod: pool.Spec.UnreadyGracePeriod,
		},
		Status: cellPoolStatusEntry{
			ObservedGeneration: pool.Status.ObservedGeneration,
			WarmCells:          pool.Status.WarmCells,
			PlacedCells:        pool.Status.PlacedCells,
			UnreadyCells:       pool.Status.UnreadyCells,
			FreeTenantSlots:    pool.Status.FreeTenantSlots,
			Tenants:            pool.Status.Tenants,
		},
	}
	for _, lib := range pool.Spec.Libraries {
		e.Spec.Libraries = append(e.Spec.Libraries, cellPoolLibraryEntry{
			Name:                 lib.Name,
			Remote:               lib.Remote,
			DefaultVersion:       lib.DefaultVersion,
			CredentialRef:        lib.CredentialRef,
			Implicit:             lib.Implicit,
			AllowVersionOverride: lib.AllowVersionOverride,
		})
	}
	if tmpl := pool.Spec.CellTemplate; tmpl != nil {
		e.Spec.CellTemplate = &cellTemplate{}
		if tmpl.Resources != nil {
			if raw, err := json.Marshal(tmpl.Resources); err == nil {
				e.Spec.CellTemplate.Resources = raw
			}
		}
		if tmpl.MiteResources != nil {
			if raw, err := json.Marshal(tmpl.MiteResources); err == nil {
				e.Spec.CellTemplate.MiteResources = raw
			}
		}
		if tmpl.PodOverrides != nil {
			if raw, err := json.Marshal(tmpl.PodOverrides); err == nil {
				e.Spec.CellTemplate.PodOverrides = raw
			}
		}
	}
	for _, c := range pool.Status.Conditions {
		entry := cellPoolConditionEntry{
			Type:               c.Type,
			Status:             string(c.Status),
			Reason:             c.Reason,
			Message:            c.Message,
			ObservedGeneration: c.ObservedGeneration,
		}
		if !c.LastTransitionTime.IsZero() {
			t := c.LastTransitionTime
			entry.LastTransitionTime = &t
		}
		e.Status.Conditions = append(e.Status.Conditions, entry)
	}
	return e
}

// cellPoolListResponse is the body of GET /cellpools: the pools of every
// cluster the list covered, and one read status for each of those clusters.
type cellPoolListResponse struct {
	Items    []cellPoolEntry       `json:"items"`
	Clusters []ClusterFanoutStatus `json:"clusters"`
}

// cellPoolEntryForCluster maps a pool of one cluster onto the wire shape.
func cellPoolEntryForCluster(p ClusterCellPool) cellPoolEntry {
	e := cellPoolEntryFrom(p.Pool)
	e.Cluster = p.Cluster
	return e
}

// HandleCellPools serves GET /cellpools. There is no POST: an operator
// creates a pool with kubectl or through GitOps.
func (s *Server) HandleCellPools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	list, serr := s.ListCellPools(r.Context(), auth.ClaimsFromContext(r.Context()), r.URL.Query().Get("cluster"))
	if serr != nil {
		s.writeJSONError(w, serr.Status, serr.Message)
		return
	}
	body := cellPoolListResponse{
		Items:    make([]cellPoolEntry, 0, len(list.Items)),
		Clusters: list.Clusters,
	}
	for _, p := range list.Items {
		body.Items = append(body.Items, cellPoolEntryForCluster(p))
	}
	s.writeJSON(w, http.StatusOK, body)
}

// HandleCellPoolDispatch serves /cellpools/{name}:
//
//	GET   /cellpools/{name} — one pool's spec and status
//	PATCH /cellpools/{name} — the four scale dials
//
// Both take an optional cluster query parameter and default to the BFF's
// own cluster. There is no DELETE: an operator deletes a pool with kubectl
// or through GitOps.
func (s *Server) HandleCellPoolDispatch(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/cellpools/")
	if name == "" || strings.Contains(name, "/") {
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleGetCellPool(w, r, name)
	case http.MethodPatch:
		s.handlePatchCellPool(w, r, name)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleGetCellPool(w http.ResponseWriter, r *http.Request, name string) {
	pool, serr := s.GetCellPool(r.Context(), auth.ClaimsFromContext(r.Context()), r.URL.Query().Get("cluster"), name)
	if serr != nil {
		s.writeJSONError(w, serr.Status, serr.Message)
		return
	}
	s.writeJSON(w, http.StatusOK, cellPoolEntryForCluster(pool))
}

// handlePatchCellPool applies the scale dials to the pool of the named
// cluster. A successful write publishes one cellpool.updated event that
// names the pool's cluster.
func (s *Server) handlePatchCellPool(w http.ResponseWriter, r *http.Request, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || claims == nil || !s.deps.Authorizer.CanUpdateCellPools(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	dials, errMsg := decodeCellPoolScaleRequest(r)
	if errMsg != "" {
		s.writeJSONError(w, http.StatusBadRequest, errMsg)
		return
	}

	pool, serr := s.ScaleCellPool(r.Context(), claims, r.URL.Query().Get("cluster"), name, dials)
	if serr != nil {
		s.writeJSONError(w, serr.Status, serr.Message)
		return
	}
	s.notifyActivity(activity.Event{
		Type:    "cellpool.updated",
		Actor:   ActorFrom(claims),
		Cluster: pool.Cluster,
		Message: "CellPool " + name + " scaled on cluster " + pool.Cluster,
	})
	s.writeJSON(w, http.StatusOK, cellPoolEntryForCluster(pool))
}

// decodeCellPoolScaleRequest reads the body and refuses every key outside
// the four dials. A body naming profileRef or cellTemplate, an unknown key,
// or no recognised key at all is a 400, so the contract and the handler
// agree on what a scale edit is. The numeric bounds are checked where the
// write runs, by cells.ApplyCellPoolDials.
func decodeCellPoolScaleRequest(r *http.Request) (cells.CellPoolDials, string) {
	var top map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&top); err != nil {
		return cells.CellPoolDials{}, "invalid request body"
	}
	for k := range top {
		if k != "spec" {
			return cells.CellPoolDials{}, fmt.Sprintf("the body may only contain spec; got %q", k)
		}
	}
	raw, ok := top["spec"]
	if !ok {
		return cells.CellPoolDials{}, "the body must contain spec"
	}

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return cells.CellPoolDials{}, "spec must be an object"
	}
	if len(keys) == 0 {
		return cells.CellPoolDials{}, "spec must name at least one of warm, maxCells, maxTenantsPerCell, or unreadyGracePeriod"
	}
	for k := range keys {
		switch k {
		case "warm", "maxCells", "maxTenantsPerCell", "unreadyGracePeriod":
		case "profileRef", "cellTemplate":
			return cells.CellPoolDials{}, fmt.Sprintf("spec.%s is not an API operation; edit the CellPool with kubectl or through GitOps", k)
		default:
			return cells.CellPoolDials{}, fmt.Sprintf("spec may only contain the scale dials; got %q", k)
		}
	}

	var req cellPoolScaleRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return cells.CellPoolDials{}, "invalid spec"
	}
	return cells.CellPoolDials{
		Warm:               req.Warm,
		MaxCells:           req.MaxCells,
		MaxTenantsPerCell:  req.MaxTenantsPerCell,
		UnreadyGracePeriod: req.UnreadyGracePeriod,
	}, ""
}
