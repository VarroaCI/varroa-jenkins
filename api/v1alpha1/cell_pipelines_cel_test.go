package v1alpha1

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The functions below mirror the literal XValidation rule text on
// ControllerSpec (the cell role), TeamSpec, and CellPoolSpec in types.go. Keep each function
// identical to its rule string. The apiserver evaluates the CEL text at
// admission time; this package has no CEL evaluator, so these functions
// are the only check that runs in `go test`. A live dry-run apply is the
// only check on the CEL text itself.

// evalCellRoleImmutable mirrors:
//
//	(has(self.cell) && has(self.cell.enabled) && self.cell.enabled) == (has(oldSelf.cell) && has(oldSelf.cell.enabled) && oldSelf.cell.enabled)
func evalCellRoleImmutable(oldSelf, self *ControllerSpec) bool {
	return cellRole(self) == cellRole(oldSelf)
}

// cellRole models the has()-guarded read of the cell role. A nil Cell models
// a missing has(self.cell); a false Enabled models a missing enabled key.
func cellRole(s *ControllerSpec) bool {
	return s.Cell != nil && s.Cell.Enabled
}

// evalPipelinesProfileNonEmpty mirrors:
//
//	!has(self.pipelines) || size(self.pipelines.profile) > 0
func evalPipelinesProfileNonEmpty(spec *TeamSpec) bool {
	return spec.Pipelines == nil || spec.Pipelines.Profile != ""
}

// evalPipelinesAgentNamespaceNonEmpty mirrors:
//
//	!has(self.pipelines) || size(self.pipelines.agentNamespace) > 0
func evalPipelinesAgentNamespaceNonEmpty(spec *TeamSpec) bool {
	return spec.Pipelines == nil || spec.Pipelines.AgentNamespace != ""
}

// evalPipelinesAgentNamespaceManaged mirrors:
//
//	!has(self.pipelines) || self.pipelines.agentNamespace in self.namespaces
func evalPipelinesAgentNamespaceManaged(spec *TeamSpec) bool {
	if spec.Pipelines == nil {
		return true
	}
	return contains(spec.Namespaces, spec.Pipelines.AgentNamespace)
}

// evalComposedBundleRefNamespaceNonEmpty mirrors:
//
//	!has(self.pipelines) || !has(self.pipelines.composedBundleRef) ||
//	  size(self.pipelines.composedBundleRef.namespace) > 0
func evalComposedBundleRefNamespaceNonEmpty(spec *TeamSpec) bool {
	if spec.Pipelines == nil || spec.Pipelines.ComposedBundleRef == nil {
		return true
	}
	return spec.Pipelines.ComposedBundleRef.Namespace != ""
}

// evalComposedBundleRefNamespaceManaged mirrors:
//
//	!has(self.pipelines) || !has(self.pipelines.composedBundleRef) ||
//	  self.pipelines.composedBundleRef.namespace in self.namespaces
func evalComposedBundleRefNamespaceManaged(spec *TeamSpec) bool {
	if spec.Pipelines == nil || spec.Pipelines.ComposedBundleRef == nil {
		return true
	}
	return contains(spec.Namespaces, spec.Pipelines.ComposedBundleRef.Namespace)
}

// evalPipelinesTenantNameFitsService mirrors:
//
//	!has(self.spec) || !has(self.spec.pipelines) ||
//	  (size(self.metadata.name) <= 47 && !self.metadata.name.contains('.'))
//
// The name of a pipelines tenant reaches a Service name as
// "varroa-agents-<team>-<slot>". A Service name is an RFC 1035 label: at most
// 63 characters and no dot. The prefix and the slot suffix leave 47.
func evalPipelinesTenantNameFitsService(team *Team) bool {
	if team.Spec.Pipelines == nil {
		return true
	}
	return len(team.Name) <= 47 && !strings.Contains(team.Name, ".")
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestCellRoleImmutableCEL(t *testing.T) {
	spec := func(c *CellSpec) *ControllerSpec { return &ControllerSpec{Cell: c} }
	tests := []struct {
		name         string
		oldSelf      *ControllerSpec
		self         *ControllerSpec
		wantAccepted bool
	}{
		{
			name:         "enabled flipped inside the block",
			oldSelf:      spec(&CellSpec{Enabled: false, MaxTenants: 3}),
			self:         spec(&CellSpec{Enabled: true, MaxTenants: 3}),
			wantAccepted: false,
		},
		{
			name:         "cell block added",
			oldSelf:      spec(nil),
			self:         spec(&CellSpec{Enabled: true}),
			wantAccepted: false,
		},
		{
			name:         "cell block removed",
			oldSelf:      spec(&CellSpec{Enabled: true}),
			self:         spec(nil),
			wantAccepted: false,
		},
		{
			name:         "maxTenants changed, enabled unchanged",
			oldSelf:      spec(&CellSpec{Enabled: true, MaxTenants: 3}),
			self:         spec(&CellSpec{Enabled: true, MaxTenants: 10}),
			wantAccepted: true,
		},
		{
			name:         "block without enabled, maxTenants changed",
			oldSelf:      spec(&CellSpec{MaxTenants: 3}),
			self:         spec(&CellSpec{MaxTenants: 4}),
			wantAccepted: true,
		},
		{
			name:         "block without enabled added",
			oldSelf:      spec(nil),
			self:         spec(&CellSpec{MaxTenants: 3}),
			wantAccepted: true,
		},
		{
			name:         "block without enabled removed",
			oldSelf:      spec(&CellSpec{MaxTenants: 3}),
			self:         spec(nil),
			wantAccepted: true,
		},
		{
			name:         "no block before or after",
			oldSelf:      spec(nil),
			self:         spec(nil),
			wantAccepted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalCellRoleImmutable(tt.oldSelf, tt.self)
			if got != tt.wantAccepted {
				t.Errorf("evalCellRoleImmutable() = %v, want %v", got, tt.wantAccepted)
			}
		})
	}
}

func TestTeamPipelinesCEL(t *testing.T) {
	tests := []struct {
		name         string
		spec         *TeamSpec
		check        func(*TeamSpec) bool
		wantAccepted bool
	}{
		{
			name: "empty profile refused",
			spec: &TeamSpec{
				Namespaces: []string{"payments"},
				Pipelines:  &TeamPipelinesSpec{Profile: "", AgentNamespace: "payments"},
			},
			check:        evalPipelinesProfileNonEmpty,
			wantAccepted: false,
		},
		{
			name: "non-empty profile accepted",
			spec: &TeamSpec{
				Namespaces: []string{"payments"},
				Pipelines:  &TeamPipelinesSpec{Profile: "lts", AgentNamespace: "payments"},
			},
			check:        evalPipelinesProfileNonEmpty,
			wantAccepted: true,
		},
		{
			name: "no pipelines object accepted",
			spec: &TeamSpec{
				Namespaces: []string{"payments"},
			},
			check:        evalPipelinesProfileNonEmpty,
			wantAccepted: true,
		},
		{
			name: "empty agentNamespace refused",
			spec: &TeamSpec{
				Namespaces: []string{"payments"},
				Pipelines:  &TeamPipelinesSpec{Profile: "lts", AgentNamespace: ""},
			},
			check:        evalPipelinesAgentNamespaceNonEmpty,
			wantAccepted: false,
		},
		{
			name: "agentNamespace outside namespaces refused",
			spec: &TeamSpec{
				Namespaces: []string{"payments"},
				Pipelines:  &TeamPipelinesSpec{Profile: "lts", AgentNamespace: "search"},
			},
			check:        evalPipelinesAgentNamespaceManaged,
			wantAccepted: false,
		},
		{
			name: "agentNamespace inside namespaces accepted",
			spec: &TeamSpec{
				Namespaces: []string{"payments"},
				Pipelines:  &TeamPipelinesSpec{Profile: "lts", AgentNamespace: "payments"},
			},
			check:        evalPipelinesAgentNamespaceManaged,
			wantAccepted: true,
		},
		{
			name: "composedBundleRef with no namespace refused",
			spec: &TeamSpec{
				Namespaces: []string{"payments"},
				Pipelines: &TeamPipelinesSpec{
					Profile:        "lts",
					AgentNamespace: "payments",
					ComposedBundleRef: &ComposedBundleRef{
						Name:      "payments-pipelines",
						Namespace: "",
					},
				},
			},
			check:        evalComposedBundleRefNamespaceNonEmpty,
			wantAccepted: false,
		},
		{
			name: "composedBundleRef naming a namespace outside spec.namespaces refused",
			spec: &TeamSpec{
				Namespaces: []string{"payments"},
				Pipelines: &TeamPipelinesSpec{
					Profile:        "lts",
					AgentNamespace: "payments",
					ComposedBundleRef: &ComposedBundleRef{
						Name:      "payments-pipelines",
						Namespace: "search",
					},
				},
			},
			check:        evalComposedBundleRefNamespaceManaged,
			wantAccepted: false,
		},
		{
			name: "composedBundleRef naming a managed namespace accepted",
			spec: &TeamSpec{
				Namespaces: []string{"payments"},
				Pipelines: &TeamPipelinesSpec{
					Profile:        "lts",
					AgentNamespace: "payments",
					ComposedBundleRef: &ComposedBundleRef{
						Name:      "payments-pipelines",
						Namespace: "payments",
					},
				},
			},
			check:        evalComposedBundleRefNamespaceManaged,
			wantAccepted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.check(tt.spec)
			if got != tt.wantAccepted {
				t.Errorf("got %v, want %v", got, tt.wantAccepted)
			}
		})
	}
}

// evalCellPoolCapAboveWarm mirrors:
//
//	!has(self.maxCells) || !has(self.warm) || self.maxCells == 0 ||
//	  self.maxCells >= self.warm
//
// The two booleans carry the has() guards. A stored object written before
// the defaults existed leaves a field absent.
func evalCellPoolCapAboveWarm(hasMaxCells, hasWarm bool, self *CellPoolSpec) bool {
	if !hasMaxCells || !hasWarm {
		return true
	}
	return self.MaxCells == 0 || self.MaxCells >= self.Warm
}

// evalCellPoolProfileRefImmutable mirrors:
//
//	self.profileRef == oldSelf.profileRef
func evalCellPoolProfileRefImmutable(oldSelf, self *CellPoolSpec) bool {
	return self.ProfileRef == oldSelf.ProfileRef
}

func TestCellPoolCapAboveWarmCEL(t *testing.T) {
	tests := []struct {
		name         string
		hasMaxCells  bool
		hasWarm      bool
		self         *CellPoolSpec
		wantAccepted bool
	}{
		{
			name:         "cap below warm refused",
			hasMaxCells:  true,
			hasWarm:      true,
			self:         &CellPoolSpec{ProfileRef: "lts-2-555", Warm: 3, MaxCells: 2},
			wantAccepted: false,
		},
		{
			name:         "cap equal to warm accepted",
			hasMaxCells:  true,
			hasWarm:      true,
			self:         &CellPoolSpec{ProfileRef: "lts-2-555", Warm: 3, MaxCells: 3},
			wantAccepted: true,
		},
		{
			name:         "unlimited cap accepted",
			hasMaxCells:  true,
			hasWarm:      true,
			self:         &CellPoolSpec{ProfileRef: "lts-2-555", Warm: 3, MaxCells: 0},
			wantAccepted: true,
		},
		{
			name:         "absent cap accepted",
			hasMaxCells:  false,
			hasWarm:      true,
			self:         &CellPoolSpec{ProfileRef: "lts-2-555", Warm: 3},
			wantAccepted: true,
		},
		{
			name:         "absent warm accepted",
			hasMaxCells:  true,
			hasWarm:      false,
			self:         &CellPoolSpec{ProfileRef: "lts-2-555", MaxCells: 2},
			wantAccepted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalCellPoolCapAboveWarm(tt.hasMaxCells, tt.hasWarm, tt.self)
			if got != tt.wantAccepted {
				t.Errorf("evalCellPoolCapAboveWarm() = %v, want %v", got, tt.wantAccepted)
			}
		})
	}
}

func TestCellPoolProfileRefImmutableCEL(t *testing.T) {
	tests := []struct {
		name         string
		oldSelf      *CellPoolSpec
		self         *CellPoolSpec
		wantAccepted bool
	}{
		{
			name:         "profileRef changed",
			oldSelf:      &CellPoolSpec{ProfileRef: "lts-2-555"},
			self:         &CellPoolSpec{ProfileRef: "lts-2-500"},
			wantAccepted: false,
		},
		{
			name:         "warm changed, profileRef unchanged",
			oldSelf:      &CellPoolSpec{ProfileRef: "lts-2-555", Warm: 1},
			self:         &CellPoolSpec{ProfileRef: "lts-2-555", Warm: 4},
			wantAccepted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalCellPoolProfileRefImmutable(tt.oldSelf, tt.self)
			if got != tt.wantAccepted {
				t.Errorf("evalCellPoolProfileRefImmutable() = %v, want %v", got, tt.wantAccepted)
			}
		})
	}
}

// TestTeamPipelinesNameFitsServiceCEL covers the root-type rule that keeps a
// pipelines tenant's name usable as an agent Service name.
func TestTeamPipelinesNameFitsServiceCEL(t *testing.T) {
	pipelines := &TeamPipelinesSpec{Profile: "lts", AgentNamespace: "payments"}
	tests := []struct {
		name         string
		team         *Team
		wantAccepted bool
	}{
		{
			name: "short name accepted",
			team: &Team{
				ObjectMeta: metav1.ObjectMeta{Name: "payments"},
				Spec:       TeamSpec{Namespaces: []string{"payments"}, Pipelines: pipelines},
			},
			wantAccepted: true,
		},
		{
			name: "name of exactly 47 characters accepted",
			team: &Team{
				ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 47)},
				Spec:       TeamSpec{Namespaces: []string{"payments"}, Pipelines: pipelines},
			},
			wantAccepted: true,
		},
		{
			name: "name of 48 characters refused",
			team: &Team{
				ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 48)},
				Spec:       TeamSpec{Namespaces: []string{"payments"}, Pipelines: pipelines},
			},
			wantAccepted: false,
		},
		{
			name: "name with a dot refused",
			team: &Team{
				ObjectMeta: metav1.ObjectMeta{Name: "payments.core"},
				Spec:       TeamSpec{Namespaces: []string{"payments"}, Pipelines: pipelines},
			},
			wantAccepted: false,
		},
		{
			name: "long name accepted without pipelines",
			team: &Team{
				ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 60) + ".core"},
				Spec:       TeamSpec{Namespaces: []string{"payments"}},
			},
			wantAccepted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPipelinesTenantNameFitsService(tt.team)
			if got != tt.wantAccepted {
				t.Errorf("evalPipelinesTenantNameFitsService() = %v, want %v", got, tt.wantAccepted)
			}
		})
	}
}
