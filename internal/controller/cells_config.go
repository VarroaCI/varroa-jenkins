package controller

import (
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
)

// Default values for the cells feature, matching charts/varroa/values.yaml.
// ParseCellsConfigFromEnv falls back to these when a variable is absent.
const (
	defaultCellsWarmPoolEnabled    = false
	defaultCellsWarmPoolPerProfile = 1
	defaultCellsMaxTenantsPerCell  = 10
)

var defaultCellsUnreadyGracePeriod = 5 * time.Minute

// APIServerEgress names the kube-apiserver egress CIDRs and ports the cell
// NetworkPolicy allows. It mirrors chart value networkPolicy.apiServerEgress,
// the same value the operator's own NetworkPolicy templates render at
// install time.
type APIServerEgress struct {
	CIDRs []string `json:"cidrs"`
	Ports []int32  `json:"ports"`
}

// CellIsolation configures the per-cell NetworkPolicy the operator renders
// for a Controller with spec.cell.enabled. It mirrors chart value
// networkPolicy.cellIsolation.
type CellIsolation struct {
	Enabled      bool                                    `json:"enabled"`
	ExtraIngress []networkingv1.NetworkPolicyIngressRule `json:"extraIngress"`
	ExtraEgress  []networkingv1.NetworkPolicyEgressRule  `json:"extraEgress"`
}

// CellsConfig holds the operator's boot-time tunables for the cell feature:
// warm pool sizing, the tenant-per-cell cap, the unready grace period, and
// the cell NetworkPolicy's ingress/egress shape. ParseCellsConfigFromEnv
// builds one value once at startup; nothing later mutates it.
type CellsConfig struct {
	WarmPoolEnabled      bool
	WarmPoolPerProfile   int
	MaxTenantsPerCell    int
	UnreadyGracePeriod   time.Duration
	NetworkPolicyEnabled bool
	APIServerEgress      APIServerEgress
	CellIsolation        CellIsolation
}

// ParseCellsConfigFromEnv reads the seven cell-related environment variables
// the chart stamps on the operator Deployment: VARROA_CELLS_WARM_POOL_ENABLED,
// VARROA_CELLS_WARM_POOL_PER_PROFILE, VARROA_CELLS_MAX_TENANTS,
// VARROA_CELLS_UNREADY_GRACE, VARROA_NETWORKPOLICY_ENABLED,
// VARROA_NETWORKPOLICY_APISERVER_EGRESS, and VARROA_CELL_ISOLATION.
//
// A missing or unparsable value falls back to its documented default and
// logs a warning. This function never fails the boot.
func ParseCellsConfigFromEnv(logger *slog.Logger) CellsConfig {
	if logger == nil {
		logger = slog.Default()
	}

	cfg := CellsConfig{
		WarmPoolEnabled:    defaultCellsWarmPoolEnabled,
		WarmPoolPerProfile: defaultCellsWarmPoolPerProfile,
		MaxTenantsPerCell:  defaultCellsMaxTenantsPerCell,
		UnreadyGracePeriod: defaultCellsUnreadyGracePeriod,
	}

	cfg.WarmPoolEnabled = parseCellsBoolEnv(logger, "VARROA_CELLS_WARM_POOL_ENABLED", cfg.WarmPoolEnabled)
	cfg.WarmPoolPerProfile = parseCellsIntEnv(logger, "VARROA_CELLS_WARM_POOL_PER_PROFILE", cfg.WarmPoolPerProfile)
	if cfg.WarmPoolPerProfile < 1 {
		// The pool keeps at least one free member per profile: design D5 keeps
		// the last free member, and the warm-pool spec deletes only above
		// perProfile, so a floor of one makes the two rules agree.
		logger.Warn("VARROA_CELLS_WARM_POOL_PER_PROFILE below 1; using 1", "value", cfg.WarmPoolPerProfile)
		cfg.WarmPoolPerProfile = 1
	}
	cfg.MaxTenantsPerCell = parseCellsIntEnv(logger, "VARROA_CELLS_MAX_TENANTS", cfg.MaxTenantsPerCell)
	cfg.UnreadyGracePeriod = parseCellsDurationEnv(logger, "VARROA_CELLS_UNREADY_GRACE", cfg.UnreadyGracePeriod)
	cfg.NetworkPolicyEnabled = parseCellsBoolEnv(logger, "VARROA_NETWORKPOLICY_ENABLED", cfg.NetworkPolicyEnabled)

	if raw := os.Getenv("VARROA_NETWORKPOLICY_APISERVER_EGRESS"); raw != "" {
		var egress APIServerEgress
		if err := json.Unmarshal([]byte(raw), &egress); err != nil {
			logger.Warn("VARROA_NETWORKPOLICY_APISERVER_EGRESS: parse error, using empty egress", "value", raw, "error", err)
		} else {
			cfg.APIServerEgress = egress
		}
	}

	if raw := os.Getenv("VARROA_CELL_ISOLATION"); raw != "" {
		var isolation CellIsolation
		if err := json.Unmarshal([]byte(raw), &isolation); err != nil {
			logger.Warn("VARROA_CELL_ISOLATION: parse error, using disabled isolation", "value", raw, "error", err)
		} else {
			cfg.CellIsolation = isolation
		}
	}

	return cfg
}

func parseCellsBoolEnv(logger *slog.Logger, key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		logger.Warn("parse error, using fallback", "key", key, "value", v, "error", err)
		return fallback
	}
	return b
}

func parseCellsIntEnv(logger *slog.Logger, key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		logger.Warn("parse error, using fallback", "key", key, "value", v, "error", err)
		return fallback
	}
	return n
}

func parseCellsDurationEnv(logger *slog.Logger, key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		logger.Warn("parse error, using fallback", "key", key, "value", v, "error", err)
		return fallback
	}
	return d
}
