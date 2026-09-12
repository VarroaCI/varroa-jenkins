package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Default values for the seven Team.spec.pipelines ledger and rehydration
// settings. The apiserver's CRD schema default applies these on create; the
// Effective* methods below apply the same values to Go code that builds or
// reads a Team without going through the apiserver, such as a test or an
// in-memory fake.
const (
	// DefaultRehydrateBuilds is spec.pipelines.rehydrateBuilds's default.
	DefaultRehydrateBuilds int32 = 10
	// DefaultInterruptedRetry is spec.pipelines.interruptedRetry's default.
	DefaultInterruptedRetry = "manual"
	// DefaultConcurrency is spec.pipelines.concurrency's default.
	DefaultConcurrency = "allow"
	// DefaultCronCatchUp is spec.pipelines.cronCatchUp's default.
	DefaultCronCatchUp = "one"
)

// DefaultRehydrateDeadline is spec.pipelines.rehydrateDeadline's default.
func DefaultRehydrateDeadline() metav1.Duration { return metav1.Duration{Duration: 30 * time.Minute} }

// DefaultCronGrace is spec.pipelines.cronGrace's default.
func DefaultCronGrace() metav1.Duration { return metav1.Duration{Duration: 5 * time.Minute} }

// EffectiveRehydrateBuilds resolves spec.pipelines.rehydrateBuilds. A nil
// spec or a nil field means the default.
func (p *TeamPipelinesSpec) EffectiveRehydrateBuilds() int32 {
	if p == nil || p.RehydrateBuilds == nil {
		return DefaultRehydrateBuilds
	}
	return *p.RehydrateBuilds
}

// EffectiveRehydrateDeadline resolves spec.pipelines.rehydrateDeadline. A
// nil spec or a nil field means the default.
func (p *TeamPipelinesSpec) EffectiveRehydrateDeadline() time.Duration {
	if p == nil || p.RehydrateDeadline == nil {
		return DefaultRehydrateDeadline().Duration
	}
	return p.RehydrateDeadline.Duration
}

// EffectiveInterruptedRetry resolves spec.pipelines.interruptedRetry. A nil
// spec or an empty field means the default.
func (p *TeamPipelinesSpec) EffectiveInterruptedRetry() string {
	if p == nil || p.InterruptedRetry == "" {
		return DefaultInterruptedRetry
	}
	return p.InterruptedRetry
}

// EffectiveConcurrency resolves spec.pipelines.concurrency. A nil spec or an
// empty field means the default.
func (p *TeamPipelinesSpec) EffectiveConcurrency() string {
	if p == nil || p.Concurrency == "" {
		return DefaultConcurrency
	}
	return p.Concurrency
}

// EffectiveCronBackstop resolves spec.pipelines.cronBackstop. A nil spec or
// a nil field means on.
func (p *TeamPipelinesSpec) EffectiveCronBackstop() bool {
	if p == nil || p.CronBackstop == nil {
		return true
	}
	return *p.CronBackstop
}

// EffectiveCronCatchUp resolves spec.pipelines.cronCatchUp. A nil spec or an
// empty field means the default.
func (p *TeamPipelinesSpec) EffectiveCronCatchUp() string {
	if p == nil || p.CronCatchUp == "" {
		return DefaultCronCatchUp
	}
	return p.CronCatchUp
}

// EffectiveCronGrace resolves spec.pipelines.cronGrace. A nil spec or a nil
// field means the default.
func (p *TeamPipelinesSpec) EffectiveCronGrace() time.Duration {
	if p == nil || p.CronGrace == nil {
		return DefaultCronGrace().Duration
	}
	return p.CronGrace.Duration
}
