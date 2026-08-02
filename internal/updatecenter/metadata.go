package updatecenter

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"github.com/varroaci/varroa-jenkins/internal/pluginver"
)

// updateCenterJSON is the shape of the Jenkins update-center JSON payload.
type updateCenterJSON struct {
	UpdateCenterVersion string                        `json:"updateCenterVersion"`
	Core                string                        `json:"core"`
	Plugins             map[string]updateCenterPlugin `json:"plugins"`
}

type updateCenterPlugin struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	URL          string   `json:"url"`
	SHA256       string   `json:"sha256"`
	Dependencies []string `json:"dependencies"`
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
// from live store inventory. MVP: dependencies is always empty, core is
// always "", updateCenterVersion is always "1".
func (s *Server) buildMetadataPayload(r *http.Request) *updateCenterJSON {
	uc := &updateCenterJSON{
		UpdateCenterVersion: "1",
		Core:                "",
		Plugins:             make(map[string]updateCenterPlugin),
	}

	// Skips are deliberately discarded: serving metadata for the packs that ARE
	// readable is strictly better than failing the whole request because one
	// unrelated manifest is unreachable. Only /api/v1/inventory fails closed.
	packs, _, err := s.listPackInfos(r.Context())
	if err != nil {
		s.logger.Warn("failed to list pack infos for metadata", "error", err)
		return uc
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
		winner, ok := selectServedVersion(name, candidates, declared)
		if !ok {
			continue
		}
		// Convert the stored hex digest ("sha256:<hex>") to base64 as
		// Jenkins' update-center schema expects.
		rawBytes, err := hex.DecodeString(stripDigestPrefix(winner.SHA256))
		if err != nil {
			s.logger.Warn("invalid hex digest for plugin", "name", name, "sha256", winner.SHA256, "error", err)
			continue
		}
		uc.Plugins[name] = updateCenterPlugin{
			Name:         name,
			Version:      winner.Version,
			URL:          name + "/" + winner.Version + "/" + name + ".hpi",
			SHA256:       base64.StdEncoding.EncodeToString(rawBytes),
			Dependencies: []string{},
		}
	}

	return uc
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
