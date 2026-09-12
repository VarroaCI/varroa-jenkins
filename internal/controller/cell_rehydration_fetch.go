package controller

import (
	"context"
	"fmt"

	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
)

// rehydrationPageSize is task 5.8's paging batch size: at most 32 job
// paths per "records" request once the tenant's full job-path set is
// known.
const rehydrationPageSize = 32

// fetchTenantJobRecords implements task 5.8: it enumerates first — one
// "records" request with an empty JobPaths, which design D6 defines as
// "every job path the tenant holds a job_numbers row or a completed run
// for" — asking for no records, only the path list. It then pages those
// paths in batches of at most rehydrationPageSize, each carrying perJob
// (Team.spec.pipelines.rehydrateBuilds), and returns every job's answer
// from the paging replies combined.
//
// A tenant the enumeration answers empty (no job path at all) answers
// nil with no error: task 6.0a's caller treats that as an empty
// selection, not a fetch failure.
func fetchTenantJobRecords(ctx context.Context, recordsFunc cells.RecordsFunc, cluster, tenant string, perJob int) ([]cells.JobRecords, error) {
	if recordsFunc == nil {
		return nil, fmt.Errorf("cell rehydration: no records requester configured")
	}

	enumeration, err := recordsFunc(ctx, cells.RecordsRequest{Cluster: cluster, Tenant: tenant, PerJob: 0})
	if err != nil {
		return nil, fmt.Errorf("enumerate tenant job paths: %w", err)
	}
	if len(enumeration.Jobs) == 0 {
		return nil, nil
	}

	paths := make([]string, len(enumeration.Jobs))
	for i, j := range enumeration.Jobs {
		paths[i] = j.JobPath
	}

	var all []cells.JobRecords
	for start := 0; start < len(paths); start += rehydrationPageSize {
		end := start + rehydrationPageSize
		if end > len(paths) {
			end = len(paths)
		}
		page, err := recordsFunc(ctx, cells.RecordsRequest{
			Cluster: cluster, Tenant: tenant, JobPaths: paths[start:end], PerJob: perJob,
		})
		if err != nil {
			return nil, fmt.Errorf("page tenant job records: %w", err)
		}
		all = append(all, page.Jobs...)
	}
	return all, nil
}
