// Command recorder is the Varroa build-record store recorder. It is the
// only writer of the record tables in schema "varroa", the only owner of
// migrations, and the only creator of the varroa_builds JetStream
// stream. It exposes no Kubernetes API access and no HTTP data path:
// /healthz, /readyz, and /metrics only. Every data path rides NATS.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/logging"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/telemetry"
)

var version = "dev"

// defaultFinalizeWaitMs is VARROA_RECORDSTORE_LOG_FINALIZE_WAIT's
// default, in milliseconds.
const defaultFinalizeWaitMs = 60000

func main() {
	logLevel := flag.String("log-level", "info", "Log level: debug, info, warn, error")
	logFormat := flag.String("log-format", "text", "Log format: text, json")
	listen := flag.String("listen", envDefault("VARROA_RECORDER_LISTEN", ":8080"), "Listen address")
	busURL := flag.String("bus-url", envDefault("VARROA_BUS_URL", ""), "NATS bus URL")
	busUser := flag.String("bus-user", "recorder", "NATS bus username")
	busPassFile := flag.String("bus-pass-file", "", "Path to NATS bus password file (overrides BUS_PASSWORD env)")
	busCAFile := flag.String("bus-ca-file", "", "Path to NATS bus CA certificate file")
	flag.Parse()

	level, err := logging.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --log-level %q: %v\n", *logLevel, err)
		os.Exit(1)
	}
	logger := logging.New(level, *logFormat, os.Stderr).With("binary", "recorder")
	slog.SetDefault(logger)

	_ = os.Setenv("VARROA_SERVICE_NAME", "varroa-recorder")
	telemetryShutdown := telemetry.InitTelemetry(context.Background())
	if !telemetry.Disabled() {
		logger = telemetry.LogHandler(logger)
		slog.SetDefault(logger)
	}
	defer func() {
		if telemetryShutdown != nil {
			if err := telemetryShutdown(context.Background()); err != nil {
				logger.Warn("telemetry shutdown failed", "error", err)
			}
		}
	}()

	cluster, err := bus.ClusterFromEnv()
	if err != nil {
		logger.Error("invalid cluster name", "error", err)
		os.Exit(1)
	}

	dsn := os.Getenv("VARROA_RECORDSTORE_DSN")
	if dsn == "" {
		logger.Error("VARROA_RECORDSTORE_DSN is required")
		os.Exit(1)
	}
	blobURL := os.Getenv("VARROA_BLOBSTORE_URL")
	if blobURL == "" {
		logger.Error("VARROA_BLOBSTORE_URL is required")
		os.Exit(1)
	}

	retentionRuns, err := recordstore.ParseRetention(envDefault("VARROA_RETENTION_RUNS", "365d"))
	if err != nil {
		logger.Error("invalid VARROA_RETENTION_RUNS", "error", err)
		os.Exit(1)
	}
	retentionLogs, err := recordstore.ParseRetention(envDefault("VARROA_RETENTION_LOGS", "90d"))
	if err != nil {
		logger.Error("invalid VARROA_RETENTION_LOGS", "error", err)
		os.Exit(1)
	}
	retentionArtifacts, err := recordstore.ParseRetention(envDefault("VARROA_RETENTION_ARTIFACTS", "30d"))
	if err != nil {
		logger.Error("invalid VARROA_RETENTION_ARTIFACTS", "error", err)
		os.Exit(1)
	}
	retentionActivity, err := recordstore.ParseRetention(envDefault("VARROA_RETENTION_ACTIVITY", "30d"))
	if err != nil {
		logger.Error("invalid VARROA_RETENTION_ACTIVITY", "error", err)
		os.Exit(1)
	}

	cfg := Config{
		Listen:             *listen,
		RecordStoreDSN:     dsn,
		BlobStoreURL:       blobURL,
		BusURL:             *busURL,
		BusUser:            *busUser,
		BusPassFile:        *busPassFile,
		BusCAFile:          *busCAFile,
		ClusterName:        cluster,
		JetStreamReplicas:  jetStreamReplicasFromEnv(),
		RetentionRuns:      retentionRuns,
		RetentionLogs:      retentionLogs,
		RetentionArtifacts: retentionArtifacts,
		RetentionActivity:  retentionActivity,
		FinalizeWait:       envDurationMillis("VARROA_RECORDSTORE_LOG_FINALIZE_WAIT", defaultFinalizeWaitMs),
		SweepMaxPending:    sweepMaxPendingFromEnv(),
		ConfirmedGoneGrace: confirmedGoneGraceFromEnv(),
		DashboardURL:       dashboardURLFromEnv(),
		Logger:             logger,
	}
	if cfg.BusPassFile == "" {
		cfg.BusPassword = os.Getenv("BUS_PASSWORD")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("varroa-recorder starting", "version", version, "cluster", cluster, "listen", cfg.Listen)
	if err := run(ctx, cfg); err != nil {
		logger.Error("recorder exited with an error", "error", err)
		os.Exit(1)
	}
}

// dashboardURLFromEnv reads VARROA_DASHBOARD_URL, the base URL a completion
// webhook payload uses to link a run. An empty value omits that link.
func dashboardURLFromEnv() string {
	return os.Getenv("VARROA_DASHBOARD_URL")
}

// envDefault returns the value of the environment variable key, or
// fallback when it is unset or empty.
func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// jetStreamReplicasFromEnv reads VARROA_JETSTREAM_REPLICAS, defaulting
// to 1 when unset or unparseable.
func jetStreamReplicasFromEnv() int {
	v := os.Getenv("VARROA_JETSTREAM_REPLICAS")
	if v == "" {
		return 1
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 1
	}
	return n
}

// sweepMaxPendingFromEnv reads VARROA_SWEEP_MAX_PENDING, defaulting to
// recordstore.DefaultSweepMaxPending when unset, unparseable, or
// negative.
func sweepMaxPendingFromEnv() uint64 {
	v := os.Getenv("VARROA_SWEEP_MAX_PENDING")
	if v == "" {
		return recordstore.DefaultSweepMaxPending
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return recordstore.DefaultSweepMaxPending
	}
	return n
}

// confirmedGoneGraceFromEnv reads
// VARROA_RECORDSTORE_CONFIRMED_GONE_GRACE as a Go duration string, for
// example "30s". A value that is empty, unparsable, or not positive
// falls back to recordstore.DefaultConfirmedGoneGrace.
//
// It does not reuse envDurationMillis: that helper takes a millisecond
// integer, which reads badly in a chart value.
func confirmedGoneGraceFromEnv() time.Duration {
	v := os.Getenv("VARROA_RECORDSTORE_CONFIRMED_GONE_GRACE")
	if v == "" {
		return recordstore.DefaultConfirmedGoneGrace
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return recordstore.DefaultConfirmedGoneGrace
	}
	return d
}

// envDurationMillis reads key as a millisecond count, defaulting to
// fallbackMs when unset, unparseable, or not positive.
func envDurationMillis(key string, fallbackMs int) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return time.Duration(fallbackMs) * time.Millisecond
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms <= 0 {
		return time.Duration(fallbackMs) * time.Millisecond
	}
	return time.Duration(ms) * time.Millisecond
}
