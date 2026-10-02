package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

func init() {
	registerRootCommand(addLocksGroup)
}

// addLocksGroup registers the top-level "locks" command group. list reads
// the fleet locks, and release force-releases a releasing fleet lock
// through POST /locks/release.
func addLocksGroup(root *cobra.Command) {
	locksCmd := &cobra.Command{
		Use:   "locks",
		Short: "Manage fleet locks",
	}

	listCmd := &cobra.Command{
		Use:   "list [--team TEAM]",
		Short: "List the fleet locks of every tenant, or of one team",
		Long: "List the fleet locks. With no flag, the command reads every lock of every cluster\n" +
			"through GET /locks, which needs the cluster-wide fleetlocks:read verb. With --team,\n" +
			"it reads the locks of one team's tenants through GET /pipelines/{team}/locks.",
		Args: cobra.NoArgs,
		RunE: runLocksList,
	}
	listCmd.Flags().String("team", "", "list only the locks of this team's tenants")
	locksCmd.AddCommand(listCmd)

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

// runLocksList implements "varroactl locks list".
func runLocksList(cmd *cobra.Command, _ []string) error {
	team, _ := cmd.Flags().GetString("team")
	team = strings.TrimSpace(team)

	c, err := apiClient(cmd)
	if err != nil {
		return err
	}
	var (
		items  []client.ComponentsSchemasFleetLock
		status int
		body   []byte
		ok     bool
	)
	if team == "" {
		resp, err := c.ListAllFleetLocksWithResponse(cmd.Context())
		if err != nil {
			return err
		}
		status, body = resp.StatusCode(), resp.Body
		if ok = resp.JSON200 != nil; ok {
			items = resp.JSON200.Items
		}
	} else {
		resp, err := c.ListTeamFleetLocksWithResponse(cmd.Context(), team)
		if err != nil {
			return err
		}
		status, body = resp.StatusCode(), resp.Body
		if ok = resp.JSON200 != nil; ok {
			items = resp.JSON200.Items
		}
	}
	if status >= 400 || !ok {
		return errFromResponse(body, status)
	}
	return renderFleetLocks(cmd, items)
}

// renderFleetLocks applies the uniform output formats. The name format
// prints the lock name of each row. A lock name repeats on its holder row
// and each waiting row, so a script that needs the run uses -o json.
func renderFleetLocks(cmd *cobra.Command, items []client.ComponentsSchemasFleetLock) error {
	out := cmd.OutOrStdout()
	o, _ := cmd.Flags().GetString("output")
	noHeaders, _ := cmd.Flags().GetBool("no-headers")
	if items == nil {
		items = []client.ComponentsSchemasFleetLock{}
	}

	switch o {
	case "json":
		return printJSON(out, items)
	case "yaml":
		return printYAML(out, items)
	case "name":
		for _, item := range items {
			_, _ = fmt.Fprintln(out, item.Name)
		}
		return nil
	}

	wide := o == "wide"
	headers := []string{"NAME", "STATE", "RUN", "TENANT", "CLUSTER", "JOB", "AGE"}
	if wide {
		headers = append(headers, "SINCE", "LINK")
	}
	rows := make([][]string, len(items))
	for i, item := range items {
		since := fleetLockSince(item)
		age, sinceText := "-", "-"
		if since != nil {
			age, sinceText = formatAge(*since), since.UTC().Format(time.RFC3339)
		}
		row := []string{item.Name, string(item.State), item.RunId, item.Tenant, item.Cluster, item.JobPath, age}
		if wide {
			link := "-"
			if item.RunLink != nil && *item.RunLink != "" {
				link = *item.RunLink
			}
			row = append(row, sinceText, link)
		}
		rows[i] = row
	}
	printTable(out, headers, rows, noHeaders)
	return nil
}

// fleetLockSince answers the time a row's current state began: the wait
// start of a waiting row, and the acquisition of a held or releasing row.
func fleetLockSince(item client.ComponentsSchemasFleetLock) *time.Time {
	if item.State == client.Waiting {
		return item.WaitingSince
	}
	return item.AcquiredAt
}
