package main

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

// ---------------------------------------------------------------------------
// Argument resolution helpers
// ---------------------------------------------------------------------------

// resolveNSName resolves a positional NS/NAME argument with -n and context
// default namespace.
//
// Forms:
//   - "NS/NAME" (exactly one slash) → ns=NS, name=NAME
//   - "NAME" with -n NS → ns=NS, name=NAME
//   - "NAME" with context defaultNS → ns=defaultNS, name=NAME
//
// Conflict: "NS/NAME" with a different -n → usageError.
// No namespace resolved → usageError "namespace required".
func resolveNSName(arg string, nFlag string, defaultNS string) (ns string, name string, err error) {
	if strings.Contains(arg, "/") {
		parts := strings.SplitN(arg, "/", 2)
		ns, name = parts[0], parts[1]
		if nFlag != "" && nFlag != ns {
			return "", "", usagef("namespace conflict: %q from -n does not match %q in %q", nFlag, ns, arg)
		}
	} else {
		name = arg
		ns = nFlag
		if ns == "" {
			ns = defaultNS
		}
	}

	if ns == "" {
		return "", "", usagef("namespace required")
	}
	return ns, name, nil
}

// resolveListNamespace resolves the namespace parameter for list operations.
//
//   - -n set → that namespace
//   - -A set → "" (all namespaces, no query param)
//   - neither → defaultNS (which may be "")
func resolveListNamespace(nFlag string, allFlag bool, defaultNS string) string {
	if allFlag {
		return ""
	}
	if nFlag != "" {
		return nFlag
	}
	return defaultNS
}

// resolveCluster resolves the effective cluster for single-resource
// addressing (a cluster path segment is always required).
// Precedence: --cluster flag > context defaultCluster > "core".
func resolveCluster(clusterFlag, defaultCluster string) string {
	if clusterFlag != "" {
		return clusterFlag
	}
	if defaultCluster != "" {
		return defaultCluster
	}
	return "core"
}

// resolveListCluster resolves the ?cluster= filter for aggregated list
// views. "" means no filter (all clusters). --all-clusters forces "",
// and is mutually exclusive with --cluster.
func resolveListCluster(clusterFlag string, allClusters bool, defaultCluster string) (string, error) {
	if allClusters && clusterFlag != "" {
		return "", usagef("--cluster and --all-clusters are mutually exclusive")
	}
	if allClusters {
		return "", nil
	}
	if clusterFlag != "" {
		return clusterFlag, nil
	}
	return defaultCluster, nil
}

// addClusterFlag registers the --cluster flag on a cobra command.
func addClusterFlag(cmd *cobra.Command) {
	cmd.Flags().String("cluster", "", "target cluster (default: context cluster, else core)")
}

// addForceFlag registers --force on a command that writes a Controller spec.
// Varroa applies those writes as the field manager "varroa-ui", so changing a
// field some other manager owns — a stray kubectl apply/patch, for instance —
// is refused with a field conflict. Without this the conflict is a dead end
// from the CLI.
func addForceFlag(cmd *cobra.Command) {
	cmd.Flags().Bool("force", false,
		"override field-ownership conflicts, taking ownership of the conflicting fields")
}

// forceParams builds the PATCH parameters from --force. The Force field is left
// nil unless the flag was set, so the request carries no force query parameter
// at all in the common case.
func forceParams(cmd *cobra.Command) *client.PatchControllerParams {
	p := &client.PatchControllerParams{}
	if force, _ := cmd.Flags().GetBool("force"); force {
		p.Force = &force
	}
	return p
}
