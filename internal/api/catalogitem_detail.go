package api

import (
	"context"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// updateCenterSourceRef is the reserved CatalogSource name. Only items derived
// from it carry a closure worth joining lock pins against.
const updateCenterSourceRef = v1alpha1.UpdateCenterCatalogSourceName

// CatalogItemDetailResponse is what the catalog-item get route returns.
//
// The lock pins are cluster state. Storing them on the item would duplicate
// every profile's lock into every derived item, where they would go stale the
// moment a profile is regenerated — so the join happens at read time.
type CatalogItemDetailResponse struct {
	Item     v1alpha1.CatalogItem  `json:"item"`
	LockPins []CatalogItemLockPins `json:"lockPins,omitempty"`
}

// CatalogItemLockPins is one profile's lock pins for this item's closure.
type CatalogItemLockPins struct {
	Profile string `json:"profile"`
	// Pins maps artifactId to the version this profile's lock pins. A closure
	// entry the lock does not mention has NO key here — distinct from the lock
	// pinning it at the same version.
	Pins map[string]string `json:"pins"`
}

// buildCatalogItemLockPins joins the item's closure against every eligible
// profile's materialized lock.
//
// It is populated only for update-center-derived items with a non-empty
// closure, so the join costs nothing for the common case. Profiles are
// enumerated with the same eligibility rule the resolver uses — ready plus a
// materialized contentRef — because showing pins from a stale lock is exactly
// the misinformation the verdict evaluator refuses to turn into a judgement.
//
// A profile whose lock cannot be read or parsed is omitted and the rest of the
// response is still returned. This is deliberately NOT the fail-before-write
// rule the sync loop applies: a detail view is a read-only convenience and must
// not fail because one profile is unhealthy. Nothing is written here.
func (s *Server) buildCatalogItemLockPins(ctx context.Context, item *v1alpha1.CatalogItem) []CatalogItemLockPins {
	if item.Spec.SourceRef != updateCenterSourceRef || len(item.Status.Closure) == 0 {
		return nil
	}
	profiles, err := crdstore.List[v1alpha1.JenkinsVersionProfile](ctx, s.deps.Store, "", "")
	if err != nil {
		s.deps.Logger.Warn("failed to list version profiles for catalog item lock pins", "error", err)
		return nil
	}

	wanted := make(map[string]struct{}, len(item.Status.Closure))
	for _, e := range item.Status.Closure {
		wanted[e.ArtifactID] = struct{}{}
	}

	out := make([]CatalogItemLockPins, 0, len(profiles))
	for _, p := range profiles {
		if !versionProfilePluginSetReady(p) || p.Status.ContentRef == "" {
			continue
		}
		cm, err := s.deps.Client.GetConfigMap(ctx, p.Status.ContentRef, s.deps.OperatorNamespace)
		if err != nil {
			s.deps.Logger.Warn("omitting a profile from catalog item lock pins: cannot read its lock",
				"profile", p.Name, "configMap", p.Status.ContentRef, "error", err)
			continue
		}
		var lockSet struct {
			Plugins []pluginlock.PluginEntry `yaml:"plugins"`
		}
		if err := yaml.Unmarshal([]byte(cm["plugins.yaml"]), &lockSet); err != nil {
			s.deps.Logger.Warn("omitting a profile from catalog item lock pins: unparseable lock",
				"profile", p.Name, "configMap", p.Status.ContentRef, "error", err)
			continue
		}
		pins := map[string]string{}
		for _, pe := range lockSet.Plugins {
			if _, ok := wanted[pe.ArtifactID]; ok {
				pins[pe.ArtifactID] = pe.Version
			}
		}
		out = append(out, CatalogItemLockPins{Profile: p.Name, Pins: pins})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// versionProfilePluginSetReady mirrors the operator's eligibility predicate.
func versionProfilePluginSetReady(p *v1alpha1.JenkinsVersionProfile) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == "PluginSetReady" && string(c.Status) == "True" {
			return true
		}
	}
	return false
}
