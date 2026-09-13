// Package cells holds the tenant-placement and cell-pool logic for the
// jenkins-cells feature (design D4, D5): binding a pipelines Team to a
// shared cell Controller, and keeping a pool of unassigned cells ready to
// receive one.
package cells

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

const (
	// LabelCellProfile names the JenkinsVersionProfile a cell was created
	// for. The pool sets it at creation and never updates it; the
	// placement pass reads it to find a candidate cell for a profile.
	LabelCellProfile = "varroa.dev/cell-profile"

	// LabelWarm marks a cell a pool created. Set at creation and never
	// updated. Controller.status.cell.warm is the live signal for whether a
	// cell is still a free pool member; this label only records where the
	// cell came from.
	LabelWarm = "varroa.dev/warm"
)

// WarmPoolConfig carries the chart values the operator seeds a new CellPool
// with. They are the defaults of a pool the operator creates, never the live
// configuration of a pool: an operator scales a live pool by editing the
// CellPool object.
type WarmPoolConfig struct {
	// Enabled gates auto-creation, and nothing else. A pool a user applies
	// by hand is still reconciled, because an explicit object is an
	// explicit request.
	Enabled bool
	// PerProfile seeds CellPool.spec.warm.
	PerProfile int
	// MaxTenantsPerCell seeds CellPool.spec.maxTenantsPerCell, and is the
	// fallback capacity of a cell whose pool sets no value.
	MaxTenantsPerCell int
	// UnreadyGracePeriod seeds CellPool.spec.unreadyGracePeriod.
	UnreadyGracePeriod time.Duration
}

// WarmPoolReconciler sizes and reports every CellPool (design D5). It is the
// only creator of a cell and the only writer of CellPool.status. It writes
// no Controller status, so it never races a Controller status writer.
type WarmPoolReconciler struct {
	Store             crdstore.Backend
	OperatorNamespace string
	Config            WarmPoolConfig
	Logger            *slog.Logger
	// Now returns the current time. Defaults to time.Now when nil; a test
	// injects a fixed clock.
	Now func() time.Time
}

func (r *WarmPoolReconciler) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func (r *WarmPoolReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// chartMaxTenants is the fallback capacity for a cell whose own
// spec.cell.maxTenants is zero and whose pool sets no cap. It is never below
// one, so the fill arithmetic never divides by zero.
func (r *WarmPoolReconciler) chartMaxTenants() int32 {
	if r.Config.MaxTenantsPerCell > 0 {
		return int32(r.Config.MaxTenantsPerCell)
	}
	return 1
}

// Reconcile runs one pass: it creates a pool for each requested profile when
// auto-creation is on, then sizes and reports every pool that exists.
func (r *WarmPoolReconciler) Reconcile(ctx context.Context) error {
	teams, err := crdstore.List[v1alpha1.Team](ctx, r.Store, "", "")
	if err != nil {
		return fmt.Errorf("list Teams: %w", err)
	}

	var created []*v1alpha1.CellPool
	if r.Config.Enabled {
		created = r.createMissingPools(ctx, teams)
	}

	pools, err := crdstore.List[v1alpha1.CellPool](ctx, r.Store, "", "")
	if err != nil {
		return fmt.Errorf("list CellPools: %w", err)
	}
	// A pool created this tick is sized in this tick, so the first tenant
	// waits one cell boot rather than two ticks.
	pools = mergePools(pools, created)

	names, err := r.controllerNames(ctx)
	if err != nil {
		return fmt.Errorf("list controllers: %w", err)
	}

	sort.Slice(pools, func(i, j int) bool { return pools[i].Name < pools[j].Name })
	for _, pool := range pools {
		r.reconcilePool(ctx, pool, teams, names)
	}
	return nil
}

// mergePools appends each created pool the list does not already carry.
func mergePools(listed, created []*v1alpha1.CellPool) []*v1alpha1.CellPool {
	have := make(map[string]bool, len(listed))
	for _, p := range listed {
		have[p.Name] = true
	}
	for _, p := range created {
		if !have[p.Name] {
			listed = append(listed, p)
		}
	}
	return listed
}

// createMissingPools creates CellPool/<profile> for each profile a pipelines
// Team requests and no pool serves yet (design D7). It never writes the spec
// of a pool that already exists, so an operator's edit survives every tick
// and every operator restart.
func (r *WarmPoolReconciler) createMissingPools(ctx context.Context, teams []*v1alpha1.Team) []*v1alpha1.CellPool {
	log := r.logger()
	var created []*v1alpha1.CellPool
	for _, profile := range requestedProfiles(teams) {
		if _, err := crdstore.Get[v1alpha1.CellPool](ctx, r.Store, profile, ""); err == nil {
			continue
		}
		pool := r.seededPool(profile)
		if err := crdstore.Create(ctx, r.Store, pool); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				log.Warn("cell pool: create pool failed", "pool", profile, "error", err)
			}
			continue
		}
		created = append(created, pool)
	}
	return created
}

// seededPool builds the pool the operator creates for profile. Its spec
// comes from the chart values the operator reads at boot.
func (r *WarmPoolReconciler) seededPool(profile string) *v1alpha1.CellPool {
	warm := int32(0)
	if r.Config.PerProfile > 0 {
		warm = int32(r.Config.PerProfile)
	}
	// The CRD refuses a maxTenantsPerCell below one, so a chart value of
	// zero is clamped up. The operator never writes a spec the apiserver
	// rejects, and the fill arithmetic never divides by zero.
	maxTenants := int32(1)
	if r.Config.MaxTenantsPerCell > 1 {
		maxTenants = int32(r.Config.MaxTenantsPerCell)
	}
	pool := &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: profile},
		Spec: v1alpha1.CellPoolSpec{
			ProfileRef:        profile,
			Warm:              warm,
			MaxCells:          0,
			MaxTenantsPerCell: maxTenants,
		},
	}
	if r.Config.UnreadyGracePeriod > 0 {
		pool.Spec.UnreadyGracePeriod = &metav1.Duration{Duration: r.Config.UnreadyGracePeriod}
	}
	return pool
}

// requestedProfiles collects every profile a pipelines Team names. A pool is
// a durable object, so the pass no longer collects the profile a placed
// tenant's live cell runs: the pool created for that profile stays after the
// Team changes spec.pipelines.profile.
func requestedProfiles(teams []*v1alpha1.Team) []string {
	set := map[string]bool{}
	for _, team := range teams {
		if team.Spec.Pipelines != nil && team.Spec.Pipelines.Profile != "" {
			set[team.Spec.Pipelines.Profile] = true
		}
	}
	profiles := make([]string, 0, len(set))
	for profile := range set {
		profiles = append(profiles, profile)
	}
	sort.Strings(profiles)
	return profiles
}

// controllerNames collects every Controller name in the operator namespace.
// The ordinal scan reads this set rather than the pool's own label list, so
// two pools whose names produce the same prefix never collide.
func (r *WarmPoolReconciler) controllerNames(ctx context.Context) (map[string]bool, error) {
	all, err := crdstore.List[v1alpha1.Controller](ctx, r.Store, r.OperatorNamespace, "")
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(all))
	for _, cr := range all {
		names[cr.Name] = true
	}
	return names, nil
}

// reconcilePool runs the ordered pass of design D5 for one pool.
func (r *WarmPoolReconciler) reconcilePool(ctx context.Context, pool *v1alpha1.CellPool, teams []*v1alpha1.Team, names map[string]bool) {
	log := r.logger()

	if pool.DeletionTimestamp != nil && !pool.DeletionTimestamp.IsZero() {
		r.drainDeletedPool(ctx, pool)
		return
	}
	r.ensureFinalizer(ctx, pool)

	versionProfile, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](ctx, r.Store, pool.Spec.ProfileRef, "")
	if err != nil {
		// A missing profile stops the pass for this pool. It creates and
		// deletes nothing, because every cell it would create would run an
		// unresolved version.
		r.writeStatus(ctx, pool, nil, v1alpha1.CellPoolReasonProfileMissing)
		return
	}

	if pool.Name == pool.Spec.ProfileRef {
		r.adoptUnlabelledCells(ctx, pool)
	}

	owned, err := r.ownedCells(ctx, pool)
	if err != nil {
		log.Warn("cell pool: list cells failed", "pool", pool.Name, "error", err)
		return
	}

	free := freeCells(owned)
	capPerCell := r.poolCapacity(pool)
	desiredFree := r.desiredFreeCount(pool, owned, teams, capPerCell)

	switch {
	case len(free) < desiredFree:
		r.fill(ctx, pool, versionProfile, names, len(owned), desiredFree-len(free))
	case len(free) > desiredFree:
		r.drainSurplus(ctx, pool, free, len(free)-desiredFree)
	}

	r.restampFreeCells(ctx, pool, versionProfile, free)

	// The status reports the pass's own result, so it is computed from a
	// fresh listing rather than from the pre-action set.
	after, err := r.ownedCells(ctx, pool)
	if err != nil {
		log.Warn("cell pool: list cells failed", "pool", pool.Name, "error", err)
		return
	}
	reason := v1alpha1.CellPoolReasonReady
	if pool.Spec.MaxCells > 0 && int32(len(after)) >= pool.Spec.MaxCells {
		reason = v1alpha1.CellPoolReasonCellCapReached
	}
	r.writeStatus(ctx, pool, after, reason)
}

// poolCapacity answers the pool's own maxTenantsPerCell, falling back to the
// chart value. It is never below one.
func (r *WarmPoolReconciler) poolCapacity(pool *v1alpha1.CellPool) int32 {
	if pool.Spec.MaxTenantsPerCell > 0 {
		return pool.Spec.MaxTenantsPerCell
	}
	return r.chartMaxTenants()
}

// desiredFreeCount computes one target for the pass (design D5 step 5), so
// the pass never fills and drains in the same tick.
func (r *WarmPoolReconciler) desiredFreeCount(pool *v1alpha1.CellPool, owned []*v1alpha1.Controller, teams []*v1alpha1.Team, capPerCell int32) int {
	// placedSlots deliberately excludes free cells. A free cell is already
	// counted by len(free), so counting its capacity again would hide real
	// demand and stall a warm: 0 pool.
	placedSlots := 0
	ownedNames := make(map[string]bool, len(owned))
	summary := SummarizeCellPool(pool)
	for _, cell := range owned {
		ownedNames[cell.Name] = true
		tenants := cellTenantCount(cell)
		if tenants == 0 {
			continue
		}
		capacity := resolveCellCapacity(cellMaxTenants(cell), &summary, r.chartMaxTenants())
		if spare := int(capacity) - tenants; spare > 0 {
			placedSlots += spare
		}
	}

	pending := 0
	for _, team := range teams {
		if team.Spec.Pipelines == nil || ResolvedPoolName(team.Spec.Pipelines) != pool.Name {
			continue
		}
		if team.Status.Placement != nil && ownedNames[team.Status.Placement.Cell] {
			continue
		}
		pending++
	}

	deficit := pending - placedSlots
	if deficit < 0 {
		deficit = 0
	}
	needed := (deficit + int(capPerCell) - 1) / int(capPerCell)
	desired := int(pool.Spec.Warm)
	if needed > desired {
		desired = needed
	}
	return desired
}

// ownedCells lists the cells this pool owns: the cells in the operator
// namespace labelled with the pool's name whose profile label also matches
// spec.profileRef. The second test keeps a stale label out of a new pool of
// the same name.
func (r *WarmPoolReconciler) ownedCells(ctx context.Context, pool *v1alpha1.CellPool) ([]*v1alpha1.Controller, error) {
	selector := fmt.Sprintf("%s=%s", LabelCellPool, pool.Name)
	listed, err := crdstore.List[v1alpha1.Controller](ctx, r.Store, r.OperatorNamespace, selector)
	if err != nil {
		return nil, err
	}
	owned := make([]*v1alpha1.Controller, 0, len(listed))
	for _, cell := range listed {
		if cell.Labels[LabelCellProfile] == pool.Spec.ProfileRef {
			owned = append(owned, cell)
		}
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].Name < owned[j].Name })
	return owned, nil
}

// adoptUnlabelledCells stamps the pool label on every cell of the pool's
// profile that carries no pool label. Only the profile's default pool adopts,
// and a cell that already carries a pool label is never restamped.
func (r *WarmPoolReconciler) adoptUnlabelledCells(ctx context.Context, pool *v1alpha1.CellPool) {
	log := r.logger()
	selector := fmt.Sprintf("%s=%s", LabelCellProfile, pool.Spec.ProfileRef)
	listed, err := crdstore.List[v1alpha1.Controller](ctx, r.Store, r.OperatorNamespace, selector)
	if err != nil {
		log.Warn("cell pool: list cells for adoption failed", "pool", pool.Name, "error", err)
		return
	}
	name := pool.Name
	for _, cell := range listed {
		if cell.Labels[LabelCellPool] != "" {
			continue
		}
		if err := crdstore.PatchLabels[v1alpha1.Controller](ctx, r.Store, cell.Name, cell.Namespace,
			map[string]*string{LabelCellPool: &name}); err != nil {
			log.Warn("cell pool: adopt cell failed", "pool", pool.Name, "cell", cell.Name, "error", err)
		}
	}
}

// ensureFinalizer adds the drain finalizer on the first tick that sees a
// pool, so a delete drains the pool's free cells before the object goes.
func (r *WarmPoolReconciler) ensureFinalizer(ctx context.Context, pool *v1alpha1.CellPool) {
	for _, f := range pool.Finalizers {
		if f == v1alpha1.CellPoolFinalizerDrain {
			return
		}
	}
	next := append(append([]string(nil), pool.Finalizers...), v1alpha1.CellPoolFinalizerDrain)
	if err := crdstore.PatchFinalizers[v1alpha1.CellPool](ctx, r.Store, pool.Name, "", next); err != nil {
		r.logger().Warn("cell pool: add finalizer failed", "pool", pool.Name, "error", err)
		return
	}
	pool.Finalizers = next
}

// drainDeletedPool runs the one drain pass of design D15: delete every free
// cell, then remove the finalizer. It deletes no cell that holds a tenant
// and it removes no label from any cell. A placed cell therefore keeps its
// labels and lives on as a cell with no effective owner.
func (r *WarmPoolReconciler) drainDeletedPool(ctx context.Context, pool *v1alpha1.CellPool) {
	log := r.logger()
	owned, err := r.ownedCells(ctx, pool)
	if err != nil {
		log.Warn("cell pool: list cells for drain failed", "pool", pool.Name, "error", err)
		return
	}
	// The same stale-snapshot race the fill-and-drain path runs
	// (deleteFreeCell): a cell listed as free here can be fenced and tenanted
	// by the placement pass before this loop reaches it.
	fenced, err := r.fencedCells(ctx)
	if err != nil {
		log.Warn("cell pool: list placements for drain failed", "pool", pool.Name, "error", err)
		return
	}
	free := freeCells(owned)
	drained := 0
	for _, cell := range free {
		if r.deleteFreeCell(ctx, pool, cell, fenced) {
			drained++
		}
	}
	if drained < len(free) {
		// A cell this pass could not drain holds the finalizer: releasing it
		// now would let the pool object go while a cell still names it, and
		// no later tick would ever revisit that cell. The next tick retries.
		log.Info("cell pool: the drain is not complete, holding the finalizer",
			"pool", pool.Name, "drained", drained, "free", len(free))
		return
	}
	remaining := make([]string, 0, len(pool.Finalizers))
	for _, f := range pool.Finalizers {
		if f != v1alpha1.CellPoolFinalizerDrain {
			remaining = append(remaining, f)
		}
	}
	if err := crdstore.PatchFinalizers[v1alpha1.CellPool](ctx, r.Store, pool.Name, "", remaining); err != nil {
		log.Warn("cell pool: remove finalizer failed", "pool", pool.Name, "error", err)
	}
}

// fill creates count cells for pool. The fill stops early when maxCells is
// above zero and the pool already owns that many cells.
func (r *WarmPoolReconciler) fill(ctx context.Context, pool *v1alpha1.CellPool, profile *v1alpha1.JenkinsVersionProfile, names map[string]bool, owned, count int) {
	log := r.logger()
	prefix := fmt.Sprintf("cell-%s-", pool.Name)
	next := 0
	for i := 0; i < count; i++ {
		if pool.Spec.MaxCells > 0 && int32(owned) >= pool.Spec.MaxCells {
			return
		}
		for names[fmt.Sprintf("%s%d", prefix, next)] {
			next++
		}
		cell := buildPoolCell(r.OperatorNamespace, pool, resolvedProfileVersion(profile), next)
		if err := crdstore.Create(ctx, r.Store, cell); err != nil && !apierrors.IsAlreadyExists(err) {
			log.Warn("cell pool: create cell failed", "pool", pool.Name, "cell", cell.Name, "error", err)
			return
		}
		names[cell.Name] = true
		next++
		owned++
	}
}

// buildPoolCell returns the Controller a fill creates: an ordinary cell,
// named cell-<pool>-<ordinal>, in the operator namespace, carrying the three
// pool labels. Its spec.cell.maxTenants is zero, so its capacity resolves
// through the pool.
func buildPoolCell(operatorNamespace string, pool *v1alpha1.CellPool, version string, ordinal int) *v1alpha1.Controller {
	cell := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("cell-%s-%d", pool.Name, ordinal),
			Namespace: operatorNamespace,
			Labels: map[string]string{
				LabelCellPool:    pool.Name,
				LabelCellProfile: pool.Spec.ProfileRef,
				LabelWarm:        "true",
			},
		},
		Spec: v1alpha1.ControllerSpec{
			Version: version,
			Cell: &v1alpha1.CellSpec{
				Enabled:    true,
				MaxTenants: 0,
			},
		},
	}
	applyCellTemplate(&cell.Spec, pool.Spec.CellTemplate)
	return cell
}

// applyCellTemplate copies the pool template's two keys onto a cell spec.
func applyCellTemplate(spec *v1alpha1.ControllerSpec, tmpl *v1alpha1.CellTemplate) {
	if tmpl == nil {
		spec.Resources = nil
		spec.PodOverrides = nil
		return
	}
	spec.Resources = tmpl.Resources.DeepCopy()
	spec.PodOverrides = tmpl.PodOverrides.DeepCopy()
}

// restampFreeCells sets spec.version, spec.resources, and spec.podOverrides
// on each free cell, from the profile and from the template. It never edits
// a cell that holds a tenant, so a template edit rolls free cells at once and
// reaches a placed cell only after that cell drains.
func (r *WarmPoolReconciler) restampFreeCells(ctx context.Context, pool *v1alpha1.CellPool, profile *v1alpha1.JenkinsVersionProfile, free []*v1alpha1.Controller) {
	log := r.logger()
	version := resolvedProfileVersion(profile)
	for _, cell := range free {
		next := cell.DeepCopy()
		next.Spec.Version = version
		applyCellTemplate(&next.Spec, pool.Spec.CellTemplate)
		if equalCellShape(&cell.Spec, &next.Spec) {
			continue
		}
		if err := crdstore.Update(ctx, r.Store, next); err != nil {
			log.Warn("cell pool: restamp cell failed", "pool", pool.Name, "cell", cell.Name, "error", err)
		}
	}
}

// equalCellShape reports whether the three template-owned fields already
// match, so an unchanged cell is never written. It compares with the
// semantic equality helper, which reads a resource.Quantity by value rather
// than by its cached string form.
func equalCellShape(a, b *v1alpha1.ControllerSpec) bool {
	return a.Version == b.Version &&
		equality.Semantic.DeepEqual(a.Resources, b.Resources) &&
		equality.Semantic.DeepEqual(a.PodOverrides, b.PodOverrides)
}

// drainSurplus deletes count free cells, highest ordinal first, so the pool
// keeps the lowest-numbered members and a later fill reuses the freed high
// ordinals. It never deletes a cell that holds a tenant, and it re-checks
// every cell against live state before the delete (deleteFreeCell). It
// answers how many cells it actually deleted; a skipped cell leaves the pool
// above its target, and the next tick decides again.
func (r *WarmPoolReconciler) drainSurplus(ctx context.Context, pool *v1alpha1.CellPool, free []*v1alpha1.Controller, count int) int {
	fenced, err := r.fencedCells(ctx)
	if err != nil {
		// Without the fence set the drain cannot tell a newly placed cell
		// from a free one, so it deletes nothing this tick.
		r.logger().Warn("cell pool: list placements for drain failed", "pool", pool.Name, "error", err)
		return 0
	}

	prefix := fmt.Sprintf("cell-%s-", pool.Name)
	ordered := append([]*v1alpha1.Controller(nil), free...)
	sort.Slice(ordered, func(i, j int) bool {
		oi, oki := cellOrdinal(ordered[i].Name, prefix)
		oj, okj := cellOrdinal(ordered[j].Name, prefix)
		if oki && okj {
			return oi > oj
		}
		return ordered[i].Name > ordered[j].Name
	})
	deleted := 0
	for i := 0; i < count && i < len(ordered); i++ {
		if r.deleteFreeCell(ctx, pool, ordered[i], fenced) {
			deleted++
		}
	}
	return deleted
}

// fencedCells answers the cells a Team placement currently names, keyed
// "namespace/name". status.placement is the fence, and TeamReconciler writes
// it **before** it patches the bound cell's tenant list, so a cell is fenced
// for a window in which its own status.cell.tenants is still empty. A drain
// that read only the tenant list would delete such a cell out from under the
// tenant that was just placed on it. This set is itself a snapshot: a fence
// written after it is read covers nothing here, which is why the placement
// pass claims the cell before it writes the fence.
func (r *WarmPoolReconciler) fencedCells(ctx context.Context) (map[string]bool, error) {
	teams, err := crdstore.List[v1alpha1.Team](ctx, r.Store, "", "")
	if err != nil {
		return nil, err
	}
	fenced := make(map[string]bool, len(teams))
	for _, team := range teams {
		if p := team.Status.Placement; p != nil && p.Cell != "" {
			fenced[cellKey(p.CellNamespace, p.Cell)] = true
		}
	}
	return fenced, nil
}

// cellKey names one cell across namespaces.
func cellKey(namespace, name string) string { return namespace + "/" + name }

// deleteFreeCell deletes one cell, and only while it is still free. It
// answers whether the delete happened.
//
// The pass's own free snapshot is stale by the time a drain runs. The
// interleaving it must survive: this pass lists a cell as free; the placement
// pass then claims that cell, writes a Team's status.placement fence onto it,
// and patches the tenant onto its status.cell.tenants; this drain, still
// holding the old snapshot, deletes the cell the tenant now runs on. Five
// guards close it — a fresh read of the cell, a live claim on that fresh
// read (status.cell.pendingTenants), the caller's fence snapshot, a fresh
// fence read, and a delete precondition on the uid and resourceVersion that
// fresh read observed. The claim is guard zero, because the caller lists the
// fences before this function re-reads the cell: a fence written after that
// listing is invisible to the snapshot, while the claim that precedes it
// either shows up in the fresh read or moves the resourceVersion the
// precondition carries. The fresh fence read covers the second stale window:
// a tenant whose cell patch keeps failing holds a fence and no tenant entry,
// and its claim can pass CellClaimTTL before the patch lands. A write that
// arrives between the read and the delete makes the apiserver answer
// Conflict, and the next tick decides again from current state.
func (r *WarmPoolReconciler) deleteFreeCell(ctx context.Context, pool *v1alpha1.CellPool, cell *v1alpha1.Controller, fenced map[string]bool) bool {
	log := r.logger()
	live, err := crdstore.Get[v1alpha1.Controller](ctx, r.Store, cell.Name, cell.Namespace)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Warn("cell pool: re-read cell before delete failed", "pool", pool.Name, "cell", cell.Name, "error", err)
		}
		return false
	}
	if cellTenantCount(live) > 0 {
		log.Info("cell pool: skipped a drain, the cell gained a tenant", "pool", pool.Name, "cell", cell.Name)
		return false
	}
	if HasLiveCellClaim(live, r.now()) {
		log.Info("cell pool: skipped a drain, a placement claim holds the cell", "pool", pool.Name, "cell", cell.Name)
		return false
	}
	if fenced[cellKey(live.Namespace, live.Name)] {
		log.Info("cell pool: skipped a drain, a placement fence names the cell", "pool", pool.Name, "cell", cell.Name)
		return false
	}
	// The caller's fence set dates from the start of the pass. This read
	// answers with the fences that exist now, which is the only set that can
	// name a cell the placement pass fenced after that listing.
	fresh, err := r.fencedCells(ctx)
	if err != nil {
		log.Warn("cell pool: re-read placements before delete failed", "pool", pool.Name, "cell", cell.Name, "error", err)
		return false
	}
	if fresh[cellKey(live.Namespace, live.Name)] {
		log.Info("cell pool: skipped a drain, a fresh placement fence names the cell", "pool", pool.Name, "cell", cell.Name)
		return false
	}
	err = crdstore.DeleteIfUnchanged[v1alpha1.Controller](ctx, r.Store,
		live.Name, live.Namespace, string(live.UID), live.ResourceVersion)
	if err != nil {
		if apierrors.IsConflict(err) {
			log.Info("cell pool: skipped a drain, the cell changed during the pass", "pool", pool.Name, "cell", cell.Name)
		} else {
			log.Warn("cell pool: delete cell failed", "pool", pool.Name, "cell", cell.Name, "error", err)
		}
		return false
	}
	return true
}

// writeStatus computes and patches the pool status (design D6). owned is nil
// when the pass stopped before it listed anything, which happens only for a
// missing profile.
func (r *WarmPoolReconciler) writeStatus(ctx context.Context, pool *v1alpha1.CellPool, owned []*v1alpha1.Controller, reason string) {
	status := &v1alpha1.CellPoolStatus{ObservedGeneration: pool.Generation}
	summary := SummarizeCellPool(pool)
	tenants := map[string]bool{}
	for _, cell := range owned {
		count := cellTenantCount(cell)
		if count == 0 {
			status.WarmCells++
		} else {
			status.PlacedCells++
			capacity := resolveCellCapacity(cellMaxTenants(cell), &summary, r.chartMaxTenants())
			if spare := capacity - int32(count); spare > 0 {
				status.FreeTenantSlots += spare
			}
		}
		if !cellReady(cell) {
			status.UnreadyCells++
		}
		if cell.Status.Cell != nil {
			for _, t := range cell.Status.Cell.Tenants {
				tenants[t] = true
			}
		}
	}
	for t := range tenants {
		status.Tenants = append(status.Tenants, t)
	}
	sort.Strings(status.Tenants)

	condStatus := metav1.ConditionTrue
	message := ""
	switch reason {
	case v1alpha1.CellPoolReasonCellCapReached:
		condStatus = metav1.ConditionFalse
		message = fmt.Sprintf("the pool owns its maxCells of %d cells", pool.Spec.MaxCells)
	case v1alpha1.CellPoolReasonProfileMissing:
		condStatus = metav1.ConditionFalse
		message = fmt.Sprintf("the JenkinsVersionProfile %q does not exist", pool.Spec.ProfileRef)
	}
	status.Conditions = []metav1.Condition{{
		Type:               v1alpha1.CellPoolConditionPoolReady,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: pool.Generation,
		LastTransitionTime: r.poolReadyTransitionTime(pool, condStatus),
	}}

	if err := crdstore.PatchStatus[v1alpha1.CellPool](ctx, r.Store, pool.Name, "", status); err != nil {
		r.logger().Warn("cell pool: patch status failed", "pool", pool.Name, "error", err)
	}
}

// poolReadyTransitionTime keeps the stored timestamp while the condition's
// Status is unchanged, so the condition records when the state began.
func (r *WarmPoolReconciler) poolReadyTransitionTime(pool *v1alpha1.CellPool, next metav1.ConditionStatus) metav1.Time {
	for _, c := range pool.Status.Conditions {
		if c.Type == v1alpha1.CellPoolConditionPoolReady && c.Status == next && !c.LastTransitionTime.IsZero() {
			return c.LastTransitionTime
		}
	}
	return metav1.NewTime(r.now())
}

// freeCells returns the owned cells that hold no tenant.
func freeCells(owned []*v1alpha1.Controller) []*v1alpha1.Controller {
	free := make([]*v1alpha1.Controller, 0, len(owned))
	for _, cell := range owned {
		if cellTenantCount(cell) == 0 {
			free = append(free, cell)
		}
	}
	return free
}

// cellTenantCount reports how many tenants a cell holds.
func cellTenantCount(cell *v1alpha1.Controller) int {
	if cell.Status.Cell == nil {
		return 0
	}
	return len(cell.Status.Cell.Tenants)
}

// cellMaxTenants reports a cell's own spec.cell.maxTenants.
func cellMaxTenants(cell *v1alpha1.Controller) int32 {
	if cell.Spec.Cell == nil {
		return 0
	}
	return cell.Spec.Cell.MaxTenants
}

// cellReady reports whether a cell's CellReady condition reads True.
func cellReady(cell *v1alpha1.Controller) bool {
	for _, c := range cell.Status.Conditions {
		if c.Type == v1alpha1.ConditionCellReady {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

// cellOrdinal parses the ordinal suffix off a cell name built with prefix
// "cell-<pool>-". It reports false for a name that does not match, so a
// hand-created or foreign object never corrupts the ordinal scan.
func cellOrdinal(name, prefix string) (int, bool) {
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// resolvedProfileVersion returns the Jenkins version a cell created for
// profile should run: ResolveVersion when the profile pins an LTS line and
// carries one, else the profile's own Version.
func resolvedProfileVersion(profile *v1alpha1.JenkinsVersionProfile) string {
	if profile.Spec.ResolveVersion != "" {
		return profile.Spec.ResolveVersion
	}
	return profile.Spec.Version
}
