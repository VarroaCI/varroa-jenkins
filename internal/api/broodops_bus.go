package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// BroodOps is the abstract interface for brood operation commands across
// clusters (§6). Mutations and preview transit the bus uniformly for every
// cluster (D-2); reads use local-direct + remote-bus fan-out.
type BroodOps interface {
	Create(ctx context.Context, clusters []string, ns, name string, spec map[string]v1alpha1.BroodOperationSpec, startedBy string) []ClusterCreateResult
	List(ctx context.Context, ns, clusterFilter string) ([]ClusterBroodOp, []ClusterFanoutStatus, error)
	Get(ctx context.Context, ns, name string) ([]ClusterBroodOp, []ClusterFanoutStatus, error)
	Cancel(ctx context.Context, clusters []string, ns, name string) []ClusterActionResult
	Suspend(ctx context.Context, clusters []string, ns, name string, suspend bool) []ClusterActionResult
	Preview(ctx context.Context, clusters []string, ns string, spec map[string]v1alpha1.BroodOperationSpec) []ClusterPreviewResult
}

// ClusterCreateResult is the per-cluster outcome of a create fan-out.
type ClusterCreateResult struct {
	Cluster string                   `json:"cluster"`
	OK      bool                     `json:"ok"`
	Code    string                   `json:"code,omitempty"`
	Error   string                   `json:"error,omitempty"`
	Op      *v1alpha1.BroodOperation `json:"op,omitempty"`
}

// ClusterActionResult is the per-cluster outcome of a cancel/suspend fan-out.
type ClusterActionResult struct {
	Cluster string `json:"cluster"`
	OK      bool   `json:"ok"`
	Code    string `json:"code,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ClusterPreviewResult is the per-cluster outcome of a preview fan-out.
type ClusterPreviewResult struct {
	Cluster string                   `json:"cluster"`
	OK      bool                     `json:"ok"`
	Code    string                   `json:"code,omitempty"`
	Error   string                   `json:"error,omitempty"`
	Targets []bus.BroodPreviewTarget `json:"targets,omitempty"`
}

// ClusterBroodOp pairs a cluster name with a BroodOperation CR.
type ClusterBroodOp struct {
	Cluster string
	Op      *v1alpha1.BroodOperation
}

// Timeout constants for broodops bus operations.
const (
	broodOpsCreateTimeout  = 10 * time.Second
	broodOpsGetTimeout     = 3 * time.Second
	broodOpsListTimeout    = 3 * time.Second
	broodOpsCancelTimeout  = 10 * time.Second
	broodOpsSuspendTimeout = 10 * time.Second
	broodOpsPreviewTimeout = 10 * time.Second
)

// busBroodOps implements BroodOps over the NATS bus.
type busBroodOps struct {
	localCluster string
	conn         *bus.Conn
	membership   *bus.ClusterDirectory
	client       controller.ResourceClient
	store        crdstore.Backend
	logger       *slog.Logger

	// request is the injectable NATS request function; defaults to conn.Request.
	request func(subject string, data []byte, timeout time.Duration) ([]byte, error)
}

// NewBusBroodOps creates a new bus-backed BroodOps.
func NewBusBroodOps(localCluster string, conn *bus.Conn, membership *bus.ClusterDirectory, client controller.ResourceClient, store crdstore.Backend, logger *slog.Logger) BroodOps {
	return &busBroodOps{
		localCluster: localCluster,
		conn:         conn,
		membership:   membership,
		client:       client,
		store:        store,
		logger:       logger.With("component", "bus_broodops"),
		request: func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
			resp, err := conn.Request(subject, data, timeout)
			if err != nil {
				return nil, err
			}
			return resp.Data, nil
		},
	}
}

// Create fans out broodops.create to the given clusters concurrently.
func (f *busBroodOps) Create(ctx context.Context, clusters []string, ns, name string, specs map[string]v1alpha1.BroodOperationSpec, startedBy string) []ClusterCreateResult {
	results := make([]ClusterCreateResult, len(clusters))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i, cluster := range clusters {
		wg.Add(1)
		go func(idx int, c string) {
			defer wg.Done()
			spec, ok := specs[c]
			if !ok {
				// Fallback: use first spec (all same spec for selector mode)
				for _, s := range specs {
					spec = s
					break
				}
			}
			specRaw, _ := json.Marshal(spec)
			req := bus.BroodOpsCreateRequest{
				Namespace: ns,
				Name:      name,
				Spec:      specRaw,
				StartedBy: startedBy,
			}
			data, _ := json.Marshal(req)
			respData, err := f.request(bus.OperatorBroodOpsSubject(c, "create"), data, broodOpsCreateTimeout)
			if err != nil {
				mu.Lock()
				results[idx] = ClusterCreateResult{Cluster: c, OK: false, Error: fmt.Sprintf("cluster %s unreachable: %v", c, err)}
				mu.Unlock()
				return
			}
			var reply bus.BroodOpsOpResponse
			if err := json.Unmarshal(respData, &reply); err != nil {
				mu.Lock()
				results[idx] = ClusterCreateResult{Cluster: c, OK: false, Error: "invalid response"}
				mu.Unlock()
				return
			}
			if reply.Code != "" {
				mu.Lock()
				results[idx] = ClusterCreateResult{Cluster: c, OK: false, Code: reply.Code, Error: reply.Error}
				mu.Unlock()
				return
			}
			var op v1alpha1.BroodOperation
			if reply.Op != nil {
				if err := json.Unmarshal(reply.Op, &op); err != nil {
					f.logger.Warn("create unmarshal op", "cluster", c, "error", err)
				}
			}
			mu.Lock()
			results[idx] = ClusterCreateResult{Cluster: c, OK: true, Op: &op}
			mu.Unlock()
		}(i, cluster)
	}
	wg.Wait()
	return results
}

// List fans out broodops.list to all known clusters (or a filtered subset).
func (f *busBroodOps) List(ctx context.Context, ns, clusterFilter string) ([]ClusterBroodOp, []ClusterFanoutStatus, error) {
	if clusterFilter != "" {
		if clusterFilter == f.localCluster {
			return f.listLocal(ctx, ns)
		}
		return f.listRemoteBus(clusterFilter, ns)
	}

	var (
		mu       sync.Mutex
		ops      []ClusterBroodOp
		statuses []ClusterFanoutStatus
		wg       sync.WaitGroup
	)

	// Local.
	wg.Add(1)
	go func() {
		defer wg.Done()
		local, localStatus, err := f.listLocal(ctx, ns)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			statuses = append(statuses, ClusterFanoutStatus{Name: f.localCluster, OK: false, Error: err.Error()})
			return
		}
		ops = append(ops, local...)
		statuses = append(statuses, localStatus...)
	}()

	// Remote members.
	members, err := f.membership.List()
	if err != nil {
		f.logger.Warn("list members for broodops fan-out failed", "error", err)
	}
	for _, m := range members {
		if m.Name == f.localCluster {
			continue
		}
		wg.Add(1)
		go func(cluster string) {
			defer wg.Done()
			cl, st, err := f.listRemoteBus(cluster, ns)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				statuses = append(statuses, ClusterFanoutStatus{Name: cluster, OK: false, Error: err.Error()})
				return
			}
			ops = append(ops, cl...)
			statuses = append(statuses, st...)
		}(m.Name)
	}

	wg.Wait()

	// Sort: local first, then members by name.
	sort.SliceStable(statuses, func(i, j int) bool {
		if statuses[i].Name == f.localCluster {
			return true
		}
		if statuses[j].Name == f.localCluster {
			return false
		}
		return statuses[i].Name < statuses[j].Name
	})

	return ops, statuses, nil
}

func (f *busBroodOps) listLocal(ctx context.Context, ns string) ([]ClusterBroodOp, []ClusterFanoutStatus, error) {
	items, err := crdstore.List[v1alpha1.BroodOperation](ctx, f.store, ns, "")
	if err != nil {
		return nil, []ClusterFanoutStatus{{Name: f.localCluster, OK: false, Error: err.Error()}}, err
	}
	cc := make([]ClusterBroodOp, len(items))
	for i, cr := range items {
		cc[i] = ClusterBroodOp{Cluster: f.localCluster, Op: cr}
	}
	return cc, []ClusterFanoutStatus{{Name: f.localCluster, OK: true}}, nil
}

func (f *busBroodOps) listRemoteBus(cluster, ns string) ([]ClusterBroodOp, []ClusterFanoutStatus, error) {
	req := bus.BroodOpsListRequest{Namespace: ns}
	data, _ := json.Marshal(req)
	respData, err := f.request(bus.OperatorBroodOpsSubject(cluster, "list"), data, broodOpsListTimeout)
	if err != nil {
		return nil, []ClusterFanoutStatus{{Name: cluster, OK: false, Error: "cluster unreachable"}}, fmt.Errorf("list remote %s: %w", cluster, err)
	}
	var resp bus.BroodOpsListResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, []ClusterFanoutStatus{{Name: cluster, OK: false, Error: "invalid response"}}, fmt.Errorf("list remote %s decode: %w", cluster, err)
	}
	if resp.Code != "" {
		return nil, []ClusterFanoutStatus{{Name: cluster, OK: false, Error: resp.Error}}, fmt.Errorf("list remote %s error: %s", cluster, resp.Error)
	}
	cc := make([]ClusterBroodOp, len(resp.Ops))
	for i, raw := range resp.Ops {
		var op v1alpha1.BroodOperation
		if err := json.Unmarshal(raw, &op); err != nil {
			f.logger.Warn("list remote unmarshal skip", "cluster", cluster, "error", err)
			continue
		}
		cc[i] = ClusterBroodOp{Cluster: cluster, Op: &op}
	}
	return cc, []ClusterFanoutStatus{{Name: cluster, OK: true}}, nil
}

// Get fans out broodops.get to all known clusters.
func (f *busBroodOps) Get(ctx context.Context, ns, name string) ([]ClusterBroodOp, []ClusterFanoutStatus, error) {
	var (
		mu       sync.Mutex
		ops      []ClusterBroodOp
		statuses []ClusterFanoutStatus
		wg       sync.WaitGroup
	)

	// Local via bus for consistency (D-2), but use direct read for efficiency.
	wg.Add(1)
	go func() {
		defer wg.Done()
		op, err := crdstore.Get[v1alpha1.BroodOperation](ctx, f.store, name, ns)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			// not_found is NOT an error row — it just contributes no child.
			statuses = append(statuses, ClusterFanoutStatus{Name: f.localCluster, OK: true})
			return
		}
		ops = append(ops, ClusterBroodOp{Cluster: f.localCluster, Op: op})
		statuses = append(statuses, ClusterFanoutStatus{Name: f.localCluster, OK: true})
	}()

	// Remote members.
	members, err := f.membership.List()
	if err != nil {
		f.logger.Warn("get members for broodops fan-out failed", "error", err)
	}
	for _, m := range members {
		if m.Name == f.localCluster {
			continue
		}
		wg.Add(1)
		go func(cluster string) {
			defer wg.Done()
			cl, st, err := f.getRemoteBus(cluster, ns, name)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				statuses = append(statuses, ClusterFanoutStatus{Name: cluster, OK: false, Error: err.Error()})
				return
			}
			ops = append(ops, cl...)
			statuses = append(statuses, st...)
		}(m.Name)
	}

	wg.Wait()

	sort.SliceStable(statuses, func(i, j int) bool {
		if statuses[i].Name == f.localCluster {
			return true
		}
		if statuses[j].Name == f.localCluster {
			return false
		}
		return statuses[i].Name < statuses[j].Name
	})

	return ops, statuses, nil
}

func (f *busBroodOps) getRemoteBus(cluster, ns, name string) ([]ClusterBroodOp, []ClusterFanoutStatus, error) {
	req := bus.BroodOpsGetRequest{Namespace: ns, Name: name}
	data, _ := json.Marshal(req)
	respData, err := f.request(bus.OperatorBroodOpsSubject(cluster, "get"), data, broodOpsGetTimeout)
	if err != nil {
		return nil, []ClusterFanoutStatus{{Name: cluster, OK: false, Error: "cluster unreachable"}}, fmt.Errorf("get remote %s: %w", cluster, err)
	}
	var resp bus.BroodOpsOpResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, []ClusterFanoutStatus{{Name: cluster, OK: false, Error: "invalid response"}}, fmt.Errorf("get remote %s decode: %w", cluster, err)
	}
	if resp.Code != "" {
		// not_found is NOT an error row — contributes no child.
		if resp.Code == bus.CodeNotFound {
			return nil, []ClusterFanoutStatus{{Name: cluster, OK: true}}, nil
		}
		return nil, []ClusterFanoutStatus{{Name: cluster, OK: false, Error: resp.Error}}, fmt.Errorf("get remote %s: %s", cluster, resp.Error)
	}
	var op v1alpha1.BroodOperation
	if resp.Op != nil {
		if err := json.Unmarshal(resp.Op, &op); err != nil {
			return nil, []ClusterFanoutStatus{{Name: cluster, OK: false, Error: "unmarshal error"}}, fmt.Errorf("get remote %s unmarshal: %w", cluster, err)
		}
	}
	return []ClusterBroodOp{{Cluster: cluster, Op: &op}}, []ClusterFanoutStatus{{Name: cluster, OK: true}}, nil
}

// Cancel fans out broodops.cancel to the given clusters.
func (f *busBroodOps) Cancel(ctx context.Context, clusters []string, ns, name string) []ClusterActionResult {
	return f.actionFanOut("cancel", clusters, ns, name, nil, broodOpsCancelTimeout)
}

// Suspend fans out broodops.suspend to the given clusters.
func (f *busBroodOps) Suspend(ctx context.Context, clusters []string, ns, name string, suspend bool) []ClusterActionResult {
	return f.actionFanOut("suspend", clusters, ns, name, &suspend, broodOpsSuspendTimeout)
}

func (f *busBroodOps) actionFanOut(verb string, clusters []string, ns, name string, suspend *bool, timeout time.Duration) []ClusterActionResult {
	results := make([]ClusterActionResult, len(clusters))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i, cluster := range clusters {
		wg.Add(1)
		go func(idx int, c string) {
			defer wg.Done()
			var data []byte
			if verb == "cancel" {
				req := bus.BroodOpsCancelRequest{Namespace: ns, Name: name}
				data, _ = json.Marshal(req)
			} else {
				s := false
				if suspend != nil {
					s = *suspend
				}
				req := bus.BroodOpsSuspendRequest{Namespace: ns, Name: name, Suspend: s}
				data, _ = json.Marshal(req)
			}
			respData, err := f.request(bus.OperatorBroodOpsSubject(c, verb), data, timeout)
			if err != nil {
				mu.Lock()
				results[idx] = ClusterActionResult{Cluster: c, OK: false, Error: fmt.Sprintf("cluster %s unreachable: %v", c, err)}
				mu.Unlock()
				return
			}
			if verb == "cancel" {
				var reply bus.BroodOpsCancelResponse
				if err := json.Unmarshal(respData, &reply); err != nil {
					mu.Lock()
					results[idx] = ClusterActionResult{Cluster: c, OK: false, Error: "invalid response"}
					mu.Unlock()
					return
				}
				mu.Lock()
				if reply.Code != "" {
					results[idx] = ClusterActionResult{Cluster: c, OK: false, Code: reply.Code, Error: reply.Error}
				} else {
					results[idx] = ClusterActionResult{Cluster: c, OK: true}
				}
				mu.Unlock()
			} else {
				var reply bus.BroodOpsOpResponse
				if err := json.Unmarshal(respData, &reply); err != nil {
					mu.Lock()
					results[idx] = ClusterActionResult{Cluster: c, OK: false, Error: "invalid response"}
					mu.Unlock()
					return
				}
				mu.Lock()
				if reply.Code != "" {
					results[idx] = ClusterActionResult{Cluster: c, OK: false, Code: reply.Code, Error: reply.Error}
				} else {
					results[idx] = ClusterActionResult{Cluster: c, OK: true}
				}
				mu.Unlock()
			}
		}(i, cluster)
	}
	wg.Wait()
	return results
}

// Preview fans out broodops.preview to the given clusters.
func (f *busBroodOps) Preview(ctx context.Context, clusters []string, ns string, specs map[string]v1alpha1.BroodOperationSpec) []ClusterPreviewResult {
	results := make([]ClusterPreviewResult, len(clusters))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i, cluster := range clusters {
		wg.Add(1)
		go func(idx int, c string) {
			defer wg.Done()
			spec, ok := specs[c]
			if !ok {
				for _, s := range specs {
					spec = s
					break
				}
			}
			specRaw, _ := json.Marshal(spec)
			req := bus.BroodOpsPreviewRequest{
				Namespace: ns,
				Spec:      specRaw,
			}
			data, _ := json.Marshal(req)
			respData, err := f.request(bus.OperatorBroodOpsSubject(c, "preview"), data, broodOpsPreviewTimeout)
			if err != nil {
				mu.Lock()
				results[idx] = ClusterPreviewResult{Cluster: c, OK: false, Error: fmt.Sprintf("cluster %s unreachable: %v", c, err)}
				mu.Unlock()
				return
			}
			var reply bus.BroodOpsPreviewResponse
			if err := json.Unmarshal(respData, &reply); err != nil {
				mu.Lock()
				results[idx] = ClusterPreviewResult{Cluster: c, OK: false, Error: "invalid response"}
				mu.Unlock()
				return
			}
			mu.Lock()
			if reply.Code != "" {
				results[idx] = ClusterPreviewResult{Cluster: c, OK: false, Code: reply.Code, Error: reply.Error}
			} else {
				results[idx] = ClusterPreviewResult{Cluster: c, OK: true, Targets: reply.Targets}
			}
			mu.Unlock()
		}(i, cluster)
	}
	wg.Wait()
	return results
}
