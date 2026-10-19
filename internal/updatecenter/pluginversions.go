package updatecenter

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

// pluginVersionsJSON is the shape of the plugin-versions document that
// jenkins-plugin-cli reads for the dependencies and checksums of exact pins.
// It maps plugin name, then version, to one entry.
type pluginVersionsJSON struct {
	Plugins map[string]map[string]updateCenterPlugin `json:"plugins"`
}

// handlePluginVersions serves GET /plugin-versions.json.
func (s *Server) handlePluginVersions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	payload := s.buildPluginVersionsPayload(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(payload)
}

// buildPluginVersionsPayload lists every stored plugin version, not one winner
// per plugin. The installer reads the entry for an exact pin, so each stored
// version needs its own entry. A version whose dependency list is unknown is
// left out, and the installer then reads the plugin's own manifest. The first
// stored pack that holds a name and version wins, as findPluginDigest does, so
// the advertised digest matches the bytes a download serves.
func (s *Server) buildPluginVersionsPayload(ctx context.Context) *pluginVersionsJSON {
	doc := &pluginVersionsJSON{Plugins: make(map[string]map[string]updateCenterPlugin)}

	// Skipped packs are dropped, as buildMetadataPayload drops them.
	packs, _, err := s.listPackInfos(ctx)
	if err != nil {
		s.logger.Warn("failed to list pack infos for plugin versions", "error", err)
		return doc
	}

	// claimed holds every name@version that a pack has offered, emitted or not.
	// findPluginDigest serves the first pack that holds a version, so a skipped
	// first entry must still block a later pack's digest for the same version.
	claimed := make(map[string]bool)
	for _, pack := range packs {
		for _, p := range pack.Plugins {
			if p.Name == "" || p.Version == "" {
				continue
			}
			key := p.Name + "@" + p.Version
			if claimed[key] {
				continue
			}
			claimed[key] = true
			rawBytes, err := hex.DecodeString(stripDigestPrefix(p.SHA256))
			if err != nil {
				s.logger.Warn("invalid hex digest for plugin version", "name", p.Name, "version", p.Version, "sha256", p.SHA256, "error", err)
				continue
			}
			deps, known := s.pluginDependencies(ctx, p)
			if !known {
				continue
			}
			served := make([]updateCenterDependency, 0, len(deps))
			for _, d := range deps {
				served = append(served, updateCenterDependency{Name: d.Name, Optional: d.Optional, Version: d.Min})
			}
			if doc.Plugins[p.Name] == nil {
				doc.Plugins[p.Name] = make(map[string]updateCenterPlugin)
			}
			doc.Plugins[p.Name][p.Version] = updateCenterPlugin{
				Name:         p.Name,
				Version:      p.Version,
				URL:          p.Name + "/" + p.Version + "/" + p.Name + ".hpi",
				SHA256:       base64.StdEncoding.EncodeToString(rawBytes),
				RequiredCore: p.RequiredCore,
				Dependencies: served,
			}
		}
	}
	return doc
}
