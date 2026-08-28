package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// BroodSchedules is the abstract interface for brood schedule commands
// across clusters. Single-target operations (Create/Get/Delete/Suspend) address
// exactly one resident cluster (default local); only List fans out, aggregating
// schedules from every known cluster.
type BroodSchedules interface {
	Create(ctx context.Context, cluster, ns, name string, spec v1alpha1.BroodScheduleSpec) (*bus.BroodScheduleResponse, error)
	Get(ctx context.Context, cluster, ns, name string) (*bus.BroodScheduleResponse, error)
	List(ctx context.Context, ns string) ([]bus.BroodScheduleResponse, error)
	Delete(ctx context.Context, cluster, ns, name string) error
	Suspend(ctx context.Context, cluster, ns, name string, suspend bool) error
}

// Timeout constants for broodschedules bus operations.
const (
	broodSchedulesCreateTimeout  = 10 * time.Second
	broodSchedulesGetTimeout     = 3 * time.Second
	broodSchedulesListTimeout    = 3 * time.Second
	broodSchedulesDeleteTimeout  = 10 * time.Second
	broodSchedulesSuspendTimeout = 10 * time.Second
)

// busBroodSchedules implements BroodSchedules over the NATS bus.
type busBroodSchedules struct {
	localCluster string
	conn         *bus.Conn
	membership   *bus.ClusterDirectory
	logger       *slog.Logger

	// request is the injectable NATS request function; defaults to conn.Request.
	request func(subject string, data []byte, timeout time.Duration) ([]byte, error)
}

// NewBusBroodSchedules creates a new bus-backed BroodSchedules.
func NewBusBroodSchedules(localCluster string, conn *bus.Conn, membership *bus.ClusterDirectory, logger *slog.Logger) BroodSchedules {
	return &busBroodSchedules{
		localCluster: localCluster,
		conn:         conn,
		membership:   membership,
		logger:       logger.With("component", "bus_broodschedules"),
		request: func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
			resp, err := conn.Request(subject, data, timeout)
			if err != nil {
				return nil, err
			}
			return resp.Data, nil
		},
	}
}

// targetCluster returns the effective cluster name, defaulting to local.
func (f *busBroodSchedules) targetCluster(cluster string) string {
	if cluster == "" {
		return f.localCluster
	}
	return cluster
}

// Create sends a create request to the given cluster.
func (f *busBroodSchedules) Create(ctx context.Context, cluster, ns, name string, spec v1alpha1.BroodScheduleSpec) (*bus.BroodScheduleResponse, error) {
	specRaw, _ := json.Marshal(spec)
	req := bus.BroodScheduleCreateRequest{
		Namespace: ns,
		Name:      name,
		Spec:      specRaw,
	}
	data, _ := json.Marshal(req)
	target := f.targetCluster(cluster)
	respData, err := f.request(bus.OperatorBroodSchedulesSubject(target, "create"), data, broodSchedulesCreateTimeout)
	if err != nil {
		return nil, fmt.Errorf("cluster %s unreachable: %w", target, err)
	}
	var reply bus.BroodScheduleResponse
	if err := json.Unmarshal(respData, &reply); err != nil {
		return nil, fmt.Errorf("invalid response: %w", err)
	}
	if reply.Error != "" {
		return &reply, fmt.Errorf("create error: %s", reply.Error)
	}
	reply.Cluster = target
	return &reply, nil
}

// Get sends a get request to the given cluster.
func (f *busBroodSchedules) Get(ctx context.Context, cluster, ns, name string) (*bus.BroodScheduleResponse, error) {
	req := bus.BroodScheduleGetRequest{Namespace: ns, Name: name}
	data, _ := json.Marshal(req)
	target := f.targetCluster(cluster)
	respData, err := f.request(bus.OperatorBroodSchedulesSubject(target, "get"), data, broodSchedulesGetTimeout)
	if err != nil {
		return nil, fmt.Errorf("cluster %s unreachable: %w", target, err)
	}
	var reply bus.BroodScheduleResponse
	if err := json.Unmarshal(respData, &reply); err != nil {
		return nil, fmt.Errorf("invalid response: %w", err)
	}
	if reply.Error != "" {
		return &reply, fmt.Errorf("get error: %s", reply.Error)
	}
	reply.Cluster = target
	return &reply, nil
}

// List gathers schedules from all known clusters.
func (f *busBroodSchedules) List(ctx context.Context, ns string) ([]bus.BroodScheduleResponse, error) {
	var all []bus.BroodScheduleResponse

	// Local cluster.
	localItems, err := f.listCluster(f.localCluster, ns)
	if err != nil {
		f.logger.Warn("broodschedules list local failed", "error", err)
	} else {
		all = append(all, localItems...)
	}

	// Remote members.
	var members []bus.ClusterInfo
	if f.membership != nil {
		var err error
		members, err = f.membership.List()
		if err != nil {
			f.logger.Warn("list members for broodschedules fan-out failed", "error", err)
		}
	}
	for _, m := range members {
		if m.Name == f.localCluster {
			continue
		}
		items, err := f.listCluster(m.Name, ns)
		if err != nil {
			f.logger.Warn("broodschedules list remote failed", "cluster", m.Name, "error", err)
			continue
		}
		all = append(all, items...)
	}

	return all, nil
}

func (f *busBroodSchedules) listCluster(cluster, ns string) ([]bus.BroodScheduleResponse, error) {
	req := bus.BroodScheduleListRequest{Namespace: ns}
	data, _ := json.Marshal(req)
	respData, err := f.request(bus.OperatorBroodSchedulesSubject(cluster, "list"), data, broodSchedulesListTimeout)
	if err != nil {
		return nil, fmt.Errorf("cluster %s unreachable: %w", cluster, err)
	}
	var resp bus.BroodScheduleListResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("invalid response: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("list error: %s", resp.Error)
	}
	for i := range resp.Items {
		resp.Items[i].Cluster = cluster
	}
	return resp.Items, nil
}

// Delete sends a delete request to the given cluster.
func (f *busBroodSchedules) Delete(ctx context.Context, cluster, ns, name string) error {
	req := bus.BroodScheduleDeleteRequest{Namespace: ns, Name: name}
	data, _ := json.Marshal(req)
	target := f.targetCluster(cluster)
	respData, err := f.request(bus.OperatorBroodSchedulesSubject(target, "delete"), data, broodSchedulesDeleteTimeout)
	if err != nil {
		return fmt.Errorf("cluster %s unreachable: %w", target, err)
	}
	var reply bus.BroodScheduleResponse
	if err := json.Unmarshal(respData, &reply); err != nil {
		return fmt.Errorf("invalid response: %w", err)
	}
	if reply.Error != "" {
		return fmt.Errorf("delete error: %s", reply.Error)
	}
	return nil
}

// Suspend sends a suspend/resume request to the given cluster.
func (f *busBroodSchedules) Suspend(ctx context.Context, cluster, ns, name string, suspend bool) error {
	req := bus.BroodScheduleSuspendRequest{Namespace: ns, Name: name, Suspend: suspend}
	data, _ := json.Marshal(req)
	target := f.targetCluster(cluster)
	respData, err := f.request(bus.OperatorBroodSchedulesSubject(target, "suspend"), data, broodSchedulesSuspendTimeout)
	if err != nil {
		return fmt.Errorf("cluster %s unreachable: %w", target, err)
	}
	var reply bus.BroodScheduleResponse
	if err := json.Unmarshal(respData, &reply); err != nil {
		return fmt.Errorf("invalid response: %w", err)
	}
	if reply.Error != "" {
		return fmt.Errorf("suspend error: %s", reply.Error)
	}
	return nil
}
