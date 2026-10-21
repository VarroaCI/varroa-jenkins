package updatecenter

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
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
// per plugin, and then fills each declared exact pin that no pack stores. The
// installer reads the entry for an exact pin, so each version needs its own
// entry. A version whose dependency list is unknown is left out, and the
// installer then reads the plugin's own manifest. The first stored pack that
// holds a name and version wins, as findPluginDigest does, so the advertised
// digest matches the bytes a download serves. The upstream fill runs after the
// store entries and never replaces one.
func (s *Server) buildPluginVersionsPayload(ctx context.Context) *pluginVersionsJSON {
	doc := &pluginVersionsJSON{Plugins: make(map[string]map[string]updateCenterPlugin)}

	// Skipped packs are dropped, as buildMetadataPayload drops them. A skipped
	// pack can hold a stored version that the scan never saw, so the upstream
	// fill stays off while any pack is skipped.
	packs, skipped, err := s.listPackInfos(ctx)
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
	if len(skipped) == 0 {
		s.fillUnstoredPins(ctx, doc, claimed)
	}
	return doc
}

// fillUnstoredPins adds an upstream entry for each declared pinned version that
// no pack offers. It runs only with pull-through on. Only a metadata source can
// supply the dependency list, so a version that only the archive resolves stays out.
func (s *Server) fillUnstoredPins(ctx context.Context, doc *pluginVersionsJSON, claimed map[string]bool) {
	if !s.pullThroughEnabled || s.resolver == nil {
		return
	}
	declared, ok := ReadDeclaredPlugins(s.declaredPluginsFile)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, latestCoreResolveTimeout)
	defer cancel()

	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		for _, v := range declared[name] {
			if ucmeta.IsLatest(v) || claimed[name+"@"+v] {
				continue
			}
			res := s.resolver.ResolveExact(ctx, name, v)
			if res.Outcome != ucmeta.Resolved {
				if ctx.Err() != nil {
					s.logger.Warn("upstream fill for plugin versions stopped at its deadline", "name", name, "version", v)
					return
				}
				continue
			}
			deps := make([]updateCenterDependency, 0, len(res.Meta.Dependencies))
			for _, d := range res.Meta.Dependencies {
				deps = append(deps, updateCenterDependency{Name: d.Name, Optional: d.Optional, Version: d.Version})
			}
			if doc.Plugins[name] == nil {
				doc.Plugins[name] = make(map[string]updateCenterPlugin)
			}
			doc.Plugins[name][v] = updateCenterPlugin{
				Name:         name,
				Version:      v,
				URL:          name + "/" + v + "/" + name + ".hpi",
				SHA256:       res.Meta.SHA256,
				RequiredCore: res.Meta.RequiredCore,
				Dependencies: deps,
			}
			claimed[name+"@"+v] = true
		}
	}
}
