package bundle

import (
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// PinConflict is a bundle-pinned plugin whose version differs from the
// resolved set's version for the same artifact.
type PinConflict struct {
	ArtifactID    string
	BundleVersion string
	SetVersion    string
}

// MissingPlugin is a bundle-pinned plugin whose artifact ID is absent from
// the resolved set. Advisory only — never a conflict.
type MissingPlugin struct {
	ArtifactID    string
	BundleVersion string
}

// PinPreflightReport is the result of comparing a bundle's plugin pins
// against a resolved plugin set.
type PinPreflightReport struct {
	Conflicts []PinConflict
	Missing   []MissingPlugin
}

// CheckPluginPins compares a bundle's composed unresolved plugins.yaml
// content against a caller-supplied resolved plugin set. It does not read or
// resolve a version profile itself — the set is always a parameter.
func CheckPluginPins(pluginsYAML string, set []pluginlock.PluginEntry) (PinPreflightReport, error) {
	pins, err := ParsePluginPins(pluginsYAML)
	if err != nil {
		return PinPreflightReport{}, err
	}

	byArtifact := make(map[string]string, len(set))
	for _, e := range set {
		byArtifact[e.ArtifactID] = e.Version
	}

	var report PinPreflightReport
	for _, p := range pins {
		setVersion, ok := byArtifact[p.ArtifactID]
		if !ok {
			report.Missing = append(report.Missing, MissingPlugin{
				ArtifactID:    p.ArtifactID,
				BundleVersion: p.Version,
			})
			continue
		}
		if ucmeta.IsLatest(p.Version) {
			continue
		}
		if p.Version != setVersion {
			report.Conflicts = append(report.Conflicts, PinConflict{
				ArtifactID:    p.ArtifactID,
				BundleVersion: p.Version,
				SetVersion:    setVersion,
			})
		}
	}
	return report, nil
}
