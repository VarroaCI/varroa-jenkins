package api

import (
	"net/http"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// dispatchControllerClasses handles /clusters/{cluster}/controller-classes[...].
// GET-only resource — list at collection, get-by-name at resource.
func (s *Server) dispatchControllerClasses(w http.ResponseWriter, r *http.Request, cluster string, segments []string) {
	if s.deps.ConfigBrood == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "config brood not available")
		return
	}
	if len(segments) == 0 || (len(segments) == 1 && segments[0] == "") {
		s.handleControllerClassesCollection(w, r, cluster)
		return
	}
	if len(segments) == 1 && segments[0] != "" {
		s.handleControllerClassResource(w, r, cluster, segments[0])
		return
	}
	http.NotFound(w, r)
}

// handleControllerClassesCollection handles GET /clusters/{cluster}/controller-classes.
func (s *Server) handleControllerClassesCollection(w http.ResponseWriter, r *http.Request, cluster string) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	ctx := r.Context()
	if cluster != s.localCluster() {
		s.writeJSONError(w, http.StatusNotImplemented, "controller-classes on remote clusters not yet supported")
		return
	}

	classes, err := crdstore.List[v1alpha1.ControllerClass](ctx, s.deps.Store, "", "")
	if err != nil {
		s.deps.Logger.Error("failed to list ControllerClass CRDs", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "failed to list controller classes")
		return
	}

	s.writeJSON(w, http.StatusOK, itemsEnvelope(classes))
}

// handleControllerClassResource handles GET /clusters/{cluster}/controller-classes/{name}.
func (s *Server) handleControllerClassResource(w http.ResponseWriter, r *http.Request, cluster, name string) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	ctx := r.Context()
	if cluster != s.localCluster() {
		s.writeJSONError(w, http.StatusNotImplemented, "controller-classes on remote clusters not yet supported")
		return
	}

	class, err := crdstore.Get[v1alpha1.ControllerClass](ctx, s.deps.Store, name, "")
	if err != nil {
		if k8serrors.IsNotFound(err) {
			s.writeJSONError(w, http.StatusNotFound, "controller class not found")
			return
		}
		s.deps.Logger.Error("failed to fetch ControllerClass CRD", "name", name, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "failed to get controller class")
		return
	}
	if class == nil {
		s.writeJSONError(w, http.StatusNotFound, "controller class not found")
		return
	}

	s.writeJSON(w, http.StatusOK, class)
}
