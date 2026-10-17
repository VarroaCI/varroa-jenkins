package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const renderedFixture = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: varroa-operator
spec:
  template:
    spec:
      initContainers:
        - name: wait
          image: busybox:1.36
      containers:
        - name: operator
          image: ghcr.io/example/varroa-jenkins:3.0.0
          env:
            - name: VARROA_MITE_IMAGE
              value: ghcr.io/example/varroa-jenkins:3.0.0
            - name: VARROA_JENKINS_IMAGE_REPOSITORY
              value: jenkins/jenkins
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: varroa-db
spec:
  imageName: ghcr.io/cloudnative-pg/postgresql:16.4
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: varroa-prune
spec:
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: prune
              image: ghcr.io/example/varroa-jenkins:3.0.0
`

func TestCollectImages_RenderedManifestsAndEnv(t *testing.T) {
	got, err := collectRenderedImages([]byte(renderedFixture))
	if err != nil {
		t.Fatalf("collectRenderedImages: %v", err)
	}
	seen := map[string]bool{}
	for _, s := range got {
		seen[s.Raw] = true
	}
	for _, want := range []string{
		"busybox:1.36",
		"ghcr.io/example/varroa-jenkins:3.0.0",
		"ghcr.io/cloudnative-pg/postgresql:16.4",
	} {
		if !seen[want] {
			t.Errorf("image %q not collected; got %v", want, got)
		}
	}
	// VARROA_JENKINS_IMAGE_REPOSITORY is a repository, not a reference: it
	// must not be read as an image.
	if seen["jenkins/jenkins"] {
		t.Errorf("repository env value read as an image")
	}
}

func TestCollectImages_RefusesLatestAndUntagged(t *testing.T) {
	cases := map[string]bool{
		"ghcr.io/example/app:latest":                            true,
		"ghcr.io/example/app":                                   true,
		"ghcr.io/example/app:1.2.3":                             false,
		"ghcr.io/example/app@sha256:" + strings.Repeat("a", 64): false,
		"localhost:5000/app:v1":                                 false,
	}
	for raw, wantErr := range cases {
		ref, err := parseImageRef(raw)
		if err != nil {
			t.Fatalf("parseImageRef(%q): %v", raw, err)
		}
		err = validateImageRef(ref)
		if (err != nil) != wantErr {
			t.Errorf("validateImageRef(%q) err=%v, want error=%v", raw, err, wantErr)
		}
	}
}

func TestCollectImages_UnattributedImageFails(t *testing.T) {
	sources := []imageSource{{Raw: "evil.example/miner:1.0", Origin: "chart", Where: "Deployment/x"}}
	_, err := planImages(sources, map[string]string{"operator.image": "ghcr.io/example/varroa-jenkins:3.0.0"})
	if err == nil || !strings.Contains(err.Error(), "has no known source") {
		t.Fatalf("expected no-known-source error, got %v", err)
	}
}

func TestImageValueKeys_MatchChartValues(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "charts", "varroa", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{}
	if err := unmarshalYAMLForTest(data, &values); err != nil {
		t.Fatal(err)
	}
	for _, spec := range imageValueKeys {
		if strings.HasPrefix(spec.Key, "nats.") {
			// The nats keys live in the subchart defaults, not in values.yaml.
			continue
		}
		if spec.Key == "operator.miteImage" {
			// Optional: the chart leaves it unset and falls back to the operator image.
			continue
		}
		if _, ok := lookupValue(values, spec.Path); !ok {
			t.Errorf("table key %s is absent from charts/varroa/values.yaml", spec.Key)
		}
	}
}

func TestBundleDirAgentImages_JCasCAndRawYAML(t *testing.T) {
	dir := t.TempDir()
	bundleYAML := `jenkins:
  clouds:
    - kubernetes:
        templates:
          - name: default
            containers:
              - name: jnlp
                image: "${varroa_jenkins_agent_image}"
          - name: tools
            containers:
              - name: tool
                image: registry.example.com/tools:2.1
            yaml: |
              apiVersion: v1
              kind: Pod
              spec:
                containers:
                  - name: sidecar
                    image: registry.example.com/sidecar:0.9
`
	if err := os.WriteFile(filepath.Join(dir, "jenkins.yaml"), []byte(bundleYAML), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := bundleDirImages([]string{dir}, "jenkins/inbound-agent:3384-1")
	if err != nil {
		t.Fatalf("bundleDirImages: %v", err)
	}
	seen := map[string]bool{}
	for _, s := range got {
		seen[s.Raw] = true
	}
	for _, want := range []string{
		"jenkins/inbound-agent:3384-1",
		"registry.example.com/tools:2.1",
		"registry.example.com/sidecar:0.9",
	} {
		if !seen[want] {
			t.Errorf("bundle image %q not found; got %v", want, got)
		}
	}
}

func TestBundleDirAgentImages_UnresolvedVariableFails(t *testing.T) {
	dir := t.TempDir()
	body := `jenkins:
  clouds:
    - kubernetes:
        templates:
          - containers:
              - image: "${varroa_unknown}/tool:1"
`
	if err := os.WriteFile(filepath.Join(dir, "b.yaml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := bundleDirImages([]string{dir}, "jenkins/inbound-agent:1"); err == nil || !strings.Contains(err.Error(), "unresolved variable") {
		t.Fatalf("expected unresolved-variable error, got %v", err)
	}
}

func TestPlanImages_ProfileAndFlagSourcesAccepted(t *testing.T) {
	sources := []imageSource{
		{Raw: "ghcr.io/example/varroa-jenkins:3.0.0", Origin: "chart", Where: "Deployment/x"},
		{Raw: "jenkins/jenkins:2.479.1", Origin: "profile:lts-2.479", ValuesKey: "jenkins.image.repository"},
		{Raw: "mirror.example/tool:1.0", Origin: "flag", Where: "--image"},
		{Raw: "ghcr.io/example/varroa-jenkins:3.0.0", Origin: "chart", Where: "Deployment/y"},
	}
	planned, err := planImages(sources, map[string]string{"operator.image": "ghcr.io/example/varroa-jenkins:3.0.0"})
	if err != nil {
		t.Fatalf("planImages: %v", err)
	}
	if len(planned) != 3 {
		t.Fatalf("got %d planned images, want 3 (duplicate merged)", len(planned))
	}
	var keyed int
	for _, p := range planned {
		if len(p.ValuesKeys) > 0 {
			keyed++
		}
	}
	if keyed != 2 {
		t.Errorf("got %d images with a values key, want 2 (operator.image and jenkins.image.repository)", keyed)
	}
}

func TestParseImageRef_DockerHubNames(t *testing.T) {
	ref, err := parseImageRef("nats:2.14.7-alpine")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Host != "docker.io" || ref.Path != "library/nats" || ref.Tag != "2.14.7-alpine" {
		t.Errorf("parse nats: %+v", ref)
	}
	ref, err = parseImageRef("jenkins/jenkins:2.479.1")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Host != "docker.io" || ref.Path != "jenkins/jenkins" {
		t.Errorf("parse jenkins/jenkins: %+v", ref)
	}
	if ref.pullRepo() != "registry-1.docker.io/jenkins/jenkins" {
		t.Errorf("pullRepo = %q", ref.pullRepo())
	}
}

func TestLayoutTagForImage_Stable(t *testing.T) {
	ref, _ := parseImageRef("nats:2.14.7-alpine")
	a, b := layoutTagForImage(ref), layoutTagForImage(ref)
	if a != b || !strings.HasPrefix(a, "img-") || len(a) != len("img-")+12 {
		t.Errorf("layoutTagForImage = %q, %q", a, b)
	}
}
