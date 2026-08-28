package controller

import (
	"sort"
	"strings"

	v1alpha1 "github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// MatchKind represents how a JenkinsVersionProfile matched a controller version.
type MatchKind int

const (
	// MatchExact means a profile with spec.version == version was found.
	MatchExact MatchKind = iota
	// MatchLine means a profile with spec.version == lineKey(version) was found
	// (e.g. version "2.479.3" matched a profile with spec.version "2.479").
	MatchLine
	// MatchBaseline means no matching profile was found; the embedded baseline
	// should be used.
	MatchBaseline
)

// lineKey strips the patch segment of a 3-segment version, returning
// major.minor (e.g. "2.479.3" -> "2.479"). For versions with fewer or
// more than 3 segments it returns "" (weekly versions have no line peers).
func lineKey(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) == 3 {
		return parts[0] + "." + parts[1]
	}
	return ""
}

// ResolveProfile picks the best-matching JenkinsVersionProfile for a given
// controller version, following the exact → line → baseline ladder.
// Exact always wins over line. Empty / "lts" returns (nil, MatchBaseline).
// When multiple profiles match the same key, the one with the alphabetically
// smallest name is returned (deterministic tiebreak).
func ResolveProfile(version string, profiles []*v1alpha1.JenkinsVersionProfile) (*v1alpha1.JenkinsVersionProfile, MatchKind) {
	v := strings.TrimSpace(version)
	if v == "" || v == "lts" {
		return nil, MatchBaseline
	}

	// Pass 1: exact match.
	var exactCandidates []*v1alpha1.JenkinsVersionProfile
	for _, p := range profiles {
		if p.Spec.Version == v {
			exactCandidates = append(exactCandidates, p)
		}
	}
	if len(exactCandidates) > 0 {
		sort.Slice(exactCandidates, func(i, j int) bool {
			return exactCandidates[i].Name < exactCandidates[j].Name
		})
		return exactCandidates[0], MatchExact
	}

	// Pass 2: line match (only if version has 3 segments).
	lk := lineKey(v)
	if lk != "" {
		var lineCandidates []*v1alpha1.JenkinsVersionProfile
		for _, p := range profiles {
			if p.Spec.Version == lk {
				lineCandidates = append(lineCandidates, p)
			}
		}
		if len(lineCandidates) > 0 {
			sort.Slice(lineCandidates, func(i, j int) bool {
				return lineCandidates[i].Name < lineCandidates[j].Name
			})
			return lineCandidates[0], MatchLine
		}
	}

	return nil, MatchBaseline
}
