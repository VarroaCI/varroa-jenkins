package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

func init() {
	registerNoun(func(v *verbCommands) {
		getCmd := &cobra.Command{
			Use:     "broodschedule [NS/NAME]",
			Aliases: []string{"broodschedules", "bs"},
			Short:   "Get brood schedule(s)",
			RunE:    runBroodScheduleGet,
		}
		v.get.AddCommand(getCmd)

		descCmd := &cobra.Command{
			Use:     "broodschedule NS/NAME",
			Aliases: []string{"broodschedules", "bs"},
			Short:   "Describe a brood schedule",
			Args:    cobra.ExactArgs(1),
			RunE:    runBroodScheduleDescribe,
		}
		v.describe.AddCommand(descCmd)

		deleteCmd := &cobra.Command{
			Use:     "broodschedule NS/NAME",
			Aliases: []string{"broodschedules", "bs"},
			Short:   "Delete a brood schedule",
			Args:    cobra.ExactArgs(1),
			RunE:    runBroodScheduleDelete,
		}
		v.delete.AddCommand(deleteCmd)
	})

	registerRootCommand(func(root *cobra.Command) {
		bsCmd := &cobra.Command{
			Use:     "broodschedule",
			Aliases: []string{"broodschedules", "bs"},
			Short:   "Manage brood schedules",
		}

		createCmd := &cobra.Command{
			Use:   "create NAME",
			Short: "Create a brood schedule",
			Long: `Create a brood schedule with a cron trigger.

Required: --verb, --cron.
Targeting: one of --selector / -l, --selector-json, or --names.
Use --dry-run to preview without creating.`,
			Args: cobra.ExactArgs(1),
			RunE: runBroodScheduleCreate,
		}
		createCmd.Flags().StringP("selector", "l", "", "Label selector")
		createCmd.Flags().String("selector-json", "", "Full JSON metav1.LabelSelector (matchLabels + matchExpressions); mutually exclusive with --selector")
		createCmd.Flags().String("names", "", "Comma-separated controller names")
		createCmd.Flags().String("clusters", "", "Comma-separated target clusters, or 'all' (selector mode)")
		createCmd.Flags().StringSlice("filter", nil, "Repeatable: key=value (phase|version|bundle)")
		createCmd.Flags().StringSlice("namespaces", nil, "Namespaces for selector mode (or 'all')")
		createCmd.Flags().String("verb", "", "Operation verb: restart, reprovision, reconcile, stop, start")
		createCmd.Flags().String("cron", "", "Cron expression for the schedule")
		createCmd.Flags().Int("max-parallel", 0, "Max parallel targets")
		createCmd.Flags().String("order", "", "Dispatch order: rolloutWave or name")
		createCmd.Flags().String("failure-policy", "", "Failure policy: FailFast, FailTidy, FailAtEnd")
		createCmd.Flags().Int("ttl", 0, "TTL seconds after the operation finishes")
		createCmd.Flags().Bool("wait-for-completion", true, "Wait for the brood operation to complete")
		createCmd.Flags().String("concurrency-policy", "", "Concurrency policy: Allow, Forbid, Replace")
		createCmd.Flags().Int64("starting-deadline-seconds", 0, "Deadline in seconds for starting the job")
		createCmd.Flags().Int32("successful-jobs-history-limit", 0, "Number of successful finished jobs to retain")
		createCmd.Flags().Int32("failed-jobs-history-limit", 0, "Number of failed finished jobs to retain")
		createCmd.Flags().Bool("dry-run", false, "Preview without creating")
		createCmd.Flags().String("cluster", "", "Target cluster for object residency")
		bsCmd.AddCommand(createCmd)

		getCmd := &cobra.Command{
			Use:   "get [NS/NAME]",
			Short: "Get brood schedule(s)",
			Args:  cobra.MaximumNArgs(1),
			RunE:  runBroodScheduleGet,
		}
		bsCmd.AddCommand(getCmd)

		describeCmd := &cobra.Command{
			Use:   "describe NS/NAME",
			Short: "Describe a brood schedule",
			Args:  cobra.ExactArgs(1),
			RunE:  runBroodScheduleDescribe,
		}
		bsCmd.AddCommand(describeCmd)

		deleteCmd := &cobra.Command{
			Use:   "delete NS/NAME",
			Short: "Delete a brood schedule",
			Args:  cobra.ExactArgs(1),
			RunE:  runBroodScheduleDelete,
		}
		bsCmd.AddCommand(deleteCmd)

		suspendCmd := &cobra.Command{
			Use:   "suspend NS/NAME",
			Short: "Suspend or resume a brood schedule",
			Args:  cobra.ExactArgs(1),
			RunE:  runBroodScheduleSuspend,
		}
		suspendCmd.Flags().Bool("resume", false, "Resume (unsuspend) the schedule")
		bsCmd.AddCommand(suspendCmd)

		root.AddCommand(bsCmd)
	})
}

func runBroodScheduleGet(cmd *cobra.Command, args []string) error {
	cl, err := apiClient(cmd)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return doBroodScheduleList(cmd, cl)
	}
	return doBroodScheduleGetOne(cmd, cl, args[0])
}

func doBroodScheduleList(cmd *cobra.Command, cl *client.ClientWithResponses) error {
	n, _ := cmd.Flags().GetString("namespace")
	a, _ := cmd.Flags().GetBool("all-namespaces")
	ns := resolveListNamespace(n, a, "")
	var p *client.ComponentsParametersNamespace
	if ns != "" {
		v := ns
		p = &v
	}
	resp, err := cl.ListBroodSchedulesWithResponse(context.Background(), &client.ListBroodSchedulesParams{Namespace: p})
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	if resp.JSON200 == nil {
		return errFromResponse(resp.Body, resp.StatusCode())
	}
	o, _ := cmd.Flags().GetString("output")
	nh, _ := cmd.Flags().GetBool("no-headers")
	switch o {
	case "json":
		return printJSON(os.Stdout, resp.JSON200.Items)
	case "yaml":
		return printYAML(os.Stdout, resp.JSON200.Items)
	case "name":
		for _, it := range resp.JSON200.Items {
			nsStr := strVal(it.Namespace)
			nStr := strVal(it.Name)
			fmt.Printf("%s/%s\n", nsStr, nStr)
		}
		return nil
	default:
		var rows [][]string
		for _, it := range resp.JSON200.Items {
			rows = append(rows, broodScheduleRow(it))
		}
		printTable(os.Stdout, []string{"NAMESPACE", "NAME", "SCHEDULE", "SUSPEND", "LAST-SCHEDULE", "CLUSTER"}, rows, nh)
		return nil
	}
}

func broodScheduleRow(it client.ComponentsSchemasBroodScheduleResponse) []string {
	nsStr := strVal(it.Namespace)
	nStr := strVal(it.Name)
	sched := ""
	suspend := ""
	lastSched := ""
	cluster := strVal(it.Cluster)
	if it.Spec != nil {
		sched = it.Spec.Schedule
		if it.Spec.Suspend != nil && *it.Spec.Suspend {
			suspend = "true"
		}
	}
	if it.Status != nil {
		if v, ok := (*it.Status)["lastScheduleTime"].(string); ok {
			lastSched = v
		}
	}
	return []string{nsStr, nStr, sched, suspend, lastSched, cluster}
}

func doBroodScheduleGetOne(cmd *cobra.Command, cl *client.ClientWithResponses, arg string) error {
	n, _ := cmd.Flags().GetString("namespace")
	ns, name, err := resolveNSName(arg, n, "")
	if err != nil {
		return err
	}
	resp, err := cl.GetBroodScheduleWithResponse(context.Background(), ns, name)
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}
	if resp.JSON200 == nil {
		return errFromResponse(resp.Body, resp.StatusCode())
	}
	o, _ := cmd.Flags().GetString("output")
	switch o {
	case "json":
		return printJSON(os.Stdout, resp.JSON200)
	case "yaml":
		return printYAML(os.Stdout, resp.JSON200)
	case "name":
		fmt.Printf("%s/%s\n", strVal(resp.JSON200.Namespace), strVal(resp.JSON200.Name))
		return nil
	default:
		printBroodScheduleDetail(os.Stdout, resp.JSON200)
		return nil
	}
}

func printBroodScheduleDetail(w io.Writer, sched *client.ComponentsSchemasBroodScheduleResponse) {
	fmt.Fprintf(w, "Schedule: %s/%s\n", strVal(sched.Namespace), strVal(sched.Name))
	if sched.Spec != nil {
		fmt.Fprintf(w, "Schedule: %s\n", sched.Spec.Schedule)
		if sched.Spec.Suspend != nil {
			fmt.Fprintf(w, "Suspend: %v\n", *sched.Spec.Suspend)
		}
		if sched.Spec.ConcurrencyPolicy != nil {
			fmt.Fprintf(w, "ConcurrencyPolicy: %s\n", string(*sched.Spec.ConcurrencyPolicy))
		}
		if sched.Spec.StartingDeadlineSeconds != nil {
			fmt.Fprintf(w, "StartingDeadlineSeconds: %d\n", *sched.Spec.StartingDeadlineSeconds)
		}
		if sched.Spec.SuccessfulJobsHistoryLimit != nil {
			fmt.Fprintf(w, "SuccessfulJobsHistoryLimit: %d\n", *sched.Spec.SuccessfulJobsHistoryLimit)
		}
		if sched.Spec.FailedJobsHistoryLimit != nil {
			fmt.Fprintf(w, "FailedJobsHistoryLimit: %d\n", *sched.Spec.FailedJobsHistoryLimit)
		}
		if sched.Spec.WaitForCompletion != nil {
			fmt.Fprintf(w, "WaitForCompletion: %v\n", *sched.Spec.WaitForCompletion)
		}
	}
	if sched.Status != nil {
		fmt.Fprintf(w, "Status:\n")
		for k, v := range *sched.Status {
			fmt.Fprintf(w, "  %s: %v\n", k, v)
		}
	}
}

func runBroodScheduleDescribe(cmd *cobra.Command, args []string) error {
	cl, err := apiClient(cmd)
	if err != nil {
		return err
	}
	return doBroodScheduleGetOne(cmd, cl, args[0])
}

func runBroodScheduleCreate(cmd *cobra.Command, args []string) error {
	name := args[0]

	verb, _ := cmd.Flags().GetString("verb")
	if !validBroodVerbs[verb] {
		return usagef("unknown verb %q: must be restart, reprovision, reconcile, stop, start", verb)
	}
	cron, _ := cmd.Flags().GetString("cron")
	if cron == "" {
		return usagef("--cron is required")
	}

	sel, _ := cmd.Flags().GetString("selector")
	selJSON, _ := cmd.Flags().GetString("selector-json")
	namesStr, _ := cmd.Flags().GetString("names")

	// Three-way mutual exclusion.
	set := 0
	if sel != "" {
		set++
	}
	if selJSON != "" {
		set++
	}
	if namesStr != "" {
		set++
	}
	if set != 1 {
		return usagef("exactly one of --selector / -l, --selector-json, or --names is required")
	}
	if selJSON != "" {
		var probe map[string]interface{}
		if err := json.Unmarshal([]byte(selJSON), &probe); err != nil {
			return usagef("invalid --selector-json: %v", err)
		}
	}

	rawFilters, _ := cmd.Flags().GetStringSlice("filter")
	fm := make(map[string]string)
	for _, f := range rawFilters {
		k, v, ok := strings.Cut(f, "=")
		if !ok || k == "" {
			return usagef("invalid filter %q: must be key=value", f)
		}
		if !validFilterKeys[k] {
			return usagef("unknown filter key %q: must be phase, version, or bundle", k)
		}
		fm[k] = v
	}

	ns, _ := cmd.Flags().GetString("namespace")
	clustersStr, _ := cmd.Flags().GetString("clusters")
	clusters := splitCSV(clustersStr)
	if err := validateClusterTargeting(clusters, namesStr); err != nil {
		return err
	}

	// Build the template via buildSpecMap (reuses 5.2/5.3 logic).
	template := buildSpecMap(verb, sel, selJSON, namesStr, fm, cmd)
	if len(clusters) > 0 {
		template["clusters"] = clusters
	}

	dryRun, _ := cmd.Flags().GetBool("dry-run")

	// Build the spec map.
	specMap := map[string]interface{}{
		"schedule": cron,
		"template": template,
	}

	waitForCompletion, _ := cmd.Flags().GetBool("wait-for-completion")
	specMap["waitForCompletion"] = waitForCompletion

	if cp, _ := cmd.Flags().GetString("concurrency-policy"); cp != "" {
		specMap["concurrencyPolicy"] = cp
	}
	if cmd.Flags().Changed("starting-deadline-seconds") {
		sds, _ := cmd.Flags().GetInt64("starting-deadline-seconds")
		specMap["startingDeadlineSeconds"] = sds
	}
	if cmd.Flags().Changed("successful-jobs-history-limit") {
		sjhl, _ := cmd.Flags().GetInt32("successful-jobs-history-limit")
		specMap["successfulJobsHistoryLimit"] = sjhl
	}
	if cmd.Flags().Changed("failed-jobs-history-limit") {
		fjhl, _ := cmd.Flags().GetInt32("failed-jobs-history-limit")
		specMap["failedJobsHistoryLimit"] = fjhl
	}

	// Convert via JSON round-trip into the typed Spec.
	specJSON, _ := json.Marshal(specMap)
	var typedSpec client.ComponentsSchemasBroodScheduleSpec
	if err := json.Unmarshal(specJSON, &typedSpec); err != nil {
		return usagef("invalid spec: %v", err)
	}

	nsVal := ns
	clusterFlag, _ := cmd.Flags().GetString("cluster")
	body := client.CreateBroodScheduleJSONRequestBody{
		Name: name,
		Spec: typedSpec,
	}
	if nsVal != "" {
		body.Namespace = &nsVal
	}
	if clusterFlag != "" {
		body.Cluster = &clusterFlag
	}

	cl, err := apiClient(cmd)
	if err != nil {
		return err
	}

	if dryRun {
		// Print the body as JSON for preview.
		return printJSON(os.Stdout, body)
	}

	resp, err := cl.CreateBroodScheduleWithResponse(context.Background(), body)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if resp.JSON201 == nil {
		return errFromResponse(resp.Body, resp.StatusCode())
	}
	sched := resp.JSON201
	fmt.Printf("%s/%s\n", strVal(sched.Namespace), strVal(sched.Name))
	return nil
}

func runBroodScheduleDelete(cmd *cobra.Command, args []string) error {
	n, _ := cmd.Flags().GetString("namespace")
	ns, name, err := resolveNSName(args[0], n, "")
	if err != nil {
		return err
	}
	cl, err := apiClient(cmd)
	if err != nil {
		return err
	}
	resp, err := cl.DeleteBroodScheduleWithResponse(context.Background(), ns, name)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if resp.StatusCode() != http.StatusOK {
		return errFromResponse(resp.Body, resp.StatusCode())
	}
	fmt.Printf("deleted %s/%s\n", ns, name)
	return nil
}

func runBroodScheduleSuspend(cmd *cobra.Command, args []string) error {
	n, _ := cmd.Flags().GetString("namespace")
	ns, name, err := resolveNSName(args[0], n, "")
	if err != nil {
		return err
	}
	resume, _ := cmd.Flags().GetBool("resume")
	cl, err := apiClient(cmd)
	if err != nil {
		return err
	}
	suspend := !resume
	body := client.SuspendBroodScheduleJSONRequestBody{Suspend: suspend}
	resp, err := cl.SuspendBroodScheduleWithResponse(context.Background(), ns, name, body)
	if err != nil {
		return fmt.Errorf("suspend: %w", err)
	}
	if resp.JSON200 == nil {
		return errFromResponse(resp.Body, resp.StatusCode())
	}
	if resume {
		fmt.Printf("resumed %s/%s\n", ns, name)
	} else {
		fmt.Printf("suspended %s/%s\n", ns, name)
	}
	return nil
}
