package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

var validBroodVerbs = map[string]bool{
	"restart": true, "reprovision": true, "reconcile": true, "stop": true, "start": true,
}

var validFilterKeys = map[string]bool{
	"phase": true, "version": true, "bundle": true,
}

func init() {
	registerNoun(func(v *verbCommands) {
		getCmd := &cobra.Command{
			Use:     "broodop [NS/NAME]",
			Aliases: []string{"broodops", "bo"},
			Short:   "Get brood operation(s)",
			RunE:    runBroodGet,
		}
		v.get.AddCommand(getCmd)

		descCmd := &cobra.Command{
			Use:     "broodop NS/NAME",
			Aliases: []string{"broodops", "bo"},
			Short:   "Describe a brood operation",
			Args:    cobra.ExactArgs(1),
			RunE:    runBroodDescribe,
		}
		v.describe.AddCommand(descCmd)

		deleteCmd := &cobra.Command{
			Use:     "broodop NS/NAME",
			Aliases: []string{"broodops", "bo"},
			Short:   "Cancel a brood operation",
			Args:    cobra.ExactArgs(1),
			RunE:    runBroodCancel,
		}
		v.delete.AddCommand(deleteCmd)
	})

	registerRootCommand(func(root *cobra.Command) {
		broodopCmd := &cobra.Command{
			Use:     "broodop",
			Aliases: []string{"broodops", "bo"},
			Short:   "Manage brood operations",
		}
		runCmd := &cobra.Command{
			Use:   "run <verb>",
			Short: "Run a brood operation",
			Long: `Create or preview a brood operation.

Verbs: restart, reprovision, reconcile, stop, start.

Exactly one targeting mode required: --selector / -l or --names.
Use --dry-run to preview without creating.`,
			Args: cobra.ExactArgs(1),
			RunE: runBroodRun,
		}
		runCmd.Flags().StringP("selector", "l", "", "Label selector")
		runCmd.Flags().String("names", "", "Comma-separated controller names")
		runCmd.Flags().String("clusters", "", "Comma-separated target clusters, or 'all' (selector mode)")
		runCmd.Flags().StringSlice("filter", nil, "Repeatable: key=value (phase|version|bundle)")
		runCmd.Flags().StringSlice("namespaces", nil, "Namespaces for selector mode (or 'all')")
		runCmd.Flags().Int("max-parallel", 0, "Max parallel targets")
		runCmd.Flags().String("order", "", "Dispatch order: rolloutWave or name")
		runCmd.Flags().String("failure-policy", "", "Failure policy: FailFast, FailTidy, FailAtEnd")
		runCmd.Flags().Bool("dry-run", false, "Preview without creating")
		runCmd.Flags().BoolP("watch", "w", false, "Watch after create")
		runCmd.Flags().Int("ttl", 0, "TTL seconds after the operation finishes (maps to spec.ttlSecondsAfterFinished)")
		runCmd.Flags().String("selector-json", "", "Full JSON metav1.LabelSelector (matchLabels + matchExpressions); mutually exclusive with --selector")
		broodopCmd.AddCommand(runCmd)

		suspendCmd := &cobra.Command{
			Use:   "suspend NS/NAME",
			Short: "Suspend or resume a brood operation",
			Args:  cobra.ExactArgs(1),
			RunE:  runBroodSuspend,
		}
		suspendCmd.Flags().Bool("off", false, "Resume (unsuspend)")
		broodopCmd.AddCommand(suspendCmd)

		getCmd := &cobra.Command{
			Use:   "get [NS/NAME]",
			Short: "Get brood operation(s)",
			Args:  cobra.MaximumNArgs(1),
			RunE:  runBroodGet,
		}
		broodopCmd.AddCommand(getCmd)

		describeCmd := &cobra.Command{
			Use:   "describe NS/NAME",
			Short: "Describe a brood operation",
			Args:  cobra.ExactArgs(1),
			RunE:  runBroodDescribe,
		}
		broodopCmd.AddCommand(describeCmd)

		cancelCmd := &cobra.Command{
			Use:   "cancel NS/NAME",
			Short: "Cancel a brood operation",
			Args:  cobra.ExactArgs(1),
			RunE:  runBroodCancel,
		}
		broodopCmd.AddCommand(cancelCmd)

		watchCmd := &cobra.Command{
			Use:   "watch NS/NAME",
			Short: "Watch a brood operation via SSE",
			Args:  cobra.ExactArgs(1),
			RunE:  runBroodWatch,
		}
		broodopCmd.AddCommand(watchCmd)

		root.AddCommand(broodopCmd)
	})
}

func runBroodRun(cmd *cobra.Command, args []string) error {
	verb := args[0]
	if !validBroodVerbs[verb] {
		return usagef("unknown verb %q: must be restart, reprovision, reconcile, stop, start", verb)
	}
	sel, _ := cmd.Flags().GetString("selector")
	namesStr, _ := cmd.Flags().GetString("names")
	selJSON, _ := cmd.Flags().GetString("selector-json")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	watchFlag, _ := cmd.Flags().GetBool("watch")
	// Three-way mutual exclusion: exactly one of --selector, --selector-json, or --names.
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
	if ttl, _ := cmd.Flags().GetInt("ttl"); cmd.Flags().Changed("ttl") && ttl < 0 {
		return fmt.Errorf("--ttl must be >= 0 (0 keeps the operation forever), got %d", ttl)
	}
	spec := buildSpecMap(verb, sel, selJSON, namesStr, fm, cmd)
	body := client.CreateBroodOperationJSONRequestBody{
		Namespace: strP(ns),
		Spec:      &spec,
	}
	if len(clusters) > 0 {
		body.Clusters = &clusters
	}
	cl, err := apiClient(cmd)
	if err != nil {
		return err
	}
	if dryRun {
		return doDryRun(cl, body)
	}
	resp, err := cl.CreateBroodOperationWithResponse(context.Background(), body)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if resp.JSON201 == nil {
		return errFromResponse(resp.Body, resp.StatusCode())
	}
	op := resp.JSON201
	fmt.Printf("%s/%s\n", op.Namespace, op.Name)
	// Per-cluster creation rows (fan-out outcome). Exit 0 on 201 regardless of rows.
	if len(op.Clusters) > 0 {
		rows := make([][]string, 0, len(op.Clusters))
		for _, c := range op.Clusters {
			okStr := "yes"
			if !c.Ok {
				okStr = "no"
			}
			errStr := ""
			if c.Error != nil {
				errStr = *c.Error
			}
			rows = append(rows, []string{c.Cluster, okStr, errStr})
		}
		printTable(os.Stdout, []string{"CLUSTER", "OK", "ERROR"}, rows, false)
	}
	if watchFlag {
		return broodWatchLoop(cl, op.Namespace, op.Name)
	}
	return nil
}

// splitCSV splits a comma-separated string into a trimmed, non-empty slice.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// validateClusterTargeting enforces the client-side usage-error subset (exit 2,
// no request) for the run/preview targeting grammar. The server re-validates
// authoritatively; these catch obvious mistakes before a round-trip.
func validateClusterTargeting(clusters []string, namesStr string) error {
	var has3, has2, hasBare bool
	for _, n := range splitCSV(namesStr) {
		switch strings.Count(n, "/") {
		case 2:
			has3 = true
		case 1:
			has2 = true
		default:
			hasBare = true
		}
	}
	// Mixing cluster-qualified (3-token) names with unqualified names.
	if has3 && (has2 || hasBare) {
		return usagef("cannot mix cluster-qualified (3-token) names with unqualified names")
	}
	hasAll := false
	nonAll := 0
	for _, c := range clusters {
		if c == "all" {
			hasAll = true
		} else {
			nonAll++
		}
	}
	if hasAll && nonAll > 0 {
		return usagef(`"all" cannot be combined with explicit cluster entries`)
	}
	if len(clusters) > 0 {
		if has3 {
			return usagef("--clusters cannot be used with cluster-qualified (3-token) names")
		}
		if hasAll && namesStr != "" {
			return usagef(`--clusters all cannot be combined with --names`)
		}
		if (hasBare || has2) && nonAll > 1 {
			return usagef("multiple clusters not allowed with unqualified names")
		}
	}
	return nil
}

func buildSpecMap(verb, sel, selJSON, namesStr string, filters map[string]string, cmd *cobra.Command) map[string]interface{} {
	m := map[string]interface{}{
		"action": map[string]interface{}{"verb": verb},
	}
	tgts := map[string]interface{}{}
	if sel != "" || selJSON != "" {
		if selJSON != "" {
			var selMap map[string]interface{}
			_ = json.Unmarshal([]byte(selJSON), &selMap)
			tgts["selector"] = selMap
		} else {
			ml := map[string]string{}
			for _, pair := range strings.Split(sel, ",") {
				if k, v, ok := strings.Cut(pair, "="); ok && k != "" {
					ml[k] = v
				}
			}
			tgts["selector"] = map[string]interface{}{"matchLabels": ml}
		}
		if nss, _ := cmd.Flags().GetStringSlice("namespaces"); len(nss) > 0 {
			tgts["namespaces"] = nss
		}
	} else {
		tgts["names"] = strings.Split(namesStr, ",")
	}
	if len(filters) > 0 {
		tgts["filters"] = filters
	}
	m["targets"] = tgts

	exec := map[string]interface{}{}
	if mp, _ := cmd.Flags().GetInt("max-parallel"); mp > 0 {
		exec["maxParallel"] = mp
	}
	if o, _ := cmd.Flags().GetString("order"); o != "" {
		exec["order"] = o
	}
	if fp, _ := cmd.Flags().GetString("failure-policy"); fp != "" {
		exec["failurePolicy"] = fp
	}
	if len(exec) > 0 {
		m["execution"] = exec
	}
	if ttl, _ := cmd.Flags().GetInt("ttl"); cmd.Flags().Changed("ttl") {
		m["ttlSecondsAfterFinished"] = ttl
	}
	return m
}

func doDryRun(cl *client.ClientWithResponses, body client.CreateBroodOperationJSONRequestBody) error {
	resp, err := cl.PreviewBroodOperationWithResponse(context.Background(), body)
	if err != nil {
		return fmt.Errorf("preview: %w", err)
	}
	if resp.JSON200 == nil {
		return errFromResponse(resp.Body, resp.StatusCode())
	}
	// One section per target cluster (fan-out preview).
	for _, cs := range resp.JSON200.Clusters {
		if !cs.Ok {
			msg := "unreachable"
			if cs.Error != nil {
				msg = *cs.Error
			}
			fmt.Printf("CLUSTER %s: %s\n", cs.Cluster, msg)
			continue
		}
		fmt.Printf("CLUSTER %s — TARGETS:\n", cs.Cluster)
		var rows [][]string
		if cs.Targets != nil {
			for _, t := range *cs.Targets {
				app := "yes"
				if !t.Applicable {
					app = "no"
				}
				r := ""
				if t.Reason != nil {
					r = *t.Reason
				}
				rows = append(rows, []string{t.Namespace, t.Name, fmt.Sprintf("%d", t.Wave), app, r})
			}
		}
		printTable(os.Stdout, []string{"NAMESPACE", "NAME", "WAVE", "APPLICABLE", "REASON"}, rows, false)
	}
	return nil
}

func runBroodGet(cmd *cobra.Command, args []string) error {
	cl, err := apiClient(cmd)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return doList(cmd, cl)
	}
	return doGetOne(cmd, cl, args[0])
}

func doList(cmd *cobra.Command, cl *client.ClientWithResponses) error {
	n, _ := cmd.Flags().GetString("namespace")
	a, _ := cmd.Flags().GetBool("all-namespaces")
	ns := resolveListNamespace(n, a, "")
	var p *client.ComponentsParametersNamespace
	if ns != "" {
		v := ns
		p = &v
	}
	resp, err := cl.ListBroodOperationsWithResponse(context.Background(), &client.ListBroodOperationsParams{Namespace: p})
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	if resp.JSON200 == nil {
		return errFromResponse(resp.Body, resp.StatusCode())
	}
	return printBroodItems(cmd, resp.JSON200.Items)
}

func printBroodItems(cmd *cobra.Command, items []client.ComponentsSchemasBroodRunSummaryRow) error {
	o, _ := cmd.Flags().GetString("output")
	nh, _ := cmd.Flags().GetBool("no-headers")
	switch o {
	case "json":
		return printJSON(os.Stdout, items)
	case "yaml":
		return printYAML(os.Stdout, items)
	case "name":
		for _, it := range items {
			fmt.Printf("%s/%s\n", it.Namespace, it.Name)
		}
		return nil
	default:
		var rows [][]string
		for _, it := range items {
			rows = append(rows, broodSummaryRow(it))
		}
		printTable(os.Stdout, []string{"NAME", "VERB", "PHASE", "SUMMARY", "CLUSTERS", "AGE"}, rows, nh)
		return nil
	}
}

func broodSummaryRow(it client.ComponentsSchemasBroodRunSummaryRow) []string {
	nsName := it.Namespace + "/" + it.Name
	v := string(it.Verb)
	p := string(it.Phase)
	sum := fmt.Sprintf("%d/%d/%d/%d", val32(it.Summary.Total), val32(it.Summary.Succeeded), val32(it.Summary.Failed), val32(it.Summary.Skipped))
	clusters := strings.Join(it.Clusters, ",")
	return []string{nsName, v, p, sum, clusters, ""}
}

func val32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

func doGetOne(cmd *cobra.Command, cl *client.ClientWithResponses, arg string) error {
	n, _ := cmd.Flags().GetString("namespace")
	ns, name, err := resolveNSName(arg, n, "")
	if err != nil {
		return err
	}
	resp, err := cl.GetBroodOperationWithResponse(context.Background(), ns, name)
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
		fmt.Printf("%s/%s\n", resp.JSON200.Namespace, resp.JSON200.Name)
		return nil
	default:
		doGetOneDetail(os.Stdout, resp.JSON200)
		return nil
	}
}

func runBroodDescribe(cmd *cobra.Command, args []string) error {
	n, _ := cmd.Flags().GetString("namespace")
	ns, name, err := resolveNSName(args[0], n, "")
	if err != nil {
		return err
	}
	cl, err := apiClient(cmd)
	if err != nil {
		return err
	}
	resp, err := cl.GetBroodOperationWithResponse(context.Background(), ns, name)
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}
	if resp.JSON200 == nil {
		return errFromResponse(resp.Body, resp.StatusCode())
	}
	doGetOneDetail(os.Stdout, resp.JSON200)
	return nil
}

func doGetOneDetail(w io.Writer, run *client.ComponentsSchemasBroodRun) {
	fmt.Fprintf(w, "Run: %s/%s\n", run.Namespace, run.Name)
	fmt.Fprintf(w, "Verb: %s  Phase: %s\n", run.Verb, run.Phase)
	sum := run.Summary
	fmt.Fprintf(w, "Summary: %d total, %d succeeded, %d failed, %d skipped\n",
		val32(sum.Total), val32(sum.Succeeded), val32(sum.Failed), val32(sum.Skipped))
	if run.StartedBy != nil {
		fmt.Fprintf(w, "Started by: %s\n", *run.StartedBy)
	}
	for _, c := range run.Clusters {
		fmt.Fprintf(w, "\n--- Cluster: %s ---\n", c.Cluster)
		if !c.Ok {
			fmt.Fprintf(w, "Error: %s\n", strValOr(c.Error, "unknown"))
			continue
		}
		if c.Op != nil {
			p := ""
			if c.Op.Status.Phase != nil {
				p = string(*c.Op.Status.Phase)
			}
			fmt.Fprintf(w, "Phase: %s\n", p)
			if c.Op.Status.Targets != nil {
				rows := make([][]string, 0, len(*c.Op.Status.Targets))
				for _, t := range *c.Op.Status.Targets {
					s := ""
					if t.State != nil {
						s = string(*t.State)
					}
					r := ""
					if t.Reason != nil {
						r = *t.Reason
					}
					rows = append(rows, []string{strVal(t.Namespace), strVal(t.Name), fmt.Sprintf("%d", val32(t.Wave)), s, r})
				}
				printTable(w, []string{"NAMESPACE", "NAME", "WAVE", "STATE", "REASON"}, rows, true)
			}
		}
	}
}

func strVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func strValOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

func tNamespace(t struct {
	DispatchedAt *time.Time `json:"dispatchedAt,omitempty"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
	Name         *string    `json:"name,omitempty"`
	Namespace    *string    `json:"namespace,omitempty"`

	Output *string                                                         `json:"output,omitempty"`
	Reason *string                                                         `json:"reason,omitempty"`
	State  *client.ComponentsSchemasBroodOperationDetailStatusTargetsState `json:"state,omitempty"`
	Wave   *int32                                                          `json:"wave,omitempty"`
}) string {
	if t.Namespace != nil {
		return *t.Namespace
	}
	return ""
}

func tName(t struct {
	DispatchedAt *time.Time `json:"dispatchedAt,omitempty"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
	Name         *string    `json:"name,omitempty"`
	Namespace    *string    `json:"namespace,omitempty"`

	Output *string                                                         `json:"output,omitempty"`
	Reason *string                                                         `json:"reason,omitempty"`
	State  *client.ComponentsSchemasBroodOperationDetailStatusTargetsState `json:"state,omitempty"`
	Wave   *int32                                                          `json:"wave,omitempty"`
}) string {
	if t.Name != nil {
		return *t.Name
	}
	return ""
}

func runBroodCancel(cmd *cobra.Command, args []string) error {
	n, _ := cmd.Flags().GetString("namespace")
	ns, name, err := resolveNSName(args[0], n, "")
	if err != nil {
		return err
	}
	cl, err := apiClient(cmd)
	if err != nil {
		return err
	}
	resp, err := cl.DeleteBroodOperationWithResponse(context.Background(), ns, name)
	if err != nil {
		return fmt.Errorf("cancel: %w", err)
	}
	if resp.StatusCode() != http.StatusOK {
		return errFromResponse(resp.Body, resp.StatusCode())
	}
	fmt.Printf("cancelled %s/%s\n", ns, name)
	return nil
}

func runBroodSuspend(cmd *cobra.Command, args []string) error {
	n, _ := cmd.Flags().GetString("namespace")
	ns, name, err := resolveNSName(args[0], n, "")
	if err != nil {
		return err
	}
	off, _ := cmd.Flags().GetBool("off")
	cl, err := apiClient(cmd)
	if err != nil {
		return err
	}
	suspend := !off
	body := client.SuspendBroodOperationJSONRequestBody{Suspend: suspend}
	resp, err := cl.SuspendBroodOperationWithResponse(context.Background(), ns, name, body)
	if err != nil {
		return fmt.Errorf("suspend: %w", err)
	}
	if resp.JSON200 == nil {
		return errFromResponse(resp.Body, resp.StatusCode())
	}
	if off {
		fmt.Printf("resumed %s/%s\n", ns, name)
	} else {
		fmt.Printf("suspended %s/%s\n", ns, name)
	}
	return nil
}

func runBroodWatch(cmd *cobra.Command, args []string) error {
	n, _ := cmd.Flags().GetString("namespace")
	ns, name, err := resolveNSName(args[0], n, "")
	if err != nil {
		return err
	}
	cl, err := apiClient(cmd)
	if err != nil {
		return err
	}
	return broodWatchLoop(cl, ns, name)
}

func broodWatchLoop(cl *client.ClientWithResponses, ns, name string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)
	defer signal.Stop(sigCh)
	resp, err := cl.StreamBroodOperation(ctx, ns, name)
	if err != nil {
		return fmt.Errorf("stream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return errFromResponse(b, resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	var lastOp *client.ComponentsSchemasBroodOperationDetail
	var currentEvent string
	var closeErr error
readLoop:
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			// SSE frames pair an "event: " line with a following "data: "
			// line; track the event name so the data line below can tell a
			// terminal "closed" frame (which may carry a reason/message,
			// e.g. the server's watch-deadline close) apart from a
			// "status" frame.
			currentEvent = strings.TrimPrefix(line, "event: ")
			continue
		case line == "":
			currentEvent = ""
			continue
		case !strings.HasPrefix(line, "data: "):
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if currentEvent == "closed" {
			if data != "{}" {
				var closed struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				}
				if err := json.Unmarshal([]byte(data), &closed); err == nil && closed.Message != "" {
					closeErr = fmt.Errorf("%s", closed.Message)
				}
			}
			break readLoop
		}
		if data == "{}" {
			break readLoop
		}
		var op client.ComponentsSchemasBroodOperationDetail
		if err := json.Unmarshal([]byte(data), &op); err != nil {
			continue
		}
		if op.Status == nil {
			// Non-status event (e.g. a keepalive frame); nothing to
			// render or track for the terminal-phase decision.
			continue
		}
		lastOp = &op
		renderBroodWatchStatus(os.Stdout, op)
		if op.Status.Phase != nil && isTerminalBroodPhase(*op.Status.Phase) {
			break readLoop
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if lastOp != nil && lastOp.Status != nil && lastOp.Status.Phase != nil {
		switch *lastOp.Status.Phase {
		case client.ComponentsSchemasBroodOperationDetailStatusPhaseSucceeded:
			return nil
		case client.ComponentsSchemasBroodOperationDetailStatusPhaseFailed,
			client.ComponentsSchemasBroodOperationDetailStatusPhaseCanceled:
			return fmt.Errorf("brood operation ended %s", string(*lastOp.Status.Phase))
		}
	}
	return nil
}

func isTerminalBroodPhase(phase client.ComponentsSchemasBroodOperationDetailStatusPhase) bool {
	switch phase {
	case client.ComponentsSchemasBroodOperationDetailStatusPhaseSucceeded,
		client.ComponentsSchemasBroodOperationDetailStatusPhaseFailed,
		client.ComponentsSchemasBroodOperationDetailStatusPhaseCanceled:
		return true
	default:
		return false
	}
}

func renderBroodWatchStatus(w io.Writer, op client.ComponentsSchemasBroodOperationDetail) {
	if op.Status == nil {
		return
	}
	p := ""
	if op.Status.Phase != nil {
		p = string(*op.Status.Phase)
	}
	ns, name := "", ""
	if op.Metadata != nil {
		if op.Metadata.Namespace != nil {
			ns = *op.Metadata.Namespace
		}
		if op.Metadata.Name != nil {
			name = *op.Metadata.Name
		}
	}
	fmt.Fprintf(w, "\n%s/%s (%s):\n", ns, name, p)
	if op.Status.Targets != nil {
		rows := make([][]string, 0, len(*op.Status.Targets))
		for _, t := range *op.Status.Targets {
			st := ""
			if t.State != nil {
				st = string(*t.State)
			}
			r := ""
			if t.Reason != nil {
				r = *t.Reason
			}
			wv := 0
			if t.Wave != nil {
				wv = int(*t.Wave)
			}
			rows = append(rows, []string{tNamespace(t), tName(t), fmt.Sprintf("%d", wv), st, r})
		}
		printTable(w, []string{"NAMESPACE", "NAME", "WAVE", "STATE", "REASON"}, rows, false)
	}
}

func strP(s string) *string { return &s }
