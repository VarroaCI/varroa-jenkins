package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// evalPipelinesRehydrateDeadlineRange mirrors:
//
//	!has(self.pipelines) || !has(self.pipelines.rehydrateDeadline) ||
//	  (duration(self.pipelines.rehydrateDeadline) >= duration('1m') &&
//	   duration(self.pipelines.rehydrateDeadline) <= duration('24h'))
func evalPipelinesRehydrateDeadlineRange(spec *TeamSpec) bool {
	if spec.Pipelines == nil || spec.Pipelines.RehydrateDeadline == nil {
		return true
	}
	d := spec.Pipelines.RehydrateDeadline.Duration
	return d >= time.Minute && d <= 24*time.Hour
}

// evalPipelinesCronGraceRange mirrors:
//
//	!has(self.pipelines) || !has(self.pipelines.cronGrace) ||
//	  (duration(self.pipelines.cronGrace) >= duration('1m') &&
//	   duration(self.pipelines.cronGrace) <= duration('60m'))
func evalPipelinesCronGraceRange(spec *TeamSpec) bool {
	if spec.Pipelines == nil || spec.Pipelines.CronGrace == nil {
		return true
	}
	d := spec.Pipelines.CronGrace.Duration
	return d >= time.Minute && d <= 60*time.Minute
}

func TestTeamPipelinesRehydrationDeadlineRangeCEL(t *testing.T) {
	tests := []struct {
		name         string
		spec         *TeamSpec
		wantAccepted bool
	}{
		{
			name:         "unset accepted",
			spec:         &TeamSpec{Pipelines: &TeamPipelinesSpec{}},
			wantAccepted: true,
		},
		{
			name: "below minimum refused",
			spec: &TeamSpec{Pipelines: &TeamPipelinesSpec{
				RehydrateDeadline: &metav1.Duration{Duration: 30 * time.Second},
			}},
			wantAccepted: false,
		},
		{
			name: "above maximum refused",
			spec: &TeamSpec{Pipelines: &TeamPipelinesSpec{
				RehydrateDeadline: &metav1.Duration{Duration: 25 * time.Hour},
			}},
			wantAccepted: false,
		},
		{
			name: "in range accepted",
			spec: &TeamSpec{Pipelines: &TeamPipelinesSpec{
				RehydrateDeadline: &metav1.Duration{Duration: 30 * time.Minute},
			}},
			wantAccepted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := evalPipelinesRehydrateDeadlineRange(tt.spec); got != tt.wantAccepted {
				t.Errorf("evalPipelinesRehydrateDeadlineRange() = %v, want %v", got, tt.wantAccepted)
			}
		})
	}
}

func TestTeamPipelinesRehydrationCronGraceRangeCEL(t *testing.T) {
	tests := []struct {
		name         string
		spec         *TeamSpec
		wantAccepted bool
	}{
		{
			name:         "unset accepted",
			spec:         &TeamSpec{Pipelines: &TeamPipelinesSpec{}},
			wantAccepted: true,
		},
		{
			name: "below minimum refused",
			spec: &TeamSpec{Pipelines: &TeamPipelinesSpec{
				CronGrace: &metav1.Duration{Duration: 30 * time.Second},
			}},
			wantAccepted: false,
		},
		{
			name: "above maximum refused",
			spec: &TeamSpec{Pipelines: &TeamPipelinesSpec{
				CronGrace: &metav1.Duration{Duration: 61 * time.Minute},
			}},
			wantAccepted: false,
		},
		{
			name: "in range accepted",
			spec: &TeamSpec{Pipelines: &TeamPipelinesSpec{
				CronGrace: &metav1.Duration{Duration: 5 * time.Minute},
			}},
			wantAccepted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := evalPipelinesCronGraceRange(tt.spec); got != tt.wantAccepted {
				t.Errorf("evalPipelinesCronGraceRange() = %v, want %v", got, tt.wantAccepted)
			}
		})
	}
}

// TestTeamPipelinesRehydrationEffectiveDefaults asserts that a tenant
// setting none of the seven ledger and rehydration fields resolves every
// default through the Effective* accessors — the same defaults the CRD
// schema applies at the apiserver.
func TestTeamPipelinesRehydrationEffectiveDefaults(t *testing.T) {
	var p *TeamPipelinesSpec // nil spec: a Team with no spec.pipelines at all
	if got := p.EffectiveRehydrateBuilds(); got != DefaultRehydrateBuilds {
		t.Errorf("EffectiveRehydrateBuilds() = %v, want %v", got, DefaultRehydrateBuilds)
	}
	if got := p.EffectiveRehydrateDeadline(); got != DefaultRehydrateDeadline().Duration {
		t.Errorf("EffectiveRehydrateDeadline() = %v, want %v", got, DefaultRehydrateDeadline().Duration)
	}
	if got := p.EffectiveInterruptedRetry(); got != DefaultInterruptedRetry {
		t.Errorf("EffectiveInterruptedRetry() = %v, want %v", got, DefaultInterruptedRetry)
	}
	if got := p.EffectiveConcurrency(); got != DefaultConcurrency {
		t.Errorf("EffectiveConcurrency() = %v, want %v", got, DefaultConcurrency)
	}
	if got := p.EffectiveCronBackstop(); got != true {
		t.Errorf("EffectiveCronBackstop() = %v, want true", got)
	}
	if got := p.EffectiveCronCatchUp(); got != DefaultCronCatchUp {
		t.Errorf("EffectiveCronCatchUp() = %v, want %v", got, DefaultCronCatchUp)
	}
	if got := p.EffectiveCronGrace(); got != DefaultCronGrace().Duration {
		t.Errorf("EffectiveCronGrace() = %v, want %v", got, DefaultCronGrace().Duration)
	}

	// A pipelines tenant setting none of the seven fields (a non-nil spec
	// with every field at its zero value) gets the same defaults.
	empty := &TeamPipelinesSpec{Profile: "lts", AgentNamespace: "payments"}
	if got := empty.EffectiveRehydrateBuilds(); got != DefaultRehydrateBuilds {
		t.Errorf("EffectiveRehydrateBuilds() = %v, want %v", got, DefaultRehydrateBuilds)
	}
	if got := empty.EffectiveRehydrateDeadline(); got != DefaultRehydrateDeadline().Duration {
		t.Errorf("EffectiveRehydrateDeadline() = %v, want %v", got, DefaultRehydrateDeadline().Duration)
	}
	if got := empty.EffectiveInterruptedRetry(); got != DefaultInterruptedRetry {
		t.Errorf("EffectiveInterruptedRetry() = %v, want %v", got, DefaultInterruptedRetry)
	}
	if got := empty.EffectiveConcurrency(); got != DefaultConcurrency {
		t.Errorf("EffectiveConcurrency() = %v, want %v", got, DefaultConcurrency)
	}
	if got := empty.EffectiveCronBackstop(); got != true {
		t.Errorf("EffectiveCronBackstop() = %v, want true", got)
	}
	if got := empty.EffectiveCronCatchUp(); got != DefaultCronCatchUp {
		t.Errorf("EffectiveCronCatchUp() = %v, want %v", got, DefaultCronCatchUp)
	}
	if got := empty.EffectiveCronGrace(); got != DefaultCronGrace().Duration {
		t.Errorf("EffectiveCronGrace() = %v, want %v", got, DefaultCronGrace().Duration)
	}
}
