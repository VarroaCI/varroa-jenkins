package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

var (
	activityEventsDropped   metric.Int64Counter
	activityEventsForwarded metric.Int64Counter
)

func init() {
	meter := otel.Meter("varroa-mite")
	activityEventsDropped, _ = meter.Int64Counter("varroa.mite.activity.events.dropped",
		metric.WithDescription("Number of events the plugin reported as dropped"),
	)
	activityEventsForwarded, _ = meter.Int64Counter("varroa.mite.activity.events.forwarded",
		metric.WithDescription("Number of Jenkins activity events forwarded to the gateway"),
	)
}

// drainResponse is the JSON response from GET /varroa-activity/events.
type drainResponse struct {
	Events  []drainEvent       `json:"events"`
	Dropped int                `json:"dropped"`
	Idle    *mitev1.IdleGauges `json:"idle,omitempty"`
}

// drainEvent is a single event from the plugin drain endpoint.
type drainEvent struct {
	Type        string `json:"type"`
	Actor       string `json:"actor"`
	Message     string `json:"message"`
	ItemPath    string `json:"itemPath"`
	BuildNumber int64  `json:"buildNumber"`
	Result      string `json:"result"`
	URL         string `json:"url"`
	Timestamp   string `json:"timestamp"`
}

// startActivityPoller starts a goroutine that polls the Jenkins plugin's
// /varroa-activity/events drain endpoint on each heartbeat tick, forwarding
// drained events onto the gRPC CommandStream.
//
// Cadence is tied to the heartbeat tick; the poller shares the agent's sendMu
// so sends do not race with heartbeat/observability goroutines.
func (a *Agent) startActivityPoller(ctx context.Context) {
	maxBatch := defaultActivityMaxBatch()
	logger := a.Logger.With("component", "activity-poller")

	logger.Debug("activity poller started", "maxBatch", maxBatch)

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(a.heartbeatInterval):
		}

		resp, err := a.pollActivityEvents(ctx, maxBatch)
		if err != nil {
			logger.Debug("activity poll failed (will retry on next tick)", "error", err)
			continue
		}

		// Cache idle gauges from every successful poll — even when there are
		// zero events — so sendHeartbeats always has the latest snapshot.
		if resp.Idle != nil {
			a.idleGauges.Store(resp.Idle)
			a.idleGaugesReceivedAt.Store(time.Now().Unix())
		}

		if resp.Dropped > 0 {
			activityEventsDropped.Add(ctx, int64(resp.Dropped))
			logger.Warn("plugin reported dropped activity events", "count", resp.Dropped)
		}

		if len(resp.Events) == 0 {
			continue
		}

		logger.Debug("forwarding activity events", "count", len(resp.Events))

		for _, e := range resp.Events {
			evt := &mitev1.JenkinsActivityEvent{
				Type:        e.Type,
				Actor:       e.Actor,
				Message:     e.Message,
				ItemPath:    e.ItemPath,
				BuildNumber: e.BuildNumber,
				Result:      e.Result,
				URL:         e.URL,
				Timestamp:   e.Timestamp,
			}
			if evt.Timestamp == "" {
				evt.Timestamp = time.Now().UTC().Format(time.RFC3339)
			}

			msg := &mitev1.MiteMessage{
				Message: &mitev1.MiteMessage_JenkinsActivity{
					JenkinsActivity: evt,
				},
			}

			a.sendMu.Lock()
			err := a.stream.Send(msg)
			a.sendMu.Unlock()
			if err != nil {
				logger.Warn("activity event send failed, dropping remaining batch", "error", err)
				_ = a.conn.Close() // wake up processCommands
				break
			}
			activityEventsForwarded.Add(ctx, 1)
		}
	}
}

// pollActivityEvents performs a single GET /varroa-activity/events request
// against the Jenkins plugin and decodes the response.
func (a *Agent) pollActivityEvents(ctx context.Context, maxBatch int) (*drainResponse, error) {
	path := fmt.Sprintf("/varroa-activity/events?max=%d", maxBatch)
	client := a.getJenkinsClient()

	httpResp, err := client.DoGet(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("do get: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status %d", httpResp.StatusCode)
	}

	var resp drainResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if resp.Events == nil {
		resp.Events = []drainEvent{}
	}

	return &resp, nil
}

// defaultActivityMaxBatch returns the configured batch size from env
// VARROA_ACTIVITY_MAX_BATCH, defaulting to 256.
func defaultActivityMaxBatch() int {
	raw := envOrDefault("VARROA_ACTIVITY_MAX_BATCH", "256")
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 256
	}
	return n
}

// envOrDefault reads an env var with a default fallback.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
