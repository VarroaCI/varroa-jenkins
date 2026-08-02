package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client"
	"github.com/varroaci/varroa-jenkins/pkg/client/stream"
)

// ---------------------------------------------------------------------------
// events controller NS/NAME
// ---------------------------------------------------------------------------

func init() {
	registerRootCommand(func(root *cobra.Command) {
		// events controller NS/NAME
		eventsCmd := &cobra.Command{
			Use:   "events controller NS/NAME",
			Short: "Stream events for a controller",
			Args:  cobra.ExactArgs(2),
			RunE:  runEvents,
		}
		eventsCmd.Flags().StringP("output", "o", "table", "output format: table or json")
		addClusterFlag(eventsCmd)
		root.AddCommand(eventsCmd)

		// mite NS/NAME
		miteCmd := &cobra.Command{
			Use:   "mite NS/NAME",
			Short: "Stream mite events for a controller",
			Args:  cobra.ExactArgs(1),
			RunE:  runMite,
		}
		miteCmd.Flags().StringP("output", "o", "table", "output format: table or json")
		addClusterFlag(miteCmd)
		root.AddCommand(miteCmd)

		// logs -f/--follow
		logsCmd := findCommand(root, "logs")
		logsCmd.Flags().BoolP("follow", "f", false, "Follow log output")
		wrapRunE(logsCmd, func(orig func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
			return func(cmd *cobra.Command, args []string) error {
				follow, _ := cmd.Flags().GetBool("follow")
				if !follow {
					return orig(cmd, args)
				}
				return runLogsFollow(cmd, args)
			}
		})
	})
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// streamConfigFromCmd builds a stream.Config from a cobra command's context.
func streamConfigFromCmd(cmd *cobra.Command, urlPath string) (stream.Config, error) {
	rc, err := resolveContext(func(name string) string {
		f := cmd.Flag(name)
		if f == nil {
			return ""
		}
		return f.Value.String()
	})
	if err != nil {
		return stream.Config{}, err
	}

	fullURL := strings.TrimRight(rc.server, "/") + "/api/v1" + urlPath
	cfg := stream.Config{
		URL:       fullURL,
		Token:     rc.apiKey,
		UserAgent: "varroactl/" + version,
		Client:    http.DefaultClient,
		OnConnect: func(reconnected bool) {},
		Backoff:   stream.DefaultBackoff(),
	}

	return cfg, nil
}

// signalCtx returns a context cancelled by SIGINT/SIGTERM, parented on ctx.
// Overrideable in tests to avoid signal-handler leakage.
var signalCtx = func(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
}

// resolveStreamTarget resolves cluster, ns, name for streaming commands.
func resolveStreamTarget(cmd *cobra.Command, arg string) (cluster, ns, name string, err error) {
	nFlag, _ := cmd.Flags().GetString("namespace")
	cFlag, _ := cmd.Flags().GetString("cluster")
	rc, cerr := resolveContext(func(n string) string {
		f := cmd.Flag(n)
		if f == nil {
			return ""
		}
		return f.Value.String()
	})
	if cerr != nil {
		return "", "", "", cerr
	}
	ns, name, err = resolveNSName(arg, nFlag, rc.defaultNamespace)
	if err != nil {
		return "", "", "", err
	}
	cluster = resolveCluster(cFlag, rc.defaultCluster)
	return cluster, ns, name, nil
}

// ---------------------------------------------------------------------------
// events
// ---------------------------------------------------------------------------

func runEvents(cmd *cobra.Command, args []string) error {
	// args[0] is "controller", args[1] is NS/NAME
	cluster, ns, name, err := resolveStreamTarget(cmd, args[1])
	if err != nil {
		return err
	}

	o, _ := cmd.Flags().GetString("output")

	ctx, cancel := signalCtx(cmd.Context())
	defer cancel()

	cfg, cerr := streamConfigFromCmd(cmd, fmt.Sprintf("/clusters/%s/controllers/%s/%s/events", cluster, ns, name))
	if cerr != nil {
		return cerr
	}

	return stream.Stream(ctx, cfg, func(e stream.Event) error {
		return printEventFrame(e, o)
	})
}

// printEventFrame renders a single unnamed event frame.
func printEventFrame(e stream.Event, format string) error {
	var data map[string]any
	if err := json.Unmarshal(e.Data, &data); err != nil {
		if format == "json" {
			_, _ = fmt.Fprintln(os.Stdout, string(e.Data))
		} else {
			_, _ = fmt.Fprintf(os.Stdout, "%s  %s\n", time.Now().Format("15:04:05"), string(e.Data))
		}
		return nil
	}

	if format == "json" {
		return printJSON(os.Stdout, data)
	}

	eventName, _ := data["event"].(string)
	parts := make([]string, 0, len(data))
	for k, v := range data {
		if k == "event" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	_, _ = fmt.Fprintf(os.Stdout, "%s  %s  %s\n", time.Now().Format("15:04:05"), eventName, strings.Join(parts, "  "))
	return nil
}

// ---------------------------------------------------------------------------
// mite
// ---------------------------------------------------------------------------

func runMite(cmd *cobra.Command, args []string) error {
	cluster, ns, name, err := resolveStreamTarget(cmd, args[0])
	if err != nil {
		return err
	}

	o, _ := cmd.Flags().GetString("output")

	ctx, cancel := signalCtx(cmd.Context())
	defer cancel()

	cfg, cerr := streamConfigFromCmd(cmd, fmt.Sprintf("/clusters/%s/controllers/%s/%s/mite/stream", cluster, ns, name))
	if cerr != nil {
		return cerr
	}

	return stream.Stream(ctx, cfg, func(e stream.Event) error {
		return printMiteFrame(e, o)
	})
}

// printMiteFrame renders a single named mite event frame.
func printMiteFrame(e stream.Event, format string) error {
	if format == "json" {
		compact := map[string]any{
			"event": e.Name,
		}
		var data any
		if err := json.Unmarshal(e.Data, &data); err == nil {
			compact["data"] = data
		} else {
			compact["data"] = string(e.Data)
		}
		b, _ := json.Marshal(compact)
		_, _ = fmt.Fprintln(os.Stdout, string(b))
		return nil
	}

	_, _ = fmt.Fprintf(os.Stdout, "%s  %s  %s\n", time.Now().Format("15:04:05"), e.Name, string(e.Data))
	return nil
}

// ---------------------------------------------------------------------------
// logs -f
// ---------------------------------------------------------------------------

func runLogsFollow(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		return usagef("NS/NAME is required")
	}

	arg := args[0]
	if arg == "controller" || arg == "controllers" || arg == "ctrl" {
		if len(args) < 2 {
			return usagef("NS/NAME is required")
		}
		arg = args[1]
	}

	cluster, ns, name, err := resolveStreamTarget(cmd, arg)
	if err != nil {
		return err
	}

	ctx, cancel := signalCtx(cmd.Context())
	defer cancel()

	path := fmt.Sprintf("/clusters/%s/controllers/%s/%s/logs?follow=true", cluster, ns, name)
	cfg, cerr := streamConfigFromCmd(cmd, path)
	if cerr != nil {
		return cerr
	}

	var lastPrinted time.Time

	return stream.Stream(ctx, cfg, func(e stream.Event) error {
		return printLogEntry(e, &lastPrinted)
	})
}

// printLogEntry decodes an SSE frame as a LogEntry and prints it.
// Uses the generated ComponentsSchemasLogEntry type from pkg/client.
func printLogEntry(e stream.Event, lastPrinted *time.Time) error {
	var entry client.ComponentsSchemasLogEntry
	if err := json.Unmarshal(e.Data, &entry); err != nil {
		_, _ = fmt.Fprintln(os.Stdout, string(e.Data))
		return nil //nolint:nilerr
	}

	// Dedupe on reconnect: skip entries with timestamp <= lastPrinted
	if !lastPrinted.IsZero() && !entry.Timestamp.After(*lastPrinted) {
		return nil
	}
	*lastPrinted = entry.Timestamp

	// Same format as C2 one-shot logs: <timestamp> <message>
	_, _ = fmt.Fprintf(os.Stdout, "%s %s\n", entry.Timestamp.Format("2006-01-02T15:04:05Z07:00"), entry.Message)
	return nil
}
