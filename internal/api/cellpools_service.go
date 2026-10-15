package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// ClusterCellPool pairs a cell pool with the cluster that holds it. A pool
// name is unique inside one cluster only.
type ClusterCellPool struct {
	Cluster string
	Pool    *v1alpha1.CellPool
}

// CellPoolList is the result of ListCellPools: the pools of every cluster
// the list covered, and one read status for each of those clusters.
type CellPoolList struct {
	Items    []ClusterCellPool
	Clusters []ClusterFanoutStatus
}

// cellPoolConflictMessage is the text of a scale that conflicted on every
// attempt.
const cellPoolConflictMessage = "the cell pool changed during the update; retry the request"

// ListCellPools lists the pools of one cluster, or of every known cluster
// when cluster is empty. A cluster whose read fails is reported with ok
// false in Clusters and adds no item; it does not fail the call. The HTTP
// list route and the list_cell_pools tool both call it.
func (s *Server) ListCellPools(ctx context.Context, claims *auth.Claims, cluster string) (CellPoolList, *ServiceError) {
	if !s.canReadCellPoolsFor(claims) {
		return CellPoolList{}, &ServiceError{Status: http.StatusForbidden, Message: "forbidden"}
	}

	local := s.localCluster()
	var targets []string
	if cluster != "" {
		name, _, serr := s.resolveCellPoolCluster(ctx, cluster)
		if serr != nil {
			return CellPoolList{}, serr
		}
		targets = []string{name}
	} else {
		targets = s.cellPoolClusters(ctx, local)
	}

	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		out    CellPoolList
		status = make([]ClusterFanoutStatus, len(targets))
	)
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			pools, err := s.cellPoolsOn(ctx, target)
			if err != nil {
				status[i] = ClusterFanoutStatus{Name: target, OK: false, Error: cellPoolReadError(err, target)}
				return
			}
			status[i] = ClusterFanoutStatus{Name: target, OK: true}
			mu.Lock()
			defer mu.Unlock()
			for j := range pools {
				out.Items = append(out.Items, ClusterCellPool{Cluster: target, Pool: &pools[j]})
			}
		}(i, target)
	}
	wg.Wait()

	sort.SliceStable(out.Items, func(i, j int) bool {
		a, b := out.Items[i], out.Items[j]
		if a.Cluster != b.Cluster {
			return clusterBefore(a.Cluster, b.Cluster, local)
		}
		return a.Pool.Name < b.Pool.Name
	})
	sort.SliceStable(status, func(i, j int) bool {
		return clusterBefore(status[i].Name, status[j].Name, local)
	})
	out.Clusters = status
	if out.Items == nil {
		out.Items = []ClusterCellPool{}
	}
	return out, nil
}

// GetCellPool reads one pool of one cluster. An empty cluster means the
// BFF's own cluster.
func (s *Server) GetCellPool(ctx context.Context, claims *auth.Claims, cluster, name string) (ClusterCellPool, *ServiceError) {
	if !s.canReadCellPoolsFor(claims) {
		return ClusterCellPool{}, &ServiceError{Status: http.StatusForbidden, Message: "forbidden"}
	}
	target, local, serr := s.resolveCellPoolCluster(ctx, cluster)
	if serr != nil {
		return ClusterCellPool{}, serr
	}

	var pool *v1alpha1.CellPool
	var err error
	switch {
	case local:
		if s.deps.Store == nil {
			return ClusterCellPool{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "store not configured"}
		}
		pool, err = crdstore.Get[v1alpha1.CellPool](ctx, s.deps.Store, name, "")
	case s.deps.ConfigBrood == nil:
		err = &ErrClusterUnreachable{Cluster: target, Err: errors.New("config brood not available")}
	default:
		pool, err = s.deps.ConfigBrood.GetCellPool(ctx, target, name)
	}
	if err != nil {
		return ClusterCellPool{}, cellPoolServiceError(err, target, "failed to read cell pool")
	}
	return ClusterCellPool{Cluster: target, Pool: pool}, nil
}

// ScaleCellPool applies the named dials to one pool of one cluster. The
// local cluster writes on the BFF's own store. A member cluster's operator
// runs the same read-modify-write against its own apiserver. The caller
// publishes the activity event, because the actor and the source belong to
// the caller.
func (s *Server) ScaleCellPool(ctx context.Context, claims *auth.Claims, cluster, name string, dials cells.CellPoolDials) (ClusterCellPool, *ServiceError) {
	if s.deps.Authorizer == nil || claims == nil || !s.deps.Authorizer.CanUpdateCellPools(claims) {
		return ClusterCellPool{}, &ServiceError{Status: http.StatusForbidden, Message: "forbidden"}
	}
	target, local, serr := s.resolveCellPoolCluster(ctx, cluster)
	if serr != nil {
		return ClusterCellPool{}, serr
	}

	var pool *v1alpha1.CellPool
	var err error
	switch {
	case local:
		if s.deps.Store == nil {
			return ClusterCellPool{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "store not configured"}
		}
		pool, err = cells.ScaleCellPool(ctx, s.deps.Store, name, dials)
	case s.deps.ConfigBrood == nil:
		err = &ErrClusterUnreachable{Cluster: target, Err: errors.New("config brood not available")}
	default:
		pool, err = s.deps.ConfigBrood.ScaleCellPool(ctx, target, bus.CellPoolScaleRequest{
			Name:               name,
			Warm:               dials.Warm,
			MaxCells:           dials.MaxCells,
			MaxTenantsPerCell:  dials.MaxTenantsPerCell,
			UnreadyGracePeriod: dials.UnreadyGracePeriod,
		})
	}
	if err != nil {
		return ClusterCellPool{}, cellPoolServiceError(err, target, "failed to update cell pool")
	}
	return ClusterCellPool{Cluster: target, Pool: pool}, nil
}

// canReadCellPoolsFor answers the read gate. An absent Authorizer or
// absent claims deny.
func (s *Server) canReadCellPoolsFor(claims *auth.Claims) bool {
	return s.deps.Authorizer != nil && claims != nil && s.deps.Authorizer.CanReadCellPools(claims)
}

// resolveCellPoolCluster maps a requested cluster onto its name and tells
// whether it is the BFF's own cluster. An empty value is the local
// cluster. A cluster the brood does not know is a 404. Without a brood
// router only the local cluster is known.
func (s *Server) resolveCellPoolCluster(ctx context.Context, cluster string) (string, bool, *ServiceError) {
	local := s.localCluster()
	if cluster == "" || cluster == local {
		return local, true, nil
	}
	if s.deps.Brood == nil || !s.deps.Brood.IsKnown(ctx, cluster) {
		return "", false, &ServiceError{Status: http.StatusNotFound, Message: "cluster " + cluster + " not found"}
	}
	return cluster, false, nil
}

// cellPoolClusters lists the clusters an unfiltered list covers: every
// cluster the brood knows, else the local cluster alone.
func (s *Server) cellPoolClusters(ctx context.Context, local string) []string {
	if s.deps.Brood == nil {
		return []string{local}
	}
	infos, err := s.deps.Brood.Clusters(ctx)
	if err != nil {
		s.deps.Logger.Warn("cell pools: the cluster list failed; listing the local cluster only", "error", err)
		return []string{local}
	}
	seen := map[string]bool{local: true}
	out := []string{local}
	for _, info := range infos {
		if info.Name == "" || seen[info.Name] {
			continue
		}
		seen[info.Name] = true
		out = append(out, info.Name)
	}
	return out
}

// cellPoolsOn lists the pools of one cluster: the local cluster on the
// BFF's own store, a member cluster through ConfigBrood.
func (s *Server) cellPoolsOn(ctx context.Context, cluster string) ([]v1alpha1.CellPool, error) {
	if s.isLocalCluster(cluster) || cluster == s.localCluster() {
		if s.deps.Store == nil {
			return nil, errors.New("store not configured")
		}
		pools, err := crdstore.List[v1alpha1.CellPool](ctx, s.deps.Store, "", "")
		if err != nil {
			return nil, err
		}
		out := make([]v1alpha1.CellPool, 0, len(pools))
		for _, p := range pools {
			out = append(out, *p)
		}
		return out, nil
	}
	if s.deps.ConfigBrood == nil {
		return nil, &ErrClusterUnreachable{Cluster: cluster, Err: errors.New("config brood not available")}
	}
	return s.deps.ConfigBrood.ListCellPools(ctx, cluster)
}

// clusterBefore orders cluster names with the local cluster first.
func clusterBefore(a, b, local string) bool {
	if a == local {
		return b != local
	}
	if b == local {
		return false
	}
	return a < b
}

// cellPoolReadError is the error text of one cluster in a list status.
func cellPoolReadError(err error, cluster string) string {
	var unreachable *ErrClusterUnreachable
	if errors.As(err, &unreachable) {
		return "cluster " + cluster + " unreachable"
	}
	return err.Error()
}

// cellPoolServiceError maps a store, scale, or ConfigBrood error onto the
// status table the local path and the member path share.
func cellPoolServiceError(err error, cluster, fallback string) *ServiceError {
	var unreachable *ErrClusterUnreachable
	var brood *BroodError
	switch {
	case errors.Is(err, cells.ErrCellPoolInvalid):
		return &ServiceError{Status: http.StatusBadRequest, Message: err.Error()}
	case errors.Is(err, cells.ErrCellPoolNotFound), apierrors.IsNotFound(err):
		return &ServiceError{Status: http.StatusNotFound, Message: "cell pool not found"}
	case errors.Is(err, cells.ErrCellPoolConflict):
		return &ServiceError{Status: http.StatusConflict, Message: cellPoolConflictMessage}
	case errors.As(err, &unreachable):
		return &ServiceError{Status: http.StatusBadGateway, Message: "cluster " + cluster + " unreachable"}
	case errors.As(err, &brood):
		switch brood.Code {
		case bus.CodeInvalid:
			return &ServiceError{Status: http.StatusBadRequest, Message: brood.Msg}
		case bus.CodeNotFound:
			return &ServiceError{Status: http.StatusNotFound, Message: "cell pool not found"}
		case bus.CodeConflict:
			return &ServiceError{Status: http.StatusConflict, Message: cellPoolConflictMessage}
		}
	}
	return &ServiceError{Status: http.StatusInternalServerError, Message: fallback}
}
