package plugininv

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/hpi"
	"github.com/varroaci/varroa-jenkins/internal/jenkins"
)

// APIDependency is the depth=2 shape of a dependency: an object with name,
// version (the declared minimum), and optional.
type APIDependency struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Optional bool   `json:"optional"`
}

// jenkinsPluginAPI is the narrow seam the collector needs from the Jenkins
// client. It exists so CollectAPI is testable without a real Jenkins.
type jenkinsPluginAPI interface {
	GetPluginManager(ctx context.Context) ([]jenkins.APIPlugin, error)
}

// CollectAPI collects the installed plugin inventory from the Jenkins API.
func CollectAPI(ctx context.Context, api jenkinsPluginAPI) (Inventory, error) {
	plugins, err := api.GetPluginManager(ctx)
	if err != nil {
		return Inventory{}, fmt.Errorf("plugininv: collect from API: %w", err)
	}

	inv := Inventory{
		Source:      SourceJenkinsAPI,
		CollectedAt: time.Now(),
	}

	for _, p := range plugins {
		rec, err := apiPluginToRecord(p)
		if err != nil {
			return Inventory{}, fmt.Errorf("plugininv: plugin %q: %w", p.ShortName, err)
		}
		inv.Records = append(inv.Records, rec)
	}

	return inv, nil
}

// apiPluginToRecord converts a jenkins.APIPlugin to a Record, decoding
// dependencies and rejecting any that are JSON strings (depth=1 shaped).
func apiPluginToRecord(p jenkins.APIPlugin) (Record, error) {
	rec := Record{
		Name:     p.ShortName,
		Version:  p.Version,
		Enabled:  boolToTri(p.Enabled),
		Detached: boolToTri(p.Detached),
		Bundled:  boolToTri(p.Bundled),
	}

	for _, raw := range p.Dependencies {
		// Reject depth=1 shaped responses: if the element is a JSON string,
		// it's a bare plugin name and we're missing the optional flag.
		if len(raw) > 0 && raw[0] == '"' {
			return Record{}, fmt.Errorf(
				"dependency is a JSON string, not an object: response appears to be depth=1 shaped; " +
					"depth=2 is required for the optional flag")
		}

		var dep APIDependency
		if err := json.Unmarshal(raw, &dep); err != nil {
			return Record{}, fmt.Errorf("decode dependency: %w", err)
		}
		rec.Deps = append(rec.Deps, Dep{
			Name:     dep.Name,
			Min:      dep.Version,
			Optional: dep.Optional,
		})
	}

	return rec, nil
}

// CollectFS collects the plugin inventory from the Jenkins home plugins
// directory via fs.FS. It reads *.jpi and *.hpi files at the root and parses
// them with hpi.ParseHPIBytes. Directories are skipped. All three Tri flags
// are set to TriUnknown.
func CollectFS(fsys fs.FS) (Inventory, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return Inventory{}, fmt.Errorf("plugininv: read plugins dir: %w", err)
	}

	inv := Inventory{
		Source:      SourceFilesystem,
		CollectedAt: time.Now(),
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jpi") && !strings.HasSuffix(name, ".hpi") {
			continue
		}

		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			inv.Warnings = append(inv.Warnings,
				fmt.Sprintf("plugininv: read %q: %v", name, err))
			continue
		}

		mf, err := hpi.ParseHPIBytes(data)
		if err != nil {
			inv.Warnings = append(inv.Warnings,
				fmt.Sprintf("plugininv: parse %q: %v", name, err))
			continue
		}

		deps := make([]Dep, len(mf.Dependencies))
		for i, d := range mf.Dependencies {
			deps[i] = Dep{
				Name:     d.Name,
				Min:      d.Min,
				Optional: d.Optional,
			}
		}

		inv.Records = append(inv.Records, Record{
			Name:     mf.ShortName,
			Version:  mf.Version,
			Enabled:  TriUnknown,
			Detached: TriUnknown,
			Bundled:  TriUnknown,
			Deps:     deps,
		})
	}

	return inv, nil
}

// CollectSelection attempts API collection first, then filesystem on any API
// error. If both fail, returns an Inventory with CollectionFailed set and the
// API error text. No sticky negative verdict — every attempt retries the API.
func CollectSelection(ctx context.Context, api jenkinsPluginAPI, fsys fs.FS) Inventory {
	inv, apiErr := CollectAPI(ctx, api)
	if apiErr == nil {
		return inv
	}

	inv, fsErr := CollectFS(fsys)
	if fsErr == nil {
		return inv
	}

	// Both failed.
	return Inventory{
		CollectionFailed: true,
		CollectionError:  apiErr.Error(),
		CollectedAt:      time.Now(),
	}
}

func boolToTri(b bool) Tri {
	if b {
		return TriTrue
	}
	return TriFalse
}
