package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// Helm media types for a chart stored as an OCI artifact. They match what
// `helm push` writes, so a bundle chart pulls back with helm.
const (
	mediaTypeHelmConfig     = "application/vnd.cncf.helm.config.v1+json"
	mediaTypeHelmChartLayer = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	mediaTypeEmptyConfig    = "application/vnd.oci.empty.v1+json"
)

// helmRunner runs the helm binary. The binary is an external dependency: the
// export never links helm, and the user chooses which binary runs.
type helmRunner struct {
	bin string
	// secrets are values strings that must never reach an error message.
	secrets []string
}

// run executes helm and returns its standard output. A missing binary fails
// with a message that names the --helm flag.
func (h helmRunner) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, h.bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("helm binary %q not found: install helm or pass --helm", h.bin)
		}
		return nil, fmt.Errorf("helm %s: %w: %s", args[0], err, h.summarize(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// chartMeta is the part of Chart.yaml the export records.
type chartMeta struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	AppVersion string `json:"appVersion,omitempty"`
}

// showChart reads a chart's metadata from a .tgz or a directory.
func (h helmRunner) showChart(ctx context.Context, chartPath string) (chartMeta, []byte, error) {
	out, err := h.run(ctx, "show", "chart", chartPath)
	if err != nil {
		return chartMeta{}, nil, err
	}
	var meta chartMeta
	if err := yaml.Unmarshal(out, &meta); err != nil {
		return chartMeta{}, nil, fmt.Errorf("parse chart metadata: %w", err)
	}
	if meta.Name == "" || meta.Version == "" {
		return chartMeta{}, nil, fmt.Errorf("chart %s has no name or version", chartPath)
	}
	// The config blob of an OCI chart is the metadata as JSON.
	js, err := yaml.YAMLToJSON(out)
	if err != nil {
		return chartMeta{}, nil, fmt.Errorf("encode chart metadata: %w", err)
	}
	return meta, js, nil
}

// showValues reads a chart's default values.
func (h helmRunner) showValues(ctx context.Context, chartPath string) (map[string]any, error) {
	out, err := h.run(ctx, "show", "values", chartPath)
	if err != nil {
		return nil, err
	}
	values := map[string]any{}
	if err := yaml.Unmarshal(out, &values); err != nil {
		return nil, fmt.Errorf("parse chart default values: %w", err)
	}
	return values, nil
}

// template renders the chart with the given values files and returns the
// rendered documents. The values are passed to helm by path; the bundle never
// stores their content.
func (h helmRunner) template(ctx context.Context, chartPath, namespace string, valuesFiles []string) ([]byte, error) {
	args := make([]string, 0, 5+2*len(valuesFiles))
	args = append(args, "template", "varroa", chartPath, "-n", namespace)
	for _, f := range valuesFiles {
		args = append(args, "-f", f)
	}
	return h.run(ctx, args...)
}

// packageDir builds a chart directory into a .tgz under outDir. The chart's
// dependencies must already be in charts/.
func (h helmRunner) packageDir(ctx context.Context, dir, outDir string) (string, error) {
	deps, _ := filepath.Glob(filepath.Join(dir, "charts", "*.tgz"))
	chartDirs, _ := filepath.Glob(filepath.Join(dir, "charts", "*", "Chart.yaml"))
	if len(deps) == 0 && len(chartDirs) == 0 {
		// A chart with no dependencies is valid. Only a Chart.lock names a
		// dependency that the build must find.
		if _, err := os.Stat(filepath.Join(dir, "Chart.lock")); err == nil {
			return "", fmt.Errorf("chart %s has a Chart.lock but no charts/ dependencies: run helm dependency build %s", dir, dir)
		}
	}
	if _, err := h.run(ctx, "package", dir, "-d", outDir); err != nil {
		return "", err
	}
	matches, _ := filepath.Glob(filepath.Join(outDir, "*.tgz"))
	if len(matches) != 1 {
		return "", fmt.Errorf("helm package wrote %d archives, want 1", len(matches))
	}
	return matches[0], nil
}

// loadChartDefaults returns the chart's default values with each dependency's
// defaults merged under its key. helm show values on the umbrella archive spells
// out only the keys that the umbrella sets. A dependency's own defaults, such as
// nats.reloader.image, come from the dependency archive in the chart's charts/
// directory. The umbrella's values win over the dependency's.
func loadChartDefaults(ctx context.Context, h helmRunner, chartTgz, workDir string) (map[string]any, error) {
	defaults, err := h.showValues(ctx, chartTgz)
	if err != nil {
		return nil, err
	}
	unpack := filepath.Join(workDir, "unpack-chart")
	if err := os.MkdirAll(unpack, 0755); err != nil {
		return nil, fmt.Errorf("create chart unpack directory: %w", err)
	}
	if err := extractTarGz(chartTgz, unpack); err != nil {
		return nil, fmt.Errorf("unpack chart for dependency defaults: %w", err)
	}
	// A packaged chart stores each dependency as a directory, and a registry chart
	// stores it as a .tgz. Helm reads both, so both are accepted.
	var deps []string
	for _, pattern := range []string{"*.tgz", "*/Chart.yaml"} {
		matches, _ := filepath.Glob(filepath.Join(unpack, "*", "charts", pattern))
		deps = append(deps, matches...)
	}
	sort.Strings(deps)
	for _, dep := range deps {
		if filepath.Base(dep) == "Chart.yaml" {
			dep = filepath.Dir(dep)
		}
		meta, _, err := h.showChart(ctx, dep)
		if err != nil {
			return nil, err
		}
		sub, err := h.showValues(ctx, dep)
		if err != nil {
			return nil, err
		}
		parent, _ := defaults[meta.Name].(map[string]any)
		defaults[meta.Name] = mergeValues(sub, parent)
	}
	return defaults, nil
}

// summarize returns the first line of helm's standard error, with every values
// secret redacted, and capped at 240 bytes. Redaction runs over the whole stream
// before the cut, so a secret that spans lines is still removed.
func (h helmRunner) summarize(stderr string) string {
	text := stderr
	for _, sec := range h.redactions() {
		text = strings.ReplaceAll(text, sec, "<redacted>")
	}
	line := strings.TrimSpace(text)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if len(line) > 240 {
		line = line[:240] + "..."
	}
	return line
}

// redactions lists every string that summarize removes, longest first, so that a
// secret which is a prefix of another does not leave the rest of the longer one
// behind. A multi-line secret also contributes each of its lines, because a
// chart can print one line of it on its own.
func (h helmRunner) redactions() []string {
	set := map[string]bool{}
	for _, sec := range h.secrets {
		set[sec] = true
		for _, line := range strings.Split(sec, "\n") {
			if t := strings.TrimSpace(line); len(t) >= 6 {
				set[t] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// valueSecrets lists the string values of a values map that a helm message must
// not repeat. Short strings are skipped, because they match ordinary text.
func valueSecrets(values map[string]any) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for _, c := range t {
				walk(c)
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		case string:
			if len(t) >= 6 {
				out = append(out, t)
			}
		}
	}
	walk(values)
	return out
}

// mergeValues merges src over dst the way helm merges values files: maps merge
// key by key, and any other value in src replaces the value in dst. The inputs
// are not changed.
func mergeValues(dst, src map[string]any) map[string]any {
	out := make(map[string]any, len(dst)+len(src))
	for k, v := range dst {
		out[k] = v
	}
	for k, sv := range src {
		sm, sIsMap := sv.(map[string]any)
		dm, dIsMap := out[k].(map[string]any)
		if sIsMap && dIsMap {
			out[k] = mergeValues(dm, sm)
			continue
		}
		out[k] = sv
	}
	return out
}

// loadValuesFile reads one values file. It returns the parsed values and the
// SHA-256 of the file. The content stays in memory for the merge only.
func loadValuesFile(path string) (map[string]any, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read values file: %w", err)
	}
	values := map[string]any{}
	if err := yaml.Unmarshal(data, &values); err != nil {
		return nil, "", fmt.Errorf("parse values file %s: %w", path, err)
	}
	digest, _, err := oci.Sha256Digest(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	return values, digest, nil
}

// sanitizeTag makes a chart version safe as an OCI tag. A "+" is not a legal
// tag character, so it becomes "_".
func sanitizeTag(v string) string {
	return strings.ReplaceAll(v, "+", "_")
}

// pushChartArtifact stores a chart .tgz in the layout as a Helm OCI artifact
// under tag. It returns the manifest descriptor.
func pushChartArtifact(ctx context.Context, layout *oci.LayoutStore, tag, tgzPath string, configJSON []byte) (oci.Descriptor, error) {
	cfgDigest, cfgSize, err := layout.PushBlob(ctx, mediaTypeHelmConfig, bytes.NewReader(configJSON))
	if err != nil {
		return oci.Descriptor{}, fmt.Errorf("store chart config: %w", err)
	}
	f, err := os.Open(tgzPath)
	if err != nil {
		return oci.Descriptor{}, fmt.Errorf("open chart archive: %w", err)
	}
	defer func() { _ = f.Close() }()
	layerDigest, layerSize, err := layout.PushBlob(ctx, mediaTypeHelmChartLayer, f)
	if err != nil {
		return oci.Descriptor{}, fmt.Errorf("store chart archive: %w", err)
	}
	m := oci.Manifest{
		Config: oci.Descriptor{MediaType: mediaTypeHelmConfig, Digest: cfgDigest, Size: cfgSize},
		Layers: []oci.Descriptor{{MediaType: mediaTypeHelmChartLayer, Digest: layerDigest, Size: layerSize}},
	}
	if err := layout.Push(ctx, tag, m); err != nil {
		return oci.Descriptor{}, fmt.Errorf("tag chart %s: %w", tag, err)
	}
	return layout.Resolve(ctx, tag)
}

// fileSHA256 returns the SHA-256 of a file's content.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
