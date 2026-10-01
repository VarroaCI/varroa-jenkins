package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// tenantSyncRequestTimeout bounds one tenant.sync request from the operator
// side, mirroring tenantPurgeRequestTimeout.
const tenantSyncRequestTimeout = 10 * time.Second

// syncPolicyWire is the wire shape of an "announce" request's policy field
// (design D6, D10): the seven Team.spec.pipelines settings, with the two
// duration fields written as Go duration strings so a later reader
// (task 9.6's backstop round) parses them the same way
// api/v1alpha1.TeamPipelinesSpec's own metav1.Duration fields marshal,
// plus the tenant's export grants under the key "exports". Exports reuses
// v1alpha1.TeamExport's own json tags ("job", "locks", "callers") from
// design D10's example, so the recorder decodes the same shape the CRD
// stores. An absent or nil Exports omits the key, matching "an absent
// list exports nothing".
type syncPolicyWire struct {
	RehydrateBuilds   int32                 `json:"rehydrateBuilds"`
	RehydrateDeadline string                `json:"rehydrateDeadline"`
	InterruptedRetry  string                `json:"interruptedRetry"`
	Concurrency       string                `json:"concurrency"`
	CronBackstop      bool                  `json:"cronBackstop"`
	CronCatchUp       string                `json:"cronCatchUp"`
	CronGrace         string                `json:"cronGrace"`
	Exports           []v1alpha1.TeamExport `json:"exports,omitempty"`
}

// marshalSyncPolicy encodes a cells.SyncPolicy for the wire. The zero
// SyncPolicy (a controller with no cell role) still encodes every scalar
// field, because the recorder's writeTenantPlacement stores whatever
// policy arrives verbatim; an empty announcement carries the zero-value
// policy and no exports key.
func marshalSyncPolicy(p cells.SyncPolicy) (json.RawMessage, error) {
	wire := syncPolicyWire{
		RehydrateBuilds:   p.RehydrateBuilds,
		RehydrateDeadline: p.RehydrateDeadline.String(),
		InterruptedRetry:  p.InterruptedRetry,
		Concurrency:       p.Concurrency,
		CronBackstop:      p.CronBackstop,
		CronCatchUp:       p.CronCatchUp,
		CronGrace:         p.CronGrace.String(),
		Exports:           p.Exports,
	}
	return json.Marshal(wire)
}

// deadInstancesToWire converts cells.DeadInstance to the recordstore wire
// type.
func deadInstancesToWire(in []cells.DeadInstance) []recordstore.TenantSyncDeadInstance {
	if len(in) == 0 {
		return nil
	}
	out := make([]recordstore.TenantSyncDeadInstance, 0, len(in))
	for _, d := range in {
		out = append(out, recordstore.TenantSyncDeadInstance{
			Instance:        d.Instance,
			LastSeenAt:      d.LastSeenAt,
			ConfirmedGoneAt: d.ConfirmedGoneAt,
		})
	}
	return out
}

// membersToWire maps the placement pass's member list onto the recorder's
// wire shape. The list must cross the wire whole: the recorder stores what
// the request carries, so a dropped list reads as an empty one and every
// claim then resolves the synthetic slot-0 member.
//
// A controller with no cell role announces no member, and the recorder
// resolves its row to that synthetic member by design.
func membersToWire(in []cells.AnnounceMember) []recordstore.TenantSyncMember {
	if len(in) == 0 {
		return nil
	}
	out := make([]recordstore.TenantSyncMember, 0, len(in))
	for _, m := range in {
		entry := recordstore.TenantSyncMember{
			Cell:               m.Cell,
			Namespace:          m.Namespace,
			Role:               m.Role,
			Instance:           m.Instance,
			RehydratedInstance: m.RehydratedInstance,
			PendingJobs:        m.PendingJobs,
			AppliedAt:          m.AppliedAt,
		}
		if m.Slot != nil {
			slot := *m.Slot
			entry.Slot = &slot
		}
		out = append(out, entry)
	}
	return out
}

// NewNATSSyncAnnouncer builds a cells.Announcer that sends a tenant.sync
// "announce" request over bus.TenantSyncSubject and decodes the recorder's
// reply (design D6, D7).
func NewNATSSyncAnnouncer(conn *bus.Conn) cells.Announcer {
	return func(ctx context.Context, req cells.AnnounceRequest) (cells.AnnounceResult, error) {
		if conn == nil {
			return cells.AnnounceResult{}, fmt.Errorf("no nats connection")
		}
		policy, err := marshalSyncPolicy(req.Policy)
		if err != nil {
			return cells.AnnounceResult{}, fmt.Errorf("encode sync policy: %w", err)
		}
		payload, err := json.Marshal(recordstore.TenantSyncRequest{
			Kind:               recordstore.TenantSyncKindAnnounce,
			Cluster:            req.Cluster,
			Tenant:             req.Tenant,
			Incarnation:        req.Incarnation,
			Namespace:          req.Namespace,
			Cell:               req.Cell,
			Generation:         req.Generation,
			CellInstance:       req.CellInstance,
			Policy:             policy,
			RehydratedInstance: req.RehydratedInstance,
			PendingJobs:        req.PendingJobs,
			CellAlive:          req.CellAlive,
			CellAliveAt:        req.CellAliveAt,
			CellLastSeenAt:     req.CellLastSeenAt,
			DeadInstances:      deadInstancesToWire(req.DeadInstances),
			Members:            membersToWire(req.Members),
			AgentNamespace:     req.AgentNamespace,
		})
		if err != nil {
			return cells.AnnounceResult{}, fmt.Errorf("marshal tenant sync announce request: %w", err)
		}
		msg, err := conn.RequestWithContext(ctx, bus.TenantSyncSubject, payload, tenantSyncRequestTimeout)
		if err != nil {
			return cells.AnnounceResult{}, err
		}
		var resp recordstore.TenantSyncResponse
		if err := json.Unmarshal(msg.Data, &resp); err != nil {
			return cells.AnnounceResult{}, fmt.Errorf("decode tenant sync announce response: %w", err)
		}
		if resp.Error != "" {
			return cells.AnnounceResult{}, fmt.Errorf("%s", resp.Error)
		}
		return cells.AnnounceResult{OK: resp.Ok, InterruptedMarked: resp.InterruptedMarked, Stale: resp.Stale}, nil
	}
}

// NewNATSSyncRecordsRequester builds a cells.RecordsFunc that sends a
// tenant.sync "records" request over bus.TenantSyncSubject and decodes the
// recorder's reply (design D6). The rehydration rendering pass (task 5.8)
// pages through it.
func NewNATSSyncRecordsRequester(conn *bus.Conn) cells.RecordsFunc {
	return func(ctx context.Context, req cells.RecordsRequest) (cells.RecordsResult, error) {
		if conn == nil {
			return cells.RecordsResult{}, fmt.Errorf("no nats connection")
		}
		payload, err := json.Marshal(recordstore.TenantSyncRequest{
			Kind:     recordstore.TenantSyncKindRecords,
			Cluster:  req.Cluster,
			Tenant:   req.Tenant,
			JobPaths: req.JobPaths,
			PerJob:   req.PerJob,
		})
		if err != nil {
			return cells.RecordsResult{}, fmt.Errorf("marshal tenant sync records request: %w", err)
		}
		msg, err := conn.RequestWithContext(ctx, bus.TenantSyncSubject, payload, tenantSyncRequestTimeout)
		if err != nil {
			return cells.RecordsResult{}, err
		}
		var resp recordstore.TenantSyncResponse
		if err := json.Unmarshal(msg.Data, &resp); err != nil {
			return cells.RecordsResult{}, fmt.Errorf("decode tenant sync records response: %w", err)
		}
		if resp.Error != "" {
			return cells.RecordsResult{}, fmt.Errorf("%s", resp.Error)
		}
		jobs := make([]cells.JobRecords, 0, len(resp.Jobs))
		for _, j := range resp.Jobs {
			records := make([]cells.BuildRecord, 0, len(j.Records))
			for _, r := range j.Records {
				records = append(records, cells.BuildRecord{
					BuildNumber:          r.BuildNumber,
					QueuedAt:             r.QueuedAt,
					StartedAt:            r.StartedAt,
					EndedAt:              r.EndedAt,
					DurationMs:           r.DurationMs,
					Result:               r.Result,
					CauseDescription:     r.CauseDescription,
					Params:               r.Params,
					MaskedParameterNames: r.MaskedParameterNames,
					Changes:              r.Changes,
				})
			}
			jobs = append(jobs, cells.JobRecords{JobPath: j.JobPath, NextBuildNumber: j.NextBuildNumber, Records: records})
		}
		return cells.RecordsResult{Jobs: jobs}, nil
	}
}

// NewNATSSyncHomesRequester builds a cells.HomesFunc that sends a
// tenant.sync "homes" request over bus.TenantSyncSubject and decodes the
// recorder's reply. The recorder stays the only owner of the home map,
// and this kind is how the cell desired state reads it.
func NewNATSSyncHomesRequester(conn *bus.Conn) cells.HomesFunc {
	return func(ctx context.Context, req cells.HomesRequest) (cells.HomesResult, error) {
		if conn == nil {
			return cells.HomesResult{}, fmt.Errorf("no nats connection")
		}
		payload, err := json.Marshal(recordstore.TenantSyncRequest{
			Kind:    recordstore.TenantSyncKindHomes,
			Cluster: req.Cluster,
			Tenant:  req.Tenant,
		})
		if err != nil {
			return cells.HomesResult{}, fmt.Errorf("marshal tenant sync homes request: %w", err)
		}
		msg, err := conn.RequestWithContext(ctx, bus.TenantSyncSubject, payload, tenantSyncRequestTimeout)
		if err != nil {
			return cells.HomesResult{}, err
		}
		var resp recordstore.TenantSyncResponse
		if err := json.Unmarshal(msg.Data, &resp); err != nil {
			return cells.HomesResult{}, fmt.Errorf("decode tenant sync homes response: %w", err)
		}
		if resp.Error != "" {
			return cells.HomesResult{}, fmt.Errorf("%s", resp.Error)
		}
		out := cells.HomesResult{JobHomes: resp.JobHomes}
		for _, m := range resp.Members {
			out.Members = append(out.Members, cells.HomesMember{
				Cell: m.Cell, Namespace: m.Namespace, Role: m.Role, Slot: m.Slot,
			})
		}
		return out, nil
	}
}
