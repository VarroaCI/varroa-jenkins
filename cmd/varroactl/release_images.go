package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

// imageRef is a parsed container image reference. Host is the registry host;
// a bare name such as "nats" reads as docker.io, with the library/ prefix on
// a single-name repository.
type imageRef struct {
	Raw    string
	Host   string
	Path   string
	Tag    string
	Digest string
}

// dockerHub is the registry host a reference without a host names.
const dockerHub = "docker.io"

// parseImageRef splits a reference into its parts. It does not judge the tag.
// Use validateImageRef for that.
func parseImageRef(raw string) (imageRef, error) {
	rest := strings.TrimSpace(raw)
	ref := imageRef{Raw: rest}
	if i := strings.Index(rest, "@"); i >= 0 {
		ref.Digest = rest[i+1:]
		rest = rest[:i]
		if err := oci.ParseDigest(ref.Digest); err != nil {
			return imageRef{}, fmt.Errorf("image %s has a malformed digest: %w", raw, err)
		}
	}
	if c := strings.LastIndex(rest, ":"); c > strings.LastIndex(rest, "/") {
		ref.Tag = rest[c+1:]
		rest = rest[:c]
	}
	host, path := dockerHub, rest
	if parts := strings.SplitN(rest, "/", 2); len(parts) == 2 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		host, path = parts[0], parts[1]
	}
	if host == dockerHub && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	if path == "" {
		return imageRef{}, fmt.Errorf("image %q names no repository", raw)
	}
	ref.Host = host
	ref.Path = path
	return ref, nil
}

// validateImageRef fails closed on an image the bundle cannot pin. A reference
// needs an exact tag or a digest. The tag latest moves, so it is refused.
func validateImageRef(ref imageRef) error {
	if ref.Digest != "" {
		return nil
	}
	if ref.Tag == "" {
		return fmt.Errorf("image %s has no tag and no digest: pin it by tag or by digest", ref.Raw)
	}
	if ref.Tag == "latest" {
		return fmt.Errorf("image %s uses tag latest, which moves: pin an exact tag or a digest", ref.Raw)
	}
	return nil
}

// canonical is the identity of an image: host, path, and tag or digest. Two
// spellings of one image compare equal through it.
func (r imageRef) canonical() string {
	if r.Digest != "" {
		return r.Host + "/" + r.Path + "@" + r.Digest
	}
	return r.Host + "/" + r.Path + ":" + r.Tag
}

// pullRepo is the repository a registry client opens for this image.
func (r imageRef) pullRepo() string {
	host := r.Host
	if host == dockerHub {
		host = "registry-1.docker.io"
	}
	return host + "/" + r.Path
}

// sourceRef is the reference the copy resolves: the digest when one is set,
// else the tag.
func (r imageRef) sourceRef() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

// destTag is the tag an import pushes the image under. A digest-only image
// gets a deterministic tag built from its digest.
func (r imageRef) destTag() string {
	if r.Tag != "" {
		return r.Tag
	}
	return "digest-" + strings.TrimPrefix(r.Digest, "sha256:")[:12]
}

// imageSource is one place an image reference was found.
type imageSource struct {
	Raw    string
	Origin string // chart, profile:<name>, bundle, or flag
	Where  string // a rendered object or bundle file, for error messages
	// ValuesKey is set when the source names its chart value directly, as the
	// Jenkins profile images do. Chart sources leave it empty and get it from
	// the value table.
	ValuesKey string
}

// plannedImage is an image source after attribution.
type plannedImage struct {
	Ref        imageRef
	Origin     string
	ValuesKeys []string
}

// imageValueKind says how a chart value holds an image.
type imageValueKind int

const (
	// imageValueMap is a block with repository, tag, and an optional registry.
	imageValueMap imageValueKind = iota
	// imageValueString is one reference string.
	imageValueString
	// imageValueRepo is a repository only. The tag comes from the profile.
	imageValueRepo
)

// imageValueSpec names one chart value that holds an image. The table is the
// attribution rule: a rendered image is accepted only when a value names it.
type imageValueSpec struct {
	Key  string
	Path []string
	Kind imageValueKind
	// Registry is true when the chart's subchart gives this image block a
	// registry key. The overrides then clear that key.
	Registry bool
}

// imageValueKeys is every chart value that holds an image the bundle carries.
// TestImageValueKeys_MatchChartValues keeps the varroa-owned keys in step with
// charts/varroa/values.yaml.
var imageValueKeys = []imageValueSpec{
	{Key: "operator.image", Path: []string{"operator", "image"}, Kind: imageValueMap},
	{Key: "operator.miteImage", Path: []string{"operator", "miteImage"}, Kind: imageValueString},
	{Key: "gateway.image", Path: []string{"gateway", "image"}, Kind: imageValueMap},
	{Key: "bff.image", Path: []string{"bff", "image"}, Kind: imageValueMap},
	{Key: "frontend.image", Path: []string{"frontend", "image"}, Kind: imageValueMap},
	{Key: "updateCenter.image", Path: []string{"updateCenter", "image"}, Kind: imageValueMap},
	{Key: "recorder.image", Path: []string{"recorder", "image"}, Kind: imageValueMap},
	{Key: "dex.image", Path: []string{"dex", "image"}, Kind: imageValueMap},
	{Key: "nats.container.image", Path: []string{"nats", "container", "image"}, Kind: imageValueMap, Registry: true},
	{Key: "nats.reloader.image", Path: []string{"nats", "reloader", "image"}, Kind: imageValueMap, Registry: true},
	{Key: "nats.natsBox.container.image", Path: []string{"nats", "natsBox", "container", "image"}, Kind: imageValueMap, Registry: true},
	{Key: "jenkins.agentImage", Path: []string{"jenkins", "agentImage"}, Kind: imageValueString},
	{Key: "jenkins.image.repository", Path: []string{"jenkins", "image", "repository"}, Kind: imageValueRepo},
}

// specForKey returns the table row for a values key, or nil.
func specForKey(key string) *imageValueSpec {
	for i := range imageValueKeys {
		if imageValueKeys[i].Key == key {
			return &imageValueKeys[i]
		}
	}
	return nil
}

// lookupValue walks a merged values map along path.
func lookupValue(values map[string]any, path []string) (any, bool) {
	var cur any = values
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// valueImageRefs returns the image each table row names under the merged
// values. A repository-only row has no full reference and is left out.
func valueImageRefs(values map[string]any) map[string]string {
	out := map[string]string{}
	for _, spec := range imageValueKeys {
		v, ok := lookupValue(values, spec.Path)
		if !ok {
			continue
		}
		switch spec.Kind {
		case imageValueString:
			if s, _ := v.(string); s != "" {
				out[spec.Key] = s
			}
		case imageValueMap:
			m, _ := v.(map[string]any)
			repo, _ := m["repository"].(string)
			if repo == "" {
				continue
			}
			tag, _ := m["tag"].(string)
			registry, _ := m["registry"].(string)
			ref := repo
			if registry != "" {
				ref = registry + "/" + repo
			}
			if tag != "" {
				ref += ":" + tag
			}
			out[spec.Key] = ref
		}
	}
	return out
}

// collectRenderedImages reads every image a rendered chart names: container and
// init container images, env values whose name ends in _IMAGE, and the CNPG
// Cluster imageName. Each object's pod spec is read from its kind's own path.
func collectRenderedImages(rendered []byte) ([]imageSource, error) {
	dec := yaml.NewDecoder(bytes.NewReader(rendered))
	var out []imageSource
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("parse rendered chart: %w", err)
		}
		if doc == nil {
			continue
		}
		kind, _ := doc["kind"].(string)
		name := ""
		if md, ok := doc["metadata"].(map[string]any); ok {
			name, _ = md["name"].(string)
		}
		where := kind + "/" + name
		spec, _ := doc["spec"].(map[string]any)
		switch kind {
		case "Pod":
			out = append(out, podSpecImages(spec, where)...)
		case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job":
			out = append(out, podSpecImages(dig(spec, "template", "spec"), where)...)
		case "CronJob":
			out = append(out, podSpecImages(dig(spec, "jobTemplate", "spec", "template", "spec"), where)...)
		case "Cluster":
			if img, _ := spec["imageName"].(string); img != "" {
				out = append(out, imageSource{Raw: img, Origin: "chart", Where: where})
			}
		}
	}
	return out, nil
}

// dig reads a nested mapping, returning nil when a level is missing.
func dig(m map[string]any, keys ...string) map[string]any {
	cur := m
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// podSpecImages reads the images of a pod spec: containers, init containers,
// and the value of every env entry whose name ends in _IMAGE.
func podSpecImages(spec map[string]any, where string) []imageSource {
	var out []imageSource
	for _, field := range []string{"initContainers", "containers"} {
		list, _ := spec[field].([]any)
		for _, item := range list {
			c, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if img, _ := c["image"].(string); img != "" {
				out = append(out, imageSource{Raw: img, Origin: "chart", Where: where})
			}
			envs, _ := c["env"].([]any)
			for _, e := range envs {
				em, ok := e.(map[string]any)
				if !ok {
					continue
				}
				name, _ := em["name"].(string)
				val, _ := em["value"].(string)
				if strings.HasSuffix(name, "_IMAGE") && val != "" {
					out = append(out, imageSource{Raw: val, Origin: "chart", Where: where + " env " + name})
				}
			}
		}
	}
	return out
}

// bundleAgentVar is the JCasC variable the starter bundle uses for the agent
// image. A bundle directory that uses it resolves to the agent image value.
const bundleAgentVar = "${varroa_jenkins_agent_image}"

// bundleDirImages reads the image references a user bundle directory names
// through JCasC kubernetes cloud pod templates: each template's containers and
// the containers of its raw yaml field. The agent variable resolves to
// agentRef. Any other variable fails closed, because its image is unknown.
func bundleDirImages(dirs []string, agentRef string) ([]imageSource, error) {
	var out []imageSource
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if ext := strings.ToLower(filepath.Ext(path)); ext != ".yaml" && ext != ".yml" {
				return nil
			}
			images, err := bundleFileImages(path, agentRef)
			if err != nil {
				return err
			}
			out = append(out, images...)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("read bundle directory %s: %w", dir, err)
		}
	}
	return out, nil
}

// bundleFileImages reads one bundle file's cloud pod templates.
func bundleFileImages(path, agentRef string) ([]imageSource, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []imageSource
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		jenkins, _ := doc["jenkins"].(map[string]any)
		clouds, _ := jenkins["clouds"].([]any)
		for _, c := range clouds {
			cloud, _ := c.(map[string]any)
			k8s, _ := cloud["kubernetes"].(map[string]any)
			templates, _ := k8s["templates"].([]any)
			for _, t := range templates {
				tpl, _ := t.(map[string]any)
				containers, _ := tpl["containers"].([]any)
				for _, ct := range containers {
					cm, _ := ct.(map[string]any)
					img, _ := cm["image"].(string)
					if img == "" {
						continue
					}
					resolved, err := resolveBundleVars(img, agentRef)
					if err != nil {
						return nil, fmt.Errorf("%s: %w", path, err)
					}
					out = append(out, imageSource{Raw: resolved, Origin: "bundle", Where: path})
				}
				if raw, _ := tpl["yaml"].(string); raw != "" {
					podImgs, err := rawPodImages(raw, agentRef)
					if err != nil {
						return nil, fmt.Errorf("%s: %w", path, err)
					}
					for _, img := range podImgs {
						out = append(out, imageSource{Raw: img, Origin: "bundle", Where: path})
					}
				}
			}
		}
	}
	return out, nil
}

// rawPodImages reads the containers of a pod template's raw yaml field.
func rawPodImages(raw, agentRef string) ([]string, error) {
	resolved, err := resolveBundleVars(raw, agentRef)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(strings.NewReader(resolved))
	var out []string
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("pod template yaml field is not a YAML document: %w", err)
		}
		spec, _ := doc["spec"].(map[string]any)
		for _, s := range podSpecImages(spec, "") {
			out = append(out, s.Raw)
		}
	}
	return out, nil
}

// resolveBundleVars replaces the agent variable and fails on any other variable.
func resolveBundleVars(s, agentRef string) (string, error) {
	if strings.Contains(s, bundleAgentVar) {
		if agentRef == "" {
			return "", fmt.Errorf("bundle uses %s but jenkins.agentImage is empty", bundleAgentVar)
		}
		s = strings.ReplaceAll(s, bundleAgentVar, agentRef)
	}
	if i := strings.Index(s, "${"); i >= 0 {
		end := strings.Index(s[i:], "}")
		name := s[i:]
		if end >= 0 {
			name = s[i : i+end+1]
		}
		return "", fmt.Errorf("image %q uses unresolved variable %s: name the image literally", s, name)
	}
	return s, nil
}

// planImages attributes every image source. A chart image must match at least
// one chart value, or it fails with "has no known source". A profile, bundle,
// or flag image is accepted as given. An image that several sources name keeps
// every values key that names it. It returns the planned images sorted by
// canonical identity, with duplicates merged.
func planImages(sources []imageSource, valueRefs map[string]string) ([]plannedImage, error) {
	keys := make([]string, 0, len(valueRefs))
	for k := range valueRefs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	byID := map[string]*plannedImage{}
	for _, src := range sources {
		ref, err := parseImageRef(src.Raw)
		if err != nil {
			return nil, err
		}
		if err := validateImageRef(ref); err != nil {
			return nil, err
		}
		if !oci.IsValidRepoPath(ref.Path) {
			return nil, fmt.Errorf("image %s has a repository path that is not a valid OCI name", src.Raw)
		}
		var valuesKeys []string
		if src.ValuesKey != "" {
			valuesKeys = []string{src.ValuesKey}
		}
		if src.Origin == "chart" && src.ValuesKey == "" {
			for _, k := range keys {
				if r, err := parseImageRef(valueRefs[k]); err == nil && r.canonical() == ref.canonical() {
					valuesKeys = append(valuesKeys, k)
				}
			}
			if len(valuesKeys) == 0 {
				where := src.Where
				if where == "" {
					where = "the chart"
				}
				return nil, fmt.Errorf("image %s (%s) has no known source: no chart value names it", src.Raw, where)
			}
		}
		id := ref.canonical()
		if existing, ok := byID[id]; ok {
			existing.ValuesKeys = mergeKeys(existing.ValuesKeys, valuesKeys)
			continue
		}
		byID[id] = &plannedImage{Ref: ref, Origin: src.Origin, ValuesKeys: mergeKeys(nil, valuesKeys)}
	}

	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]plannedImage, 0, len(ids))
	for _, id := range ids {
		out = append(out, *byID[id])
	}
	return out, nil
}

// mergeKeys returns the union of two key lists, sorted, without repeats.
func mergeKeys(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range append(append([]string{}, a...), b...) {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// layoutTagForImage is the layout tag an image copy writes under. It is a
// fixed function of the image identity.
func layoutTagForImage(ref imageRef) string {
	sum := sha256.Sum256([]byte(ref.canonical()))
	return fmt.Sprintf("img-%x", sum[:6])
}
