package pluginresolve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/pluginver"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// fakeEntry is the one version a fakeMetaSource lists for a plugin, mirroring
// a real metadata source's one-version-per-plugin property.
type fakeEntry struct {
	version      string
	requiredCore string
	deps         []ucmeta.Dep
	degraded     bool
}

// fakeMetaSource is an in-memory MetadataSource test double. calls records
// every "name@minVersion" it was asked to resolve, in order.
type fakeMetaSource struct {
	plugins map[string]fakeEntry
	calls   []string
}

func (s *fakeMetaSource) Resolve(_ context.Context, name, minVersion string) ucmeta.Resolution {
	s.calls = append(s.calls, name+"@"+minVersion)
	e, ok := s.plugins[name]
	if !ok {
		return ucmeta.Resolution{Outcome: ucmeta.NotListed}
	}
	if e.degraded {
		return ucmeta.Resolution{Outcome: ucmeta.SourcesDegraded}
	}
	if !pluginver.AtLeast(e.version, minVersion) {
		return ucmeta.Resolution{Outcome: ucmeta.NotListed}
	}
	return ucmeta.Resolution{Outcome: ucmeta.Resolved, Meta: ucmeta.PluginMeta{
		Name:         name,
		Version:      e.version,
		RequiredCore: e.requiredCore,
		Dependencies: e.deps,
	}}
}

func assertPins(t *testing.T, got Closure, want []PluginPin) {
	t.Helper()
	if len(got.Plugins) != len(want) {
		t.Fatalf("Plugins = %+v, want %+v", got.Plugins, want)
	}
	for i, w := range want {
		g := got.Plugins[i]
		if g.ArtifactID != w.ArtifactID || g.Version != w.Version {
			t.Errorf("Plugins[%d] = %+v, want ArtifactID %q Version %q", i, g, w.ArtifactID, w.Version)
		}
	}
}

func TestResolve_LinearChain(t *testing.T) {
	source := &fakeMetaSource{plugins: map[string]fakeEntry{
		"a": {version: "1.0", deps: []ucmeta.Dep{{Name: "b", Version: "1.0"}}},
		"b": {version: "1.0"},
	}}
	closure, err := Resolve(context.Background(), "2.479.3", []string{"a"}, source)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertPins(t, closure, []PluginPin{
		{ArtifactID: "a", Version: "1.0"},
		{ArtifactID: "b", Version: "1.0"},
	})
}

func TestResolve_DiamondReRaisesSharedDependency(t *testing.T) {
	source := &fakeMetaSource{plugins: map[string]fakeEntry{
		"a":      {version: "1.0", deps: []ucmeta.Dep{{Name: "shared", Version: "1.0"}}},
		"b":      {version: "1.0", deps: []ucmeta.Dep{{Name: "shared", Version: "3.0"}}},
		"shared": {version: "5.0"},
	}}
	closure, err := Resolve(context.Background(), "2.479.3", []string{"a", "b"}, source)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertPins(t, closure, []PluginPin{
		{ArtifactID: "a", Version: "1.0"},
		{ArtifactID: "b", Version: "1.0"},
		{ArtifactID: "shared", Version: "5.0"},
	})

	// Each diamond arm raises "shared" once; a visited-set walk would dedup
	// the second raise away and resolve it only once. The worklist must not:
	// it re-enqueues on every raise, so "shared" is resolved via both arms.
	sharedCalls := 0
	for _, c := range source.calls {
		if strings.HasPrefix(c, "shared@") {
			sharedCalls++
		}
	}
	if sharedCalls != 2 {
		t.Errorf("shared resolved %d times, want 2 (once per diamond arm)", sharedCalls)
	}
}

func TestResolve_OptionalDependencyExcluded(t *testing.T) {
	source := &fakeMetaSource{plugins: map[string]fakeEntry{
		"a": {version: "1.0", deps: []ucmeta.Dep{{Name: "b", Version: "1.0", Optional: true}}},
		// "b" is deliberately absent from the source: if the optional
		// dependency were raised, Resolve would fail with ErrUnresolved.
	}}
	closure, err := Resolve(context.Background(), "2.479.3", []string{"a"}, source)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertPins(t, closure, []PluginPin{{ArtifactID: "a", Version: "1.0"}})
}

func TestResolve_CoreFloorExceeded(t *testing.T) {
	source := &fakeMetaSource{plugins: map[string]fakeEntry{
		"a": {version: "1.0", requiredCore: "2.999"},
	}}
	_, err := Resolve(context.Background(), "2.479.3", []string{"a"}, source)
	if !errors.Is(err, ErrCoreFloorExceeded) {
		t.Fatalf("err = %v, want ErrCoreFloorExceeded", err)
	}
}

func TestResolve_EmptyRequiredCoreSkipsGate(t *testing.T) {
	source := &fakeMetaSource{plugins: map[string]fakeEntry{
		"a": {version: "1.0"}, // RequiredCore is empty
	}}
	// A target far below any real Jenkins version would trip the floor if it
	// were checked; an empty RequiredCore must skip the gate instead.
	if _, err := Resolve(context.Background(), "1.0.0", []string{"a"}, source); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

func TestResolve_UnparseableTargetFailsPreLookup(t *testing.T) {
	source := &fakeMetaSource{plugins: map[string]fakeEntry{"a": {version: "1.0"}}}
	_, err := Resolve(context.Background(), "not-a-version", []string{"a"}, source)
	if !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("err = %v, want ErrInvalidVersion", err)
	}
	if len(source.calls) != 0 {
		t.Errorf("target validation must run before any lookup, got calls %v", source.calls)
	}
}

func TestResolve_NameAbsentFailsUnresolved(t *testing.T) {
	source := &fakeMetaSource{plugins: map[string]fakeEntry{}}
	_, err := Resolve(context.Background(), "2.479.3", []string{"missing"}, source)
	if !errors.Is(err, ErrUnresolved) {
		t.Fatalf("err = %v, want ErrUnresolved", err)
	}
}

func TestResolve_SourcesDegradedFailsMetadataUnavailable(t *testing.T) {
	source := &fakeMetaSource{plugins: map[string]fakeEntry{"flaky": {degraded: true}}}
	_, err := Resolve(context.Background(), "2.479.3", []string{"flaky"}, source)
	if !errors.Is(err, ErrMetadataUnavailable) {
		t.Fatalf("err = %v, want ErrMetadataUnavailable", err)
	}
}

func TestResolve_MaxDepthOverflow(t *testing.T) {
	plugins := map[string]fakeEntry{}
	const chainLen = maxResolveDepth + 5
	for i := 0; i < chainLen; i++ {
		name := fmt.Sprintf("p%d", i)
		entry := fakeEntry{version: "1.0"}
		if i+1 < chainLen {
			entry.deps = []ucmeta.Dep{{Name: fmt.Sprintf("p%d", i+1), Version: "1.0"}}
		}
		plugins[name] = entry
	}
	source := &fakeMetaSource{plugins: plugins}
	_, err := Resolve(context.Background(), "2.479.3", []string{"p0"}, source)
	if !errors.Is(err, ErrTooDeep) {
		t.Fatalf("err = %v, want ErrTooDeep", err)
	}
}

// TestResolve_PluginVersionComparatorMustBeUsed guards against jenkinsver and
// pluginver being confused for one another (both parse dotted-numeric
// strings, but only one of them understands a plugin's "v"-qualifier
// segment). "534.vNN" is a real Jenkins plugin version shape:
// jenkinsver.Core cannot parse the "vNN" segment, so this case only resolves
// correctly if the worklist compares minimums with pluginver.
func TestResolve_PluginVersionComparatorMustBeUsed(t *testing.T) {
	source := &fakeMetaSource{plugins: map[string]fakeEntry{
		"a":      {version: "1.0", deps: []ucmeta.Dep{{Name: "mailer", Version: "534.v1"}}},
		"b":      {version: "1.0", deps: []ucmeta.Dep{{Name: "mailer", Version: "534.v20"}}},
		"mailer": {version: "534.v99"},
	}}
	closure, err := Resolve(context.Background(), "2.479.3", []string{"a", "b"}, source)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertPins(t, closure, []PluginPin{
		{ArtifactID: "a", Version: "1.0"},
		{ArtifactID: "b", Version: "1.0"},
		{ArtifactID: "mailer", Version: "534.v99"},
	})
}
