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

// Timeout constants (§2.2).
const (
	broodListTimeout       = 3 * time.Second
	broodGetTimeout        = 3 * time.Second
	broodCreateTimeout     = 30 * time.Second
	broodDryRunTimeout     = 15 * time.Second
	broodUpdateTimeout     = 30 * time.Second
	broodDeleteTimeout     = 10 * time.Second
	broodDeletePodTimeout  = 10 * time.Second
	broodDrainTimeout      = 10 * time.Second
	broodNamespacesTimeout = 3 * time.Second
)

// ClusterInfo is the API-facing cluster info struct: the KV heartbeat entry
// plus BFF-derived fields. Core is true on exactly one row: the BFF's own cluster.
type ClusterInfo struct {
	bus.ClusterInfo
	Core    bool `json:"core"`
	Healthy bool `json:"healthy"`
}

// ClusterFanoutStatus records the per-cluster outcome of a fan-out list.
type ClusterFanoutStatus struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// ClusterController pairs a cluster name with a controller CR.
type ClusterController struct {
	Cluster string
	CR      *v1alpha1.Controller
}

// ControllersCreateArgs carries the creation parameters for Brood.Create/Preflight.
type ControllersCreateArgs struct {
	Namespace  string
	Controller json.RawMessage
	Bundle     json.RawMessage
	DryRun     bool
}

// BroodError wraps a bus error code into a Go error.
type BroodError struct {
	Code      string
	Msg       string
	Conflicts []bus.FieldConflict
}

func (e *BroodError) Error() string { return e.Msg }

// ErrClusterUnreachable is returned when the target cluster's operator leader
// does not respond within the timeout.
type ErrClusterUnreachable struct {
	Cluster string
	Err     error
}

func (e *ErrClusterUnreachable) Error() string {
	return fmt.Sprintf("cluster %s unreachable: %v", e.Cluster, e.Err)
}

func (e *ErrClusterUnreachable) Unwrap() error { return e.Err }

// Brood is the abstract interface for controller CRUD across clusters (§4).
type Brood interface {
	// LocalCluster returns the BFF's own cluster name.
	LocalCluster() string

	// Clusters returns all known clusters (KV members + self row).
	Clusters(ctx context.Context) ([]ClusterInfo, error)

	// IsKnown returns true if the named cluster is the local cluster or a KV member.
	IsKnown(ctx context.Context, cluster string) bool

	// ListAll fetches controllers across clusters. When clusterFilter is
	// non-empty, only that cluster is queried (caller must have called IsKnown
	// first). Returns controllers and per-cluster fan-out status.
	ListAll(ctx context.Context, ns, clusterFilter string) ([]ClusterController, []ClusterFanoutStatus, error)

	// Get fetches a single controller by cluster/ns/name.
	Get(ctx context.Context, cluster, ns, name string) (*v1alpha1.Controller, error)

	// Create creates a controller on the target cluster.
	Create(ctx context.Context, cluster string, req ControllersCreateArgs) (*v1alpha1.Controller, []bus.Check, error)

	// Preflight runs preflight without persisting (dry-run).
	Preflight(ctx context.Context, cluster string, req ControllersCreateArgs) ([]bus.Check, error)

	// Update applies a partial spec to a controller on the target cluster via
	// server-side apply, completed from the field manager's ownership set.
	// fieldManager identifies the owning client; force=true re-asserts ownership over conflicting fields.
	// The third return is the requested spec removals (explicit nulls) that did
	// not take effect on the target cluster, for relay into the detail response.
	Update(ctx context.Context, cluster, ns, name string, patch json.RawMessage, fieldManager string, force bool) (*v1alpha1.Controller, []bus.Check, []bus.UnappliedRemoval, error)

	// Delete removes a controller from the target cluster.
	Delete(ctx context.Context, cluster, ns, name string) error

	// DeletePod deletes the Jenkins pod for a controller (hard restart).
	DeletePod(ctx context.Context, cluster, ns, name string) error

	// Drain starts a drain on the target cluster. Returns the resulting state.
	Drain(ctx context.Context, cluster, requestedBy string) (string, error)

	// DrainCancel cancels a drain. Returns the resulting state.
	DrainCancel(ctx context.Context, cluster, requestedBy string) (string, error)

	// StateOf returns the target cluster's lifecycle state from the directory.
	StateOf(ctx context.Context, cluster string) (string, error)

	// DiscoverNamespaces fetches the target cluster's deployable-namespace
	// discovery inputs over the bus. Callers must route the local cluster
	// themselves (core fast path); calling this with the local cluster is a
	// programming error and returns an internal error.
	DiscoverNamespaces(ctx context.Context, cluster string) (*bus.NamespacesListResponse, error)
}

// busBrood implements Brood over the NATS bus.
type busBrood struct {
	localCluster string
	conn         *bus.Conn
	membership   *bus.ClusterDirectory
	client       controller.ResourceClient
	store        crdstore.Backend
	logger       *slog.Logger

	// request is the injectable NATS request function; defaults to conn.Request.
	// Override in tests to avoid needing a live NATS connection.
	request func(subject string, data []byte, timeout time.Duration) ([]byte, error)

	// membershipCache stores the last List() result (nil = no cache).
	// Guarded by membershipMu: busBrood is a shared singleton hit by
	// concurrent HTTP handlers.
	membershipMu        sync.Mutex
	membershipCache     []bus.ClusterInfo
	membershipCacheTime time.Time
}

// NewBusBrood creates a new bus-backed Brood.
func NewBusBrood(localCluster string, conn *bus.Conn, membership *bus.ClusterDirectory, client controller.ResourceClient, store crdstore.Backend, logger *slog.Logger) Brood {
	return &busBrood{
		localCluster: localCluster,
		conn:         conn,
		membership:   membership,
		client:       client,
		store:        store,
		logger:       logger.With("component", "bus_brood"),
		request: func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
			resp, err := conn.Request(subject, data, timeout)
			if err != nil {
				return nil, err
			}
			return resp.Data, nil
		},
	}
}

// getMembers returns the list of member clusters from the KV directory,
// with a 5s in-process memoization.
func (f *busBrood) getMembers() ([]bus.ClusterInfo, error) {
	f.membershipMu.Lock()
	defer f.membershipMu.Unlock()
	if f.membershipCache != nil && time.Since(f.membershipCacheTime) < 5*time.Second {
		return f.membershipCache, nil
	}
	members, err := f.membership.List()
	if err != nil {
		return nil, err
	}
	f.membershipCache = members
	f.membershipCacheTime = time.Now()
	return members, nil
}

func (f *busBrood) LocalCluster() string { return f.localCluster }

func (f *busBrood) IsKnown(ctx context.Context, cluster string) bool {
	if cluster == f.localCluster {
		return true
	}
	members, err := f.getMembers()
	if err != nil {
		return false
	}
	for _, m := range members {
		if m.Name == cluster {
			return true
		}
	}
	return false
}

func (f *busBrood) Clusters(ctx context.Context) ([]ClusterInfo, error) {
	members, err := f.getMembers()
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}

	// Self row: always first, always healthy (it's us), the only Core=true row.
	result := []ClusterInfo{{
		ClusterInfo: bus.ClusterInfo{Name: f.localCluster, State: bus.ClusterStateActive},
		Core:        true,
		Healthy:     true,
	}}

	for _, m := range members {
		if m.Name == f.localCluster {
			// Merge operator-provided info into the self row.
			result[0].ClusterInfo = m
			continue
		}
		result = append(result, ClusterInfo{
			ClusterInfo: m,
			Healthy:     time.Since(m.LastHeartbeat) < bus.ClusterEntryTTL,
		})
	}

	return result, nil
}

func (f *busBrood) ListAll(ctx context.Context, ns, clusterFilter string) ([]ClusterController, []ClusterFanoutStatus, error) {
	if clusterFilter != "" {
		// Single-cluster query.
		if clusterFilter == f.localCluster {
			return f.listLocal(ctx, ns)
		}
		return f.listRemote(clusterFilter, ns)
	}

	// Fan-out: local + all remote members.
	var (
		mu          sync.Mutex
		controllers []ClusterController
		statuses    []ClusterFanoutStatus
		wg          sync.WaitGroup
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
		controllers = append(controllers, local...)
		statuses = append(statuses, localStatus...)
	}()

	// Remote members.
	members, err := f.getMembers()
	if err != nil {
		f.logger.Warn("list members for fan-out failed", "error", err)
		// Still return local results
	}
	for _, m := range members {
		if m.Name == f.localCluster {
			continue
		}
		wg.Add(1)
		go func(cluster string) {
			defer wg.Done()
			cl, st, err := f.listRemote(cluster, ns)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				statuses = append(statuses, ClusterFanoutStatus{Name: cluster, OK: false, Error: err.Error()})
				return
			}
			controllers = append(controllers, cl...)
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

	return controllers, statuses, nil
}

func (f *busBrood) listLocal(ctx context.Context, ns string) ([]ClusterController, []ClusterFanoutStatus, error) {
	items, err := crdstore.List[v1alpha1.Controller](ctx, f.store, ns, "")
	if err != nil {
		return nil, []ClusterFanoutStatus{{Name: f.localCluster, OK: false, Error: err.Error()}}, err
	}
	cc := make([]ClusterController, len(items))
	for i, cr := range items {
		cc[i] = ClusterController{Cluster: f.localCluster, CR: cr}
	}
	return cc, []ClusterFanoutStatus{{Name: f.localCluster, OK: true}}, nil
}

func (f *busBrood) listRemote(cluster, ns string) ([]ClusterController, []ClusterFanoutStatus, error) {
	req := bus.ControllersListRequest{Namespace: ns}
	data, _ := json.Marshal(req)
	respData, err := f.request(bus.OperatorControllersSubject(cluster, "list"), data, broodListTimeout)
	if err != nil {
		return nil, []ClusterFanoutStatus{{Name: cluster, OK: false, Error: "cluster unreachable"}}, fmt.Errorf("list remote %s: %w", cluster, err)
	}
	var resp bus.ControllersListResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, []ClusterFanoutStatus{{Name: cluster, OK: false, Error: "invalid response"}}, fmt.Errorf("list remote %s decode: %w", cluster, err)
	}
	if resp.Error != "" {
		return nil, []ClusterFanoutStatus{{Name: cluster, OK: false, Error: resp.Error}}, fmt.Errorf("list remote %s error: %s", cluster, resp.Error)
	}
	cc := make([]ClusterController, len(resp.Items))
	for i, raw := range resp.Items {
		var cr v1alpha1.Controller
		if err := json.Unmarshal(raw, &cr); err != nil {
			f.logger.Warn("list remote unmarshal skip", "cluster", cluster, "error", err)
			continue
		}
		cc[i] = ClusterController{Cluster: cluster, CR: &cr}
	}
	return cc, []ClusterFanoutStatus{{Name: cluster, OK: true}}, nil
}

func (f *busBrood) Get(ctx context.Context, cluster, ns, name string) (*v1alpha1.Controller, error) {
	if cluster == f.localCluster {
		cr, err := crdstore.Get[v1alpha1.Controller](ctx, f.store, name, ns)
		if err != nil {
			return nil, f.wrapK8sErr(err)
		}
		return cr, nil
	}
	req := bus.ControllersGetRequest{Namespace: ns, Name: name}
	data, _ := json.Marshal(req)
	respData, err := f.request(bus.OperatorControllersSubject(cluster, "get"), data, broodGetTimeout)
	if err != nil {
		return nil, &ErrClusterUnreachable{Cluster: cluster, Err: err}
	}
	var resp bus.ControllersGetResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("get decode: %w", err)
	}
	if resp.Code != "" {
		return nil, &BroodError{Code: resp.Code, Msg: resp.Error}
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("get %s/%s/%s: %s", cluster, ns, name, resp.Error)
	}
	var cr v1alpha1.Controller
	if err := json.Unmarshal(resp.Item, &cr); err != nil {
		return nil, fmt.Errorf("get unmarshal: %w", err)
	}
	return &cr, nil
}

func (f *busBrood) Create(ctx context.Context, cluster string, req ControllersCreateArgs) (*v1alpha1.Controller, []bus.Check, error) {
	busReq := bus.ControllersCreateRequest{
		Namespace:  req.Namespace,
		Controller: req.Controller,
		Bundle:     req.Bundle,
		DryRun:     req.DryRun,
	}
	if req.DryRun {
		resp, err := f.requestTo("create", cluster, busReq, broodDryRunTimeout)
		if err != nil {
			return nil, nil, err
		}
		var createResp bus.ControllersCreateResponse
		if err := json.Unmarshal(resp, &createResp); err != nil {
			return nil, nil, fmt.Errorf("create decode: %w", err)
		}
		return nil, createResp.Checks, f.codeToErr(createResp.Code, createResp.Error)
	}
	resp, err := f.requestTo("create", cluster, busReq, broodCreateTimeout)
	if err != nil {
		return nil, nil, err
	}
	var createResp bus.ControllersCreateResponse
	if err := json.Unmarshal(resp, &createResp); err != nil {
		return nil, nil, fmt.Errorf("create decode: %w", err)
	}
	if createResp.Code != "" {
		return nil, createResp.Checks, &BroodError{Code: createResp.Code, Msg: createResp.Error}
	}
	if createResp.Error != "" {
		return nil, nil, fmt.Errorf("create: %s", createResp.Error)
	}
	var cr v1alpha1.Controller
	if err := json.Unmarshal(createResp.Item, &cr); err != nil {
		return nil, nil, fmt.Errorf("create unmarshal: %w", err)
	}
	return &cr, nil, nil
}

func (f *busBrood) Preflight(ctx context.Context, cluster string, req ControllersCreateArgs) ([]bus.Check, error) {
	req.DryRun = true
	_, checks, err := f.Create(ctx, cluster, req)
	return checks, err
}

func (f *busBrood) Update(ctx context.Context, cluster, ns, name string, patch json.RawMessage, fieldManager string, force bool) (*v1alpha1.Controller, []bus.Check, []bus.UnappliedRemoval, error) {
	busReq := bus.ControllersUpdateRequest{Namespace: ns, Name: name, Patch: patch, FieldManager: fieldManager, Force: force}
	resp, err := f.requestTo("update", cluster, busReq, broodUpdateTimeout)
	if err != nil {
		return nil, nil, nil, err
	}
	var updateResp bus.ControllersUpdateResponse
	if err := json.Unmarshal(resp, &updateResp); err != nil {
		return nil, nil, nil, fmt.Errorf("update decode: %w", err)
	}
	if updateResp.Code != "" {
		return nil, updateResp.Checks, nil, &BroodError{Code: updateResp.Code, Msg: updateResp.Error, Conflicts: updateResp.Conflicts}
	}
	if updateResp.Error != "" {
		return nil, nil, nil, fmt.Errorf("update: %s", updateResp.Error)
	}
	var cr v1alpha1.Controller
	if err := json.Unmarshal(updateResp.Item, &cr); err != nil {
		return nil, nil, nil, fmt.Errorf("update unmarshal: %w", err)
	}
	return &cr, nil, updateResp.UnappliedRemovals, nil
}

func (f *busBrood) Delete(ctx context.Context, cluster, ns, name string) error {
	busReq := bus.ControllersDeleteRequest{Namespace: ns, Name: name}
	resp, err := f.requestTo("delete", cluster, busReq, broodDeleteTimeout)
	if err != nil {
		return err
	}
	var delResp bus.ControllersDeleteResponse
	if err := json.Unmarshal(resp, &delResp); err != nil {
		return fmt.Errorf("delete decode: %w", err)
	}
	return f.codeToErr(delResp.Code, delResp.Error)
}

func (f *busBrood) DeletePod(ctx context.Context, cluster, ns, name string) error {
	busReq := bus.ControllersDeletePodRequest{Namespace: ns, Name: name}
	resp, err := f.requestTo("deletepod", cluster, busReq, broodDeletePodTimeout)
	if err != nil {
		return err
	}
	var delResp bus.ControllersDeleteResponse
	if err := json.Unmarshal(resp, &delResp); err != nil {
		return fmt.Errorf("deletepod decode: %w", err)
	}
	return f.codeToErr(delResp.Code, delResp.Error)
}

func (f *busBrood) Drain(ctx context.Context, cluster, requestedBy string) (string, error) {
	req := bus.ClusterDrainRequest{RequestedBy: requestedBy}
	data, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal drain request: %w", err)
	}
	subject := bus.OperatorClusterSubject(cluster, "drain")
	respData, err := f.request(subject, data, broodDrainTimeout)
	if err != nil {
		return "", &ErrClusterUnreachable{Cluster: cluster, Err: err}
	}
	var resp bus.ClusterDrainResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return "", fmt.Errorf("drain decode: %w", err)
	}
	if resp.Code != "" {
		return "", &BroodError{Code: resp.Code, Msg: resp.Error}
	}
	return resp.State, nil
}

func (f *busBrood) DrainCancel(ctx context.Context, cluster, requestedBy string) (string, error) {
	req := bus.ClusterDrainCancelRequest{RequestedBy: requestedBy}
	data, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal draincancel request: %w", err)
	}
	subject := bus.OperatorClusterSubject(cluster, "draincancel")
	respData, err := f.request(subject, data, broodDrainTimeout)
	if err != nil {
		return "", &ErrClusterUnreachable{Cluster: cluster, Err: err}
	}
	var resp bus.ClusterDrainCancelResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return "", fmt.Errorf("draincancel decode: %w", err)
	}
	if resp.Code != "" {
		return "", &BroodError{Code: resp.Code, Msg: resp.Error}
	}
	return resp.State, nil
}

func (f *busBrood) StateOf(ctx context.Context, cluster string) (string, error) {
	if cluster == f.localCluster {
		return bus.ClusterStateActive, nil
	}
	members, err := f.getMembers()
	if err != nil {
		return "", err
	}
	for _, m := range members {
		if m.Name == cluster {
			if m.State == "" {
				return bus.ClusterStateActive, nil
			}
			return m.State, nil
		}
	}
	return "", &BroodError{Code: bus.CodeNotFound, Msg: "unknown cluster: " + cluster}
}

func (f *busBrood) DiscoverNamespaces(ctx context.Context, cluster string) (*bus.NamespacesListResponse, error) {
	if cluster == f.localCluster {
		return nil, fmt.Errorf("DiscoverNamespaces called for local cluster %q", cluster)
	}
	data, _ := json.Marshal(bus.NamespacesListRequest{})
	respData, err := f.request(bus.OperatorNamespacesSubject(cluster), data, broodNamespacesTimeout)
	if err != nil {
		return nil, &ErrClusterUnreachable{Cluster: cluster, Err: err}
	}
	var resp bus.NamespacesListResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("namespaces discover decode: %w", err)
	}
	if resp.Error != "" {
		return nil, &BroodError{Code: resp.Code, Msg: resp.Error}
	}
	return &resp, nil
}

// requestTo sends a request to the given cluster and returns the raw response bytes.
func (f *busBrood) requestTo(verb, cluster string, req any, timeout time.Duration) ([]byte, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal %s request: %w", verb, err)
	}
	subject := bus.OperatorControllersSubject(cluster, verb)
	resp, err := f.request(subject, data, timeout)
	if err != nil {
		return nil, &ErrClusterUnreachable{Cluster: cluster, Err: err}
	}
	return resp, nil
}

func (f *busBrood) wrapK8sErr(err error) error {
	if err != nil {
		return &BroodError{Code: "internal", Msg: err.Error()}
	}
	return nil
}

func (f *busBrood) codeToErr(code, msg string) error {
	if code == "" {
		return nil
	}
	return &BroodError{Code: code, Msg: msg}
}
