package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TenantRetention answers the fake's policy for one cluster and tenant.
func (f *fakeRecordReader) TenantRetention(_ context.Context, cluster, tenant string) (recordstore.TenantRetentionPolicy, error) {
	return f.retention[cluster+"/"+tenant], nil
}

func TestHandleListPipelines_ReportsRetention(t *testing.T) {
	client := newSingleControllerFixture(t)
	authz := pipelineGrantAuthorizer([]string{"read"}, "test-ns/ctrl-a")
	reader := newFakeRecordReader()
	reader.retention["core/test-ns/ctrl-a"] = recordstore.TenantRetentionPolicy{
		Runs: 30 * day,
		Jobs: []recordstore.JobRetentionRule{
			{Job: "pr-*", Runs: 7 * day},
			{Job: "release/**", Runs: 400 * day},
		},
	}

	srv := pipelinesTestServer(client, authz, reader, nil)
	srv.deps.RecordStoreRunRetention = 365 * day
	rec := doPipelinesRequest(t, srv.handleListPipelines, "/pipelines/ctrl-a", map[string]string{"tenant": "ctrl-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp pipelineListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r := resp.Retention
	if r.StoreDays != 365 || r.TenantDays == nil || *r.TenantDays != 30 || r.EffectiveDays != 30 {
		t.Fatalf("retention = %+v, want store 365, tenant 30, effective 30", r)
	}
	want := []pipelineJobRetentionJSON{
		{Job: "pr-*", Days: 7, EffectiveDays: 7},
		{Job: "release/**", Days: 400, EffectiveDays: 365},
	}
	if !reflect.DeepEqual(r.Jobs, want) {
		t.Fatalf("job rules = %+v, want %+v", r.Jobs, want)
	}
}

func TestBuildPipelineRetention(t *testing.T) {
	tenant30 := 30
	tests := []struct {
		name   string
		store  time.Duration
		policy recordstore.TenantRetentionPolicy
		want   pipelineRetentionJSON
	}{
		{
			name:   "zero store window means the 365d default",
			store:  0,
			policy: recordstore.TenantRetentionPolicy{},
			want:   pipelineRetentionJSON{StoreDays: 365, EffectiveDays: 365, Jobs: []pipelineJobRetentionJSON{}},
		},
		{
			name:   "no tenant window gives the store window",
			store:  90 * day,
			policy: recordstore.TenantRetentionPolicy{},
			want:   pipelineRetentionJSON{StoreDays: 90, EffectiveDays: 90, Jobs: []pipelineJobRetentionJSON{}},
		},
		{
			name:   "tenant window longer than the store is capped",
			store:  90 * day,
			policy: recordstore.TenantRetentionPolicy{Runs: 180 * day},
			want:   pipelineRetentionJSON{StoreDays: 90, TenantDays: intPtr(180), EffectiveDays: 90, Jobs: []pipelineJobRetentionJSON{}},
		},
		{
			name:   "tenant window shorter than the store wins",
			store:  365 * day,
			policy: recordstore.TenantRetentionPolicy{Runs: 30 * day},
			want:   pipelineRetentionJSON{StoreDays: 365, TenantDays: &tenant30, EffectiveDays: 30, Jobs: []pipelineJobRetentionJSON{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildPipelineRetention(tt.store, tt.policy)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("buildPipelineRetention() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func intPtr(n int) *int { return &n }

// day is one day as a time.Duration.
const day = 24 * time.Hour
