package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/varroaci/varroa-jenkins/internal/logging"
	"github.com/varroaci/varroa-jenkins/internal/telemetry"
)

func main() {
	logLevel := flag.String("log-level", "info", "Log level: debug, info, warn, error")
	logFormat := flag.String("log-format", "text", "Log format: text, json")
	flag.Parse()

	level, err := logging.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --log-level %q: %v\n", *logLevel, err)
		os.Exit(1)
	}
	logger := logging.New(level, *logFormat, os.Stderr).With("binary", "mite")
	slog.SetDefault(logger)

	_ = os.Setenv("VARROA_SERVICE_NAME", "varroa-mite")
	telemetryShutdown := telemetry.InitTelemetry(context.Background())
	if !telemetry.Disabled() {
		logger = telemetry.LogHandler(logger)
		slog.SetDefault(logger)
	}
	defer func() {
		if err := telemetryShutdown(context.Background()); err != nil {
			logger.Warn("telemetry shutdown failed", "error", err)
		}
	}()

	cfg := Config{
		VarroaEndpoint: os.Getenv("VARROA_ENDPOINT"),
		JenkinsURL:     os.Getenv("JENKINS_URL"),
		ControllerName: os.Getenv("CONTROLLER_NAME"),
		Namespace:      os.Getenv("NAMESPACE"),
		CAPEM:          os.Getenv("VARROA_CA_PEM"),
	}

	if cfg.VarroaEndpoint == "" {
		logger.Error("VARROA_ENDPOINT is required")
		os.Exit(1)
	}
	if cfg.JenkinsURL == "" {
		logger.Error("JENKINS_URL is required")
		os.Exit(1)
	}
	if cfg.ControllerName == "" {
		logger.Error("CONTROLLER_NAME is required")
		os.Exit(1)
	}
	if cfg.Namespace == "" {
		logger.Error("NAMESPACE is required")
		os.Exit(1)
	}
	if bf := os.Getenv("BOOTSTRAP_FILE"); bf != "" {
		cfg.BootstrapFile = bf
	}

	agent := NewAgent(cfg)
	agent.Logger = logger
	logger.Info("Mite agent starting",
		"controller", cfg.ControllerName,
		"namespace", cfg.Namespace,
		"varroa", cfg.VarroaEndpoint,
		"jenkins", cfg.JenkinsURL,
	)

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		logger.Info("signal received; running termination drain")
		agent.drainForTermination() // D7: drain BEFORE cancel
		cancel()
	}()
	if err := agent.Run(ctx); err != nil {
		logger.Error("Mite agent fatal", "error", err)
		os.Exit(1)
	}
}
