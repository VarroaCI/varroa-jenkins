package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/profileview"
)

// VersionCatalogEntry is a BFF DTO for a Jenkins version sourced from a
// JenkinsVersionProfile CRD.
type VersionCatalogEntry struct {
	Version     string `json:"version"`
	Channel     string `json:"channel"`
	Recommended bool   `json:"recommended,omitempty"`
	EOL         string `json:"eol,omitempty"`
	// NEW (owned by change D; change C consumes read-only):
	Name           string `json:"name"`                     // profile object name (stable handle)
	PluginSetReady *bool  `json:"pluginSetReady,omitempty"` // nil = metadata-only profile (no pluginSetRef)
	PluginCount    int    `json:"pluginCount,omitempty"`    // status.pluginCount
}

// VersionProfileCondition is a BFF DTO for a JenkinsVersionProfile status condition.
type VersionProfileCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"` // RFC3339
}

// VersionProfileDetail is the full per-profile detail served by GET /version-profiles.
type VersionProfileDetail struct {
	Name            string                    `json:"name"`
	Version         string                    `json:"version"`
	Channel         string                    `json:"channel"`
	Recommended     bool                      `json:"recommended,omitempty"`
	EOL             string                    `json:"eol,omitempty"`
	PluginSetRef    string                    `json:"pluginSetRef,omitempty"` // spec source CM name
	ContentRef      string                    `json:"contentRef,omitempty"`   // materialized CM name
	PluginCount     int                       `json:"pluginCount,omitempty"`
	ResolveVersion  string                    `json:"resolveVersion,omitempty"` // LTS-line exact patch (dynamic-stable metadata endpoint)
	Plugins         []string                  `json:"plugins,omitempty"`        // full pinned lines "name@version" (bare name if unversioned)
	HasJcasc        bool                      `json:"hasJcasc"`
	RequiredPlugins []string                  `json:"requiredPlugins,omitempty"` // spec.jcasc.requiredPlugins
	Conditions      []VersionProfileCondition `json:"conditions"`                // [] never null
}

type provisioningConfigResponse struct {
	RootDomain        string                `json:"rootDomain"`
	DashboardHost     string                `json:"dashboardHost"`
	DefaultNamespace  string                `json:"defaultNamespace"`
	Namespaces        []string              `json:"namespaces"`
	DefaultVersion    string                `json:"defaultVersion"`
	Versions          []VersionCatalogEntry `json:"versions"`
	SizePresets       []v1alpha1.SizePreset `json:"sizePresets"`
	InjectedVariables []string              `json:"injectedVariables"`
}

// HandleProvisioningConfig serves GET /provisioning/config with the aggregate
// provisioning read surface (root domain, namespaces, versions, size presets,
// and injected variable names). Any authenticated caller may read this.
func (s *Server) HandleProvisioningConfig(w http.ResponseWriter, r *http.Request) {
	s.handleProvisioningConfigForCluster(w, r, s.localCluster())
}

func (s *Server) handleProvisioningConfigForCluster(w http.ResponseWriter, r *http.Request, cluster string) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx := r.Context()
	var defaults v1alpha1.ProvisioningDefaults
	var profiles []*v1alpha1.JenkinsVersionProfile
	if s.deps.ConfigBrood == nil {
		pd, err := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, s.deps.Store, "varroa-defaults", "")
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				s.deps.Logger.Error("failed to fetch provisioning defaults", "error", err)
				s.writeJSONError(w, http.StatusInternalServerError, "failed to fetch provisioning configuration")
				return
			}
		} else if pd != nil {
			defaults = *pd
		}
		listed, err := crdstore.List[v1alpha1.JenkinsVersionProfile](ctx, s.deps.Store, "", "")
		if err != nil {
			s.deps.Logger.Error("failed to list JenkinsVersionProfile CRDs", "error", err)
			profiles = []*v1alpha1.JenkinsVersionProfile{}
		} else {
			profiles = listed
		}
	} else {
		rawDefaults, err := s.deps.ConfigBrood.GetProvisioningDefaults(ctx, cluster, "varroa-defaults")
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				s.deps.Logger.Error("failed to fetch provisioning defaults", "error", err)
				writeConfigBroodError(w, err, cluster)
				return
			}
		} else if err := json.Unmarshal(rawDefaults, &defaults); err != nil {
			s.writeJSONError(w, http.StatusInternalServerError, "failed to decode provisioning defaults")
			return
		}
		rawProfiles, err := s.deps.ConfigBrood.ListVersionProfiles(ctx, cluster)
		if err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		profiles = make([]*v1alpha1.JenkinsVersionProfile, 0, len(rawProfiles))
		for _, raw := range rawProfiles {
			var p v1alpha1.JenkinsVersionProfile
			if err := json.Unmarshal(raw, &p); err != nil {
				s.writeJSONError(w, http.StatusInternalServerError, "failed to decode version profile")
				return
			}
			profiles = append(profiles, &p)
		}
	}

	defaultNS := defaults.Spec.DefaultNamespace
	if defaultNS == "" {
		defaultNS = "varroa"
	}

	namespaces := []string{defaultNS}
	for _, ns := range defaults.Spec.Namespaces {
		if ns != defaultNS {
			namespaces = append(namespaces, ns)
		}
	}

	versions := buildVersionCatalogFromProfiles(profiles)
	presets := defaults.Spec.SizePresets
	if presets == nil {
		presets = []v1alpha1.SizePreset{}
	}

	resp := provisioningConfigResponse{
		RootDomain:        defaults.Spec.RootDomain,
		DashboardHost:     s.deps.DashboardHost,
		DefaultNamespace:  defaultNS,
		Namespaces:        namespaces,
		DefaultVersion:    defaults.Spec.DefaultVersion,
		Versions:          versions,
		SizePresets:       presets,
		InjectedVariables: bundle.InjectedVariableNames,
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// buildVersionCatalog builds the version catalog from JenkinsVersionProfile CRDs.
// On error it logs and returns an empty slice (never nil).
func (s *Server) buildVersionCatalog(ctx context.Context) []VersionCatalogEntry {
	profiles, err := crdstore.List[v1alpha1.JenkinsVersionProfile](ctx, s.deps.Store, "", "")
	if err != nil {
		s.deps.Logger.Error("failed to list JenkinsVersionProfile CRDs", "error", err)
		return []VersionCatalogEntry{}
	}
	return buildVersionCatalogFromProfiles(profiles)
}

func buildVersionCatalogFromProfiles(profiles []*v1alpha1.JenkinsVersionProfile) []VersionCatalogEntry {
	if len(profiles) == 0 {
		return []VersionCatalogEntry{}
	}
	entries := make([]VersionCatalogEntry, 0, len(profiles))
	for _, p := range profiles {
		e := VersionCatalogEntry{
			Version:     p.Spec.Version,
			Channel:     p.Spec.Channel,
			Recommended: p.Spec.Recommended,
			EOL:         p.Spec.EOL,
			Name:        p.Name,
			PluginCount: p.Status.PluginCount,
		}
		e.PluginSetReady = profilePluginSetReady(p) // nil for metadata-only profiles
		entries = append(entries, e)
	}
	sortVersionsDesc(entries)
	return entries
}

// sortVersionsDesc sorts catalog entries version-descending. Versions are
// compared segment-wise numerically (a missing segment pads to 0, so "2.552"
// compares as "2.552.0"); a non-numeric segment falls back to string compare
// for that position.
func sortVersionsDesc(entries []VersionCatalogEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		return compareVersionsDesc(entries[i].Version, entries[j].Version)
	})
}

// compareVersionsDesc reports whether version a should sort before version b in
// descending order (i.e. a > b).
func compareVersionsDesc(a, b string) bool {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for k := 0; k < n; k++ {
		var av, bv string
		if k < len(as) {
			av = as[k]
		}
		if k < len(bs) {
			bv = bs[k]
		}
		ai, aErr := strconv.Atoi(av)
		bi, bErr := strconv.Atoi(bv)
		if aErr == nil && bErr == nil {
			if ai != bi {
				return ai > bi
			}
			continue
		}
		if cmp := strings.Compare(av, bv); cmp != 0 {
			return cmp > 0
		}
	}
	return false
}

// HandleVersionProfiles serves GET /version-profiles with the full
// JenkinsVersionProfile catalog and status. Any authenticated caller may read
// this (same posture as /provisioning/config; catalog state is non-sensitive).
func (s *Server) HandleVersionProfiles(w http.ResponseWriter, r *http.Request) {
	s.handleVersionProfilesForCluster(w, r, s.localCluster())
}

func (s *Server) handleVersionProfilesForCluster(w http.ResponseWriter, r *http.Request, cluster string) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	ctx := r.Context()
	if cluster != s.localCluster() {
		s.deps.Logger.Debug("version-profiles request for non-local cluster", "cluster", cluster, "localCluster", s.localCluster())
		views, err := s.deps.ConfigBrood.ViewVersionProfiles(ctx, cluster)
		if err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		out := make([]VersionProfileDetail, 0, len(views))
		for _, view := range views {
			var p v1alpha1.JenkinsVersionProfile
			if err := json.Unmarshal(view.Item, &p); err != nil {
				s.writeJSONError(w, http.StatusInternalServerError, "failed to decode version profile")
				return
			}
			out = append(out, versionProfileDetailFromCR(&p, view.ResolvedPlugins))
		}
		sort.SliceStable(out, func(i, j int) bool { return compareVersionsDesc(out[i].Version, out[j].Version) })
		s.writeJSON(w, http.StatusOK, itemsEnvelope(out))
		return
	}
	profiles, err := crdstore.List[v1alpha1.JenkinsVersionProfile](ctx, s.deps.Store, "", "")
	if err != nil {
		s.deps.Logger.Error("failed to list version profiles", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "failed to list version profiles")
		return
	}

	out := make([]VersionProfileDetail, 0, len(profiles))
	for _, p := range profiles {
		// An empty ContentRef is a legitimate not-yet-materialized state and
		// returns 200 with no plugins. A NON-empty ContentRef asserts the
		// plugin set exists, so failing to read it — or reading it and finding
		// no usable plugin list — is a broken cluster state, not an empty
		// profile. Reporting either as 200-with-no-plugins is what published
		// an empty plugin pack and exited 0 (issue #416). The diagnostic is the
		// status code: VersionProfileDetail is a generated schema and must not
		// grow a field here.
		var plugins []string
		if p.Status.ContentRef != "" {
			cm, cmErr := s.deps.Client.GetConfigMap(ctx, p.Status.ContentRef, s.deps.OperatorNamespace)
			if cmErr != nil {
				s.deps.Logger.Error("failed to read version profile plugin set",
					"profile", p.Name, "configMap", p.Status.ContentRef,
					"namespace", s.deps.OperatorNamespace, "error", cmErr)
				s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf(
					"version profile %q: cannot read plugin set ConfigMap %q in namespace %q: %v",
					p.Name, p.Status.ContentRef, s.deps.OperatorNamespace, cmErr))
				return
			}
			y := cm["plugins.yaml"]
			if y != "" {
				var parseErr error
				plugins, parseErr = profileview.PluginLinesFromYAML(y)
				if parseErr != nil {
					s.deps.Logger.Error("failed to parse version profile plugin set",
						"profile", p.Name, "configMap", p.Status.ContentRef, "error", parseErr)
					s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf(
						"version profile %q: plugin set ConfigMap %q has unparseable plugins.yaml: %v",
						p.Name, p.Status.ContentRef, parseErr))
					return
				}
			}
			if len(plugins) == 0 {
				s.deps.Logger.Error("version profile plugin set is materialized but empty",
					"profile", p.Name, "configMap", p.Status.ContentRef)
				s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf(
					"version profile %q: plugin set ConfigMap %q has no usable plugins.yaml",
					p.Name, p.Status.ContentRef))
				return
			}
		}
		out = append(out, versionProfileDetailFromCR(p, plugins))
	}

	sort.SliceStable(out, func(i, j int) bool {
		return compareVersionsDesc(out[i].Version, out[j].Version)
	})

	s.writeJSON(w, http.StatusOK, itemsEnvelope(out))
}

func versionProfileDetailFromCR(p *v1alpha1.JenkinsVersionProfile, plugins []string) VersionProfileDetail {
	d := VersionProfileDetail{
		Name:           p.Name,
		Version:        p.Spec.Version,
		Channel:        p.Spec.Channel,
		Recommended:    p.Spec.Recommended,
		EOL:            p.Spec.EOL,
		ContentRef:     p.Status.ContentRef,
		PluginCount:    p.Status.PluginCount,
		ResolveVersion: p.Spec.ResolveVersion,
		Plugins:        plugins,
		HasJcasc:       p.Spec.JCasC != nil,
		Conditions:     []VersionProfileCondition{},
	}
	if p.Spec.PluginSetRef != nil {
		d.PluginSetRef = p.Spec.PluginSetRef.Name
	}
	if p.Spec.JCasC != nil {
		d.RequiredPlugins = p.Spec.JCasC.RequiredPlugins
	}
	for _, c := range p.Status.Conditions {
		d.Conditions = append(d.Conditions, VersionProfileCondition{
			Type:               c.Type,
			Status:             string(c.Status),
			Reason:             c.Reason,
			Message:            c.Message,
			LastTransitionTime: c.LastTransitionTime.Format(time.RFC3339),
		})
	}
	return d
}

// profilePluginSetReady derives the wizard-facing plugin-set readiness for a
// profile: nil for metadata-only profiles (no pluginSetRef), otherwise the
// PluginSetReady condition truth. The single derivation shared by the
// catalog and detail projections (the bug-#416 drift class lived in having
// two of these).
func profilePluginSetReady(p *v1alpha1.JenkinsVersionProfile) *bool {
	if p.Spec.PluginSetRef == nil {
		return nil
	}
	ready := false
	for _, c := range p.Status.Conditions {
		if c.Type == "PluginSetReady" {
			ready = c.Status == metav1.ConditionTrue
			break
		}
	}
	return &ready
}

func (s *Server) localCluster() string {
	if s.deps.Brood != nil && s.deps.Brood.LocalCluster() != "" {
		return s.deps.Brood.LocalCluster()
	}
	return "core"
}

func (s *Server) dispatchProvisioning(w http.ResponseWriter, r *http.Request, cluster string, segments []string) {
	if len(segments) == 1 && segments[0] == "config" {
		s.handleProvisioningConfigForCluster(w, r, cluster)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) dispatchVersionProfiles(w http.ResponseWriter, r *http.Request, cluster string, segments []string) {
	if s.deps.ConfigBrood == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "config brood not available")
		return
	}
	if len(segments) == 0 || (len(segments) == 1 && segments[0] == "") {
		s.handleVersionProfilesCollection(w, r, cluster)
		return
	}
	if len(segments) == 1 && segments[0] != "" {
		s.handleVersionProfileResource(w, r, cluster, segments[0])
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleVersionProfilesCollection(w http.ResponseWriter, r *http.Request, cluster string) {
	switch r.Method {
	case http.MethodGet:
		s.handleVersionProfilesForCluster(w, r, cluster)
	case http.MethodPost:
		claims := auth.ClaimsFromContext(r.Context())
		if s.deps.Authorizer == nil || !s.deps.Authorizer.CanCreateVersionProfile(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		var cr v1alpha1.JenkinsVersionProfile
		if err := json.NewDecoder(r.Body).Decode(&cr); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		cr.APIVersion = "varroa.dev/v1alpha1"
		cr.Kind = "JenkinsVersionProfile"
		obj, _ := json.Marshal(cr)
		item, err := s.deps.ConfigBrood.CreateVersionProfile(r.Context(), cluster, cr.Name, obj)
		if err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		var created v1alpha1.JenkinsVersionProfile
		_ = json.Unmarshal(item, &created)
		s.writeJSON(w, http.StatusCreated, created)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleVersionProfileResource(w http.ResponseWriter, r *http.Request, cluster, name string) {
	switch r.Method {
	case http.MethodGet:
		item, err := s.deps.ConfigBrood.GetVersionProfile(r.Context(), cluster, name)
		if err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		var cr v1alpha1.JenkinsVersionProfile
		_ = json.Unmarshal(item, &cr)
		s.writeJSON(w, http.StatusOK, cr)
	case http.MethodPut:
		claims := auth.ClaimsFromContext(r.Context())
		if s.deps.Authorizer == nil || !s.deps.Authorizer.CanUpdateVersionProfile(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		var cr v1alpha1.JenkinsVersionProfile
		if err := json.NewDecoder(r.Body).Decode(&cr); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		cr.Name = name
		cr.APIVersion = "varroa.dev/v1alpha1"
		cr.Kind = "JenkinsVersionProfile"
		obj, _ := json.Marshal(cr)
		item, err := s.deps.ConfigBrood.UpdateVersionProfile(r.Context(), cluster, name, obj)
		if err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		var updated v1alpha1.JenkinsVersionProfile
		_ = json.Unmarshal(item, &updated)
		s.writeJSON(w, http.StatusOK, updated)
	case http.MethodDelete:
		claims := auth.ClaimsFromContext(r.Context())
		if s.deps.Authorizer == nil || !s.deps.Authorizer.CanDeleteVersionProfile(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		if err := s.deps.ConfigBrood.DeleteVersionProfile(r.Context(), cluster, name); err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) dispatchProvisioningDefaults(w http.ResponseWriter, r *http.Request, cluster string, segments []string) {
	if s.deps.ConfigBrood == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "config brood not available")
		return
	}
	if len(segments) != 1 || segments[0] == "" {
		http.NotFound(w, r)
		return
	}
	name := segments[0]
	switch r.Method {
	case http.MethodGet:
		item, err := s.deps.ConfigBrood.GetProvisioningDefaults(r.Context(), cluster, name)
		if err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		var cr v1alpha1.ProvisioningDefaults
		_ = json.Unmarshal(item, &cr)
		s.writeJSON(w, http.StatusOK, cr)
	case http.MethodPut:
		claims := auth.ClaimsFromContext(r.Context())
		if s.deps.Authorizer == nil || !s.deps.Authorizer.CanUpdateProvisioningDefaults(claims) {
			s.writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		var cr v1alpha1.ProvisioningDefaults
		if err := json.NewDecoder(r.Body).Decode(&cr); err != nil {
			s.writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		cr.Name = name
		cr.APIVersion = "varroa.dev/v1alpha1"
		cr.Kind = "ProvisioningDefaults"
		obj, _ := json.Marshal(cr)
		item, err := s.deps.ConfigBrood.UpdateProvisioningDefaults(r.Context(), cluster, name, obj)
		if err != nil {
			writeConfigBroodError(w, err, cluster)
			return
		}
		var updated v1alpha1.ProvisioningDefaults
		_ = json.Unmarshal(item, &updated)
		s.writeJSON(w, http.StatusOK, updated)
	default:
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
