package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

func init() {
	registerRootCommand(addLocksGroup)
}

// addLocksGroup registers the top-level "locks" command group. Its one
// verb, release, force-releases a releasing fleet lock through POST
// /locks/release.
func addLocksGroup(root *cobra.Command) {
	locksCmd := &cobra.Command{
		Use:   "locks",
		Short: "Manage fleet locks",
	}

	releaseCmd := &cobra.Command{
		Use:   "release NAME --run RUN_ID --reason TEXT",
		Short: "Force-release a releasing fleet lock that no live pod holds",
		Long: "Force-release one releasing fleet lock. The record store refuses a held or waiting lock,\n" +
			"and a lock whose cell instance the placement still runs. Confirm first that the holder's\n" +
			"build runs on no cell. Needs the cluster-wide fleetlocks:release verb.",
		Args: cobra.MaximumNArgs(1),
		RunE: runLocksRelease,
	}
	releaseCmd.Flags().String("run", "", "the run that holds the releasing lock (required)")
	releaseCmd.Flags().String("reason", "", "why the lock is released; the audit record carries it (required)")
	locksCmd.AddCommand(releaseCmd)

	root.AddCommand(locksCmd)
}

// runLocksRelease implements "varroactl locks release NAME". It checks the
// three inputs before any request, so a missing input is a usage error.
func runLocksRelease(cmd *cobra.Command, args []string) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return usagef("lock name is required")
	}
	runID, _ := cmd.Flags().GetString("run")
	reason, _ := cmd.Flags().GetString("reason")
	if strings.TrimSpace(runID) == "" {
		return usagef("--run is required")
	}
	if strings.TrimSpace(reason) == "" {
		return usagef("--reason is required")
	}

	c, err := apiClient(cmd)
	if err != nil {
		return err
	}
	resp, err := c.ReleaseFleetLockWithResponse(cmd.Context(), client.ReleaseFleetLockJSONRequestBody{
		Name: args[0], RunId: runID, Reason: reason,
	})
	if err != nil {
		return err
	}
	if resp.HTTPResponse.StatusCode >= 400 || resp.JSON200 == nil {
		return errFromResponse(resp.Body, resp.StatusCode())
	}
	out := resp.JSON200
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "released fleet lock %s (run %s, tenant %s, cluster %s)\n",
		out.Name, out.RunId, out.Tenant, out.Cluster)
	return nil
}
