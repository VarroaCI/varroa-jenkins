package controller

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
)

// Default values for the cells feature, matching charts/varroa/values.yaml.
// ParseCellsConfigFromEnv falls back to these when a variable is absent.
const (
	defaultCellsWarmPoolEnabled    = false
	defaultCellsWarmPoolPerProfile = 1
	defaultCellsMaxTenantsPerCell  = 10
)

var defaultCellsUnreadyGracePeriod = 5 * time.Minute

// defaultCellTemplate returns the template the operator seeds on a pool it
// creates when VARROA_CELLS_CELL_TEMPLATE is absent or does not parse. It
// must equal the chart default of cells.cellTemplate; a unit test compares
// the two. Each call returns a new value, so no caller can change the
// default another caller reads.
func defaultCellTemplate() *v1alpha1.CellTemplate {
	return &v1alpha1.CellTemplate{
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("3Gi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("6Gi"),
			},
		},
		MiteResources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
		PodOverrides: &v1alpha1.PodOverrides{
			JvmOpts: "-XX:MaxRAMPercentage=50",
			Affinity: &corev1.Affinity{
				PodAntiAffinity: &corev1.PodAntiAffinity{
					PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
						Weight: 100,
						PodAffinityTerm: corev1.PodAffinityTerm{
							TopologyKey: corev1.LabelHostname,
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{{
									Key:      cells.LabelCellPool,
									Operator: metav1.LabelSelectorOpExists,
								}},
							},
						},
					}},
				},
			},
		},
	}
}

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
	WarmPoolEnabled    bool
	WarmPoolPerProfile int
	MaxTenantsPerCell  int
	UnreadyGracePeriod time.Duration
	// CellTemplate seeds CellPool.spec.cellTemplate on a pool the operator
	// creates. Nil seeds a pool with no template.
	CellTemplate         *v1alpha1.CellTemplate
	NetworkPolicyEnabled bool
	APIServerEgress      APIServerEgress
	CellIsolation        CellIsolation
}

// ParseCellsConfigFromEnv reads the eight cell-related environment variables
// the chart stamps on the operator Deployment: VARROA_CELLS_WARM_POOL_ENABLED,
// VARROA_CELLS_WARM_POOL_PER_PROFILE, VARROA_CELLS_MAX_TENANTS,
// VARROA_CELLS_UNREADY_GRACE, VARROA_CELLS_CELL_TEMPLATE,
// VARROA_NETWORKPOLICY_ENABLED,
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
	if cfg.MaxTenantsPerCell > cells.MaxCellTenants {
		// status.cell.tenants holds at most MaxCellTenants items. A larger
		// capacity would bind a tenant the apiserver then refuses to record.
		logger.Warn("VARROA_CELLS_MAX_TENANTS above the item limit; using the limit", "value", cfg.MaxTenantsPerCell, "limit", cells.MaxCellTenants)
		cfg.MaxTenantsPerCell = cells.MaxCellTenants
	}
	cfg.UnreadyGracePeriod = parseCellsDurationEnv(logger, "VARROA_CELLS_UNREADY_GRACE", cfg.UnreadyGracePeriod)
	cfg.CellTemplate = parseCellTemplateEnv(logger, "VARROA_CELLS_CELL_TEMPLATE")
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

// parseCellTemplateEnv reads the pool seed template as JSON. An absent value
// answers the default. null, {}, or an object that sets no known key answer
// nil: the chart renders {} for an explicit opt-out, and that opt-out must
// not read as an absent value. Invalid JSON and an unknown key answer the
// default with a warning, so a misspelled key is never dropped in silence.
func parseCellTemplateEnv(logger *slog.Logger, key string) *v1alpha1.CellTemplate {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return defaultCellTemplate()
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var tmpl *v1alpha1.CellTemplate
	if err := dec.Decode(&tmpl); err != nil {
		logger.Warn("parse error, using the default cell template", "key", key, "value", raw, "error", err)
		return defaultCellTemplate()
	}
	// A second value after the first is invalid input too, not a template.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		logger.Warn("trailing data, using the default cell template", "key", key, "value", raw, "error", err)
		return defaultCellTemplate()
	}
	if tmpl == nil || (tmpl.Resources == nil && tmpl.MiteResources == nil && tmpl.PodOverrides == nil) {
		return nil
	}
	return tmpl
}
