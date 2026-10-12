package recordstore

import (
	"encoding/json"
	"testing"
	"time"
)

func day(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

func TestDecodeRetention(t *testing.T) {
	tests := []struct {
		name   string
		policy string
		want   TenantRetentionPolicy
	}{
		{name: "empty", policy: "", want: TenantRetentionPolicy{}},
		{name: "malformed", policy: `{"retention":`, want: TenantRetentionPolicy{}},
		{name: "no key", policy: `{"cronBackstop":true}`, want: TenantRetentionPolicy{}},
		{
			name:   "valid",
			policy: `{"retention":{"runs":"30d","jobs":[{"job":"pr-*","runs":"7d"}]}}`,
			want:   TenantRetentionPolicy{Runs: day(30), Jobs: []JobRetentionRule{{Job: "pr-*", Runs: day(7)}}},
		},
		{
			name:   "invalid tenant window leaves zero",
			policy: `{"retention":{"runs":"abc"}}`,
			want:   TenantRetentionPolicy{},
		},
		{
			name:   "invalid rule is dropped",
			policy: `{"retention":{"runs":"30d","jobs":[{"job":"a","runs":"0d"},{"job":"b","runs":"9d"}]}}`,
			want:   TenantRetentionPolicy{Runs: day(30), Jobs: []JobRetentionRule{{Job: "b", Runs: day(9)}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodeRetention(json.RawMessage(tt.policy))
			if got.Runs != tt.want.Runs || len(got.Jobs) != len(tt.want.Jobs) {
				t.Fatalf("decodeRetention() = %+v, want %+v", got, tt.want)
			}
			for i := range got.Jobs {
				if got.Jobs[i] != tt.want.Jobs[i] {
					t.Fatalf("job %d = %+v, want %+v", i, got.Jobs[i], tt.want.Jobs[i])
				}
			}
		})
	}
}

func TestEffectiveRunRetention(t *testing.T) {
	const tenant = "payments"
	store := day(365)
	tests := []struct {
		name    string
		policy  TenantRetentionPolicy
		jobPath string
		want    time.Duration
	}{
		{
			name:    "no policy gives the store window",
			policy:  TenantRetentionPolicy{},
			jobPath: "payments/build",
			want:    day(365),
		},
		{
			name:    "shorter tenant window wins",
			policy:  TenantRetentionPolicy{Runs: day(30)},
			jobPath: "payments/build",
			want:    day(30),
		},
		{
			name:    "longer tenant window is capped at the store window",
			policy:  TenantRetentionPolicy{Runs: day(900)},
			jobPath: "payments/build",
			want:    day(365),
		},
		{
			name:    "shorter job rule wins over the tenant window",
			policy:  TenantRetentionPolicy{Runs: day(30), Jobs: []JobRetentionRule{{Job: "pr-*", Runs: day(2)}}},
			jobPath: "payments/pr-42",
			want:    day(2),
		},
		{
			name:    "longer job rule replaces the tenant window, capped at the store",
			policy:  TenantRetentionPolicy{Runs: day(30), Jobs: []JobRetentionRule{{Job: "release/**", Runs: day(400)}}},
			jobPath: "payments/release/1.2",
			want:    day(365),
		},
		{
			name:    "longer job rule within the store window wins",
			policy:  TenantRetentionPolicy{Runs: day(30), Jobs: []JobRetentionRule{{Job: "release/**", Runs: day(200)}}},
			jobPath: "payments/release/1.2",
			want:    day(200),
		},
		{
			name: "first matching rule wins",
			policy: TenantRetentionPolicy{Jobs: []JobRetentionRule{
				{Job: "pr-*", Runs: day(3)},
				{Job: "*", Runs: day(9)},
			}},
			jobPath: "payments/pr-1",
			want:    day(3),
		},
		{
			name:    "unmatched job gives the tenant window",
			policy:  TenantRetentionPolicy{Runs: day(30), Jobs: []JobRetentionRule{{Job: "pr-*", Runs: day(2)}}},
			jobPath: "payments/deploy",
			want:    day(30),
		},
		{
			name:    "folder-qualified path is stripped before matching",
			policy:  TenantRetentionPolicy{Jobs: []JobRetentionRule{{Job: "pr-*", Runs: day(2)}}},
			jobPath: "payments/pr-7",
			want:    day(2),
		},
		{
			name:    "controller tenant with no prefix matches the raw path",
			policy:  TenantRetentionPolicy{Jobs: []JobRetentionRule{{Job: "pr-*", Runs: day(2)}}},
			jobPath: "pr-7",
			want:    day(2),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectiveRunRetention(store, tt.policy, tenant, tt.jobPath); got != tt.want {
				t.Fatalf("EffectiveRunRetention() = %v, want %v", got, tt.want)
			}
		})
	}
}
