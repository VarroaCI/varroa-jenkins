package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client"
	"github.com/varroaci/varroa-jenkins/pkg/client/stream"
)

func init() {
	registerRootCommand(func(root *cobra.Command) {
		activityCmd := &cobra.Command{
			Use:   "activity",
			Short: "Show activity feed",
			RunE:  runActivity,
		}
		activityCmd.Flags().BoolP("follow", "f", false, "Follow activity stream")
		activityCmd.Flags().String("controller", "", "Filter by controller (NAME or NAMESPACE/NAME); matches on every cluster unless --cluster is set")
		activityCmd.Flags().String("cluster", "", "Filter by cluster (default: every cluster)")
		root.AddCommand(activityCmd)
	})
}

func runActivity(cmd *cobra.Command, args []string) error {
	follow, _ := cmd.Flags().GetBool("follow")
	ctrlFilter, _ := cmd.Flags().GetString("controller")
	clusterFilter, _ := cmd.Flags().GetString("cluster")
	query := activityQuery(ctrlFilter, clusterFilter)

	rc, err := resolveContext(func(name string) string {
		f := cmd.Flag(name)
		if f == nil {
			return ""
		}
		return f.Value.String()
	})
	if err != nil {
		return err
	}

	// Backfill
	backfillPath := "/activity" + query

	fullURL := strings.TrimRight(rc.server, "/") + "/api/v1" + backfillPath
	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+rc.apiKey)
	req.Header.Set("User-Agent", "varroactl/"+version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("error from server (%d)", resp.StatusCode)
	}

	var envelope struct {
		Items []client.ComponentsSchemasActivityEvent `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("failed to decode activity: %w", err)
	}

	// Print backfill table
	printActivityTable(os.Stdout, envelope.Items)

	if !follow {
		return nil
	}

	// Stream follow
	ctx, cancel := signalCtx(cmd.Context())
	defer cancel()

	streamPath := "/activity/stream" + query

	var lastPrinted atomic.Value // stores time.Time

	// Seed lastPrinted from backfill
	if len(envelope.Items) > 0 {
		lastPrinted.Store(envelope.Items[len(envelope.Items)-1].Timestamp)
	}

	scfg := stream.Config{
		URL:       strings.TrimRight(rc.server, "/") + "/api/v1" + streamPath,
		Token:     rc.apiKey,
		UserAgent: "varroactl/" + version,
		Client:    http.DefaultClient,
		OnConnect: func(reconnected bool) {
			if !reconnected {
				return
			}
			// Reconnect: re-fetch backfill, print only events with timestamp > lastPrinted
			lp := lastPrinted.Load()
			if lp == nil {
				return
			}
			lastTS := lp.(time.Time)

			req2, _ := http.NewRequest("GET", strings.TrimRight(rc.server, "/")+"/api/v1"+backfillPath, nil)
			req2.Header.Set("Authorization", "Bearer "+rc.apiKey)
			req2.Header.Set("User-Agent", "varroactl/"+version)
			resp2, err2 := http.DefaultClient.Do(req2)
			if err2 != nil {
				return
			}
			defer func() { _ = resp2.Body.Close() }()
			if resp2.StatusCode >= 400 {
				return
			}
			var env2 struct {
				Items []client.ComponentsSchemasActivityEvent `json:"items"`
			}
			if err2 := json.NewDecoder(resp2.Body).Decode(&env2); err2 != nil {
				return
			}
			for _, item := range env2.Items {
				if item.Timestamp.After(lastTS) {
					printActivityRow(item)
					lastPrinted.Store(item.Timestamp)
				}
			}
		},
		Backoff: stream.DefaultBackoff(),
	}

	return stream.Stream(ctx, scfg, func(e stream.Event) error {
		return printActivityStreamEvent(e, &lastPrinted)
	})
}

// activityQuery returns the query string of the activity filters, or "" when
// no filter is set. The BFF applies these filters to the backfill and to the
// stream, so the command does not filter streamed events again.
func activityQuery(controller, cluster string) string {
	v := url.Values{}
	if controller != "" {
		v.Set("controller", controller)
	}
	if cluster != "" {
		v.Set("cluster", cluster)
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// printActivityTable renders the backfill table.
func printActivityTable(w *os.File, items []client.ComponentsSchemasActivityEvent) {
	if len(items) == 0 {
		return
	}
	headers := []string{"TIME", "TYPE", "SOURCE", "CONTROLLER", "MESSAGE"}
	rows := make([][]string, len(items))
	for i, item := range items {
		controller := "-"
		if item.Controller != nil && *item.Controller != "" {
			if item.Namespace != nil && *item.Namespace != "" {
				controller = *item.Namespace + "/" + *item.Controller
			} else {
				controller = *item.Controller
			}
		}
		rows[i] = []string{
			item.Timestamp.Format("15:04:05"),
			item.Type,
			string(item.Source),
			controller,
			item.Message,
		}
	}
	printTable(w, headers, rows, false)
}

// printActivityRow prints a single activity row without a header.
func printActivityRow(item client.ComponentsSchemasActivityEvent) {
	controller := "-"
	if item.Controller != nil && *item.Controller != "" {
		if item.Namespace != nil && *item.Namespace != "" {
			controller = *item.Namespace + "/" + *item.Controller
		} else {
			controller = *item.Controller
		}
	}
	row := []string{
		item.Timestamp.Format("15:04:05"),
		item.Type,
		string(item.Source),
		controller,
		item.Message,
	}
	printTable(os.Stdout, []string{"TIME", "TYPE", "SOURCE", "CONTROLLER", "MESSAGE"}, [][]string{row}, true)
}

// printActivityStreamEvent handles a single stream event for activity.
func printActivityStreamEvent(e stream.Event, lastPrinted *atomic.Value) error {
	// Stream frames are named "activity"
	if e.Name != "" && e.Name != "activity" {
		return nil
	}

	var event client.ComponentsSchemasActivityEvent
	if err := json.Unmarshal(e.Data, &event); err != nil {
		return nil //nolint:nilerr
	}

	// Dedupe
	lp := lastPrinted.Load()
	if lp != nil {
		lastTS := lp.(time.Time)
		if !event.Timestamp.After(lastTS) {
			return nil
		}
	}
	lastPrinted.Store(event.Timestamp)

	printActivityRow(event)
	return nil
}
