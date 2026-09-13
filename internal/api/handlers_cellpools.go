package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// cellPoolPatchAttempts bounds the read-modify-write retry. A conflict past
// this bound is reported as 409, so a caller retries with a fresh read
// rather than the handler looping.
const cellPoolPatchAttempts = 3

// cellPoolEntry is the JSON shape of GET /cellpools entries and of
// GET /cellpools/{name}. Field names match v1alpha1.CellPoolSpec and
// CellPoolStatus's own JSON tags.
type cellPoolEntry struct {
	Name   string              `json:"name"`
	Spec   cellPoolSpecEntry   `json:"spec"`
	Status cellPoolStatusEntry `json:"status"`
}

// cellPoolSpecEntry mirrors v1alpha1.CellPoolSpec.
type cellPoolSpecEntry struct {
	ProfileRef         string           `json:"profileRef"`
	Warm               int32            `json:"warm"`
	MaxCells           int32            `json:"maxCells,omitempty"`
	MaxTenantsPerCell  int32            `json:"maxTenantsPerCell,omitempty"`
	UnreadyGracePeriod *metav1.Duration `json:"unreadyGracePeriod,omitempty"`
	CellTemplate       *cellTemplate    `json:"cellTemplate,omitempty"`
}

// cellTemplate mirrors v1alpha1.CellTemplate. The BFF carries it as raw JSON
// on the read path: the surface is read and scale only, so no handler ever
// parses its two keys.
type cellTemplate struct {
	Resources    json.RawMessage `json:"resources,omitempty"`
	PodOverrides json.RawMessage `json:"podOverrides,omitempty"`
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
	if tmpl := pool.Spec.CellTemplate; tmpl != nil {
		e.Spec.CellTemplate = &cellTemplate{}
		if tmpl.Resources != nil {
			if raw, err := json.Marshal(tmpl.Resources); err == nil {
				e.Spec.CellTemplate.Resources = raw
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

// HandleCellPools serves GET /cellpools. There is no POST: an operator
// creates a pool with kubectl or through GitOps.
func (s *Server) HandleCellPools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.canReadCellPools(r) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	pools, err := crdstore.List[v1alpha1.CellPool](r.Context(), s.deps.Store, "", "")
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to list cell pools")
		return
	}
	result := make([]cellPoolEntry, 0, len(pools))
	for _, pool := range pools {
		result = append(result, cellPoolEntryFrom(pool))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	s.writeJSON(w, http.StatusOK, itemsEnvelope(result))
}

// HandleCellPoolDispatch serves /cellpools/{name}:
//
//	GET   /cellpools/{name} — one pool's spec and status
//	PATCH /cellpools/{name} — the four scale dials
//
// There is no DELETE: an operator deletes a pool with kubectl or through
// GitOps.
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
	if !s.canReadCellPools(r) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	pool, err := crdstore.Get[v1alpha1.CellPool](r.Context(), s.deps.Store, name, "")
	if err != nil {
		s.writeJSONError(w, http.StatusNotFound, "cell pool not found")
		return
	}
	s.writeJSON(w, http.StatusOK, cellPoolEntryFrom(pool))
}

// handlePatchCellPool applies the four scale dials with a read-modify-write.
// crdstore exposes no spec-level merge patch, so the handler merges in Go
// and calls Update. A conflict is retried, because the pool reconciler
// writes the same object on its own tick.
func (s *Server) handlePatchCellPool(w http.ResponseWriter, r *http.Request, name string) {
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanUpdateCellPools(auth.ClaimsFromContext(r.Context())) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	req, errMsg := decodeCellPoolScaleRequest(r)
	if errMsg != "" {
		s.writeJSONError(w, http.StatusBadRequest, errMsg)
		return
	}

	ctx := r.Context()
	for attempt := 0; attempt < cellPoolPatchAttempts; attempt++ {
		existing, err := crdstore.Get[v1alpha1.CellPool](ctx, s.deps.Store, name, "")
		if err != nil {
			s.writeJSONError(w, http.StatusNotFound, "cell pool not found")
			return
		}
		updated := existing.DeepCopy()
		if errMsg := applyCellPoolScale(&updated.Spec, req); errMsg != "" {
			s.writeJSONError(w, http.StatusBadRequest, errMsg)
			return
		}
		err = crdstore.Update(ctx, s.deps.Store, updated)
		if err == nil {
			s.writeJSON(w, http.StatusOK, cellPoolEntryFrom(updated))
			return
		}
		if !apierrors.IsConflict(err) {
			s.writeJSONError(w, http.StatusInternalServerError, "failed to update cell pool")
			return
		}
	}
	s.writeJSONError(w, http.StatusConflict, "the cell pool changed during the update; retry the request")
}

// decodeCellPoolScaleRequest reads the body and refuses every key outside
// the four dials. A body naming profileRef or cellTemplate, an unknown key,
// or no recognised key at all is a 400, so the contract and the handler
// agree on what a scale edit is.
func decodeCellPoolScaleRequest(r *http.Request) (cellPoolScaleRequest, string) {
	var top map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&top); err != nil {
		return cellPoolScaleRequest{}, "invalid request body"
	}
	for k := range top {
		if k != "spec" {
			return cellPoolScaleRequest{}, fmt.Sprintf("the body may only contain spec; got %q", k)
		}
	}
	raw, ok := top["spec"]
	if !ok {
		return cellPoolScaleRequest{}, "the body must contain spec"
	}

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return cellPoolScaleRequest{}, "spec must be an object"
	}
	if len(keys) == 0 {
		return cellPoolScaleRequest{}, "spec must name at least one of warm, maxCells, maxTenantsPerCell, or unreadyGracePeriod"
	}
	for k := range keys {
		switch k {
		case "warm", "maxCells", "maxTenantsPerCell", "unreadyGracePeriod":
		case "profileRef", "cellTemplate":
			return cellPoolScaleRequest{}, fmt.Sprintf("spec.%s is not an API operation; edit the CellPool with kubectl or through GitOps", k)
		default:
			return cellPoolScaleRequest{}, fmt.Sprintf("spec may only contain the scale dials; got %q", k)
		}
	}

	var req cellPoolScaleRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return cellPoolScaleRequest{}, "invalid spec"
	}
	return req, ""
}

// applyCellPoolScale merges the named dials onto a spec and repeats the
// CRD's numeric bounds. The CRD schema stays the authority; this check is a
// message-quality measure, so a bad PATCH returns the uniform 400 envelope
// instead of a raw apiserver 422.
func applyCellPoolScale(spec *v1alpha1.CellPoolSpec, req cellPoolScaleRequest) string {
	if req.Warm != nil {
		if *req.Warm < 0 {
			return "spec.warm must be 0 or above"
		}
		spec.Warm = *req.Warm
	}
	if req.MaxCells != nil {
		if *req.MaxCells < 0 {
			return "spec.maxCells must be 0 or above"
		}
		spec.MaxCells = *req.MaxCells
	}
	if req.MaxTenantsPerCell != nil {
		if *req.MaxTenantsPerCell < 1 {
			return "spec.maxTenantsPerCell must be 1 or above"
		}
		spec.MaxTenantsPerCell = *req.MaxTenantsPerCell
	}
	if req.UnreadyGracePeriod != nil {
		d, err := time.ParseDuration(*req.UnreadyGracePeriod)
		if err != nil || d < 0 {
			return "spec.unreadyGracePeriod must be a duration such as 5m"
		}
		spec.UnreadyGracePeriod = &metav1.Duration{Duration: d}
	}
	if spec.MaxCells > 0 && spec.MaxCells < spec.Warm {
		return "spec.maxCells must be 0 or at least spec.warm"
	}
	return ""
}

// canReadCellPools answers the read gate. An absent Authorizer denies, the
// way every other cluster-scoped gate does.
func (s *Server) canReadCellPools(r *http.Request) bool {
	return s.deps.Authorizer != nil &&
		s.deps.Authorizer.CanReadCellPools(auth.ClaimsFromContext(r.Context()))
}
