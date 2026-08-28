package api

import (
	"sort"
	"strings"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/pluginrange"
	"github.com/varroaci/varroa-jenkins/internal/pluginver"
)

// ---------------------------------------------------------------------------
// Input types
// ---------------------------------------------------------------------------

// controllerRow is one controller row fed into the aggregator. It is already
// RBAC-filtered and carries only controllers the caller may read.
type controllerRow struct {
	Cluster, Namespace, Name string
	Phase                    v1alpha1.ControllerPhase
	Inv                      ControllerInventory
	HasInv                   bool

	// Surfaced from Controller.status.pluginInventory, never recomputed.
	Stale, Degraded, Truncated, OptionalEdgesDropped bool
	BootstrapApproximate                             bool
	Source                                           string

	// Envelope is the classified record's own header, and DetailStale is the
	// result of the cross-check against status.
	Envelope    ClassifiedEnvelope
	DetailStale bool
}

// fleetInput is the complete, already-fetched, already-RBAC-filtered basis for
// both aggregations. Rows come only from clusters that answered; Statuses
// carries every cluster the fan-out attempted, answered or not, and is the only
// source for coverage.complete and coverage.clustersNotCovered. A cluster that
// is not covered contributes no rows at all.
type fleetInput struct {
	Rows     []controllerRow
	Statuses []ClusterFanoutStatus
}

// ---------------------------------------------------------------------------
// Output types
// ---------------------------------------------------------------------------

// RollupItem is one plugin-name row in the rollup response.
type RollupItem struct {
	Name            string         `json:"name"`
	ControllerCount int            `json:"controllerCount"`
	Versions        []VersionCount `json:"versions"`
	Classes         []ClassCount   `json:"classes"`
}

// VersionCount is one entry in the versions histogram.
type VersionCount struct {
	Version         string `json:"version"`
	ControllerCount int    `json:"controllerCount"`
}

// ClassCount is one entry in the provenance-class breakdown.
type ClassCount struct {
	Class           string `json:"class"`
	ControllerCount int    `json:"controllerCount"`
}

// DrillItem is one controller row in the drilldown response.
type DrillItem struct {
	Cluster              string `json:"cluster"`
	Namespace            string `json:"namespace"`
	Controller           string `json:"controller"`
	Version              string `json:"version"`
	Class                string `json:"class"`
	Source               string `json:"source"`
	CollectedAt          string `json:"collectedAt"`
	DetailPath           string `json:"detailPath"`
	DetailStale          bool   `json:"detailStale"`
	Stale                bool   `json:"stale"`
	Degraded             bool   `json:"degraded"`
	Truncated            bool   `json:"truncated"`
	OptionalEdgesDropped bool   `json:"optionalEdgesDropped"`
	BootstrapApproximate bool   `json:"bootstrapApproximate"`
}

// Coverage is the fleet-observability block returned beside items.
type Coverage struct {
	Complete               bool                `json:"complete"`
	ControllersTotal       int                 `json:"controllersTotal"`
	ControllersReporting   int                 `json:"controllersReporting"`
	ControllersStale       int                 `json:"controllersStale"`
	ControllersDegraded    int                 `json:"controllersDegraded"`
	ControllersTruncated   int                 `json:"controllersTruncated"`
	ControllersDetailStale int                 `json:"controllersDetailStale"`
	ControllersMissing     []MissingController `json:"controllersMissing"`
	ClustersNotCovered     int                 `json:"clustersNotCovered"`
}

// MissingController is one controller with no observed inventory.
type MissingController struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Reason    string `json:"reason"`
}

// ---------------------------------------------------------------------------
// Rollup
// ---------------------------------------------------------------------------

// Rollup aggregates rows into one item per plugin name.
//
// q is a case-insensitive substring filter on plugin name (only on the rollup).
// affected is the version-range filter. Both filter items only and never touch
// Coverage. No quality flag ever excludes a row.
func Rollup(in fleetInput, q string, affected pluginrange.Expr) ([]RollupItem, Coverage) {
	// Index: plugin name → per-controller (name, version, class).
	byPlugin := make(map[string][]fleetPluginEntry)
	for _, r := range in.Rows {
		if !r.HasInv {
			continue
		}
		for _, p := range r.Inv.Plugins {
			// q filter is case-insensitive substring on plugin name.
			if q != "" && !strings.Contains(strings.ToLower(p.Name), strings.ToLower(q)) {
				continue
			}
			// affected filter on installed version.
			if !affected.Empty() && !affected.Match(p.Version) {
				continue
			}
			byPlugin[p.Name] = append(byPlugin[p.Name], fleetPluginEntry{
				name:    p.Name,
				version: p.Version,
				class:   p.Class,
			})
		}
	}

	// Build rollup items.
	items := make([]RollupItem, 0, len(byPlugin))
	for name, entries := range byPlugin {
		item := RollupItem{
			Name:            name,
			ControllerCount: len(entries),
			Versions:        versionHistogram(entries),
			Classes:         classBreakdown(entries),
		}
		items = append(items, item)
	}

	// Sort by plugin name ascending for determinism.
	sort.Slice(items, func(i, j int) bool {
		return items[i].Name < items[j].Name
	})

	cov := computeCoverage(in)
	return items, cov
}

// ---------------------------------------------------------------------------
// Drill
// ---------------------------------------------------------------------------

// Drill returns one item per (cluster, namespace, controller) for the given
// plugin name. The name is matched exactly, never as a substring.
func Drill(in fleetInput, name string, affected pluginrange.Expr) ([]DrillItem, []VersionCount, Coverage) {
	var items []DrillItem
	versions := map[string]int{}

	for _, r := range in.Rows {
		if !r.HasInv {
			continue
		}
		for _, p := range r.Inv.Plugins {
			if p.Name != name {
				continue // exact match only
			}
			if !affected.Empty() && !affected.Match(p.Version) {
				continue
			}
			di := DrillItem{
				Cluster:              r.Cluster,
				Namespace:            r.Namespace,
				Controller:           r.Name,
				Version:              p.Version,
				Class:                p.Class,
				Source:               r.Source,
				CollectedAt:          r.Inv.CollectedAt.UTC().Format("2006-01-02T15:04:05Z"),
				DetailPath:           controllerDetailPath(r.Cluster, r.Namespace, r.Name),
				DetailStale:          r.DetailStale,
				Stale:                r.Stale,
				Degraded:             r.Degraded,
				Truncated:            r.Truncated,
				OptionalEdgesDropped: r.OptionalEdgesDropped,
				BootstrapApproximate: r.BootstrapApproximate,
			}
			items = append(items, di)
			versions[p.Version]++
		}
	}

	cov := computeCoverage(in)
	return items, versionHistogramFromMap(versions), cov
}

// ---------------------------------------------------------------------------
// Version histogram
// ---------------------------------------------------------------------------

// fleetPluginEntry is a single plugin occurrence used during aggregation.
type fleetPluginEntry struct {
	name    string
	version string
	class   string
}

// versionHistogram collects version → count and orders by the rank key.
func versionHistogram(entries []fleetPluginEntry) []VersionCount {
	counts := make(map[string]int)
	for _, e := range entries {
		counts[e.version]++
	}
	return versionHistogramFromMap(counts)
}

// versionHistogramFromMap orders a version → count map by:
//
//	rank(v) = |{w : pluginver.Compare(v, w) > 0}| descending,
//	ties broken by raw version string descending (bytewise).
//
// rank is a pure function of the set, so output is independent of input order.
func versionHistogramFromMap(counts map[string]int) []VersionCount {
	if len(counts) == 0 {
		return []VersionCount{}
	}

	versions := make([]string, 0, len(counts))
	for v := range counts {
		versions = append(versions, v)
	}

	// Pre-compute ranks: for each version v, count how many w it compares >.
	rank := make(map[string]int, len(versions))
	for _, v := range versions {
		r := 0
		for _, w := range versions {
			if v == w {
				continue
			}
			if pluginver.Compare(v, w) > 0 {
				r++
			}
		}
		rank[v] = r
	}

	sort.Slice(versions, func(i, j int) bool {
		ri, rj := rank[versions[i]], rank[versions[j]]
		if ri != rj {
			return ri > rj // descending rank
		}
		// Tiebreak: raw version string descending bytewise.
		return versions[i] > versions[j]
	})

	out := make([]VersionCount, len(versions))
	for i, v := range versions {
		out[i] = VersionCount{Version: v, ControllerCount: counts[v]}
	}
	return out
}

// ---------------------------------------------------------------------------
// Class breakdown
// ---------------------------------------------------------------------------

// classBreakdown orders classes by class label ascending, bytewise.
// Deterministic, and explicitly not a ranking (R19).
func classBreakdown(entries []fleetPluginEntry) []ClassCount {
	counts := make(map[string]int)
	for _, e := range entries {
		counts[e.class]++
	}

	classes := make([]string, 0, len(counts))
	for c := range counts {
		classes = append(classes, c)
	}
	sort.Strings(classes) // bytewise ascending

	out := make([]ClassCount, len(classes))
	for i, c := range classes {
		out[i] = ClassCount{Class: c, ControllerCount: counts[c]}
	}
	return out
}

// ---------------------------------------------------------------------------
// Coverage
// ---------------------------------------------------------------------------

// computeCoverage derives coverage from in.Statuses and in.Rows.
// complete and clustersNotCovered derive from in.Statuses, never from rows.
func computeCoverage(in fleetInput) Coverage {
	// Determine complete from statuses.
	complete := true
	clustersNotCovered := 0
	for _, s := range in.Statuses {
		if !s.OK {
			complete = false
			clustersNotCovered++
		}
	}

	total := len(in.Rows)

	reporting := 0
	stale := 0
	degraded := 0
	truncated := 0
	detailStale := 0
	var missing []MissingController

	for _, r := range in.Rows {
		if r.HasInv {
			reporting++
			if r.Stale {
				stale++
			}
			if r.Degraded {
				degraded++
			}
			if r.Truncated {
				truncated++
			}
			if r.DetailStale {
				detailStale++
			}
		} else {
			missing = append(missing, MissingController{
				Cluster:   r.Cluster,
				Namespace: r.Namespace,
				Name:      r.Name,
				Reason:    reason(r.Phase),
			})
		}
	}

	// Sort missing controllers for determinism: by cluster, namespace, name.
	sort.Slice(missing, func(i, j int) bool {
		if missing[i].Cluster != missing[j].Cluster {
			return missing[i].Cluster < missing[j].Cluster
		}
		if missing[i].Namespace != missing[j].Namespace {
			return missing[i].Namespace < missing[j].Namespace
		}
		return missing[i].Name < missing[j].Name
	})

	return Coverage{
		Complete:               complete,
		ControllersTotal:       total,
		ControllersReporting:   reporting,
		ControllersStale:       stale,
		ControllersDegraded:    degraded,
		ControllersTruncated:   truncated,
		ControllersDetailStale: detailStale,
		ControllersMissing:     missing,
		ClustersNotCovered:     clustersNotCovered,
	}
}

// ---------------------------------------------------------------------------
// reason
// ---------------------------------------------------------------------------

// reason returns the coverage reason label for a controller with no observed
// inventory, derived entirely from the controller's lifecycle phase.
//
// Connected → never-reported, Hibernated → hibernated, Stopped → stopped,
// everything else → not-connected (including Pending, Provisioning, Running,
// Failed, the empty string, and any future phase).
//
// These labels describe the persisted lifecycle phase, not live connectivity.
func reason(phase v1alpha1.ControllerPhase) string {
	switch phase {
	case v1alpha1.ControllerPhaseConnected:
		return "never-reported"
	case v1alpha1.ControllerPhaseHibernated:
		return "hibernated"
	case v1alpha1.ControllerPhaseStopped:
		return "stopped"
	default:
		return "not-connected"
	}
}

// ---------------------------------------------------------------------------
// Envelope cross-check
// ---------------------------------------------------------------------------

// checkEnvelope compares the classified record's envelope against the status
// summary fields. It compares exactly eight fields: hash, source, stale,
// degraded, truncated, optionalEdgesDropped, bootstrapApproximate, and
// driftTruncated. Any difference sets detailStale to true.
//
// Comparing the hash alone is not sufficient: a disconnect or a
// collection-failure freshness transition can flip stale or degraded while the
// inventory hash is unchanged, so a hash-only check would call an out-of-date
// classification fresh. T2.1's guard compares both; so does this one.
func checkEnvelope(env ClassifiedEnvelope, hash, source string, stale, degraded, truncated, optionalEdgesDropped, bootstrapApproximate, driftTruncated bool) bool {
	if env.Hash != hash {
		return true
	}
	if env.Source != source {
		return true
	}
	if env.Stale != stale {
		return true
	}
	if env.Degraded != degraded {
		return true
	}
	if env.Truncated != truncated {
		return true
	}
	if env.OptionalEdgesDropped != optionalEdgesDropped {
		return true
	}
	if env.BootstrapApproximate != bootstrapApproximate {
		return true
	}
	if env.DriftTruncated != driftTruncated {
		return true
	}
	return false
}

// controllerDetailPath builds the path to the per-controller classified
// sub-resource (R14).
func controllerDetailPath(cluster, namespace, name string) string {
	// Path shape: /api/v1/clusters/{cluster}/controllers/{namespace}/{name}/plugins
	return "/api/v1/clusters/" + cluster + "/controllers/" + namespace + "/" + name + "/plugins"
}
