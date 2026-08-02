package updatecenter

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/varroaci/varroa-jenkins/internal/pluginver"
)

// pluginDep is one declared dependency of a stored plugin, in the shape T1.1's
// dev.varroa.plugin.dependencies annotation carries. Min is a MINIMUM, not a
// pin, and optional dependencies are excluded from every closure — the two
// properties the upload planner, the derived-catalog solver, and the drift
// closure must all agree on.
type pluginDep struct {
	Name     string `json:"name"`
	Min      string `json:"min"`
	Optional bool   `json:"optional"`
}

// inventoryEntry is the JSON shape of one plugin in /api/v1/inventory's
// "plugins" array. Every field past sizeBytes is optional: packs written
// before T1.1's annotations existed carry only name/version/sha256/upstreamUrl,
// and such a plugin must stay listable.
type inventoryEntry struct {
	Name         string      `json:"name"`
	Version      string      `json:"version"`
	SHA256       string      `json:"sha256"`
	SizeBytes    int64       `json:"sizeBytes"`
	DisplayName  string      `json:"displayName,omitempty"`
	Description  string      `json:"description,omitempty"`
	Tags         []string    `json:"tags,omitempty"`
	RequiredCore string      `json:"requiredCore,omitempty"`
	Dependencies []pluginDep `json:"dependencies,omitempty"`
}

// skippedPackEntry is the JSON shape of one unreadable plugin-pack manifest
// disclosed in /api/v1/inventory's "skippedPacks" array.
type skippedPackEntry struct {
	Ref   string `json:"ref"`
	Error string `json:"error"`
}

// inventoryResponse is the JSON shape of /api/v1/inventory: the plugin listing
// built from every readable pack, plus any pack the scan could not read.
//
// A non-empty SkippedPacks means Plugins is a LOWER BOUND on store contents,
// not a complete listing: any plugin held only by an unreadable pack is
// absent, indistinguishable from having been deleted. A consumer that prunes
// against this listing (the operator's derived-catalog sync) MUST treat a
// non-empty SkippedPacks as a reason to withhold pruning for this pass, not as
// evidence those plugins are gone.
type inventoryResponse struct {
	Plugins      []inventoryEntry   `json:"plugins"`
	SkippedPacks []skippedPackEntry `json:"skippedPacks,omitempty"`
}

// canonicalize normalizes the list-valued fields so that two semantically
// identical entries have byte-identical canonical forms. Without it, "the whole
// entry value" would not be a well-defined dedupe key: two entries differing
// only in tag order would read as a store conflict.
//
// tags are deduplicated and sorted. dependencies are sorted by name; a name
// repeated within one entry collapses to its greatest min under the pluginver
// total order, staying optional only if every occurrence was optional; an entry
// with an empty name is dropped. Because that order is total over every string,
// an arbitrary min is still ordered rather than rejected, so canonicalization
// never has to decide whether a version "parses".
func (e inventoryEntry) canonicalize() inventoryEntry {
	if len(e.Tags) > 0 {
		seen := make(map[string]struct{}, len(e.Tags))
		tags := make([]string, 0, len(e.Tags))
		for _, t := range e.Tags {
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			tags = append(tags, t)
		}
		sort.Strings(tags)
		e.Tags = tags
	}

	if len(e.Dependencies) > 0 {
		merged := make(map[string]pluginDep, len(e.Dependencies))
		order := make([]string, 0, len(e.Dependencies))
		for _, d := range e.Dependencies {
			if d.Name == "" {
				continue
			}
			prev, ok := merged[d.Name]
			if !ok {
				merged[d.Name] = d
				order = append(order, d.Name)
				continue
			}
			if pluginver.Compare(d.Min, prev.Min) > 0 {
				prev.Min = d.Min
			}
			prev.Optional = prev.Optional && d.Optional
			merged[d.Name] = prev
		}
		sort.Strings(order)
		deps := make([]pluginDep, 0, len(order))
		for _, n := range order {
			deps = append(deps, merged[n])
		}
		if len(deps) == 0 {
			deps = nil
		}
		e.Dependencies = deps
	}

	return e
}

// compareEntries is the total order the response is sorted by, over canonical
// entries: name, version, sha256, then the remaining fields in declaration
// order. Being total over every field is what makes both the payload and the
// operator's inventory digest stable across scans, given ListManifests is
// unordered on both backends.
func compareEntries(a, b inventoryEntry) int {
	if c := cmpStr(a.Name, b.Name); c != 0 {
		return c
	}
	if c := cmpStr(a.Version, b.Version); c != 0 {
		return c
	}
	if c := cmpStr(a.SHA256, b.SHA256); c != 0 {
		return c
	}
	switch {
	case a.SizeBytes < b.SizeBytes:
		return -1
	case a.SizeBytes > b.SizeBytes:
		return 1
	}
	if c := cmpStr(a.DisplayName, b.DisplayName); c != 0 {
		return c
	}
	if c := cmpStr(a.Description, b.Description); c != 0 {
		return c
	}
	if c := cmpStrSlice(a.Tags, b.Tags); c != 0 {
		return c
	}
	if c := cmpStr(a.RequiredCore, b.RequiredCore); c != 0 {
		return c
	}
	return cmpDeps(a.Dependencies, b.Dependencies)
}

func cmpStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpStrSlice(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := cmpStr(a[i], b[i]); c != 0 {
			return c
		}
	}
	return len(a) - len(b)
}

func cmpDeps(a, b []pluginDep) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := cmpStr(a[i].Name, b[i].Name); c != 0 {
			return c
		}
		if c := cmpStr(a[i].Min, b[i].Min); c != 0 {
			return c
		}
		if a[i].Optional != b[i].Optional {
			if a[i].Optional {
				return 1
			}
			return -1
		}
	}
	return len(a) - len(b)
}

// handleInventory serves GET /api/v1/inventory: the plugin listing built from
// every readable pack, plus any pack the scan could not read.
//
// It degrades gracefully rather than failing closed: one unreadable manifest
// (e.g. a legacy pack written before the pack-kind field existed) no longer
// takes the whole inventory offline. The readable packs are served, and every
// unreadable one is disclosed by ref and error in "skippedPacks" so a caller —
// human or the operator's derived-catalog sync — can act on the specifics
// instead of a bare 503.
//
// It still fails closed in exactly one case: the scan found NO readable pack
// at all while some were skipped. A 200 with an empty "plugins" array there
// would be indistinguishable from a genuinely empty store and would license a
// pruning caller to delete everything; a scan that read every pack and simply
// found zero packs (skippedPacks empty) is the genuine-empty-store case and
// still returns 200.
func (s *Server) handleInventory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	packs, skipped, err := s.listPackInfos(r.Context())
	if err != nil {
		s.logger.Warn("failed to list pack infos for inventory", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read store")
		return
	}

	skippedEntries := make([]skippedPackEntry, len(skipped))
	refs := make([]string, len(skipped))
	for i, sp := range skipped {
		skippedEntries[i] = skippedPackEntry(sp)
		refs[i] = sp.Ref
	}

	if len(packs) == 0 && len(skipped) > 0 {
		s.logger.Warn("no plugin packs were readable; refusing to serve an empty inventory",
			"unreadableManifests", len(skipped), "refs", refs)
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error": fmt.Sprintf(
				"store scan found no readable plugin pack: %d manifest(s) could not be read",
				len(skipped)),
			"skippedPacks": skippedEntries,
		})
		return
	}

	if len(skipped) > 0 {
		s.logger.Warn("serving a partial inventory: some plugin packs could not be read",
			"unreadableManifests", len(skipped), "refs", refs)
	}

	// Dedupe on the WHOLE canonical entry, not on (name, version). Two
	// manifests carrying the same plugin with different bytes or different
	// metadata both surface, and the caller treats that as a store integrity
	// failure for that one plugin rather than the service silently picking a
	// winner from an unordered manifest listing.
	seen := make(map[string]struct{})
	entries := make([]inventoryEntry, 0, len(packs))
	for _, pack := range packs {
		for _, p := range pack.Plugins {
			e := inventoryEntry{
				Name:         p.Name,
				Version:      p.Version,
				SHA256:       p.SHA256,
				SizeBytes:    p.SizeBytes,
				DisplayName:  p.DisplayName,
				Description:  p.Description,
				Tags:         p.Tags,
				RequiredCore: p.RequiredCore,
			}
			for _, d := range p.Dependencies {
				e.Dependencies = append(e.Dependencies, pluginDep{
					Name: d.Name, Min: d.Min, Optional: d.Optional,
				})
			}
			e = e.canonicalize()
			key := entryKey(e)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			entries = append(entries, e)
		}
	}

	sort.SliceStable(entries, func(i, j int) bool { return compareEntries(entries[i], entries[j]) < 0 })

	writeJSON(w, http.StatusOK, inventoryResponse{Plugins: entries, SkippedPacks: skippedEntries})
}

// entryKey renders a canonical entry as a comparison key. It is built from the
// same total order used for sorting, so "collapses under dedupe" and "compares
// equal in the sort" can never disagree.
func entryKey(e inventoryEntry) string {
	return fmt.Sprintf("%q|%q|%q|%d|%q|%q|%q|%q|%v",
		e.Name, e.Version, e.SHA256, e.SizeBytes,
		e.DisplayName, e.Description, e.Tags, e.RequiredCore, e.Dependencies)
}

// handleHealthz serves GET /healthz (always 200 once serving).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz serves GET /readyz (200 only after store is reachable).
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if !s.isReady() {
		// Lazy check: try to reach the store once.
		if _, err := s.store.ListManifests(context.TODO()); err == nil {
			s.MarkReady()
		}
	}

	if s.isReady() {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	} else {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
	}
}
