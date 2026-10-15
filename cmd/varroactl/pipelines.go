// Package main: the pipelines commands (varroa-cli, design D2, D3, D5).
//
// `run` and `builds` register as new top-level commands. `logs` extends the
// top-level command root.go already registers. A UUIDv7 argument names a
// run. Every other argument names a controller. No second `logs` command is
// ever registered.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client"
	"github.com/varroaci/varroa-jenkins/pkg/client/stream"
)

// waitForRunPollInterval and waitForRunTimeout bound how long `run --follow`
// waits for the run to appear (varroa-cli). Tests override both.
var (
	waitForRunPollInterval = time.Second
	waitForRunTimeout      = 60 * time.Second
)

func init() {
	registerRootCommand(registerPipelineCommands)
}

func registerPipelineCommands(root *cobra.Command) {
	runCmd := &cobra.Command{
		Use:   "run <team>/<job>",
		Short: "Trigger a pipeline run",
		Long: `Trigger a pipeline run.

<team>/<job> splits on the first slash. <job> may hold further slashes.
<job> may omit the team's own folder segment; the server adds it. <job>
may also repeat the folder segment; both forms name the same job.
--param is repeatable. It takes key=value. --follow waits for the run to
appear. It then streams the log until the run ends. It exits non-zero
when the result is not a success.`,
		Args: cobra.ExactArgs(1),
		RunE: runTriggerRun,
	}
	runCmd.Flags().StringArray("param", nil, "build parameter key=value (repeatable)")
	runCmd.Flags().BoolP("follow", "f", false, "wait for the run and stream its log until it ends")
	root.AddCommand(runCmd)

	buildsCmd := &cobra.Command{
		Use:   "builds <team>/<job>",
		Short: "List runs for a pipeline job",
		Args:  cobra.ExactArgs(1),
		RunE:  runListBuilds,
	}
	root.AddCommand(buildsCmd)

	// Extend the shared "logs" command with the run form (design D2). No
	// second "logs" command is registered.
	logsCmd := findCommand(root, "logs")
	logsCmd.Use = "logs (NS/NAME | RUN)"
	logsCmd.Long = `Show a controller's logs, or a pipeline run's log.

Accepts NS/NAME or "controller NS/NAME" for a controller. Accepts a UUIDv7
run identifier for a run instead. A run identifier holds no "/". A
controller argument is never a UUIDv7. The two forms cannot collide.
One-shot GET by default. -f/--follow streams either form until it ends.`
	wrapRunE(logsCmd, func(orig func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && looksLikeRunID(args[0]) {
				return runOneShotRunLog(cmd, args[0])
			}
			return orig(cmd, args)
		}
	})
}

// looksLikeRunID reports whether arg is a run identifier, not a controller
// argument. Design D2 states {run} is a UUIDv7 string.
func looksLikeRunID(arg string) bool {
	id, err := uuid.Parse(arg)
	if err != nil {
		return false
	}
	return id.Version() == 7
}

// splitTeamJob splits "<team>/<job>" on the first slash. The generated
// client percent-encodes the job path when it builds the request (D2). This
// function returns the raw, unescaped job path.
func splitTeamJob(arg string) (team, job string, err error) {
	team, job, ok := strings.Cut(arg, "/")
	if !ok || team == "" || job == "" {
		return "", "", usagef("expected <team>/<job>, got %q", arg)
	}
	return team, job, nil
}

// parseParamFlags reads --param key=value flags into a map. A repeated key
// is a usage error, not a silent overwrite (varroa-cli).
func parseParamFlags(cmd *cobra.Command) (map[string]string, error) {
	raw, _ := cmd.Flags().GetStringArray("param")
	if len(raw) == 0 {
		return nil, nil
	}
	params := make(map[string]string, len(raw))
	for _, kv := range raw {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, usagef("--param must be key=value, got %q", kv)
		}
		if _, exists := params[k]; exists {
			return nil, usagef("--param %q repeated", k)
		}
		params[k] = v
	}
	return params, nil
}

// ---------------------------------------------------------------------------
// run <team>/<job>
// ---------------------------------------------------------------------------

func runTriggerRun(cmd *cobra.Command, args []string) error {
	team, job, err := splitTeamJob(args[0])
	if err != nil {
		return err
	}

	params, err := parseParamFlags(cmd)
	if err != nil {
		return err
	}

	follow, _ := cmd.Flags().GetBool("follow")

	c, err := apiClient(cmd)
	if err != nil {
		return err
	}

	// The client mints the token once per invocation. A retried request
	// carries the same token. The ledger then runs the build once
	// (varroa-cli).
	token, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("failed to mint client token: %w", err)
	}

	body := client.CreateRunJSONRequestBody{ClientToken: &token}
	if len(params) > 0 {
		body.Parameters = &params
	}

	resp, err := c.CreateRunWithResponse(cmd.Context(), team, job, body)
	if err != nil {
		return err
	}
	if resp.HTTPResponse.StatusCode >= 400 || resp.JSON202 == nil {
		return errFromResponse(resp.Body, resp.StatusCode())
	}

	runID := resp.JSON202.RunId
	_, _ = fmt.Fprintln(os.Stdout, runID)

	if !follow {
		return nil
	}

	if err := waitForRunToAppear(cmd.Context(), c, runID); err != nil {
		return err
	}
	return followRunLog(cmd, runID)
}

// waitForRunToAppear polls GetRun until the run row exists or the deadline
// passes. The log stream answers a terminal 404 for a run that does not yet
// exist. `run --follow` cannot hand off to it until the row is there.
func waitForRunToAppear(ctx context.Context, c *client.ClientWithResponses, runID string) error {
	parsed, err := uuid.Parse(runID)
	if err != nil {
		return usagef("invalid run id %q", runID)
	}

	deadline := time.Now().Add(waitForRunTimeout)
	for {
		resp, err := c.GetRunWithResponse(ctx, parsed)
		if err != nil {
			return err
		}
		if resp.JSON200 != nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("run %s did not appear within %s", runID, waitForRunTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitForRunPollInterval):
		}
	}
}

// ---------------------------------------------------------------------------
// builds <team>/<job>
// ---------------------------------------------------------------------------

func runListBuilds(cmd *cobra.Command, args []string) error {
	team, job, err := splitTeamJob(args[0])
	if err != nil {
		return err
	}

	c, err := apiClient(cmd)
	if err != nil {
		return err
	}

	resp, err := c.ListRunsWithResponse(cmd.Context(), team, job, &client.ListRunsParams{})
	if err != nil {
		return err
	}
	if resp.HTTPResponse.StatusCode >= 400 || resp.JSON200 == nil {
		return errFromResponse(resp.Body, resp.StatusCode())
	}

	return printBuildsList(cmd, resp.JSON200.Items)
}

// printBuildsList honours the shared -o {table,wide,json,yaml,name} contract
// (varroa-cli: table columns BUILD, RESULT, BRANCH, DURATION, AGE).
func printBuildsList(cmd *cobra.Command, items []client.ComponentsSchemasRunSummary) error {
	o, _ := cmd.Flags().GetString("output")
	nh, _ := cmd.Flags().GetBool("no-headers")
	switch o {
	case "json":
		return printJSON(os.Stdout, items)
	case "yaml":
		return printYAML(os.Stdout, items)
	case "name":
		for _, it := range items {
			_, _ = fmt.Fprintln(os.Stdout, it.RunId)
		}
		return nil
	default:
		rows := make([][]string, len(items))
		for i, it := range items {
			rows[i] = buildSummaryRow(it)
		}
		printTable(os.Stdout, []string{"BUILD", "RESULT", "BRANCH", "DURATION", "AGE"}, rows, nh)
		return nil
	}
}

func buildSummaryRow(it client.ComponentsSchemasRunSummary) []string {
	build := "-"
	if it.BuildNumber != nil {
		build = fmt.Sprintf("#%d", *it.BuildNumber)
	}

	result := it.Status
	if it.Result != nil && *it.Result != "" {
		result = *it.Result
	}

	branch := "-"
	if it.Branch != nil && *it.Branch != "" {
		branch = *it.Branch
	}

	duration := "-"
	if it.DurationMs != nil {
		duration = formatDurationMs(*it.DurationMs)
	}

	age := "-"
	switch {
	case it.QueuedAt != nil:
		age = formatAge(*it.QueuedAt)
	case it.StartedAt != nil:
		age = formatAge(*it.StartedAt)
	}

	return []string{build, result, branch, duration, age}
}

// formatDurationMs renders a millisecond duration at one-second resolution.
func formatDurationMs(ms int64) string {
	return time.Duration(ms * int64(time.Millisecond)).Round(time.Second).String()
}

// formatAge renders the elapsed time since t as a single unit. It picks the
// coarsest unit that keeps the value at least 1.
func formatAge(t time.Time) string {
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// ---------------------------------------------------------------------------
// logs RUN (one-shot)
// ---------------------------------------------------------------------------

// runOneShotRunLog prints a run's log once and exits. It reads the "raw"
// mode (design D5). Raw mode is a plain-text GET, not the reconnecting
// stream -f uses.
func runOneShotRunLog(cmd *cobra.Command, runArg string) error {
	runID, err := uuid.Parse(runArg)
	if err != nil {
		return usagef("invalid run id %q", runArg)
	}

	c, err := apiClient(cmd)
	if err != nil {
		return err
	}

	rawMode := true
	resp, err := c.StreamRunLogWithResponse(cmd.Context(), runID, &client.StreamRunLogParams{Raw: &rawMode})
	if err != nil {
		return err
	}
	if resp.HTTPResponse.StatusCode >= 400 {
		return errFromResponse(resp.Body, resp.StatusCode())
	}

	_, err = os.Stdout.Write(resp.Body)
	return err
}

// ---------------------------------------------------------------------------
// logs RUN -f / run --follow
// ---------------------------------------------------------------------------

// errRunLogEnded is a control-flow sentinel. The log stream's "end" event
// (design D5) means the run finished. The handler below returns it to stop
// stream.Stream's reconnect loop. Without it, the loop retries a closed
// connection that produces no more data.
var errRunLogEnded = errors.New("run log ended")

// runEndEvent is the "end" event's payload (design D5).
type runEndEvent struct {
	Result           string `json:"result"`
	LogComplete      bool   `json:"logComplete"`
	IncompleteReason string `json:"incompleteReason"`
}

// runLogLineEvent is one "line" event's payload (design D5). N is the
// line number and Off is the byte offset where the line starts.
type runLogLineEvent struct {
	N    int64  `json:"n"`
	Off  int64  `json:"off"`
	Text string `json:"text"`
}

// runLogWatermark records the last line the command printed. Only a
// "line" event moves it. A "gap" event can name an offset below a line
// the command already printed, so it must never move the watermark.
type runLogWatermark struct {
	seen bool
	off  int64
	n    int64
}

// accept reports whether line is new, and moves the watermark to it when
// it is. A line at or below the watermark offset is a replay.
func (w *runLogWatermark) accept(line runLogLineEvent) bool {
	if w.seen && line.Off <= w.off {
		return false
	}
	w.seen, w.off, w.n = true, line.Off, line.N
	return true
}

// resumeURL answers base with the resume cursor the BFF reads: from is
// one byte past the last printed line's start, so the server starts at
// the next line, and fromLine is that next line's number. It answers ""
// before the first line, which tells the stream to reuse base.
func (w *runLogWatermark) resumeURL(base string) string {
	if !w.seen {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("from", strconv.FormatInt(w.off+1, 10))
	q.Set("fromLine", strconv.FormatInt(w.n+1, 10))
	u.RawQuery = q.Encode()
	return u.String()
}

// runRunLogsFollow is streamcmds.go's entry point for `logs RUN -f`.
func runRunLogsFollow(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		return usagef("RUN is required")
	}
	return followRunLog(cmd, args[0])
}

// followRunLog streams a run's log to stdout, copying runLogsFollow's
// stream.Config and signalCtx shape, until the run ends. It returns a
// non-nil error when the run's result is not a success (varroa-cli).
func followRunLog(cmd *cobra.Command, runID string) error {
	if _, err := uuid.Parse(runID); err != nil {
		return usagef("invalid run id %q", runID)
	}

	ctx, cancel := signalCtx(cmd.Context())
	defer cancel()

	cfg, err := streamConfigFromCmd(cmd, "/runs/"+runID+"/log")
	if err != nil {
		return err
	}

	var end runEndEvent
	var sawEnd bool
	var mark runLogWatermark
	baseURL := cfg.URL
	cfg.ResumeURL = func() string { return mark.resumeURL(baseURL) }

	streamErr := stream.Stream(ctx, cfg, func(e stream.Event) error {
		switch e.Name {
		case "line":
			printRunLogLine(e, &mark)
		case "end":
			_ = json.Unmarshal(e.Data, &end)
			sawEnd = true
			return errRunLogEnded
		}
		return nil
	})

	if streamErr != nil && !errors.Is(streamErr, errRunLogEnded) {
		return streamErr
	}
	if !sawEnd {
		// The context was cancelled (Ctrl-C) before the run ended. This is
		// not a build failure, so it is not reported as one.
		return nil
	}
	if end.Result != "SUCCESS" {
		return fmt.Errorf("run %s finished with result %s", runID, end.Result)
	}
	return nil
}

// printRunLogLine renders a single "line" event's text. It skips a line
// that mark reports as a replay.
func printRunLogLine(e stream.Event, mark *runLogWatermark) {
	var line runLogLineEvent
	if err := json.Unmarshal(e.Data, &line); err != nil {
		_, _ = fmt.Fprintln(os.Stdout, string(e.Data))
		return
	}
	if !mark.accept(line) {
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, line.Text)
}
