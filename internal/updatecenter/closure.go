package updatecenter

import (
	"context"
	"fmt"

	"github.com/varroaci/varroa-jenkins/internal/hpi"
	"github.com/varroaci/varroa-jenkins/internal/pluginver"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// maxClosureDepth bounds a pathological manifest chain. A legitimate plugin's
// mandatory closure is nowhere near this deep; exceeding it means a corrupt or
// hostile chain, not a real dependency graph.
const maxClosureDepth = 32

// Dependency-resolution statuses. The first four are terminal successes or
// warnings; the last four reject.
const (
	StatusSatisfiedStore       = "satisfied-store"
	StatusLockTooOld           = "lock-too-old"
	StatusDeclaredNotYetStored = "declared-not-yet-stored"
	StatusPlannedFetch         = "planned-fetch"

	StatusNotInStore          = "not-in-store"
	StatusUnreachable         = "unreachable"
	StatusMetadataUnavailable = "metadata-unavailable"
	StatusClosureUnverifiable = "closure-unverifiable"
)

// Resolution sources reported on a closure entry.
const (
	sourceStore    = "store"
	sourceDeclared = "declared"
	sourceUpstream = "upstream"
)

// ClosureEntry is one resolved mandatory dependency in the upload response.
type ClosureEntry struct {
	Name            string `json:"name"`
	Min             string `json:"min"`
	Status          string `json:"status"`
	ResolvedVersion string `json:"resolvedVersion,omitempty"`
	Source          string `json:"source,omitempty"`
	Fetched         bool   `json:"fetched,omitempty"`
}

// OptionalDependency is a recorded-but-never-resolved optional dependency.
type OptionalDependency struct {
	Name string `json:"name"`
	Min  string `json:"min"`
}

// UploadWarning is a non-rejecting finding.
type UploadWarning struct {
	Code    string `json:"code"`
	Plugin  string `json:"plugin"`
	Min     string `json:"min"`
	Message string `json:"message"`
}

// UnresolvedDependency is one row of the actionable rejection diff. The
// foundInStore/foundDeclared/foundUpstream triple is what makes the diff
// actionable, and is why the reason codes are per-dependency rather than a
// single top-level cause.
type UnresolvedDependency struct {
	Name          string  `json:"name"`
	Min           string  `json:"min"`
	Reason        string  `json:"reason"`
	FoundInStore  *string `json:"foundInStore"`
	FoundDeclared *string `json:"foundDeclared"`
	FoundUpstream *string `json:"foundUpstream"`
	Remediation   string  `json:"remediation"`
}

// plannedFetch is a dependency COMMIT must download. It is not part of the wire
// shape; the wire carries the ClosureEntry.
type plannedFetch struct {
	Name    string
	Version string
	SHA256  string // upstream base64, verbatim
}

// Plan is the byte-free result of the PLAN phase.
type Plan struct {
	Closure    []ClosureEntry
	Optional   []OptionalDependency
	Warnings   []UploadWarning
	Unresolved []UnresolvedDependency
	Fetches    []plannedFetch

	// TooDeep records that a branch exceeded maxClosureDepth. It is a structural
	// failure and outranks every other rejection.
	TooDeep bool
}

// storeLookup is the planner's narrow view of the blob store, so the planner is
// unit-testable without an OCI store.
type storeLookup interface {
	// Versions returns every version of name the store holds, in no order.
	Versions(ctx context.Context, name string) []string
	// Dependencies returns the dependency list recorded for name@version.
	//
	// authoritative reports whether the answer can be trusted as complete. The
	// dev.varroa.plugin.dependencies annotation is omitted when a plugin has no
	// dependencies, so its absence alone cannot distinguish "no dependencies"
	// from "a pack written before the annotation contract existed". The
	// discriminator is the pack config's `kind`, which every post-contract pack
	// carries and no legacy pack does.
	Dependencies(ctx context.Context, name, version string) (deps []hpi.Dependency, authoritative bool)
}

// metaResolver is the planner's narrow view of ucmeta.Resolver.
type metaResolver interface {
	ResolveExact(ctx context.Context, name, version string) ucmeta.Resolution
	ResolveSatisfying(ctx context.Context, name, minVersion string) ucmeta.Resolution
}

// closurePlanner walks a plugin's transitive mandatory dependency closure using
// metadata only. It fetches nothing.
type closurePlanner struct {
	store       storeLookup
	declared    DeclaredSet
	resolver    metaResolver
	pullThrough bool
}

// planClosure walks the transitive mandatory closure of root.
//
// It uses a worklist rather than a visited set, because the same plugin can be
// reached by two paths with different minimums: resolving B≥1 first must not
// validate away a later B≥2. A name whose minimum increases is re-evaluated AND
// re-descended. Termination is guaranteed because a requirement only ever
// increases and the version space reachable from a fixed metadata snapshot is
// finite; the depth cap additionally bounds a pathological chain.
func (p *closurePlanner) planClosure(ctx context.Context, root hpi.PluginManifest) Plan {
	var plan Plan

	requirements := map[string]string{} // name -> highest minimum seen
	depths := map[string]int{}
	entries := map[string]ClosureEntry{}
	order := []string{} // first-seen order, so the response is stable
	var queue []string

	// raise records a (possibly new, possibly higher) minimum for name and
	// enqueues it when that minimum actually moved.
	raise := func(name, minVersion string, depth int) {
		if depth > maxClosureDepth {
			plan.TooDeep = true
			return
		}
		cur, seen := requirements[name]
		if seen && pluginver.AtLeast(cur, minVersion) {
			return // the standing requirement already covers this one
		}
		if !seen {
			order = append(order, name)
			depths[name] = depth
		}
		requirements[name] = minVersion
		queue = append(queue, name)
	}

	for _, d := range root.Dependencies {
		if d.Optional {
			plan.Optional = append(plan.Optional, OptionalDependency{Name: d.Name, Min: d.Min})
			continue
		}
		raise(d.Name, d.Min, 1)
	}

	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		minVersion := requirements[name]

		entry, descendVersion, deps := p.resolveOne(ctx, name, minVersion)
		entries[name] = entry

		// Descent is universal: every mandatory dependency is descended into,
		// whatever its outcome. Stopping at satisfied-store would defeat the
		// purpose — served metadata carries no dependencies (D13) and a
		// pull-through pack holds exactly one plugin, so a stored direct
		// dependency can perfectly well have an absent nested dependency and the
		// caller would never be told. Bytes are fetched only for planned-fetch;
		// metadata descent is unconditional.
		if descendVersion == "" {
			continue
		}
		for _, d := range deps {
			if d.Optional {
				continue // optional dependencies are recorded, never resolved
			}
			raise(d.Name, d.Min, depths[name]+1)
		}
	}

	for _, name := range order {
		e := entries[name]
		plan.Closure = append(plan.Closure, e)
		switch e.Status {
		case StatusLockTooOld:
			plan.Warnings = append(plan.Warnings, UploadWarning{
				Code: StatusLockTooOld, Plugin: name, Min: e.Min,
				Message: fmt.Sprintf("the declared version %s is older than the minimum %s this plugin requires; "+
					"provisioning's plugin-version gate gets the final say", e.ResolvedVersion, e.Min),
			})
		case StatusDeclaredNotYetStored:
			plan.Warnings = append(plan.Warnings, UploadWarning{
				Code: StatusDeclaredNotYetStored, Plugin: name, Min: e.Min,
				Message: fmt.Sprintf("the declared version %s is not in the store yet; on-demand pull-through will "+
					"fetch it at that exact version", e.ResolvedVersion),
			})
		case StatusPlannedFetch:
			plan.Fetches = append(plan.Fetches, p.fetchFor(ctx, name, e.ResolvedVersion))
		}
	}

	plan.Unresolved = p.diff(ctx, order, entries, requirements)
	return plan
}

// resolveOne applies the decision tree to a single (name, min). It returns the
// closure entry, the version to descend into ("" = do not descend), and that
// version's own mandatory dependency list.
//
// The tree is total and mutually exclusive by construction: every path
// terminates in exactly one row. The store can only ever *satisfy* a
// requirement — it can never terminate one, which is why a stale stored copy
// falls through to the declared or upstream tier like any other unsatisfied
// dependency.
func (p *closurePlanner) resolveOne(ctx context.Context, name, minVersion string) (ClosureEntry, string, []hpi.Dependency) {
	entry := ClosureEntry{Name: name, Min: minVersion}

	// step 1 — the store satisfies it.
	if stored, ok := p.highestStored(ctx, name); ok && pluginver.AtLeast(stored, minVersion) {
		entry.Status = StatusSatisfiedStore
		entry.ResolvedVersion = stored
		entry.Source = sourceStore
		deps, status := p.dependenciesOf(ctx, name, stored)
		if status != "" {
			entry.Status = status
			return entry, "", nil
		}
		return entry, stored, deps
	}

	// step 2 — declared. Upgrading a declared plugin is never the upload's
	// decision, which is why this branch never falls through to step 3.
	if vhi, declared := p.declared.Highest(name); declared {
		entry.Source = sourceDeclared
		entry.ResolvedVersion = vhi

		if !pluginver.AtLeast(vhi, minVersion) {
			// row b — the pin cannot satisfy the requirement at all, so
			// availability is moot. D6 makes this a warning: provisioning's
			// pluginVersionConflict is the gate, and two gates for one condition
			// is the failure mode being avoided.
			entry.Status = StatusLockTooOld
			// Best-effort descent into what would actually be installed. An
			// undeterminable list here is NOT escalated to closure-unverifiable:
			// this row is already a warning that defers to provisioning.
			deps, status := p.dependenciesOf(ctx, name, vhi)
			if status != "" {
				return entry, "", nil
			}
			return entry, vhi, deps
		}
		if !p.pullThrough {
			// row e2 — air gap: the pin is right but nothing can make it appear.
			entry.Status = StatusNotInStore
			return entry, "", nil
		}
		// row c / c2 / c3 — the pin IS the version that would satisfy, so verify
		// it is actually resolvable before promising it. The existing on-demand
		// pull-through resolves the EXACT pinned version, which is precisely the
		// aged-pin case that can fail.
		switch res := p.resolver.ResolveExact(ctx, name, vhi); res.Outcome {
		case ucmeta.Resolved:
			entry.Status = StatusDeclaredNotYetStored
			return entry, vhi, mandatoryOf(res.Meta.Dependencies)
		case ucmeta.SourcesDegraded:
			entry.Status = StatusMetadataUnavailable
		default:
			entry.Status = StatusUnreachable
		}
		return entry, "", nil
	}

	// step 3 — not declared.
	if !p.pullThrough {
		entry.Status = StatusNotInStore // row e
		return entry, "", nil
	}
	switch res := p.resolver.ResolveSatisfying(ctx, name, minVersion); res.Outcome {
	case ucmeta.Resolved:
		entry.Status = StatusPlannedFetch // row f
		entry.ResolvedVersion = res.Meta.Version
		entry.Source = sourceUpstream
		entry.Fetched = true
		return entry, res.Meta.Version, mandatoryOf(res.Meta.Dependencies)
	case ucmeta.SourcesDegraded:
		entry.Status = StatusMetadataUnavailable // row g
	default:
		entry.Status = StatusUnreachable // row h
	}
	return entry, "", nil
}

// dependenciesOf returns name@version's own mandatory dependency list, in the
// mandated source order: the stored pack's annotation, then upstream metadata
// for that exact version, then a rejecting status.
//
// A degraded metadata source yields metadata-unavailable (retryable) rather than
// closure-unverifiable (permanent): the list may well be determinable on the
// next attempt, and returning a permanent rejection for a transient outage is
// the same mistake the outcome typing exists to avoid.
func (p *closurePlanner) dependenciesOf(ctx context.Context, name, version string) ([]hpi.Dependency, string) {
	if deps, authoritative := p.store.Dependencies(ctx, name, version); authoritative {
		return mandatoryOfHPI(deps), ""
	}
	if !p.pullThrough {
		return nil, StatusClosureUnverifiable
	}
	switch res := p.resolver.ResolveExact(ctx, name, version); res.Outcome {
	case ucmeta.Resolved:
		return mandatoryOf(res.Meta.Dependencies), ""
	case ucmeta.SourcesDegraded:
		return nil, StatusMetadataUnavailable
	default:
		return nil, StatusClosureUnverifiable
	}
}

// fetchFor looks up the upstream checksum for a planned fetch. The version was
// already resolved during PLAN, so this is a cache hit in practice.
func (p *closurePlanner) fetchFor(ctx context.Context, name, version string) plannedFetch {
	f := plannedFetch{Name: name, Version: version}
	if res := p.resolver.ResolveExact(ctx, name, version); res.Outcome == ucmeta.Resolved {
		f.SHA256 = res.Meta.SHA256
	}
	return f
}

// diff builds the actionable per-dependency rejection table. EVERY rejecting
// dependency is listed regardless of which envelope code the caller selects, so
// a caller sees all the problems in one round trip rather than discovering them
// one retry at a time.
func (p *closurePlanner) diff(ctx context.Context, order []string, entries map[string]ClosureEntry, requirements map[string]string) []UnresolvedDependency {
	var out []UnresolvedDependency
	for _, name := range order {
		e := entries[name]
		if !rejects(e.Status) {
			continue
		}
		minVersion := requirements[name]
		row := UnresolvedDependency{Name: name, Min: minVersion, Reason: e.Status}
		if v, ok := p.highestStored(ctx, name); ok {
			row.FoundInStore = &v
		}
		if v, ok := p.declared.Highest(name); ok {
			row.FoundDeclared = &v
		}
		if p.pullThrough {
			if res := p.resolver.ResolveSatisfying(ctx, name, "0"); res.Best != nil {
				v := res.Best.Version
				row.FoundUpstream = &v
			}
		}
		row.Remediation = remediationFor(e.Status, name, minVersion, row)
		out = append(out, row)
	}
	return out
}

func remediationFor(status, name, minVersion string, row UnresolvedDependency) string {
	switch status {
	case StatusNotInStore:
		return fmt.Sprintf("pull-through is disabled; seed %s via spec.seed.refs or `varroactl import --to uc://…`", name)
	case StatusUnreachable:
		if row.FoundDeclared != nil {
			return fmt.Sprintf("the declared version %s of %s is listed by no healthy metadata source — an aged pin. "+
				"Pre-seed it with `varroactl export`/`import` or spec.seed.refs, or refresh the pin",
				*row.FoundDeclared, name)
		}
		if row.FoundUpstream != nil {
			return fmt.Sprintf("upstream's newest version %s is older than the required minimum %s", *row.FoundUpstream, minVersion)
		}
		return fmt.Sprintf("no metadata source lists %s at all; seed it via spec.seed.refs or `varroactl import --to uc://…`", name)
	case StatusMetadataUnavailable:
		return "one or more upstream metadata sources are unavailable; this is retryable"
	case StatusClosureUnverifiable:
		return fmt.Sprintf("%s's own dependency list cannot be determined: re-import it as a pack carrying the "+
			"dependency annotation, or pre-seed a version upstream still lists", name)
	default:
		return ""
	}
}

func rejects(status string) bool {
	switch status {
	case StatusNotInStore, StatusUnreachable, StatusMetadataUnavailable, StatusClosureUnverifiable:
		return true
	default:
		return false
	}
}

// highestStored returns the highest version of name the store holds.
func (p *closurePlanner) highestStored(ctx context.Context, name string) (string, bool) {
	versions := p.store.Versions(ctx, name)
	if len(versions) == 0 {
		return "", false
	}
	best := versions[0]
	for _, v := range versions[1:] {
		if pluginver.Compare(v, best) > 0 {
			best = v
		}
	}
	return best, true
}

func mandatoryOf(deps []ucmeta.Dep) []hpi.Dependency {
	out := make([]hpi.Dependency, 0, len(deps))
	for _, d := range deps {
		if d.Optional {
			continue
		}
		out = append(out, hpi.Dependency{Name: d.Name, Min: d.Version})
	}
	return out
}

func mandatoryOfHPI(deps []hpi.Dependency) []hpi.Dependency {
	out := make([]hpi.Dependency, 0, len(deps))
	for _, d := range deps {
		if d.Optional {
			continue
		}
		out = append(out, d)
	}
	return out
}

// ---------------------------------------------------------------------------
// Envelope selection (§5.4)
// ---------------------------------------------------------------------------

// Envelope codes returned by the upload endpoint.
const (
	ErrClosureTooDeep          = "closure-too-deep"
	ErrClosureUnverifiable     = "closure-unverifiable"
	ErrUnresolvedDependencies  = "unresolved-dependencies"
	ErrMetadataUnavailable     = "metadata-unavailable"
	ErrDeclaredSetUnavailable  = "declared-set-unavailable"
	ErrInvalidArtifact         = "invalid-artifact"
	ErrMissingManifestFields   = "missing-manifest-fields"
	ErrDuplicate               = "duplicate"
	ErrVersionDigestConflict   = "version-digest-conflict"
	ErrTooLarge                = "too-large"
	ErrUploadsRequireSingleWri = "uploads-require-single-writer"
	ErrFetchFailed             = "fetch-failed"
)

// envelope selects the rejection envelope for a plan, or ("", 0) when the plan
// is acceptable.
//
// The order is total: structural failures (the graph could not be walked at all)
// before validation failures (it was walked but a branch could not be checked)
// before resolution failures, and PERMANENT before RETRYABLE — a 503 must never
// be returned when something in the closure will still be broken after a retry.
func (plan *Plan) envelope() (string, int, string) {
	if plan.TooDeep {
		return ErrClosureTooDeep, 422, fmt.Sprintf("the dependency closure exceeded the depth cap of %d", maxClosureDepth)
	}

	var unverifiable, unresolved, degraded int
	for _, e := range plan.Closure {
		switch e.Status {
		case StatusClosureUnverifiable:
			unverifiable++
		case StatusNotInStore, StatusUnreachable:
			unresolved++
		case StatusMetadataUnavailable:
			degraded++
		}
	}
	total := len(plan.Closure)
	switch {
	case unverifiable > 0:
		return ErrClosureUnverifiable, 422,
			fmt.Sprintf("%d of %d mandatory dependencies have an undeterminable dependency list", unverifiable, total)
	case unresolved > 0:
		return ErrUnresolvedDependencies, 422,
			fmt.Sprintf("%d of %d mandatory dependencies could not be resolved", unresolved, total)
	case degraded > 0:
		return ErrMetadataUnavailable, 503,
			fmt.Sprintf("%d of %d mandatory dependencies could not be checked: upstream metadata is degraded", degraded, total)
	}
	return "", 0, ""
}
