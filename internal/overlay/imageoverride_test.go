package overlay

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestImageOverride(t *testing.T) {
	tests := []struct {
		name          string
		patchYAML     string
		containerName string
		wantImage     string
		wantOK        bool
		wantErr       bool
	}{
		{
			name: "overlay declaring jenkins image",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: jenkins
        image: my-registry/custom:1.0
`,
			containerName: "jenkins",
			wantImage:     "my-registry/custom:1.0",
			wantOK:        true,
		},
		{
			name: "overlay patching only resources (no image)",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: jenkins
        resources:
          limits:
            cpu: "2"
`,
			containerName: "jenkins",
			wantImage:     "",
			wantOK:        false,
		},
		{
			name: "other-container image only",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: sidecar
        image: some-other:1.0
`,
			containerName: "jenkins",
			wantImage:     "",
			wantOK:        false,
		},
		{
			name:          "empty string YAML",
			patchYAML:     "",
			containerName: "jenkins",
			wantImage:     "",
			wantOK:        false,
		},
		{
			name:      "invalid YAML",
			patchYAML: "spec: [unclosed",
			wantErr:   true,
			wantOK:    false,
		},
		{
			name: "containers present but not a list",
			patchYAML: `
spec:
  template:
    spec:
      containers: foo
`,
			containerName: "jenkins",
			wantImage:     "",
			wantOK:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotImage, gotOK, gotErr := ImageOverride([]byte(tt.patchYAML), tt.containerName)
			if (gotErr != nil) != tt.wantErr {
				t.Errorf("ImageOverride() error = %v, wantErr %v", gotErr, tt.wantErr)
				return
			}
			if gotOK != tt.wantOK {
				t.Errorf("ImageOverride() ok = %v, wantOK %v", gotOK, tt.wantOK)
			}
			if gotImage != tt.wantImage {
				t.Errorf("ImageOverride() image = %q, want %q", gotImage, tt.wantImage)
			}
		})
	}
}

func TestResourcesOverride(t *testing.T) {
	tests := []struct {
		name            string
		patchYAML       string
		containerName   string
		wantRR          *corev1.ResourceRequirements
		wantNullReqs    []corev1.ResourceName
		wantNullLims    []corev1.ResourceName
		wantDropAllReqs bool
		wantDropAllLims bool
		wantOK          bool
		wantErr         bool
	}{
		{
			name: "overlay declaring both cpu and memory requests",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources:
          requests:
            cpu: "250m"
            memory: "256Mi"
`,
			containerName: "mite",
			wantRR: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("250m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
			wantOK: true,
		},
		{
			// Regression for PR #373 review: sigs.k8s.io/yaml round-trips YAML
			// through JSON, so an unquoted numeric scalar decodes as float64,
			// not string. A bare `.(string)` type assertion silently missed
			// this and read as "no override" even though the overlay declares
			// one — the overlay-declared value then got ignored in favor of
			// the CRD spec default, and the drift check compared against the
			// wrong desired value.
			name: "overlay declaring unquoted numeric cpu and memory (YAML decodes as float64)",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources:
          requests:
            cpu: 1
            memory: 2
`,
			containerName: "mite",
			wantRR: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("2"),
				},
			},
			wantOK: true,
		},
		{
			name: "overlay declaring only cpu (partial override)",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources:
          requests:
            cpu: "250m"
`,
			containerName: "mite",
			wantRR: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("250m"),
				},
			},
			wantOK: true,
		},
		{
			name: "overlay patching only image (no resources)",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: mite
        image: my-registry/custom-mite:1.0
`,
			containerName: "mite",
			wantOK:        false,
		},
		{
			name: "overlay declaring limits but not requests",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources:
          limits:
            cpu: "1"
`,
			containerName: "mite",
			wantRR: &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				},
			},
			wantOK: true,
		},
		{
			name: "overlay declaring both requests and limits",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources:
          requests:
            cpu: "500m"
            memory: "256Mi"
          limits:
            cpu: "2"
            memory: "1Gi"
`,
			containerName: "mite",
			wantRR: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
			wantOK: true,
		},
		{
			name: "other-container resources only",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: jenkins
        resources:
          requests:
            cpu: "1"
`,
			containerName: "mite",
			wantOK:        false,
		},
		{
			name:          "empty string YAML",
			patchYAML:     "",
			containerName: "mite",
			wantOK:        false,
		},
		{
			name:      "invalid YAML",
			patchYAML: "spec: [unclosed",
			wantErr:   true,
			wantOK:    false,
		},
		{
			name: "null requests.cpu with requests.memory set",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources:
          requests:
            cpu: null
            memory: "256Mi"
`,
			containerName: "mite",
			wantRR: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
			wantNullReqs: []corev1.ResourceName{corev1.ResourceCPU},
			wantOK:       true,
		},
		{
			name: "only requests.cpu null (no other resources)",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources:
          requests:
            cpu: null
`,
			containerName: "mite",
			wantRR:        nil,
			wantNullReqs:  []corev1.ResourceName{corev1.ResourceCPU},
			wantOK:        true,
		},
		{
			name: "limits.memory null",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources:
          limits:
            memory: null
`,
			containerName: "mite",
			wantRR:        nil,
			wantNullLims:  []corev1.ResourceName{corev1.ResourceMemory},
			wantOK:        true,
		},
		{
			name: "map-level resources null (deletes entire resources block)",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources: null
`,
			containerName:   "mite",
			wantRR:          nil,
			wantDropAllReqs: true,
			wantDropAllLims: true,
			wantOK:          true,
		},
		{
			name: "map-level requests null (deletes all requests, limits preserved)",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources:
          requests: null
`,
			containerName:   "mite",
			wantRR:          nil,
			wantDropAllReqs: true,
			wantOK:          true,
		},
		{
			name: "map-level limits null (deletes all limits, requests preserved)",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources:
          limits: null
`,
			containerName:   "mite",
			wantRR:          nil,
			wantDropAllLims: true,
			wantOK:          true,
		},
		{
			name: "mixed: requests null + limits set",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: mite
        resources:
          requests: null
          limits:
            cpu: "1"
`,
			containerName: "mite",
			wantRR: &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				},
			},
			wantDropAllReqs: true,
			wantOK:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRR, gotNullReqs, gotNullLims, gotDropAllReqs, gotDropAllLims, gotOK, gotErr := ResourcesOverride([]byte(tt.patchYAML), tt.containerName)
			if (gotErr != nil) != tt.wantErr {
				t.Errorf("ResourcesOverride() error = %v, wantErr %v", gotErr, tt.wantErr)
				return
			}
			if gotOK != tt.wantOK {
				t.Errorf("ResourcesOverride() ok = %v, wantOK %v", gotOK, tt.wantOK)
			}
			if tt.wantRR == nil && gotRR != nil {
				t.Errorf("ResourcesOverride() rr = %+v, want nil", gotRR)
			}
			if tt.wantRR != nil {
				if gotRR == nil {
					t.Fatalf("ResourcesOverride() rr = nil, want %+v", tt.wantRR)
				}
				if !reflect.DeepEqual(gotRR, tt.wantRR) {
					t.Errorf("ResourcesOverride() rr = %+v, want %+v", gotRR, tt.wantRR)
				}
			}
			if !reflect.DeepEqual(gotNullReqs, tt.wantNullReqs) {
				t.Errorf("ResourcesOverride() nullReqs = %v, want %v", gotNullReqs, tt.wantNullReqs)
			}
			if !reflect.DeepEqual(gotNullLims, tt.wantNullLims) {
				t.Errorf("ResourcesOverride() nullLims = %v, want %v", gotNullLims, tt.wantNullLims)
			}
			if gotDropAllReqs != tt.wantDropAllReqs {
				t.Errorf("ResourcesOverride() dropAllReqs = %v, want %v", gotDropAllReqs, tt.wantDropAllReqs)
			}
			if gotDropAllLims != tt.wantDropAllLims {
				t.Errorf("ResourcesOverride() dropAllLims = %v, want %v", gotDropAllLims, tt.wantDropAllLims)
			}
		})
	}
}

func TestPullPolicyOverride(t *testing.T) {
	tests := []struct {
		name          string
		patchYAML     string
		containerName string
		wantPolicy    string
		wantOK        bool
		wantErr       bool
	}{
		{
			name: "overlay declaring mite imagePullPolicy",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: mite
        imagePullPolicy: Always
`,
			containerName: "mite",
			wantPolicy:    "Always",
			wantOK:        true,
		},
		{
			name: "overlay patching only image (no pull policy)",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: mite
        image: my-registry/custom-mite:1.0
`,
			containerName: "mite",
			wantOK:        false,
		},
		{
			name: "other-container pull policy only",
			patchYAML: `
spec:
  template:
    spec:
      containers:
      - name: jenkins
        imagePullPolicy: Always
`,
			containerName: "mite",
			wantOK:        false,
		},
		{
			name:          "empty string YAML",
			patchYAML:     "",
			containerName: "mite",
			wantOK:        false,
		},
		{
			name:      "invalid YAML",
			patchYAML: "spec: [unclosed",
			wantErr:   true,
			wantOK:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPolicy, gotOK, gotErr := PullPolicyOverride([]byte(tt.patchYAML), tt.containerName)
			if (gotErr != nil) != tt.wantErr {
				t.Errorf("PullPolicyOverride() error = %v, wantErr %v", gotErr, tt.wantErr)
				return
			}
			if gotOK != tt.wantOK {
				t.Errorf("PullPolicyOverride() ok = %v, wantOK %v", gotOK, tt.wantOK)
			}
			if gotPolicy != tt.wantPolicy {
				t.Errorf("PullPolicyOverride() pullPolicy = %q, want %q", gotPolicy, tt.wantPolicy)
			}
		})
	}
}
