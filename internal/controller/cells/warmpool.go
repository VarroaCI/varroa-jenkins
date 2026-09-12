// Package cells holds the tenant-placement and warm-pool logic for the
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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

const (
	// LabelCellProfile names the JenkinsVersionProfile a cell was created
	// for. The warm pool sets it at creation and never updates it; the
	// placement pass reads it to find a candidate cell for a profile.
	LabelCellProfile = "varroa.dev/cell-profile"

	// LabelWarm marks a cell the warm pool created. Set at creation and
	// never updated. Controller.status.cell.warm is the live signal for
	// whether a cell is still a free pool member; this label only records
	// where the cell came from.
	LabelWarm = "varroa.dev/warm"
)

// WarmPoolConfig carries the pool's boot-time dial state, read once from
// the chart values cells.warmPool.enabled and cells.warmPool.perProfile
// (design D5).
type WarmPoolConfig struct {
	// Enabled turns the pool on. A disabled pool lists nothing and creates
	// nothing.
	Enabled bool
	// PerProfile is the target free-cell count for each requested profile.
	PerProfile int
}

// WarmPoolReconciler keeps a pool of unassigned cells for every profile a
// pipelines Team requests (design D5). It writes no status: it only
// creates and deletes Controller CRDs, so it never races a status writer.
type WarmPoolReconciler struct {
	Store             crdstore.Backend
	OperatorNamespace string
	Config            WarmPoolConfig
	Logger            *slog.Logger
}

func (r *WarmPoolReconciler) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// Reconcile runs one warm-pool pass. It returns at once, before listing
// anything, when the pool is disabled.
func (r *WarmPoolReconciler) Reconcile(ctx context.Context) error {
	if !r.Config.Enabled {
		return nil
	}

	profiles, err := r.requestedProfiles(ctx)
	if err != nil {
		return fmt.Errorf("collect requested profiles: %w", err)
	}

	for _, profile := range profiles {
		r.reconcileProfile(ctx, profile)
	}
	return nil
}

// requestedProfiles collects every profile any pipelines Team names, plus
// every profile a placed tenant currently uses (design D5 step 1). The
// second set is read off the tenant's bound cell rather than the Team's own
// spec, so a Team whose spec.pipelines.profile has since changed still
// keeps the pool filled for the profile its live cell actually runs.
func (r *WarmPoolReconciler) requestedProfiles(ctx context.Context) ([]string, error) {
	teams, err := crdstore.List[v1alpha1.Team](ctx, r.Store, "", "")
	if err != nil {
		return nil, fmt.Errorf("list Teams: %w", err)
	}

	set := map[string]bool{}
	for _, team := range teams {
		if team.Spec.Pipelines != nil && team.Spec.Pipelines.Profile != "" {
			set[team.Spec.Pipelines.Profile] = true
		}
		if team.Status.Placement != nil {
			if profile := r.placedProfile(ctx, team.Status.Placement); profile != "" {
				set[profile] = true
			}
		}
	}

	profiles := make([]string, 0, len(set))
	for profile := range set {
		profiles = append(profiles, profile)
	}
	sort.Strings(profiles)
	return profiles, nil
}

// placedProfile reads the profile label off a placed tenant's bound cell.
// A cell already torn down, or a placement naming no cell, reports no
// profile: the pool then relies on the Team's own spec.pipelines.profile,
// already collected by the caller.
func (r *WarmPoolReconciler) placedProfile(ctx context.Context, placement *v1alpha1.TeamPlacementStatus) string {
	if placement.Cell == "" {
		return ""
	}
	cell, err := crdstore.Get[v1alpha1.Controller](ctx, r.Store, placement.Cell, placement.CellNamespace)
	if err != nil {
		return ""
	}
	return cell.Labels[LabelCellProfile]
}

// reconcileProfile applies design D5 steps 2 through 4 for one profile: list
// its labelled cells, then fill or drain to the configured perProfile
// target. A missing JenkinsVersionProfile is logged and skipped — the
// placement pass already refuses a Team naming an absent profile, so this
// is a transient race, not a steady-state condition.
func (r *WarmPoolReconciler) reconcileProfile(ctx context.Context, profile string) {
	log := r.logger()

	versionProfile, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](ctx, r.Store, profile, "")
	if err != nil {
		log.Warn("warm pool: profile not found, skipping", "profile", profile, "error", err)
		return
	}

	prefix := fmt.Sprintf("cell-%s-", profile)
	selector := fmt.Sprintf("%s=%s", LabelCellProfile, profile)
	existing, err := crdstore.List[v1alpha1.Controller](ctx, r.Store, r.OperatorNamespace, selector)
	if err != nil {
		log.Warn("warm pool: list cells failed", "profile", profile, "error", err)
		return
	}

	free := make([]*v1alpha1.Controller, 0, len(existing))
	used := map[int]bool{}
	for _, cell := range existing {
		if ordinal, ok := cellOrdinal(cell.Name, prefix); ok {
			used[ordinal] = true
		}
		if cellIsFree(cell) {
			free = append(free, cell)
		}
	}

	switch {
	case len(free) < r.Config.PerProfile:
		r.fillProfile(ctx, profile, resolvedProfileVersion(versionProfile), used, r.Config.PerProfile-len(free))
	case len(free) > r.Config.PerProfile:
		r.drainSurplus(ctx, prefix, free, len(free)-r.Config.PerProfile)
	}
}

// cellIsFree reports whether cell holds no tenant, the warm pool's
// definition of a free member (design D5 steps 3 and 4). It is
// independent of the CellReady/WarmPool conditions the controller
// reconciler computes: the pool counts by status.cell.tenants alone.
func cellIsFree(cell *v1alpha1.Controller) bool {
	return cell.Status.Cell == nil || len(cell.Status.Cell.Tenants) == 0
}

// cellOrdinal parses the ordinal suffix off a cell name built with prefix
// "cell-<profile>-". It reports false for a name that does not match, so a
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
// carries one, else the profile's own Version (design D5's
// spec.version: <profile.spec.resolveVersion>).
func resolvedProfileVersion(profile *v1alpha1.JenkinsVersionProfile) string {
	if profile.Spec.ResolveVersion != "" {
		return profile.Spec.ResolveVersion
	}
	return profile.Spec.Version
}

// fillProfile creates count new free cells for profile, starting from the
// lowest non-negative ordinal not already in used (design D5's <ordinal>
// rule). used is updated in place so a multi-cell fill never reuses a name
// it just picked.
func (r *WarmPoolReconciler) fillProfile(ctx context.Context, profile, version string, used map[int]bool, count int) {
	log := r.logger()
	next := 0
	for i := 0; i < count; i++ {
		for used[next] {
			next++
		}
		cell := buildWarmPoolCell(r.OperatorNamespace, profile, version, next)
		if err := crdstore.Create[v1alpha1.Controller](ctx, r.Store, cell); err != nil {
			log.Warn("warm pool: create cell failed", "profile", profile, "name", cell.Name, "error", err)
			return
		}
		used[next] = true
		next++
	}
}

// buildWarmPoolCell returns the Controller a warm-pool fill creates: an
// ordinary cell, named cell-<profile>-<ordinal>, in the operator namespace,
// labelled for the pool's own bookkeeping (design D5).
func buildWarmPoolCell(operatorNamespace, profile, version string, ordinal int) *v1alpha1.Controller {
	return &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("cell-%s-%d", profile, ordinal),
			Namespace: operatorNamespace,
			Labels: map[string]string{
				LabelCellProfile: profile,
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
}

// drainSurplus deletes count free cells of profile, highest ordinal first,
// so the pool always keeps the lowest-numbered members and a later fill
// reuses the freed high ordinals (design D5 step 4: delete a member only
// when it holds no tenant and the free count is above perProfile).
func (r *WarmPoolReconciler) drainSurplus(ctx context.Context, prefix string, free []*v1alpha1.Controller, count int) {
	log := r.logger()
	sort.Slice(free, func(i, j int) bool {
		oi, oki := cellOrdinal(free[i].Name, prefix)
		oj, okj := cellOrdinal(free[j].Name, prefix)
		if oki && okj {
			return oi > oj
		}
		return free[i].Name > free[j].Name
	})
	for i := 0; i < count && i < len(free); i++ {
		cell := free[i]
		if err := crdstore.Delete[v1alpha1.Controller](ctx, r.Store, cell.Name, cell.Namespace); err != nil {
			log.Warn("warm pool: delete surplus cell failed", "name", cell.Name, "error", err)
		}
	}
}
