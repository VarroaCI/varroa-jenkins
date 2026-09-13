package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

func init() {
	registerNoun(addCellPoolCommands)
	registerRootCommand(addScaleCommand)
}

// addCellPoolCommands attaches the cell-pool noun to the get and describe
// verb parents. There is no create and no delete: the API serves no such
// route, and a pool is created and deleted with kubectl or through GitOps.
func addCellPoolCommands(v *verbCommands) {
	v.get.AddCommand(&cobra.Command{
		Use:     "cellpool [NAME]",
		Aliases: []string{"cellpools"},
		Short:   "Get cell pool(s)",
		Args:    cobra.MaximumNArgs(1),
		RunE:    runGetCellPools,
	})
	v.describe.AddCommand(&cobra.Command{
		Use:     "cellpool NAME",
		Aliases: []string{"cellpools"},
		Short:   "Describe a cell pool",
		Args:    cobra.ExactArgs(1),
		RunE:    runDescribeCellPool,
	})
}

// addScaleCommand registers the top-level verb scale. It is registered from
// this noun file through the root-level attachment seam, so no framework file
// changes.
func addScaleCommand(root *cobra.Command) {
	scale := &cobra.Command{
		Use:   "scale",
		Short: "Scale a resource",
	}
	scale.AddCommand(newScaleCellPoolCmd())
	root.AddCommand(scale)
}

func newScaleCellPoolCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "cellpool NAME",
		Aliases: []string{"cellpools"},
		Short:   "Scale a cell pool",
		Args:    cobra.ExactArgs(1),
		RunE:    runScaleCellPool,
	}
	cmd.Flags().Int32("warm", 0, "Target count of free cells the pool keeps ready")
	cmd.Flags().Int32("max-cells", 0, "Cap on the pool's cells, placed cells included. 0 means unlimited")
	return cmd
}

// refuseCellPoolNamespaceFlags rejects a namespace flag before any request.
// CellPool is cluster-scoped, so a namespace has no meaning here.
func refuseCellPoolNamespaceFlags(cmd *cobra.Command) error {
	if n, _ := cmd.Flags().GetString("namespace"); n != "" {
		return usagef("--namespace/-n is not supported for cell pools")
	}
	if all, _ := cmd.Flags().GetBool("all-namespaces"); all {
		return usagef("--all-namespaces/-A is not supported for cell pools")
	}
	return nil
}

func runGetCellPools(cmd *cobra.Command, args []string) error {
	if err := refuseCellPoolNamespaceFlags(cmd); err != nil {
		return err
	}
	c, err := apiClient(cmd)
	if err != nil {
		return err
	}

	if len(args) == 1 {
		pool, err := fetchCellPool(cmd, c, args[0])
		if err != nil {
			return err
		}
		return renderCellPools(cmd, []client.ComponentsSchemasCellPool{*pool})
	}

	resp, err := c.ListCellPoolsWithResponse(cmd.Context())
	if err != nil {
		return err
	}
	if resp.HTTPResponse.StatusCode >= 400 {
		return errFromResponse(resp.Body, resp.HTTPResponse.StatusCode)
	}
	if resp.JSON200 == nil {
		return fmt.Errorf("unexpected response: no data")
	}
	return renderCellPools(cmd, resp.JSON200.Items)
}

func runDescribeCellPool(cmd *cobra.Command, args []string) error {
	if err := refuseCellPoolNamespaceFlags(cmd); err != nil {
		return err
	}
	c, err := apiClient(cmd)
	if err != nil {
		return err
	}
	pool, err := fetchCellPool(cmd, c, args[0])
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Name:                %s\n", pool.Name)
	fmt.Fprintf(out, "Profile:             %s\n", pool.Spec.ProfileRef)
	fmt.Fprintf(out, "Warm:                %s\n", int32PtrString(pool.Spec.Warm))
	fmt.Fprintf(out, "Max cells:           %s\n", int32PtrString(pool.Spec.MaxCells))
	fmt.Fprintf(out, "Max tenants per cell: %s\n", int32PtrString(pool.Spec.MaxTenantsPerCell))
	if pool.Spec.UnreadyGracePeriod != nil && *pool.Spec.UnreadyGracePeriod != "" {
		fmt.Fprintf(out, "Unready grace:       %s\n", *pool.Spec.UnreadyGracePeriod)
	}
	fmt.Fprintf(out, "Warm cells:          %d\n", pool.Status.WarmCells)
	fmt.Fprintf(out, "Placed cells:        %d\n", pool.Status.PlacedCells)
	fmt.Fprintf(out, "Unready cells:       %d\n", pool.Status.UnreadyCells)
	fmt.Fprintf(out, "Free tenant slots:   %d\n", pool.Status.FreeTenantSlots)

	_, _ = fmt.Fprintln(out, "Tenants:")
	if pool.Status.Tenants == nil || len(*pool.Status.Tenants) == 0 {
		_, _ = fmt.Fprintln(out, "  <none>")
	} else {
		for _, tenant := range *pool.Status.Tenants {
			fmt.Fprintf(out, "  %s\n", tenant)
		}
	}

	_, _ = fmt.Fprintln(out, "Conditions:")
	if pool.Status.Conditions == nil || len(*pool.Status.Conditions) == 0 {
		_, _ = fmt.Fprintln(out, "  <none>")
		return nil
	}
	for _, cond := range *pool.Status.Conditions {
		fmt.Fprintf(out, "  %s=%s", cond.Type, cond.Status)
		if cond.Reason != nil && *cond.Reason != "" {
			fmt.Fprintf(out, " reason=%s", *cond.Reason)
		}
		if cond.Message != nil && *cond.Message != "" {
			fmt.Fprintf(out, " message=%s", *cond.Message)
		}
		_, _ = fmt.Fprintln(out)
	}
	return nil
}

func runScaleCellPool(cmd *cobra.Command, args []string) error {
	if err := refuseCellPoolNamespaceFlags(cmd); err != nil {
		return err
	}

	warmSet := cmd.Flags().Changed("warm")
	maxSet := cmd.Flags().Changed("max-cells")
	if !warmSet && !maxSet {
		return usagef("name at least one of --warm or --max-cells")
	}

	var spec struct {
		MaxCells           *int32  `json:"maxCells,omitempty"`
		MaxTenantsPerCell  *int32  `json:"maxTenantsPerCell,omitempty"`
		UnreadyGracePeriod *string `json:"unreadyGracePeriod,omitempty"`
		Warm               *int32  `json:"warm,omitempty"`
	}
	if warmSet {
		warm, err := cmd.Flags().GetInt32("warm")
		if err != nil {
			return err
		}
		if warm < 0 {
			return usagef("--warm must be 0 or above")
		}
		spec.Warm = &warm
	}
	if maxSet {
		maxCells, err := cmd.Flags().GetInt32("max-cells")
		if err != nil {
			return err
		}
		if maxCells < 0 {
			return usagef("--max-cells must be 0 or above")
		}
		spec.MaxCells = &maxCells
	}

	c, err := apiClient(cmd)
	if err != nil {
		return err
	}
	body := client.PatchCellPoolJSONRequestBody{Spec: spec}
	resp, err := c.PatchCellPoolWithResponse(cmd.Context(), args[0], body)
	if err != nil {
		return err
	}
	if resp.HTTPResponse.StatusCode >= 400 {
		return errFromResponse(resp.Body, resp.HTTPResponse.StatusCode)
	}
	if resp.JSON200 == nil {
		return fmt.Errorf("unexpected response: no data")
	}
	return renderCellPools(cmd, []client.ComponentsSchemasCellPool{*resp.JSON200})
}

func fetchCellPool(cmd *cobra.Command, c *client.ClientWithResponses, name string) (*client.ComponentsSchemasCellPool, error) {
	resp, err := c.GetCellPoolWithResponse(cmd.Context(), name)
	if err != nil {
		return nil, err
	}
	if resp.HTTPResponse.StatusCode >= 400 {
		return nil, errFromResponse(resp.Body, resp.HTTPResponse.StatusCode)
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("unexpected response: no data")
	}
	return resp.JSON200, nil
}

// renderCellPools applies the uniform output formats.
func renderCellPools(cmd *cobra.Command, items []client.ComponentsSchemasCellPool) error {
	out := cmd.OutOrStdout()
	o, _ := cmd.Flags().GetString("output")
	noHeaders, _ := cmd.Flags().GetBool("no-headers")

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

	headers := []string{"NAME", "PROFILE", "WARM", "PLACED", "FREE", "READY"}
	rows := make([][]string, len(items))
	for i, item := range items {
		rows[i] = []string{
			item.Name,
			item.Spec.ProfileRef,
			strconv.Itoa(int(item.Status.WarmCells)),
			strconv.Itoa(int(item.Status.PlacedCells)),
			strconv.Itoa(int(item.Status.FreeTenantSlots)),
			cellPoolReadyCell(item),
		}
	}
	printTable(out, headers, rows, noHeaders)
	return nil
}

// cellPoolReadyCell renders the READY column from the PoolReady condition.
func cellPoolReadyCell(pool client.ComponentsSchemasCellPool) string {
	if pool.Status.Conditions == nil {
		return "Unknown"
	}
	for _, cond := range *pool.Status.Conditions {
		if cond.Type != "PoolReady" {
			continue
		}
		if cond.Status == "True" {
			return "True"
		}
		if cond.Reason != nil && *cond.Reason != "" {
			return "False/" + *cond.Reason
		}
		return "False"
	}
	return "Unknown"
}

// int32PtrString renders an optional int32 spec field, which the generated
// client models as a pointer.
func int32PtrString(v *int32) string {
	if v == nil {
		return "0"
	}
	return strconv.Itoa(int(*v))
}
