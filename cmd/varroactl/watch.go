package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client/stream"
)

// broodEvents is a package-level channel for debounced watch re-listing.
// Declared here so OnConnect closures can reference it.
var broodEvents = make(chan struct{}, 1)

func init() {
	registerRootCommand(func(root *cobra.Command) {
		// Top-level watch
		watchCmd := &cobra.Command{
			Use:   "watch [-n NS]",
			Short: "Watch controller changes via brood stream",
			RunE:  runWatch,
		}
		addClusterFlag(watchCmd)
		watchCmd.Flags().Bool("all-clusters", false, "watch controllers across all clusters")
		root.AddCommand(watchCmd)

		// Attach -w/--watch to get controller
		getCtrl := findCommand(root, "get", "controller")
		getCtrl.Flags().BoolP("watch", "w", false, "Watch for changes")
		wrapRunE(getCtrl, func(orig func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
			return func(cmd *cobra.Command, args []string) error {
				watch, _ := cmd.Flags().GetBool("watch")
				if !watch {
					return orig(cmd, args)
				}
				// -w with -o json/yaml/name is a usage error
				o, _ := cmd.Flags().GetString("output")
				if o == "json" || o == "yaml" || o == "name" {
					return usagef("--watch / -w is only supported with table output")
				}
				return runWatch(cmd, args)
			}
		})
	})
}

// resetTimer stops a timer if non-nil and returns a new one.
func resetTimer(t *time.Timer, d time.Duration) *time.Timer {
	if t != nil {
		t.Stop()
	}
	return time.NewTimer(d)
}

// timerC returns a timer's C channel, or nil (blocks forever) if t is nil.
func timerC(t *time.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

// controllerRow holds a single controller's rendered data.
type controllerRow struct {
	nsName  string // "ns/name"
	columns []string
	phase   string
}

// renderControllerRow duplicates C2's controller list columns locally.
// See design.md §1 Q2 — do NOT refactor cmd/varroactl/controllers.go (C2-owned).
func renderControllerRow(item map[string]any) controllerRow {
	cluster, _ := item["cluster"].(string)
	ns, _ := item["namespace"].(string)
	name, _ := item["name"].(string)
	phase, _ := item["phase"].(string)

	key := name
	if ns != "" {
		key = cluster + "/" + ns + "/" + name
	}

	ver := ""
	if v, ok := item["version"].(string); ok {
		ver = v
	}

	mite := "-"
	if mc, ok := item["miteConnected"].(bool); ok && mc {
		mite = "connected"
	}

	health := ""
	if h, ok := item["jenkinsHealth"].(string); ok {
		health = h
	}

	columns := []string{cluster, ns, name, phase, ver, mite, health}
	return controllerRow{nsName: key, columns: columns, phase: phase}
}

// fetchAndRenderAll fetches the controller list and returns a map of
// key (cluster/ns/name) -> rendered tab-separated row string. It also prints the header
// and all rows to stdout on first call.
func fetchAndRenderAll(cmd *cobra.Command, headers bool) (map[string]string, error) {
	rc, err := resolveContext(func(name string) string {
		f := cmd.Flag(name)
		if f == nil {
			return ""
		}
		return f.Value.String()
	})
	if err != nil {
		return nil, err
	}

	nFlag, _ := cmd.Flags().GetString("namespace")
	aFlag, _ := cmd.Flags().GetBool("all-namespaces")
	cFlag, _ := cmd.Flags().GetString("cluster")
	acFlag, _ := cmd.Flags().GetBool("all-clusters")
	ns := resolveListNamespace(nFlag, aFlag, rc.defaultNamespace)
	cl, err := resolveListCluster(cFlag, acFlag, rc.defaultCluster)
	if err != nil {
		return nil, err
	}

	// Build query params using url.Values
	params := make([]string, 0)
	if ns != "" {
		params = append(params, "namespace="+ns)
	}
	if cl != "" {
		params = append(params, "cluster="+cl)
	}

	urlPath := "/controllers"
	if len(params) > 0 {
		urlPath = "/controllers?" + strings.Join(params, "&")
	}

	fullURL := strings.TrimRight(rc.server, "/") + "/api/v1" + urlPath
	req, _ := http.NewRequest("GET", fullURL, nil)
	req.Header.Set("Authorization", "Bearer "+rc.apiKey)
	req.Header.Set("User-Agent", "varroactl/"+version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("error from server (%d)", resp.StatusCode)
	}

	var envelope struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, err
	}

	result := make(map[string]string, len(envelope.Items))

	// Print header on first render
	if headers {
		_, _ = fmt.Fprintln(os.Stdout, "CLUSTER\tNAMESPACE\tNAME\tPHASE\tVERSION\tMITE\tHEALTH")
	}

	for _, item := range envelope.Items {
		row := renderControllerRow(item)
		result[row.nsName] = strings.Join(row.columns, "\t")
		_, _ = fmt.Fprintln(os.Stdout, strings.Join(row.columns, "\t"))
	}

	return result, nil
}

// withPhase replaces the phase column in a rendered tab-separated row.
func withPhase(row string, phase string) string {
	parts := strings.Split(row, "\t")
	if len(parts) >= 4 {
		parts[3] = phase
	}
	return strings.Join(parts, "\t")
}

func runWatch(cmd *cobra.Command, args []string) error {
	// Initial render: print header + all rows
	last, err := fetchAndRenderAll(cmd, true)
	if err != nil {
		return err
	}

	ctx, cancel := signalCtx(cmd.Context())
	defer cancel()

	rc, cerr := resolveContext(func(name string) string {
		f := cmd.Flag(name)
		if f == nil {
			return ""
		}
		return f.Value.String()
	})
	if cerr != nil {
		return cerr
	}

	broodURL := strings.TrimRight(rc.server, "/") + "/api/v1/stream/brood"
	scfg := stream.Config{
		URL:       broodURL,
		Token:     rc.apiKey,
		UserAgent: "varroactl/" + version,
		Client:    http.DefaultClient,
		OnConnect: func(reconnected bool) {
			if reconnected {
				select {
				case broodEvents <- struct{}{}:
				default:
				}
			}
		},
		Backoff: stream.DefaultBackoff(),
	}

	var timer *time.Timer

	// Start stream in background
	go func() {
		_ = stream.Stream(ctx, scfg, func(e stream.Event) error {
			select {
			case broodEvents <- struct{}{}:
			default:
			}
			return nil
		})
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-broodEvents:
			timer = resetTimer(timer, 2*time.Second)
		case <-timerC(timer):
			timer = nil

			cur, err := fetchAndRenderAll(cmd, false)
			if err != nil {
				_, _ = fmt.Fprintln(os.Stderr, "watch error:", err)
				continue
			}

			// New/changed rows
			for k, row := range cur {
				if last[k] != row {
					_, _ = fmt.Fprintln(os.Stdout, row)
				}
			}

			// Deleted rows
			for k, row := range last {
				if _, ok := cur[k]; !ok {
					_, _ = fmt.Fprintln(os.Stdout, withPhase(row, "Deleted"))
				}
			}

			last = cur
		}
	}
}
