package mite

import (
	"context"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// cellTenantResolveTTL is the cache lifetime design.md D14 sets for both
// the Controller cell lookup and the Team placement lookup.
const cellTenantResolveTTL = 30 * time.Second

// cellTenantResolveTimeout bounds one cold lookup against the CRD store,
// so a slow API server never consumes the whole claim-path reply budget.
const cellTenantResolveTimeout = 2 * time.Second

// crdCellTenantResolver is the production CellTenantResolver (design.md
// D14). It reads the owning Controller, by namespace and cell name, and —
// for a cell — the named Team, from the CRD store, each cached for
// cellTenantResolveTTL. It is unexported: callers build one through
// NewCellTenantResolver and use it only through the CellTenantResolver
// interface.
type crdCellTenantResolver struct {
	store crdstore.Backend

	mu          sync.Mutex
	controllers map[string]cellFlagEntry      // key "ns/cell"
	teams       map[string]teamPlacementEntry // key Team name
}

// cellFlagEntry caches whether one Controller is a cell.
type cellFlagEntry struct {
	isCell  bool
	expires time.Time
}

// teamPlacementEntry caches one Team's placement. placed is false for a
// Team that carries no placement yet, or that does not exist — an
// unplaced Team and an absent one answer a job path the same way: no
// tenant hosts it.
type teamPlacementEntry struct {
	placed        bool
	cluster       string
	cellNamespace string
	cell          string
	generation    int64
	expires       time.Time
}

// NewCellTenantResolver builds the resolver design.md D14 owns. store
// reads Controller and Team CRDs.
func NewCellTenantResolver(store crdstore.Backend) CellTenantResolver {
	return &crdCellTenantResolver{
		store:       store,
		controllers: make(map[string]cellFlagEntry),
		teams:       make(map[string]teamPlacementEntry),
	}
}

// ResolveTenant implements CellTenantResolver. It resolves without a job
// path, so a cell always answers unknown: a cell hosts a tenant per job
// path, never as a whole.
func (r *crdCellTenantResolver) ResolveTenant(cluster, ns, cell string) (string, int64) {
	return r.ResolveJobTenant(cluster, ns, cell, "")
}

// ResolveJobTenant implements CellTenantResolver.
func (r *crdCellTenantResolver) ResolveJobTenant(cluster, ns, cell, jobPath string) (string, int64) {
	isCell, resolved := r.isCell(ns, cell)
	if !resolved {
		// The Controller's cell status could not be read. Fail closed:
		// guessing "not a cell" here could record a build under the wrong
		// tenant.
		return "", 0
	}
	if !isCell {
		return ns + "/" + cell, 0
	}

	team := firstPathSegment(jobPath)
	if team == "" {
		return "", 0
	}

	placement, resolved := r.teamPlacement(team)
	if !resolved || !placement.placed {
		return "", 0
	}
	if placement.cellNamespace != ns || placement.cell != cell {
		// The team is placed on a different cell. This cell must not
		// answer for a tenant it does not host.
		return "", 0
	}
	if placement.cluster != "" && placement.cluster != cluster {
		return "", 0
	}
	return team, placement.generation
}

// firstPathSegment returns the first "/"-delimited segment of jobPath,
// which is the tenant folder for a cell job path.
func firstPathSegment(jobPath string) string {
	jobPath = strings.TrimPrefix(jobPath, "/")
	if jobPath == "" {
		return ""
	}
	if idx := strings.IndexByte(jobPath, '/'); idx >= 0 {
		return jobPath[:idx]
	}
	return jobPath
}

// isCell reports whether the Controller named ns/cell carries
// spec.cell.enabled true, cached for cellTenantResolveTTL. resolved is
// false when the Controller could not be read at all.
func (r *crdCellTenantResolver) isCell(ns, cell string) (isCell, resolved bool) {
	key := ns + "/" + cell
	now := time.Now()

	r.mu.Lock()
	if entry, found := r.controllers[key]; found && now.Before(entry.expires) {
		r.mu.Unlock()
		return entry.isCell, true
	}
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), cellTenantResolveTimeout)
	defer cancel()
	cr, err := crdstore.Get[v1alpha1.Controller](ctx, r.store, cell, ns)
	if err != nil {
		// A deleted Controller and a transient read error both leave the
		// cell status unknown; neither is cached, so the next call
		// retries rather than latching a wrong answer for 30 seconds.
		return false, false
	}

	isCellFlag := cr.Spec.Cell != nil && cr.Spec.Cell.Enabled
	r.mu.Lock()
	r.controllers[key] = cellFlagEntry{isCell: isCellFlag, expires: now.Add(cellTenantResolveTTL)}
	r.mu.Unlock()
	return isCellFlag, true
}

// teamPlacement reads team's current placement, cached for
// cellTenantResolveTTL. resolved is false only on a transient read
// error; an absent Team caches as placed=false like an unplaced one.
func (r *crdCellTenantResolver) teamPlacement(team string) (entry teamPlacementEntry, resolved bool) {
	now := time.Now()

	r.mu.Lock()
	if e, found := r.teams[team]; found && now.Before(e.expires) {
		r.mu.Unlock()
		return e, true
	}
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), cellTenantResolveTimeout)
	defer cancel()
	tm, err := crdstore.Get[v1alpha1.Team](ctx, r.store, team, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			e := teamPlacementEntry{placed: false, expires: now.Add(cellTenantResolveTTL)}
			r.mu.Lock()
			r.teams[team] = e
			r.mu.Unlock()
			return e, true
		}
		return teamPlacementEntry{}, false
	}

	var e teamPlacementEntry
	if p := tm.Status.Placement; p != nil && p.Cell != "" && p.CellNamespace != "" {
		e = teamPlacementEntry{
			placed:        true,
			cluster:       p.Cluster,
			cellNamespace: p.CellNamespace,
			cell:          p.Cell,
			generation:    p.Generation,
		}
	}
	e.expires = now.Add(cellTenantResolveTTL)

	r.mu.Lock()
	r.teams[team] = e
	r.mu.Unlock()
	return e, true
}

var _ CellTenantResolver = (*crdCellTenantResolver)(nil)
