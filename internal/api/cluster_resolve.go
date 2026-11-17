package api

import (
	"context"
	"errors"
	"net/http"
)

// ErrBroodNotConfigured refuses a cluster-routed call on a server without a
// brood. The caller must not use the local store in place of the brood.
var ErrBroodNotConfigured = errors.New("brood not configured")

// UnknownClusterError refuses a cluster name that the brood does not know.
type UnknownClusterError struct {
	Cluster string
}

// Error returns the refusal text, which names the cluster.
func (e *UnknownClusterError) Error() string { return "unknown cluster: " + e.Cluster }

// ResolveCluster maps the cluster value of a call onto one cluster of brood.
// An empty name selects the local cluster of brood. Any other name must be a
// cluster that brood knows. Without a brood, it returns ErrBroodNotConfigured.
// Every REST handler and MCP tool that takes a cluster shares this rule.
func ResolveCluster(ctx context.Context, brood Brood, name string) (string, error) {
	if brood == nil {
		return "", ErrBroodNotConfigured
	}
	if name == "" {
		return brood.LocalCluster(), nil
	}
	if !brood.IsKnown(ctx, name) {
		return "", &UnknownClusterError{Cluster: name}
	}
	return name, nil
}

// clusterServiceError maps a ResolveCluster error onto a ServiceError: 503
// without a brood, 404 for an unknown cluster.
func clusterServiceError(err error) *ServiceError {
	status := http.StatusNotFound
	if errors.Is(err, ErrBroodNotConfigured) {
		status = http.StatusServiceUnavailable
	}
	return &ServiceError{Status: status, Message: err.Error()}
}

// writeClusterRefusal writes a ResolveCluster error: 503 without a brood,
// 404 for an unknown cluster.
func (s *Server) writeClusterRefusal(w http.ResponseWriter, err error) {
	serr := clusterServiceError(err)
	s.writeJSONError(w, serr.Status, serr.Message)
}
