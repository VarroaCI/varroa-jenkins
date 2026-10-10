package updatecenter

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/hpi"
)

// defaultManifestReadTimeout bounds one stored-manifest read shared by
// concurrent metadata requests. A timeout is a fetch error and is not cached.
const defaultManifestReadTimeout = 30 * time.Second

// storedDeps is one cached manifest read. known is false when the stored
// blob is not a readable plugin manifest.
type storedDeps struct {
	deps  []hpi.Dependency
	known bool
}

// pluginDependencies returns the dependency list for one stored plugin version.
// A layer that carries the dependencies annotation answers from the annotation.
// A layer without it is read once from its stored manifest, and the result is
// cached by layer digest. The second result is false when the list is unknown.
// The metadata document, the inventory, and the upload planner share this order.
func (s *Server) pluginDependencies(ctx context.Context, p pluginLayerInfo) ([]hpi.Dependency, bool) {
	if p.DependenciesKnown {
		return p.Dependencies, true
	}
	if p.LayerDigest == "" {
		return nil, false
	}

	// The cache is checked inside the flight, so a read that finished while this
	// caller waited is reused and never repeated.
	// The shared read runs once for every caller that joins it. Detach it from
	// the first caller's context, so one canceled request cannot fail the others.
	// The timeout bounds the read, because no caller can cancel it.
	v, err, _ := s.depsGroup.Do(p.LayerDigest, func() (any, error) {
		s.depsMu.Lock()
		cached, ok := s.depsCache[p.LayerDigest]
		s.depsMu.Unlock()
		if ok {
			return cached, nil
		}
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.manifestReadTimeout)
		defer cancel()
		deps, known, err := s.readManifestDeps(fetchCtx, p)
		if err != nil {
			// A fetch failure is not cached, so the next request retries.
			return nil, err
		}
		result := storedDeps{deps: deps, known: known}
		s.depsMu.Lock()
		s.depsCache[p.LayerDigest] = result
		s.depsMu.Unlock()
		return result, nil
	})
	if err != nil {
		s.logger.Warn("plugin manifest unavailable; its dependency list is unknown",
			"name", p.Name, "version", p.Version, "digest", p.LayerDigest, "error", err)
		return nil, false
	}
	result := v.(storedDeps)
	return result.deps, result.known
}

// readManifestDeps reads the stored blob of one plugin layer and parses its
// MANIFEST.MF. A blob that is not a readable plugin returns known false and no
// error, which the caller caches. A failed fetch returns an error.
func (s *Server) readManifestDeps(ctx context.Context, p pluginLayerInfo) ([]hpi.Dependency, bool, error) {
	rc, err := s.store.FetchBlob(ctx, p.LayerDigest)
	if err != nil {
		return nil, false, fmt.Errorf("fetch plugin blob: %w", err)
	}
	defer func() { _ = rc.Close() }()

	f, err := os.CreateTemp("", "varroa-uc-deps-*.hpi")
	if err != nil {
		return nil, false, fmt.Errorf("create temp file: %w", err)
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()

	_, copyErr := io.Copy(f, rc)
	closeErr := f.Close()
	if copyErr != nil {
		return nil, false, fmt.Errorf("copy plugin blob: %w", copyErr)
	}
	if closeErr != nil {
		return nil, false, fmt.Errorf("close temp file: %w", closeErr)
	}

	mf, err := hpi.ParseHPIFile(path)
	if err != nil {
		s.logger.Warn("plugin manifest unreadable; its dependency list is unknown",
			"name", p.Name, "version", p.Version, "digest", p.LayerDigest, "error", err)
		return nil, false, nil
	}
	deps := mf.Dependencies
	if deps == nil {
		deps = []hpi.Dependency{}
	}
	return deps, true, nil
}

// storedDependencies resolves the dependency list of one plugin version from
// every layer that stores its bytes. An annotated layer answers first. Otherwise
// the stored manifests are read in turn, one per distinct digest, until one
// answers. The second result is false when no layer gives a known list.
func (s *Server) storedDependencies(ctx context.Context, layers []pluginLayerInfo) ([]hpi.Dependency, bool) {
	for _, l := range layers {
		if l.DependenciesKnown {
			return l.Dependencies, true
		}
	}
	seen := make(map[string]struct{}, len(layers))
	for _, l := range layers {
		if l.LayerDigest == "" {
			continue
		}
		if _, dup := seen[l.LayerDigest]; dup {
			continue
		}
		seen[l.LayerDigest] = struct{}{}
		if deps, known := s.pluginDependencies(ctx, l); known {
			return deps, true
		}
	}
	return nil, false
}
