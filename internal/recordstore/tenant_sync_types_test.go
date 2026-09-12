package recordstore

import (
	"encoding/json"
	"testing"
	"time"
)

// TestTenantSyncRequestRoundTrip asserts a TenantSyncRequest of kind
// "announce" survives a marshal/unmarshal round trip with every field
// set, including a dead instance entry.
func TestTenantSyncRequestRoundTrip(t *testing.T) {
	aliveAt := time.Now().UTC().Truncate(time.Second)
	lastSeenAt := aliveAt.Add(-time.Minute)
	deadAt := aliveAt.Add(-time.Hour)

	want := TenantSyncRequest{
		Kind:               TenantSyncKindAnnounce,
		Cluster:            "core",
		Tenant:             "acme",
		Namespace:          "jenkins",
		Cell:               "cell-a",
		Generation:         5,
		CellInstance:       "pod-a",
		Policy:             json.RawMessage(`{"rehydrateBuilds":10}`),
		RehydratedInstance: "pod-a",
		PendingJobs:        []string{"acme/app"},
		CellAlive:          true,
		CellAliveAt:        &aliveAt,
		CellLastSeenAt:     &lastSeenAt,
		DeadInstances: []TenantSyncDeadInstance{
			{Instance: "pod-b", LastSeenAt: deadAt},
		},
	}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got TenantSyncRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Kind != want.Kind || got.Cluster != want.Cluster || got.Tenant != want.Tenant ||
		got.Namespace != want.Namespace || got.Cell != want.Cell || got.Generation != want.Generation ||
		got.CellInstance != want.CellInstance || got.RehydratedInstance != want.RehydratedInstance ||
		got.CellAlive != want.CellAlive {
		t.Fatalf("round trip mismatch: want %+v, got %+v", want, got)
	}
	if string(got.Policy) != string(want.Policy) {
		t.Fatalf("policy round trip: want %s, got %s", want.Policy, got.Policy)
	}
	if len(got.PendingJobs) != 1 || got.PendingJobs[0] != "acme/app" {
		t.Fatalf("pending_jobs round trip: got %v", got.PendingJobs)
	}
	if got.CellAliveAt == nil || !got.CellAliveAt.Equal(aliveAt) {
		t.Fatalf("cell_alive_at round trip: got %v", got.CellAliveAt)
	}
	if got.CellLastSeenAt == nil || !got.CellLastSeenAt.Equal(lastSeenAt) {
		t.Fatalf("cell_last_seen_at round trip: got %v", got.CellLastSeenAt)
	}
	if len(got.DeadInstances) != 1 || got.DeadInstances[0].Instance != "pod-b" ||
		!got.DeadInstances[0].LastSeenAt.Equal(deadAt) {
		t.Fatalf("dead_instances round trip: got %v", got.DeadInstances)
	}
}

// TestTenantSyncRecordsRequestRoundTrip asserts a TenantSyncRequest of
// kind "records" survives a marshal/unmarshal round trip.
func TestTenantSyncRecordsRequestRoundTrip(t *testing.T) {
	want := TenantSyncRequest{
		Kind:     TenantSyncKindRecords,
		Cluster:  "core",
		Tenant:   "acme",
		JobPaths: []string{"acme/app", "acme/multi/main"},
		PerJob:   10,
	}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got TenantSyncRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Kind != want.Kind || got.Cluster != want.Cluster || got.Tenant != want.Tenant || got.PerJob != want.PerJob {
		t.Fatalf("round trip mismatch: want %+v, got %+v", want, got)
	}
	if len(got.JobPaths) != 2 || got.JobPaths[0] != "acme/app" || got.JobPaths[1] != "acme/multi/main" {
		t.Fatalf("job_paths round trip: got %v", got.JobPaths)
	}
}

// TestTenantSyncResponseRoundTrip asserts a TenantSyncResponse carrying
// a "records" reply survives a marshal/unmarshal round trip.
func TestTenantSyncResponseRoundTrip(t *testing.T) {
	startedAt := time.Now().UTC().Truncate(time.Second)

	want := TenantSyncResponse{
		Jobs: []TenantSyncJobRecords{
			{
				JobPath:         "acme/app",
				NextBuildNumber: 5,
				Records: []TenantSyncRecord{
					{
						BuildNumber:          4,
						StartedAt:            &startedAt,
						Result:               "SUCCESS",
						CauseDescription:     "Started by user tester",
						Params:               json.RawMessage(`{"BRANCH":"main"}`),
						MaskedParameterNames: []string{"TOKEN"},
						Changes:              json.RawMessage(`[{"commitId":"a1"}]`),
					},
				},
			},
		},
	}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got TenantSyncResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Jobs) != 1 || got.Jobs[0].JobPath != "acme/app" || got.Jobs[0].NextBuildNumber != 5 {
		t.Fatalf("jobs round trip: got %+v", got.Jobs)
	}
	if len(got.Jobs[0].Records) != 1 {
		t.Fatalf("records round trip: got %+v", got.Jobs[0].Records)
	}
	rec := got.Jobs[0].Records[0]
	if rec.BuildNumber != 4 || rec.Result != "SUCCESS" || rec.CauseDescription != "Started by user tester" {
		t.Fatalf("record round trip mismatch: got %+v", rec)
	}
	if rec.StartedAt == nil || !rec.StartedAt.Equal(startedAt) {
		t.Fatalf("started_at round trip: got %v", rec.StartedAt)
	}
	if len(rec.MaskedParameterNames) != 1 || rec.MaskedParameterNames[0] != "TOKEN" {
		t.Fatalf("masked_parameter_names round trip: got %v", rec.MaskedParameterNames)
	}
}
