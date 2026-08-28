// Command updatecenter is the Varroa Update Center HTTP service.
// It serves Jenkins plugin metadata and HPI downloads backed by an
// OCI BlobStore, with optional pull-through caching from upstream.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/varroaci/varroa-jenkins/internal/logging"
	"github.com/varroaci/varroa-jenkins/internal/oci"
	"github.com/varroaci/varroa-jenkins/internal/telemetry"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

var version = "dev"

func main() {
	logLevel := flag.String("log-level", "info", "Log level: debug, info, warn, error")
	logFormat := flag.String("log-format", "text", "Log format: text, json")
	listen := flag.String("listen", envDefault("VARROA_UC_LISTEN", ":8080"), "Listen address")
	flag.Parse()

	level, err := logging.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --log-level %q: %v\n", *logLevel, err)
		os.Exit(1)
	}
	logger := logging.New(level, *logFormat, os.Stderr).With("binary", "updatecenter")
	slog.SetDefault(logger)

	// --- Telemetry ---
	_ = os.Setenv("VARROA_SERVICE_NAME", "varroa-updatecenter")
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

	// --- Build BlobStore from env ---
	store, err := buildStore(logger)
	if err != nil {
		logger.Error("failed to build blob store", "error", err)
		os.Exit(1)
	}

	// Probe store readiness at startup.
	if _, err := store.ListManifests(context.Background()); err != nil {
		logger.Warn("store not ready at startup", "error", err)
	}

	// --- Server ---
	var opts []updatecenter.Option

	// Import token.
	if token := os.Getenv("VARROA_UC_IMPORT_TOKEN"); token != "" {
		opts = append(opts, updatecenter.WithImportToken(token))
	} else {
		logger.Warn("VARROA_UC_IMPORT_TOKEN not set — import endpoint will reject all requests")
	}

	// Declared plugin set. Deliberately NOT gated on pull-through: the air-gapped
	// configuration is exactly where the upload planner needs to know what is
	// pinned. The path is re-read per request so an operator ConfigMap update
	// lands without a restart.
	declaredFile := os.Getenv("VARROA_UC_DECLARED_PLUGINS_FILE")
	if declaredFile != "" {
		opts = append(opts, updatecenter.WithDeclaredPluginsFile(declaredFile))
	} else {
		logger.Warn("VARROA_UC_DECLARED_PLUGINS_FILE not set — plugin uploads will be rejected with declared-set-unavailable")
	}

	// Upload endpoint. Uploads are a read-modify-write against a store with no
	// conditional-push primitive, so they require a genuine single writer; the
	// chart sets this only for a one-replica Deployment with a non-overlapping
	// rollout strategy.
	singleWriter := os.Getenv("VARROA_UC_SINGLE_WRITER") == "true"
	opts = append(opts, updatecenter.WithSingleWriter(singleWriter))
	if raw := os.Getenv("VARROA_UC_MAX_UPLOAD_BYTES"); raw != "" {
		n, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || n <= 0 {
			logger.Warn("invalid VARROA_UC_MAX_UPLOAD_BYTES — keeping the default", "value", raw)
		} else {
			opts = append(opts, updatecenter.WithMaxUploadBytes(n))
		}
	}

	// Pull-through.
	pullThroughEnabled := os.Getenv("VARROA_UC_PULLTHROUGH_ENABLED") == "true"
	if pullThroughEnabled {
		upstreamURL := envDefault("VARROA_UC_PULLTHROUGH_UPSTREAM_URL", "https://updates.jenkins.io")
		downloadURL := envDefault("VARROA_UC_PULLTHROUGH_DOWNLOAD_URL", "https://updates.jenkins.io/download")

		// Resolver sources: the weekly metadata first, then any operator-supplied
		// LTS-line sources listed (newline-delimited) in VARROA_UC_LTS_METADATA_FILE.
		// The file is re-read on every resolve so operator ConfigMap updates are picked
		// up without a restart; a missing/empty/unreadable file means weekly-only.
		ltsFile := os.Getenv("VARROA_UC_LTS_METADATA_FILE")
		weeklyMeta := strings.TrimRight(upstreamURL, "/") + "/update-center.actual.json"
		sources := func() []ucmeta.Source {
			out := []ucmeta.Source{{URL: weeklyMeta}}
			for _, u := range readMetadataURLs(ltsFile) {
				if u != weeklyMeta {
					out = append(out, ucmeta.Source{URL: u})
				}
			}
			return out
		}
		resolver := ucmeta.NewResolver(sources, time.Hour, &http.Client{Timeout: 60 * time.Second}, logger)

		// Checksum fallback for pins no metadata source still lists. This targets a
		// different host than upstreamURL, so a deployment with a narrowed egress
		// allowlist may need to redirect it at an internal mirror or switch it off.
		archiveURL := archiveBaseURL(os.LookupEnv("VARROA_UC_ARCHIVE_BASE_URL"))
		resolver.SetArchiveBaseURL(archiveURL)

		opts = append(opts,
			updatecenter.WithPullThrough(true, upstreamURL, downloadURL),
			updatecenter.WithMetadataResolver(resolver),
		)
		logger.Info("pull-through enabled", "upstreamURL", upstreamURL, "downloadURL", downloadURL, "ltsMetadataFile", ltsFile)
		if archiveURL == "" {
			logger.Warn("archive checksum fallback disabled — plugin versions that have aged out of update-center metadata cannot be pulled through")
		} else {
			logger.Info("archive checksum fallback enabled", "archiveURL", archiveURL)
		}
	}

	srv := updatecenter.NewServer(store, logger, opts...)

	// Mark ready if the startup probe succeeded.
	if _, err := store.ListManifests(context.Background()); err == nil {
		srv.MarkReady()
	}

	// --- HTTP routes ---
	mux := http.NewServeMux()
	mux.Handle("/metrics", telemetry.MetricsAuthMiddleware(promhttp.Handler()))
	srv.RegisterRoutes(mux)

	// --- Start HTTP ---
	httpServer := &http.Server{
		Addr:    *listen,
		Handler: mux,
	}
	go func() {
		logger.Info("updatecenter listening", "addr", *listen, "version", version)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server error", "error", err)
			os.Exit(1)
		}
	}()

	// --- Wait for shutdown ---
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	logger.Info("updatecenter shutting down...")
	_ = httpServer.Shutdown(context.Background())
}

// buildStore constructs an oci.BlobStore from the env-var contract.
func buildStore(logger *slog.Logger) (oci.BlobStore, error) {
	storageType := os.Getenv("VARROA_UC_STORAGE_TYPE")
	switch storageType {
	case "local":
		path := os.Getenv("VARROA_UC_LOCAL_PATH")
		if path == "" {
			return nil, fmt.Errorf("VARROA_UC_LOCAL_PATH is required for storage type 'local'")
		}
		logger.Info("using local OCI layout store", "path", path)
		return oci.NewLayoutStore(path)

	case "oci":
		ref := os.Getenv("VARROA_UC_OCI_REF")
		if ref == "" {
			return nil, fmt.Errorf("VARROA_UC_OCI_REF is required for storage type 'oci'")
		}
		opts := oci.RegistryOptions{
			Insecure: os.Getenv("VARROA_UC_OCI_INSECURE") == "true",
		}
		if credsPath := os.Getenv("VARROA_UC_OCI_CREDS_PATH"); credsPath != "" {
			opts.CredentialConfigPath = credsPath
		}
		logger.Info("using OCI registry store", "ref", ref, "insecure", opts.Insecure)
		return oci.NewRegistryStore(ref, opts)

	default:
		return nil, fmt.Errorf("unknown VARROA_UC_STORAGE_TYPE %q (must be 'local' or 'oci')", storageType)
	}
}

// archiveBaseURL resolves the target for the Maven-repository checksum fallback from
// VARROA_UC_ARCHIVE_BASE_URL, taking os.LookupEnv's (value, set) pair directly.
//
// The three states are deliberately distinct: unset keeps the built-in default, a
// non-empty value redirects the fallback at an internal mirror, and an explicitly empty
// value disables it. Disabling is the escape hatch for deployments whose egress cannot
// reach the Maven repository — the fallback already fails closed there, but each blocked
// lookup still costs a request timeout, so switching it off is cheaper than letting it
// fail.
func archiveBaseURL(raw string, set bool) string {
	if !set {
		return ucmeta.DefaultArchiveBaseURL
	}
	return strings.TrimSpace(raw)
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// readMetadataURLs reads a newline-delimited list of update-center metadata URLs from
// path, skipping blank lines. An empty path or a missing/unreadable file yields nil
// (the resolver falls back to weekly-only).
func readMetadataURLs(path string) []string {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var urls []string
	for _, line := range strings.Split(string(data), "\n") {
		if u := strings.TrimSpace(line); u != "" {
			urls = append(urls, u)
		}
	}
	return urls
}
