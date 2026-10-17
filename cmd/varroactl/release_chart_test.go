package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// realChartValues holds placeholders for the values that the chart requires to
// render. The export only chooses the image set, so the placeholders are
// harmless here. The image tags are pinned, because the export refuses tag
// latest and a released chart pins its own tags the same way.
const realChartValues = `global:
  domain: example.com
auth:
  oidc:
    clientId: placeholder
    clientSecret: placeholder
  dashboardUrl: https://example.com
bff:
  oidcClientSecret: placeholder
  image:
    tag: "0.4.1"
operator:
  image:
    tag: "0.4.1"
gateway:
  image:
    tag: "0.4.1"
frontend:
  image:
    tag: "0.4.1"
recorder:
  image:
    tag: "0.4.1"
`

// TestRealChartRender_EveryImageHasSource renders the real chart with the
// default values, plus placeholders, and checks that every image the render
// names has a source. It skips when helm or the chart dependencies are absent.
func TestRealChartRender_EveryImageHasSource(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not on PATH")
	}
	chartDir := filepath.Join("..", "..", "charts", "varroa")
	if _, err := os.Stat(filepath.Join(chartDir, "charts")); err != nil {
		t.Skip("chart dependencies are not built: run helm dependency build charts/varroa")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	helm := helmRunner{bin: "helm"}

	tgz, err := helm.packageDir(ctx, chartDir, tmp)
	if err != nil {
		t.Fatalf("package chart: %v", err)
	}
	valuesPath := filepath.Join(tmp, "placeholders.yaml")
	if err := os.WriteFile(valuesPath, []byte(realChartValues), 0600); err != nil {
		t.Fatal(err)
	}

	defaults, err := loadChartDefaults(ctx, helm, tgz, tmp)
	if err != nil {
		t.Fatalf("chart defaults: %v", err)
	}
	user, _, err := loadValuesFile(valuesPath)
	if err != nil {
		t.Fatal(err)
	}
	merged := mergeValues(defaults, user)

	// A key carries a registry field exactly when its table row says so.
	for _, spec := range imageValueKeys {
		if spec.Kind != imageValueMap {
			continue
		}
		v, _ := lookupValue(defaults, spec.Path)
		block, ok := v.(map[string]any)
		if !ok {
			t.Errorf("table key %s has no image block in the chart defaults", spec.Key)
			continue
		}
		_, has := block["registry"]
		if has != spec.Registry {
			t.Errorf("table key %s: registry key present=%v, table says %v", spec.Key, has, spec.Registry)
		}
	}

	rendered, err := helm.template(ctx, tgz, "varroa-system", []string{valuesPath})
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	sources, err := collectRenderedImages(rendered)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := planImages(sources, valueImageRefs(merged))
	if err != nil {
		t.Fatalf("a rendered image has no source: %v", err)
	}

	keys := map[string]bool{}
	for _, p := range planned {
		for _, k := range p.ValuesKeys {
			keys[k] = true
		}
	}
	for _, want := range []string{
		"jenkins.agentImage",
		"dex.image",
		"nats.container.image",
		"nats.reloader.image",
		"nats.natsBox.container.image",
	} {
		if !keys[want] {
			t.Errorf("no rendered image is attributed to %s; got %v", want, keys)
		}
	}
}

// TestLoadChartDefaults_MergesDependencyDefaults checks that a dependency's own
// defaults appear under its key, and that the umbrella values win.
func TestLoadChartDefaults_MergesDependencyDefaults(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not on PATH")
	}
	chartDir := filepath.Join("..", "..", "charts", "varroa")
	if _, err := os.Stat(filepath.Join(chartDir, "charts")); err != nil {
		t.Skip("chart dependencies are not built")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	helm := helmRunner{bin: "helm"}
	tgz, err := helm.packageDir(ctx, chartDir, tmp)
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := loadChartDefaults(ctx, helm, tgz, tmp)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := lookupValue(defaults, []string{"nats", "reloader", "image", "repository"})
	if !ok || v != "natsio/nats-server-config-reloader" {
		t.Errorf("nats.reloader.image.repository = %v, ok=%v", v, ok)
	}
	// The umbrella sets the NATS server tag, and it must win over the subchart.
	tag, _ := lookupValue(defaults, []string{"nats", "container", "image", "tag"})
	if tag == "2.10.16-alpine" {
		t.Errorf("subchart default won over the umbrella's nats server tag")
	}
}
