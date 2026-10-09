package updatecenter

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/pluginver"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// updateCenterJSON is the shape of the Jenkins update-center JSON payload.
type updateCenterJSON struct {
	UpdateCenterVersion string                        `json:"updateCenterVersion"`
	Core                string                        `json:"core"`
	Plugins             map[string]updateCenterPlugin `json:"plugins"`
}

type updateCenterPlugin struct {
	Name         string                   `json:"name"`
	Version      string                   `json:"version"`
	URL          string                   `json:"url"`
	SHA256       string                   `json:"sha256"`
	RequiredCore string                   `json:"requiredCore"`
	Dependencies []updateCenterDependency `json:"dependencies"`
}

// updateCenterDependency is one entry of a served plugin's dependency list.
// Version is the manifest minimum, as Jenkins reads it.
type updateCenterDependency struct {
	Name     string `json:"name"`
	Optional bool   `json:"optional"`
	Version  string `json:"version"`
}

// handleMetadataPlain serves GET /update-center.actual.json.
func (s *Server) handleMetadataPlain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	payload := s.buildMetadataPayload(r)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(payload)
}

// handleMetadataJSONP serves GET /update-center.json (JSONP-wrapped).
func (s *Server) handleMetadataJSONP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	payload := s.buildMetadataPayload(r)
	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		s.logger.Warn("failed to marshal metadata", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to marshal metadata")
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "updateCenter.post(%s);", jsonBytes)
}

// buildMetadataPayload regenerates the update-center JSON per-request
// from live store inventory. Each plugin lists its dependencies from its own
// manifest. A plugin whose dependency list is unknown is left out of the
// document, but it stays downloadable at its explicit version. core is always
// "", and updateCenterVersion is always "1".
//
// A ?version=<core> query names the requesting controller's core. The document then
// lists, for each plugin, the highest stored version that core runs. A declared
// latest plugin with no compatible stored version takes the newest compatible
// version from the upstream metadata sources, when pull-through is on. Without the
// query, the served version does not depend on the core.
func (s *Server) buildMetadataPayload(r *http.Request) *updateCenterJSON {
	uc := &updateCenterJSON{
		UpdateCenterVersion: "1",
		Core:                "",
		Plugins:             make(map[string]updateCenterPlugin),
	}
	core := strings.TrimSpace(r.URL.Query().Get("version"))

	winners := s.servedPlugins(r.Context(), core)
	// withheld names a stored winner that the document omits. Its name must not
	// be filled from upstream, because the stored bytes would not match the
	// advertised digest.
	withheld := make(map[string]bool)
	for name, winner := range winners {
		// Convert the stored hex digest ("sha256:<hex>") to base64 as
		// Jenkins' update-center schema expects.
		rawBytes, err := hex.DecodeString(stripDigestPrefix(winner.SHA256))
		if err != nil {
			s.logger.Warn("invalid hex digest for plugin", "name", name, "sha256", winner.SHA256, "error", err)
			withheld[name] = true
			continue
		}
		deps, known := s.pluginDependencies(r.Context(), winner)
		if !known {
			withheld[name] = true
			continue
		}
		served := make([]updateCenterDependency, 0, len(deps))
		for _, d := range deps {
			served = append(served, updateCenterDependency{Name: d.Name, Optional: d.Optional, Version: d.Min})
		}
		uc.Plugins[name] = updateCenterPlugin{
			Name:         name,
			Version:      winner.Version,
			URL:          name + "/" + winner.Version + "/" + name + ".hpi",
			SHA256:       base64.StdEncoding.EncodeToString(rawBytes),
			RequiredCore: winner.RequiredCore,
			Dependencies: served,
		}
	}

	declared, _ := ReadDeclaredPlugins(s.declaredPluginsFile)
	for _, name := range declaredLatestNames(declared) {
		if winner, stored := winners[name]; stored {
			// A stored winner that is withheld, compatible, or pinned by the declared
			// set stays as it is. Only an incompatible stored winner is replaced.
			if withheld[name] || ucmeta.CoreCompatible(winner.RequiredCore, core) || slices.Contains(declared[name], winner.Version) {
				continue
			}
		}
		if entry, ok := s.upstreamLatestEntry(r.Context(), name, core); ok {
			uc.Plugins[name] = entry
		}
	}

	return uc
}

// servedPlugins returns the one version each stored plugin serves, keyed by name.
// The metadata document and the latest download both read it, so a latest request
// gets the version that update-center.actual.json advertises. A non-empty core
// restricts the choice to versions that core runs, when any such version is stored.
func (s *Server) servedPlugins(ctx context.Context, core string) map[string]pluginLayerInfo {
	served := make(map[string]pluginLayerInfo)

	// Skips are deliberately discarded: serving metadata for the packs that ARE
	// readable is strictly better than failing the whole request because one
	// unrelated manifest is unreachable. Only /api/v1/inventory fails closed.
	packs, _, err := s.listPackInfos(ctx)
	if err != nil {
		s.logger.Warn("failed to list pack infos for metadata", "error", err)
		return served
	}

	// The declared set is advisory here: metadata serving must never fail because
	// the operator has not written the file yet, so an unreadable set simply means
	// rule 1 does not apply.
	declared, _ := ReadDeclaredPlugins(s.declaredPluginsFile)

	byName := make(map[string][]pluginLayerInfo)
	for _, pack := range packs {
		for _, p := range pack.Plugins {
			if p.Name == "" || p.Version == "" {
				continue
			}
			byName[p.Name] = append(byName[p.Name], p)
		}
	}

	for name, candidates := range byName {
		eligible := candidates
		if !pinnedInStore(declared, name, candidates) {
			eligible = preferCompatible(candidates, core)
		}
		if winner, ok := selectServedVersion(name, eligible, declared); ok {
			served[name] = winner
		}
	}
	return served
}

// selectServedVersion applies the total version-precedence rule across every
// pack holding one plugin name. It replaces the old first-wins dedupe, which
// resolved by ListManifests order — index.json insertion order for the local
// layout and tag order for a registry, i.e. a backend-dependent served version.
//
//  1. If the declared set names the plugin at any version, packs matching a
//     declared version are eligible ahead of all others and the highest such
//     version wins. This holds whether the declared set names one version or
//     several, so it is not defeated by the lack of deduplication upstream. It
//     is what stops an upload shadowing a declared version that is present in
//     the store — the case that matters, because a pin is present exactly when
//     coverage is complete.
//  2. Otherwise the highest version by pluginver wins. This is how uploading a
//     newer version of your own plugin behaves correctly, given that
//     oci.BlobStore cannot delete the older pack.
//  3. Remaining ties go to the lowest plugin-layer sha256 — a canonical
//     deterministic ordering, deliberately NOT a byte-stability guarantee.
//     PackConfig.CreatedAt is self-reported by the pack builder, not a
//     store-assigned ingest time, so it cannot order ingestion.
func selectServedVersion(name string, candidates []pluginLayerInfo, declared DeclaredSet) (pluginLayerInfo, bool) {
	if len(candidates) == 0 {
		return pluginLayerInfo{}, false
	}

	eligible := candidates
	if declared.Declared(name) {
		var matching []pluginLayerInfo
		for _, c := range candidates {
			if slices.Contains(declared[name], c.Version) {
				matching = append(matching, c)
			}
		}
		// Explicit fall-through: a plugin can be declared at a version the store
		// does not hold, in which case rule 1 selects nothing and rule 2 decides.
		if len(matching) > 0 {
			eligible = matching
		}
	}

	best := eligible[0]
	for _, c := range eligible[1:] {
		switch cmp := pluginver.Compare(c.Version, best.Version); {
		case cmp > 0:
			best = c
		case cmp == 0 && c.SHA256 < best.SHA256:
			best = c
		}
	}
	return best, true
}

// stripDigestPrefix removes the "sha256:" prefix from a digest string.
func stripDigestPrefix(digest string) string {
	const prefix = "sha256:"
	if len(digest) > len(prefix) && digest[:len(prefix)] == prefix {
		return digest[len(prefix):]
	}
	return digest
}
