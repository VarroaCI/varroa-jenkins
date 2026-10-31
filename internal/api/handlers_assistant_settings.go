package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/datashare"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// assistantLimits is the admin view of the configured limits.
type assistantLimits struct {
	Budgets             provider.Budgets `json:"budgets"`
	ExternalDataSharing string           `json:"externalDataSharing"`
	ApprovalExpiry      string           `json:"approvalExpiry"`
	Egress              map[string]any   `json:"egress"`
}

func (rt *AssistantRuntime) limits() assistantLimits {
	return assistantLimits{
		Budgets:             rt.Config.Budgets,
		ExternalDataSharing: string(rt.Config.ExternalDataSharing),
		ApprovalExpiry:      rt.Config.ApprovalExpiryText,
		Egress: map[string]any{
			"networkPolicy": rt.Config.Egress.NetworkPolicy,
			"cidrs":         rt.Config.Egress.CIDRs,
			"ports":         rt.Config.Egress.Ports,
		},
	}
}

// handleGetAssistantSettings serves GET /assistant/settings. Every user sees
// the kill switch. An admin also sees the defaults, the tenant levels, and the
// configured limits.
func (s *Server) handleGetAssistantSettings(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if !s.assistantCanUse(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	g, err := rt.Store.GetSettings(r.Context(), agent.ScopeGlobal)
	if err != nil {
		s.assistantStoreError(w, err)
		return
	}
	out := map[string]any{"paused": g.Paused, "pausedBy": g.PausedBy, "pausedAt": g.PausedAt}
	if s.assistantIsAdmin(claims) {
		out["defaultProvider"] = g.DefaultProvider
		out["defaultModel"] = g.DefaultModel
		out["tenantDataSharing"] = g.TenantDataSharing
		out["updatedAt"] = g.UpdatedAt
		out["updatedBy"] = g.UpdatedBy
		out["limits"] = rt.limits()
	}
	s.writeJSON(w, http.StatusOK, out)
}

// assistantSettingsUpdate is the body of PUT /assistant/settings. A nil field
// is unchanged.
type assistantSettingsUpdate struct {
	Paused            *bool   `json:"paused"`
	DefaultProvider   *string `json:"defaultProvider"`
	DefaultModel      *string `json:"defaultModel"`
	TenantDataSharing *struct {
		Set *struct {
			Tenant string `json:"tenant"`
			Level  string `json:"level"`
		} `json:"set"`
		Remove *string `json:"remove"`
	} `json:"tenantDataSharing"`
}

// handlePutAssistantSettings serves PUT /assistant/settings. Only an admin may
// change the kill switch, the defaults, or a tenant level.
func (s *Server) handlePutAssistantSettings(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if !s.assistantIsAdmin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	var in assistantSettingsUpdate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	patch := store.SettingsPatch{Paused: in.Paused, DefaultProvider: in.DefaultProvider, DefaultModel: in.DefaultModel}
	if in.DefaultProvider != nil && *in.DefaultProvider != "" {
		if _, ok := rt.Registry.Get(*in.DefaultProvider); !ok {
			s.writeJSONError(w, http.StatusBadRequest, "unknown default provider")
			return
		}
	}
	if in.DefaultProvider != nil || in.DefaultModel != nil {
		patch.Check = func(after agent.Settings) error {
			return rt.pairRefusal(after.DefaultProvider, after.DefaultModel, "a default model needs a default provider")
		}
	}
	if in.TenantDataSharing != nil {
		if set := in.TenantDataSharing.Set; set != nil {
			level := agent.DataSharing(set.Level)
			if set.Tenant == "" || !level.Valid() {
				s.writeJSONError(w, http.StatusBadRequest, "tenant and a valid level are required")
				return
			}
			patch.TenantSet = &agent.TenantLevel{Tenant: set.Tenant, Level: level}
		}
		patch.TenantRemove = in.TenantDataSharing.Remove
	}
	g, err := rt.Store.PutSettings(r.Context(), agent.ScopeGlobal, patch, ActorFrom(claims))
	if err != nil {
		s.writeSettingsError(w, err)
		return
	}
	if in.Paused != nil {
		_ = publishControl(rt.Publish, g.Paused, g.PausedBy, g.UpdatedAt)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"paused": g.Paused, "pausedBy": g.PausedBy, "pausedAt": g.PausedAt,
		"defaultProvider": g.DefaultProvider, "defaultModel": g.DefaultModel,
		"tenantDataSharing": g.TenantDataSharing, "updatedAt": g.UpdatedAt,
		"updatedBy": g.UpdatedBy, "limits": rt.limits(),
	})
}

// assistantRefusal is a refusal from a settings check. The handler answers 400 with its text.
type assistantRefusal string

func (e assistantRefusal) Error() string { return string(e) }

// writeSettingsError answers a refusal from a settings check with 400. Any
// other error goes to the store error mapping.
func (s *Server) writeSettingsError(w http.ResponseWriter, err error) {
	var ref assistantRefusal
	if errors.As(err, &ref) {
		s.writeJSONError(w, http.StatusBadRequest, string(ref))
		return
	}
	s.assistantStoreError(w, err)
}

// pairRefusal checks a provider and model pair. An empty model passes.
// noProvider is the text for a model with no provider.
func (rt *AssistantRuntime) pairRefusal(p, m, noProvider string) error {
	switch {
	case m == "":
		return nil
	case p == "":
		return assistantRefusal(noProvider)
	case !rt.modelKnown(p, m):
		return assistantRefusal(fmt.Sprintf("the provider %s does not list the model %s", p, m))
	}
	return nil
}

// modelKnown reports whether the model is listed for the provider.
func (rt *AssistantRuntime) modelKnown(name, model string) bool {
	for _, e := range rt.Config.Providers {
		if e.Name != name {
			continue
		}
		if assistantContains(e.Models, model) {
			return true
		}
	}
	return false
}

// assistantProviderJSON is one provider in the list.
type assistantProviderJSON struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Models    []string `json:"models"`
	Trust     string   `json:"trust"`
	Default   bool     `json:"default"`
	Available bool     `json:"available"`
	BaseURL   string   `json:"baseURL,omitempty"`
}

// handleListAssistantProviders serves GET /assistant/providers. The base URL
// of a provider is for admins only.
func (s *Server) handleListAssistantProviders(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if !s.assistantCanUse(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	admin := s.assistantIsAdmin(claims)
	items := make([]assistantProviderJSON, 0, len(rt.Config.Providers))
	for _, e := range rt.Config.Providers {
		item := assistantProviderJSON{
			Name: e.Name, Kind: string(e.Kind), Models: e.Models, Trust: string(e.Trust),
			Default: e.Default, Available: rt.Registry.Available(e.Name),
		}
		if admin {
			item.BaseURL = e.BaseURL
		}
		items = append(items, item)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleGetMyAssistant serves GET /me/assistant. With the assistant off the
// answer is available false, and no gate runs.
func (s *Server) handleGetMyAssistant(w http.ResponseWriter, r *http.Request) {
	if s.deps.Assistant == nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	rt := s.deps.Assistant
	claims := auth.ClaimsFromContext(r.Context())
	if !s.assistantCanUse(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	body, err := s.myAssistantJSON(r, rt, claims)
	if err != nil {
		s.assistantStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, body)
}

// myAssistantJSON builds the caller's assistant view: the settings, the key
// state, the level per provider, and the day spend. A failed store read
// returns its error. The view never holds the defaults of a read that failed.
func (s *Server) myAssistantJSON(r *http.Request, rt *AssistantRuntime, claims *auth.Claims) (map[string]any, error) {
	ctx := r.Context()
	user, err := rt.Store.GetSettings(ctx, agent.UserScope(claims.Subject))
	if err != nil {
		return nil, err
	}
	global, err := rt.Store.GetSettings(ctx, agent.ScopeGlobal)
	if err != nil {
		return nil, err
	}
	day, err := rt.Store.DaySpend(ctx, claims.Subject)
	if err != nil {
		return nil, err
	}

	levels := map[string]string{}
	for _, e := range rt.Config.Providers {
		level := datashare.Effective(datashare.Inputs{
			User: effectiveUserLevel(user), TenantLevels: global.TenantDataSharing,
			Trust: e.Trust, ExternalCeiling: rt.Config.ExternalDataSharing,
		})
		levels[e.Name] = string(level)
	}

	var keyState any
	if c, err := r.Cookie(AssistantKeyCookie); err == nil && rt.Sealer != nil {
		if name, _, ok := rt.Sealer.Open(c.Value, claims.Subject, assistantIAT(claims)); ok {
			keyState = map[string]string{"provider": name}
		}
	}

	return map[string]any{
		"available":       len(rt.Config.Providers) > 0,
		"enabled":         user.Enabled,
		"provider":        user.Provider,
		"model":           user.Model,
		"dataSharing":     effectiveUserLevel(user),
		"sessionKey":      keyState,
		"effectiveLevels": levels,
		"dayTokens":       day,
		"dayBudget":       rt.Config.Budgets.UserDay,
	}, nil
}

// effectiveUserLevel returns the user's data level, or logs when none is set.
func effectiveUserLevel(user agent.Settings) agent.DataSharing {
	if user.DataSharing.Valid() {
		return user.DataSharing
	}
	return agent.DataLogs
}

// assistantMyUpdate is the body of PUT /me/assistant.
type assistantMyUpdate struct {
	Enabled     *bool   `json:"enabled"`
	Provider    *string `json:"provider"`
	Model       *string `json:"model"`
	DataSharing *string `json:"dataSharing"`
}

// handlePutMyAssistant serves PUT /me/assistant. A bad value writes nothing.
func (s *Server) handlePutMyAssistant(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if !s.assistantCanUse(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	var in assistantMyUpdate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	patch := store.SettingsPatch{Enabled: in.Enabled, Provider: in.Provider, Model: in.Model}
	if in.Provider != nil && *in.Provider != "" {
		if _, ok := rt.Registry.Get(*in.Provider); !ok {
			s.writeJSONError(w, http.StatusBadRequest, "unknown provider")
			return
		}
	}
	if in.Provider != nil || in.Model != nil {
		patch.Check = func(after agent.Settings) error {
			return rt.pairRefusal(after.Provider, after.Model, "a model needs a provider")
		}
	}
	if in.DataSharing != nil {
		level := agent.DataSharing(*in.DataSharing)
		if !level.Valid() {
			s.writeJSONError(w, http.StatusBadRequest, "data sharing must be none, metadata, or logs")
			return
		}
		patch.DataSharing = &level
	}
	user, err := rt.Store.PutSettings(r.Context(), agent.UserScope(claims.Subject), patch, claims.Subject)
	if err != nil {
		s.writeSettingsError(w, err)
		return
	}
	_ = user
	body, err := s.myAssistantJSON(r, rt, claims)
	if err != nil {
		s.assistantStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, body)
}

// assistantKeyRequest is the body of PUT /me/assistant/key.
type assistantKeyRequest struct {
	Provider string `json:"provider"`
	APIKey   string `json:"apiKey"`
}

// handlePutAssistantKey serves PUT /me/assistant/key. It seals the key into a
// session cookie. An API key caller has no login session to bind it to, so the
// call answers 409.
func (s *Server) handlePutAssistantKey(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.assistantOrDisabled(w)
	if !ok {
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if !s.assistantCanUse(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	if assistantIAT(claims) == 0 {
		s.writeJSONError(w, http.StatusConflict, "a provider key needs a login session")
		return
	}
	if rt.Sealer == nil {
		writeAssistantError(s, w, http.StatusServiceUnavailable, assistantErrKeyUnavail, "provider keys are not available")
		return
	}
	var in assistantKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.APIKey == "" {
		s.writeJSONError(w, http.StatusBadRequest, "provider and apiKey are required")
		return
	}
	if _, ok := rt.Registry.Get(in.Provider); !ok {
		s.writeJSONError(w, http.StatusBadRequest, "unknown provider")
		return
	}
	sealed, err := rt.Sealer.Seal(claims.Subject, assistantIAT(claims), in.Provider, []byte(in.APIKey))
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "seal key failed")
		return
	}
	http.SetCookie(w, s.assistantKeyCookie(sealed, false))
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteAssistantKey serves DELETE /me/assistant/key.
func (s *Server) handleDeleteAssistantKey(w http.ResponseWriter, r *http.Request) {
	if s.deps.Assistant == nil {
		writeAssistantError(s, w, http.StatusServiceUnavailable, assistantErrDisabled, "the assistant is not enabled")
		return
	}
	http.SetCookie(w, s.assistantKeyCookie("", true))
	w.WriteHeader(http.StatusNoContent)
}

// assistantKeyCookie builds the sealed key cookie. It is HttpOnly, scoped to
// the API path, and SameSite Strict. It has no lifetime, so it ends with the
// browser session.
func (s *Server) assistantKeyCookie(value string, remove bool) *http.Cookie {
	domain := ""
	if s.deps.Auth != nil {
		domain = s.deps.Auth.CookieDomain()
	}
	c := &http.Cookie{
		Name:     AssistantKeyCookie,
		Value:    value,
		Domain:   domain,
		Path:     "/api/v1",
		HttpOnly: true,
		Secure:   s.deps.SecureCookies,
		SameSite: http.SameSiteStrictMode,
	}
	if remove {
		c.MaxAge = -1
		c.Expires = time.Unix(0, 0)
	}
	return c
}
