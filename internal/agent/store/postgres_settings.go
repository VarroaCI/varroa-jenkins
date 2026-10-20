package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// settingsPatchSQL writes one settings patch in one statement. Each field is
// written on its own, so two admins who change two tenants do not overwrite
// each other. $3 holds the scalar keys, $4 and $5 set one tenant level, and $6
// removes one tenant level.
const settingsPatchSQL = `
UPDATE varroa.agent_settings SET
  paused    = CASE WHEN $2::boolean IS NULL THEN paused ELSE $2 END,
  paused_by = CASE WHEN $2::boolean IS NULL THEN paused_by WHEN $2 THEN $7 ELSE NULL END,
  paused_at = CASE WHEN $2::boolean IS NULL THEN paused_at WHEN $2 THEN $8 ELSE NULL END,
  settings  = (CASE WHEN $4::text IS NULL THEN settings || $3::jsonb
               ELSE (settings || $3::jsonb) || jsonb_build_object('tenant_data_sharing',
                    COALESCE(settings->'tenant_data_sharing','{}'::jsonb) || jsonb_build_object($4::text, $5::text)) END)
              #- CASE WHEN $6::text IS NULL THEN '{}'::text[] ELSE ARRAY['tenant_data_sharing', $6::text] END,
  updated_at = $8, updated_by = $7
WHERE scope = $1`

// GetSettings returns one scope's settings. A user scope with no row gives
// the defaults with Stored false.
func (p *Postgres) GetSettings(ctx context.Context, scope string) (agent.Settings, error) {
	if !scopeValid(scope) {
		return agent.Settings{}, fmt.Errorf("%w: scope %q", ErrInvalid, scope)
	}
	st, found, err := readSettings(ctx, p.pool, scope, false)
	if err != nil {
		return agent.Settings{}, err
	}
	if !found {
		return defaultSettings(scope), nil
	}
	return st, nil
}

// readSettings reads one scope row. forUpdate takes the row lock for the
// rest of the transaction. found is false when the scope has no row.
func readSettings(ctx context.Context, q rowQuerier, scope string, forUpdate bool) (agent.Settings, bool, error) {
	var st agent.Settings
	var pausedBy *string
	var raw []byte
	query := `
		SELECT paused, paused_by, paused_at, settings, updated_at, COALESCE(updated_by, '')
		FROM varroa.agent_settings WHERE scope = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	err := q.QueryRow(ctx, query, scope).Scan(&st.Paused, &pausedBy, &st.PausedAt, &raw, &st.UpdatedAt, &st.UpdatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return agent.Settings{}, false, nil
	}
	if err != nil {
		return agent.Settings{}, false, fmt.Errorf("read settings: %w", err)
	}
	st.Scope = scope
	st.Stored = true
	if pausedBy != nil {
		st.PausedBy = *pausedBy
	}
	if err := decodeSettingsJSON(raw, scope, &st); err != nil {
		return agent.Settings{}, false, err
	}
	return st, true, nil
}

// settingsJSON is the wire shape of the settings column. A key that is absent
// keeps its default.
type settingsJSON struct {
	DefaultProvider   string            `json:"default_provider"`
	DefaultModel      string            `json:"default_model"`
	TenantDataSharing map[string]string `json:"tenant_data_sharing"`
	Enabled           *bool             `json:"enabled"`
	Provider          string            `json:"provider"`
	Model             string            `json:"model"`
	DataSharing       string            `json:"data_sharing"`
	// TeamAgentUnattended holds the unattended grants, keyed namespace/name.
	TeamAgentUnattended map[string]agent.UnattendedGrant `json:"team_agent_unattended"`
}

// decodeSettingsJSON reads the settings column into st. A user scope takes
// the defaults for absent keys: enabled true, and data sharing logs.
func decodeSettingsJSON(raw []byte, scope string, st *agent.Settings) error {
	var w settingsJSON
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &w); err != nil {
			return fmt.Errorf("decode settings: %w", err)
		}
	}
	st.DefaultProvider = w.DefaultProvider
	st.DefaultModel = w.DefaultModel
	st.TenantDataSharing = map[string]agent.DataSharing{}
	for k, v := range w.TenantDataSharing {
		st.TenantDataSharing[k] = agent.DataSharing(v)
	}
	st.Enabled = true
	st.Provider = w.Provider
	st.Model = w.Model
	st.DataSharing = agent.DataLogs
	st.TeamAgentUnattended = w.TeamAgentUnattended
	if w.Enabled != nil {
		st.Enabled = *w.Enabled
	}
	if w.DataSharing != "" {
		st.DataSharing = agent.DataSharing(w.DataSharing)
	}
	if scope == agent.ScopeGlobal {
		st.Enabled = false
		st.DataSharing = ""
	}
	return nil
}

// PutSettings applies a patch to one scope and returns the stored result.
func (p *Postgres) PutSettings(ctx context.Context, scope string, patch SettingsPatch, by string) (agent.Settings, error) {
	if !scopeValid(scope) {
		return agent.Settings{}, fmt.Errorf("%w: scope %q", ErrInvalid, scope)
	}
	if err := validatePatch(scope, patch); err != nil {
		return agent.Settings{}, err
	}
	scalars := map[string]any{}
	if scope == agent.ScopeGlobal {
		if patch.DefaultProvider != nil {
			scalars["default_provider"] = *patch.DefaultProvider
		}
		if patch.DefaultModel != nil {
			scalars["default_model"] = *patch.DefaultModel
		}
	} else {
		if patch.Enabled != nil {
			scalars["enabled"] = *patch.Enabled
		}
		if patch.Provider != nil {
			scalars["provider"] = *patch.Provider
		}
		if patch.Model != nil {
			scalars["model"] = *patch.Model
		}
		if patch.DataSharing != nil {
			scalars["data_sharing"] = string(*patch.DataSharing)
		}
	}
	scalarRaw, err := json.Marshal(scalars)
	if err != nil {
		return agent.Settings{}, fmt.Errorf("%w: patch: %v", ErrInvalid, err)
	}

	var paused *bool
	if patch.Paused != nil {
		paused = patch.Paused
	}
	var setTenant, setLevel, removeTenant any
	if patch.TenantSet != nil {
		setTenant = patch.TenantSet.Tenant
		setLevel = string(patch.TenantSet.Level)
	}
	if patch.TenantRemove != nil {
		removeTenant = *patch.TenantRemove
	}
	now := p.now()
	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if scope != agent.ScopeGlobal {
			if _, err := tx.Exec(ctx,
				`INSERT INTO varroa.agent_settings (scope) VALUES ($1) ON CONFLICT DO NOTHING`, scope,
			); err != nil {
				return fmt.Errorf("ensure settings row: %w", err)
			}
		}
		if patch.Check != nil {
			cur, found, err := readSettings(ctx, tx, scope, true)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("%w: settings scope %s", ErrNotFound, scope)
			}
			applyPatch(&cur, scope, patch, by, now)
			if err := patch.Check(cur); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, settingsPatchSQL,
			scope, paused, string(scalarRaw), setTenant, setLevel, removeTenant, by, now,
		)
		if err != nil {
			return fmt.Errorf("write settings: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: settings scope %s", ErrNotFound, scope)
		}
		return nil
	})
	if err != nil {
		return agent.Settings{}, err
	}
	return p.GetSettings(ctx, scope)
}
