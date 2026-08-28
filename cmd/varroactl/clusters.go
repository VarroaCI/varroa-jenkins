package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

func init() {
	registerNoun(addClusterCommands)
	registerRootCommand(addDrainCommand)
}

func addClusterCommands(v *verbCommands) {
	getCl := &cobra.Command{
		Use:     "cluster [NAME]",
		Aliases: []string{"clusters"},
		Short:   "Get cluster(s)",
		RunE:    runGetClusters,
	}
	v.get.AddCommand(getCl)
}

func runGetClusters(cmd *cobra.Command, args []string) error {
	// Reject -n and -A (cluster-scoped noun convention)
	nFlag, _ := cmd.Flags().GetString("namespace")
	if nFlag != "" {
		return usagef("--namespace/-n is not supported for clusters")
	}
	aFlag, _ := cmd.Flags().GetBool("all-namespaces")
	if aFlag {
		return usagef("--all-namespaces/-A is not supported for clusters")
	}

	c, err := apiClient(cmd)
	if err != nil {
		return err
	}

	resp, err := c.ListClustersWithResponse(cmd.Context())
	if err != nil {
		return err
	}
	if resp.HTTPResponse.StatusCode >= 400 {
		return errFromResponse(resp.Body, resp.HTTPResponse.StatusCode)
	}
	if resp.JSON200 == nil {
		return fmt.Errorf("unexpected response: no data")
	}

	items := resp.JSON200.Items

	// Single-get by name
	if len(args) == 1 {
		name := args[0]
		var found *client.ComponentsSchemasClusterInfo
		for i := range items {
			if items[i].Name == name {
				found = &items[i]
				break
			}
		}
		if found == nil {
			return fmt.Errorf("cluster %q not found", name)
		}
		return renderCluster(found, cmd)
	}

	// List
	o, _ := cmd.Flags().GetString("output")
	noHeaders, _ := cmd.Flags().GetBool("no-headers")

	switch o {
	case "json":
		return printJSON(os.Stdout, items)
	case "yaml":
		return printYAML(os.Stdout, items)
	case "name":
		for _, item := range items {
			_, _ = fmt.Fprintln(os.Stdout, item.Name)
		}
		return nil
	case "table", "wide":
		headers, rows := clusterListTable(items, o == "wide")
		printTable(os.Stdout, headers, rows, noHeaders)
		return nil
	}
	return nil
}

func renderCluster(c *client.ComponentsSchemasClusterInfo, cmd *cobra.Command) error {
	o, _ := cmd.Flags().GetString("output")
	switch o {
	case "json":
		return printJSON(os.Stdout, c)
	case "yaml":
		return printYAML(os.Stdout, c)
	case "name":
		_, _ = fmt.Fprintln(os.Stdout, c.Name)
		return nil
	case "table", "wide":
		headers, rows := clusterListTable([]client.ComponentsSchemasClusterInfo{*c}, o == "wide")
		printTable(os.Stdout, headers, rows, false)
		return nil
	}
	return nil
}

func clusterListTable(items []client.ComponentsSchemasClusterInfo, wide bool) ([]string, [][]string) {
	headers := []string{"NAME", "STATUS", "STATE", "CONTROLLERS", "CONNECTED", "LAST-HEARTBEAT"}
	if wide {
		headers = append(headers, "OPERATOR-VERSION", "K8S-VERSION")
	}
	rows := make([][]string, len(items))
	for i, item := range items {
		status := "Unhealthy"
		if item.Healthy {
			status = "Healthy"
		}
		state := "Active"
		if item.State != "" {
			// Title-case the state string
			switch item.State {
			case "active":
				state = "Active"
			case "draining":
				state = "Draining"
			case "drained":
				state = "Drained"
			}
		}
		heartbeat := item.LastHeartbeat.Format(time.RFC3339)
		row := []string{item.Name, status, state, strconv.Itoa(int(item.ControllerCount)), strconv.Itoa(int(item.ConnectedCount)), heartbeat}
		if wide {
			row = append(row, item.OperatorVersion, item.K8sVersion)
		}
		rows[i] = row
	}
	return headers, rows
}

// ---------------------------------------------------------------------------
// Drain verb
// ---------------------------------------------------------------------------

func addDrainCommand(root *cobra.Command) {
	drainCmd := &cobra.Command{
		Use:   "drain",
		Short: "Drain resources",
	}
	clusterCmd := &cobra.Command{
		Use:   "cluster NAME [--yes] [--cancel]",
		Short: "Drain or cancel drain of a cluster",
		Args:  cobra.ExactArgs(1),
		RunE:  runDrainCluster,
	}
	clusterCmd.Flags().Bool("yes", false, "Skip confirmation prompt")
	clusterCmd.Flags().Bool("cancel", false, "Cancel an active drain")
	drainCmd.AddCommand(clusterCmd)
	root.AddCommand(drainCmd)
}

func runDrainCluster(cmd *cobra.Command, args []string) error {
	name := args[0]
	cancel, _ := cmd.Flags().GetBool("cancel")
	yes, _ := cmd.Flags().GetBool("yes")

	c, err := apiClient(cmd)
	if err != nil {
		return err
	}

	if cancel {
		resp, err := c.CancelClusterDrainWithResponse(cmd.Context(), name)
		if err != nil {
			return err
		}
		if resp.HTTPResponse.StatusCode >= 400 {
			return errFromResponse(resp.Body, resp.HTTPResponse.StatusCode)
		}
		fmt.Printf("cluster %s: state %s\n", name, *resp.JSON200.State)
		fmt.Fprintln(os.Stderr, "note: controllers already deleting are not restored")
		return nil
	}

	// Non-cancel path: require confirmation.
	if !yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return usagef("refusing to drain without --yes in non-interactive mode")
		}
		fmt.Fprint(os.Stderr, "Type the cluster name to confirm drain: ")
		reader := bufio.NewReader(os.Stdin)
		input, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read confirmation: %w", err)
		}
		input = strings.TrimSpace(input)
		if input != name {
			fmt.Fprintln(os.Stderr, "aborted")
			return usagef("cluster name mismatch")
		}
	}

	body := client.DrainClusterJSONRequestBody{
		Confirm: name,
	}
	resp, err := c.DrainClusterWithResponse(cmd.Context(), name, body)
	if err != nil {
		return err
	}
	if resp.HTTPResponse.StatusCode >= 400 {
		return errFromResponse(resp.Body, resp.HTTPResponse.StatusCode)
	}
	fmt.Printf("cluster %s: state %s\n", name, *resp.JSON202.State)
	return nil
}
