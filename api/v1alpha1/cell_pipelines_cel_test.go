package v1alpha1

import "testing"

// The functions below mirror the literal XValidation rule text on
// ControllerSpec.Cell and TeamSpec in types.go. Keep each function
// identical to its rule string. The apiserver evaluates the CEL text at
// admission time; this package has no CEL evaluator, so these functions
// are the only check that runs in `go test`. A live dry-run apply is the
// only check on the CEL text itself.

// evalCellEnabledImmutable mirrors:
//
//	self.enabled == oldSelf.enabled
func evalCellEnabledImmutable(oldSelf, self *CellSpec) bool {
	return self.Enabled == oldSelf.Enabled
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

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestCellEnabledImmutableCEL(t *testing.T) {
	tests := []struct {
		name         string
		oldSelf      *CellSpec
		self         *CellSpec
		wantAccepted bool
	}{
		{
			name:         "enabled changed",
			oldSelf:      &CellSpec{Enabled: false, MaxTenants: 3},
			self:         &CellSpec{Enabled: true, MaxTenants: 3},
			wantAccepted: false,
		},
		{
			name:         "maxTenants changed, enabled unchanged",
			oldSelf:      &CellSpec{Enabled: true, MaxTenants: 3},
			self:         &CellSpec{Enabled: true, MaxTenants: 10},
			wantAccepted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalCellEnabledImmutable(tt.oldSelf, tt.self)
			if got != tt.wantAccepted {
				t.Errorf("evalCellEnabledImmutable() = %v, want %v", got, tt.wantAccepted)
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
