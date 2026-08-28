package api

import (
	"net/http"
	"strings"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	v1alpha1 "github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// HandleDeployableNamespaces serves GET /api/v1/clusters/{cluster}/namespaces/deployable
// — the caller's create-authorized namespaces for the target cluster.
// Per-caller (varies by claims); not cacheable.
func (s *Server) HandleDeployableNamespaces(w http.ResponseWriter, r *http.Request, cluster string) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	claims := auth.ClaimsFromContext(r.Context())

	var managed, curated []string
	var curatedDefault string
	degraded := false

	local := s.deps.Brood == nil || cluster == s.deps.Brood.LocalCluster()
	if local {
		// Core-local input assembly: reusing the exact resolution from HandleProvisioningConfig.
		defaults, err := crdstore.Get[v1alpha1.ProvisioningDefaults](r.Context(), s.deps.Store, "varroa-defaults", "")
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				s.deps.Logger.Error("failed to fetch provisioning defaults", "error", err)
				s.writeJSONError(w, http.StatusInternalServerError, "failed to fetch provisioning configuration")
				return
			}
			defaults = &v1alpha1.ProvisioningDefaults{}
		}

		curatedDefault = defaults.Spec.DefaultNamespace
		if curatedDefault == "" {
			curatedDefault = "varroa"
		}

		curated = []string{curatedDefault}
		for _, ns := range defaults.Spec.Namespaces {
			if ns != curatedDefault {
				curated = append(curated, ns)
			}
		}

		// Parse the raw ManagedNamespaces string (space/comma-separated) into []string.
		managed = parseManagedNamespaces(s.deps.ManagedNamespaces)
	} else {
		inputs, err := s.deps.Brood.DiscoverNamespaces(r.Context(), cluster)
		if err == nil {
			managed, curated, curatedDefault = inputs.ManagedNamespaces, inputs.CuratedNamespaces, inputs.CuratedDefault
		} else {
			// Degraded: transport failure OR structured operator error — target
			// inputs are unknown either way. Assembly proceeds with nil inputs.
			s.deps.Logger.Warn("deployable namespaces: target cluster discovery failed",
				"cluster", cluster, "error", err)
			degraded = true // managed/curated stay nil, curatedDefault ""
		}
	}

	// Nil-authorizer fail-closed guard: RELOCATED to after the degraded
	// computation so the empty reply still reports degradation.
	if s.deps.Authorizer == nil {
		s.writeJSON(w, http.StatusOK, DeployableNamespaces{Namespaces: []string{}, Degraded: degraded})
		return
	}

	resp := s.deps.Authorizer.DeployableNamespaces(claims, managed, curated, curatedDefault)
	resp.Degraded = degraded
	s.writeJSON(w, http.StatusOK, resp)
}

// parseManagedNamespaces splits a space- and/or comma-separated string into a
// non-nil string slice, dropping empty entries. Returns nil if the input is
// empty or contains only separators (cluster-wide mode).
func parseManagedNamespaces(raw string) []string {
	if raw == "" {
		return nil
	}
	f := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ' ' || r == ','
	})
	if len(f) == 0 {
		return nil
	}
	return f
}
