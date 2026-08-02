package wakeserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/hibernation"
)

const statusPath = "/.varroa/wake/status"

// ControllerLister supplies the informer-backed controller view used for routing.
type ControllerLister interface {
	ListControllers() []*v1alpha1.Controller
}

// Waker performs the existing reconciler wake operations.
type Waker interface {
	WakeHibernatedController(ctx context.Context, namespace, name string)
	WakeController(cluster, namespace, name string)
}

// Server maps controller traffic to wake actions and responses.
type Server struct {
	Lister     ControllerLister
	Waker      Waker
	RootDomain func() string
	Logger     *slog.Logger
}

// ServeHTTP handles controller wake traffic routed through a custom EndpointSlice.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cr, prefix := s.resolveController(r)
	if cr == nil {
		if s.Logger != nil {
			s.Logger.Debug("wake request did not match a controller", "host", r.Host, "path", r.URL.Path)
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "controller not found"})
		return
	}

	remainder := r.URL.Path
	if prefix != "" {
		remainder = strings.TrimPrefix(remainder, prefix)
		if remainder == "" {
			remainder = "/"
		}
	}
	if remainder == statusPath {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"varroaWake": true,
			"phase":      cr.Status.Phase,
		})
		return
	}

	if cr.Spec.PowerState == "Stopped" {
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error": "controller is stopped",
			"phase": cr.Status.Phase,
		})
		return
	}
	if cr.Spec.PowerState == "Hibernated" {
		s.Waker.WakeHibernatedController(r.Context(), cr.Namespace, cr.Name)
		if s.Logger != nil {
			s.Logger.Info("woke hibernated controller from navigation", "controller", cr.Namespace+"/"+cr.Name, "host", r.Host, "path", r.URL.Path)
		}
	} else if cr.Status.Phase == v1alpha1.ControllerPhaseConnected {
		s.Waker.WakeController("", cr.Namespace, cr.Name)
		if s.Logger != nil {
			s.Logger.Info("nudged connected controller to remove stale wake slice", "controller", cr.Namespace+"/"+cr.Name, "host", r.Host, "path", r.URL.Path)
		}
	}

	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html") {
		hibernation.WriteInterstitial(w, hibernation.InterstitialParams{
			StatusPath:                prefix + statusPath,
			TargetURL:                 r.URL.RequestURI(),
			HTTPStatus:                http.StatusServiceUnavailable,
			RedirectOnNonWakeResponse: true,
		})
		return
	}
	w.Header().Set("Retry-After", "5")
	writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
		"error": "controller is waking",
		"phase": cr.Status.Phase,
	})
}

func (s *Server) resolveController(r *http.Request) (*v1alpha1.Controller, string) {
	controllers := s.Lister.ListControllers()
	if strings.HasPrefix(r.URL.Path, "/jenkins/") {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/jenkins/"), "/")
		if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
			prefix := v1alpha1.PathPrefix(parts[0], parts[1])
			if r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/") {
				for _, cr := range controllers {
					if cr != nil && cr.Namespace == parts[0] && cr.Name == parts[1] &&
						cr.Spec.IngressSpec != nil && cr.Spec.IngressSpec.RoutingMode() == v1alpha1.RoutingModePath {
						return cr, prefix
					}
				}
			}
		}
	}

	host := r.Host
	if host == "" {
		host = r.Header.Get("X-Forwarded-Host")
	}
	host = stripPort(strings.TrimSpace(strings.Split(host, ",")[0]))
	if host == "" {
		return nil, ""
	}
	rootDomain := ""
	if s.RootDomain != nil {
		rootDomain = s.RootDomain()
	}
	for _, cr := range controllers {
		if cr == nil {
			continue
		}
		resolved := v1alpha1.ResolveHost(cr, rootDomain)
		if resolved == "" {
			continue
		}
		if strings.EqualFold(host, resolved) {
			return cr, ""
		}
	}
	return nil, ""
}

func stripPort(host string) string {
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		return strings.Trim(parsedHost, "[]")
	}
	return strings.Trim(host, "[]")
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
