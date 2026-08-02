package api

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

const mintSuffixChars = "bcdfghjklmnpqrstvwxz2456789"

// mintBroodOpName generates a DNS-1123 label name for a brood operation run.
// Format: broodop-<verb>-<suffix> where suffix is 5 random characters.
func mintBroodOpName(verb v1alpha1.BroodVerb) string {
	suffix := make([]byte, 5)
	for i := range suffix {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(mintSuffixChars))))
		if err != nil {
			// Fallback: use a deterministic suffix if crypto/rand fails.
			suffix[i] = mintSuffixChars[i%len(mintSuffixChars)]
		} else {
			suffix[i] = mintSuffixChars[n.Int64()]
		}
	}
	// Lowercase the verb: it is embedded in a k8s object name, which must be a
	// valid DNS-1123 label. executeGroovy is the only camelCase verb; without this
	// its uppercase 'G' produces an invalid name and the create is rejected.
	return fmt.Sprintf("broodop-%s-%s", strings.ToLower(string(verb)), string(suffix))
}

// is3TokenName checks if a name has the form "cluster/ns/name" (3 tokens).
func is3TokenName(name string) bool {
	parts := strings.Split(name, "/")
	return len(parts) == 3
}

// isQualifiedName checks if a name has the form "ns/name" (2 tokens, not bare).
func isQualifiedName(name string) bool {
	parts := strings.Split(name, "/")
	return len(parts) == 2
}

// validateAndPartition validates the create request targeting grammar (§5) and
// partitions per-cluster specs. Returns a map of cluster → BroodOperationSpec
// or an error describing the first violation.
//
// known is a function that returns true for valid cluster names (local + members).
func validateAndPartition(req broodCreateRequest, localCluster string, known func(string) bool, operatorNS string) (map[string]v1alpha1.BroodOperationSpec, error) {
	ns := req.Namespace
	if ns == "" {
		ns = operatorNS
	}

	isTeamMode := ns != operatorNS

	// Track whether the user explicitly set clusters (including empty array).
	explicitClusters := req.Clusters != nil

	// Normalize clusters.
	clusters := req.Clusters
	if !explicitClusters || len(clusters) == 0 {
		clusters = []string{localCluster}
	}

	// Dedup silently.
	seen := make(map[string]bool)
	deduped := make([]string, 0, len(clusters))
	for _, c := range clusters {
		if !seen[c] {
			seen[c] = true
			deduped = append(deduped, c)
		}
	}
	clusters = deduped

	// Check for "all" mixed with explicit entries.
	hasAll := false
	hasExplicit := false
	for _, c := range clusters {
		if c == "all" {
			hasAll = true
		} else {
			hasExplicit = true
		}
	}
	if hasAll && hasExplicit {
		return nil, fmt.Errorf(`"all" cannot be combined with explicit cluster entries`)
	}

	// Expand ["all"] — accepted, caller resolves membership.
	_ = hasAll

	// Parse names to determine targeting mode.
	names := req.Spec.Targets.Names
	has3Token := false
	has2Token := false
	hasBareName := false
	for _, n := range names {
		if is3TokenName(n) {
			has3Token = true
		} else if isQualifiedName(n) {
			has2Token = true
		} else {
			hasBareName = true
		}
	}

	// Rule: mixing qualified and unqualified names.
	if (hasBareName || has2Token) && has3Token {
		return nil, fmt.Errorf("cannot mix cluster-qualified (3-token) names with unqualified names")
	}
	if hasBareName && has2Token {
		return nil, fmt.Errorf("cannot mix bare names with ns/name qualified names")
	}

	// Rule: 3-token names in team mode.
	if isTeamMode && has3Token {
		return nil, fmt.Errorf("cluster-qualified names not allowed in team namespace mode")
	}

	// Rule: clusters alongside qualified names (only when user explicitly set clusters).
	if explicitClusters && has3Token {
		return nil, fmt.Errorf("clusters field cannot be used with cluster-qualified names")
	}

	// Rule: multi-entry clusters with unqualified names (only single-cluster
	// allowed for unqualified).
	if hasBareName || has2Token {
		if len(clusters) > 1 {
			return nil, fmt.Errorf("multiple clusters not allowed with unqualified names")
		}
	}

	// Rule: "all" combined with names mode.
	if hasAll {
		if has3Token || has2Token || hasBareName {
			return nil, fmt.Errorf(`"all" cannot be combined with explicit names`)
		}
	}

	// Rule: team mode with >1 cluster.
	if isTeamMode && len(clusters) > 1 {
		return nil, fmt.Errorf("team namespace mode supports at most one target cluster")
	}

	// Rule: unknown cluster anywhere.
	for _, c := range clusters {
		if c == "all" {
			continue
		}
		if !known(c) {
			return nil, fmt.Errorf("unknown cluster %q", c)
		}
	}

	// Build per-cluster specs.
	result := make(map[string]v1alpha1.BroodOperationSpec)

	if has3Token {
		// Partition 3-token names into per-cluster specs with 2-token remainders.
		clusterNames := make(map[string][]string)
		for _, n := range names {
			parts := strings.SplitN(n, "/", 3)
			c, remainder := parts[0], parts[1]+"/"+parts[2]
			clusterNames[c] = append(clusterNames[c], remainder)
		}
		for c, cNames := range clusterNames {
			spec := req.Spec
			spec.Targets.Names = cNames
			result[c] = spec
		}
	} else {
		// Same spec for all target clusters.
		for _, c := range clusters {
			result[c] = req.Spec
		}
	}

	return result, nil
}
