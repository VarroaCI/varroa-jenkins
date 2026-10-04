package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
	"github.com/varroaci/varroa-jenkins/internal/telemetry"
)

// httpShutdownTimeout bounds how long run waits for in-flight HTTP
// requests to finish during shutdown.
const httpShutdownTimeout = 10 * time.Second

// Config carries every value run needs to start the recorder.
type Config struct {
	Listen             string
	RecordStoreDSN     string
	BlobStoreURL       string
	BusURL             string
	BusUser            string
	BusPassword        string
	BusPassFile        string
	BusCAFile          string
	ClusterName        string
	JetStreamReplicas  int
	RetentionRuns      time.Duration
	RetentionLogs      time.Duration
	RetentionArtifacts time.Duration
	RetentionActivity  time.Duration
	FinalizeWait       time.Duration
	// SweepMaxPending is VARROA_SWEEP_MAX_PENDING, the interrupted
	// sweep's consumer-freshness gate (design D9, task 9.6a).
	SweepMaxPending uint64
	// ConfirmedGoneGrace is VARROA_RECORDSTORE_CONFIRMED_GONE_GRACE, the
	// wait a pod-read confirmation serves before the recorder counts the
	// instance gone.
	ConfirmedGoneGrace time.Duration
	Logger             *slog.Logger
}

// confirmedGoneGrace resolves the grace once for every seam that reads
// it. A zero field applies the package default, so a Config built
// without main's own environment parsing never runs a zero grace, which
// would count an instance gone the moment the operator clears it. It
// mirrors recordstore.BackstopConfig's own fallback.
func (cfg Config) confirmedGoneGrace() time.Duration {
	if cfg.ConfirmedGoneGrace > 0 {
		return cfg.ConfirmedGoneGrace
	}
	return recordstore.DefaultConfirmedGoneGrace
}

// wireGrantHold wires the ledger's grant hold with the confirmed-gone
// grace and the backlog bound the interrupted sweep also reads. run is
// its only caller, and a Ledger without it holds no repeat claim.
func wireGrantHold(ledger *recordstore.Ledger, busConn *bus.Conn, cfg Config) {
	ledger.SetGrantHold(busConn, cfg.confirmedGoneGrace(), cfg.SweepMaxPending)
}

// run wires the recorder's dependencies in the order design D6 states:
// open the pool, run migrations, ensure the builds stream, ensure the
// ingest consumer, ensure the activity stream, start the ten
// goroutines, then serve HTTP. It blocks until ctx is cancelled, then
// shuts every goroutine and the HTTP server down before returning.
func run(ctx context.Context, cfg Config) error {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	confirmedGoneGrace := cfg.confirmedGoneGrace()

	pool, err := pgxpool.New(ctx, cfg.RecordStoreDSN)
	if err != nil {
		return fmt.Errorf("open record store pool: %w", err)
	}
	defer pool.Close()

	sqlDB, err := sql.Open("pgx", cfg.RecordStoreDSN)
	if err != nil {
		return fmt.Errorf("open record store sql.DB: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	if err := recordstore.RunMigrations(ctx, cfg.RecordStoreDSN); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	var migrated atomic.Bool
	migrated.Store(true)

	store, err := blob.Open(ctx, cfg.BlobStoreURL, pool)
	if err != nil {
		return fmt.Errorf("open blob store: %w", err)
	}

	busCfg := bus.Config{
		Username:    cfg.BusUser,
		CAFile:      cfg.BusCAFile,
		InboxPrefix: "_INBOX_recorder",
		Logger:      logger,
	}
	if cfg.BusPassFile != "" {
		busCfg.PasswordFile = cfg.BusPassFile
	} else {
		busCfg.Password = cfg.BusPassword
	}
	busConn, err := bus.Connect(cfg.BusURL, busCfg)
	if err != nil {
		return fmt.Errorf("connect to bus: %w", err)
	}
	defer busConn.Close()
	if err := busConn.RegisterMetrics(otel.Meter("varroa-recorder"), "recorder"); err != nil {
		logger.Warn("failed to register bus metrics", "error", err)
	}
	busConn.SetReplicas(cfg.JetStreamReplicas)

	if err := recordstore.EnsureBuildsStream(busConn, cfg.JetStreamReplicas); err != nil {
		return fmt.Errorf("ensure builds stream: %w", err)
	}
	if err := recordstore.EnsureIngestConsumer(busConn, bus.BuildsStreamName); err != nil {
		return fmt.Errorf("ensure ingest consumer: %w", err)
	}
	if err := recordstore.EnsureActivityStream(busConn, cfg.JetStreamReplicas); err != nil {
		return fmt.Errorf("ensure activity stream: %w", err)
	}

	fenceMetrics, err := recordstore.NewFenceMetrics()
	if err != nil {
		return fmt.Errorf("register fence metrics: %w", err)
	}
	ledger := recordstore.NewLedger(pool, recordstore.NewPlacementFence(pool, fenceMetrics))
	claimMetrics, err := recordstore.NewClaimMetrics()
	if err != nil {
		return fmt.Errorf("register claim metrics: %w", err)
	}

	actionMetrics, err := recordstore.NewActionMetrics()
	if err != nil {
		return fmt.Errorf("register action metrics: %w", err)
	}
	concurrencyMetrics, err := recordstore.NewConcurrencyMetrics()
	if err != nil {
		return fmt.Errorf("register concurrency metrics: %w", err)
	}
	ledger.SetConcurrencyCancel(busConn, concurrencyMetrics, actionMetrics, logger)
	wireGrantHold(ledger, busConn, cfg)

	sweepMetrics, err := recordstore.NewInterruptedMetrics()
	if err != nil {
		return fmt.Errorf("register interrupted-sweep metrics: %w", err)
	}
	backstopMetrics, err := recordstore.NewBackstopMetrics()
	if err != nil {
		return fmt.Errorf("register backstop metrics: %w", err)
	}
	claimExpiryMetrics, err := recordstore.NewClaimExpiryMetrics()
	if err != nil {
		return fmt.Errorf("register claim-expiry metrics: %w", err)
	}
	runNotifier := recordstore.NewRunNotifier(busConn, logger)
	ledger.SetVetoMarking(runNotifier)
	backstopCfg := recordstore.BackstopConfig{
		MaxSweepPending:    cfg.SweepMaxPending,
		ConfirmedGoneGrace: confirmedGoneGrace,
		SweepMetrics:       sweepMetrics,
		BackstopMetrics:    backstopMetrics,
		ActionMetrics:      actionMetrics,
		ClaimExpiryMetrics: claimExpiryMetrics,
		RunNotifier:        runNotifier,
		StateDeleter:       store,
		LogReader:          store,
		Logger:             logger,
	}

	ingester := recordstore.NewIngester(pool, busConn, logger)
	metrics, err := recordstore.NewIngestMetrics(busConn, bus.BuildsStreamName, recordstore.RecorderConsumerName)
	if err != nil {
		return fmt.Errorf("register ingest metrics: %w", err)
	}
	activityIngester := recordstore.NewActivityIngester(pool)
	activityMetrics, err := recordstore.NewActivityMetrics(busConn, bus.ActivityStreamName, recordstore.ActivityRecorderConsumerName)
	if err != nil {
		return fmt.Errorf("register activity metrics: %w", err)
	}

	fleetLockMetrics, err := recordstore.NewFleetLockMetrics(pool)
	if err != nil {
		return fmt.Errorf("register fleet lock metrics: %w", err)
	}
	fanoutMetrics, err := recordstore.NewUpstreamFanoutMetrics()
	if err != nil {
		return fmt.Errorf("register upstream fan-out metrics: %w", err)
	}
	ingester.SetUpstreamFanout(recordstore.NewGrantEvaluator(pool), fanoutMetrics)
	ingester.SetSweepMetrics(sweepMetrics)
	ingester.SetStateDeleter(store)
	ingester.SetRunNotifier(runNotifier)

	maintCfg := recordstore.MaintenanceConfig{
		RunRetention:      cfg.RetentionRuns,
		LogRetention:      cfg.RetentionLogs,
		ArtifactRetention: cfg.RetentionArtifacts,
		ActivityRetention: cfg.RetentionActivity,
		LogStore:          store,
		Liveness:          recordstore.NewPlacementLiveness(pool, confirmedGoneGrace),
		FinalizeWait:      cfg.FinalizeWait,
		FleetLockMetrics:  fleetLockMetrics,
		LockNotifier:      runNotifier,
	}

	goroutineCtx, cancelGoroutines := context.WithCancel(ctx)
	defer cancelGoroutines()

	ingestFn := func(ctx context.Context) error {
		return recordstore.RunIngestConsumer(ctx, busConn, ingester, metrics, cfg.RetentionRuns, logger)
	}
	activityIngestFn := func(ctx context.Context) error {
		return recordstore.RunActivityIngestConsumer(ctx, busConn, activityIngester, activityMetrics, cfg.RetentionActivity, logger)
	}
	responderFn := func(ctx context.Context) error {
		sub, err := recordstore.RegisterClaimResponder(busConn, ledger, claimMetrics, logger)
		if err != nil {
			return err
		}
		<-ctx.Done()
		return sub.Unsubscribe()
	}
	ownerResponderFn := func(ctx context.Context) error {
		sub, err := recordstore.RegisterOwnerResponder(busConn, pool, logger)
		if err != nil {
			return err
		}
		<-ctx.Done()
		return sub.Unsubscribe()
	}
	tenantPurgeResponderFn := func(ctx context.Context) error {
		sub, err := recordstore.RegisterTenantPurgeResponder(busConn, pool, logger)
		if err != nil {
			return err
		}
		<-ctx.Done()
		return sub.Unsubscribe()
	}
	tenantSyncResponderFn := func(ctx context.Context) error {
		sub, err := recordstore.RegisterTenantSyncResponder(busConn, pool, confirmedGoneGrace, store, store, runNotifier, logger)
		if err != nil {
			return err
		}
		<-ctx.Done()
		return sub.Unsubscribe()
	}
	crossTenantResponderFn := func(ctx context.Context) error {
		sub, err := recordstore.RegisterCrossTenantResponder(busConn, pool, store, fleetLockMetrics, runNotifier, logger)
		if err != nil {
			return err
		}
		<-ctx.Done()
		return sub.Unsubscribe()
	}
	fleetLockReleaseResponderFn := func(ctx context.Context) error {
		sub, err := recordstore.RegisterFleetLockReleaseResponder(busConn, pool, runNotifier, logger)
		if err != nil {
			return err
		}
		<-ctx.Done()
		return sub.Unsubscribe()
	}
	maintenanceFn := func(ctx context.Context) error { //nolint:unparam // matches runWorkers' func(context.Context) error shape; RunMaintenance logs its own failures and never returns one to the caller
		recordstore.RunMaintenance(ctx, pool, sqlDB, maintCfg, logger)
		return nil
	}
	backstopFn := func(ctx context.Context) error { //nolint:unparam // matches runWorkers' func(context.Context) error shape; RunBackstopScheduler logs its own failures and never returns one to the caller
		recordstore.RunBackstopScheduler(ctx, pool, busConn, backstopCfg)
		return nil
	}

	runNotifierFn := func(ctx context.Context) error { //nolint:unparam // matches runWorkers' func(context.Context) error shape; Start sweeps until ctx ends and reports nothing
		runNotifier.Start(ctx)
		return nil
	}

	workerDone := make(chan error, 1)
	go func() {
		workerDone <- runWorkers(goroutineCtx,
			namedWorker{"ingest", ingestFn},
			namedWorker{"claim responder", responderFn},
			namedWorker{"owner responder", ownerResponderFn},
			namedWorker{"tenant purge responder", tenantPurgeResponderFn},
			namedWorker{"tenant sync responder", tenantSyncResponderFn},
			namedWorker{"cross-tenant responder", crossTenantResponderFn},
			namedWorker{"fleet lock release responder", fleetLockReleaseResponderFn},
			namedWorker{"maintenance", maintenanceFn},
			namedWorker{"activity ingest", activityIngestFn},
			namedWorker{"run notifier", runNotifierFn},
			namedWorker{"backstop scheduler", backstopFn},
		)
	}()

	mux := newMux(&migrated, sqlDB, busConn.Connected)
	httpServer := &http.Server{Addr: cfg.Listen, Handler: mux}
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("varroa-recorder listening", "addr", cfg.Listen)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	workerReceived, runErr := awaitShutdown(ctx, serverErr, workerDone)
	if runErr != nil {
		logger.Error("varroa-recorder is shutting down because of an error", "error", runErr)
	}

	cancelGoroutines()

	// The notifier publishes what it still holds while the bus connection
	// is open, so a run that ends at shutdown still reaches a reader.
	runNotifier.Close()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("HTTP server shutdown error", "error", err)
	}

	if !workerReceived {
		if err := <-workerDone; err != nil {
			logger.Warn("a worker returned an error during shutdown", "error", err)
		}
	}
	return runErr
}

// namedWorker pairs a human-readable name with the goroutine function
// runWorkers supervises, so its error message can name the one that
// stopped early.
type namedWorker struct {
	name string
	fn   func(context.Context) error
}

// runWorkers starts every named worker concurrently, each receiving a
// context derived from ctx. It returns nil once every worker has
// returned after ctx itself is done. The moment any worker returns
// before that, whether with an error or with nil, runWorkers cancels the
// context the others share and returns a named error: a recorder with
// one dead worker must not go on looking healthy to the rest. The set of
// workers is open-ended; run passes ingest, the claim responder, the
// owner responder, the tenant purge responder, the tenant sync
// responder, maintenance, the activity ingest consumer, the run
// notifier, and the backstop scheduler.
func runWorkers(ctx context.Context, workers ...namedWorker) error {
	workerCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()

	type outcome struct {
		name string
		err  error
	}
	results := make(chan outcome, len(workers))
	for _, w := range workers {
		go func(w namedWorker) {
			results <- outcome{w.name, w.fn(workerCtx)}
		}(w)
	}

	var reported error
	for range workers {
		o := <-results
		if reported == nil && workerCtx.Err() == nil {
			if o.err != nil {
				reported = fmt.Errorf("worker %q stopped unexpectedly: %w", o.name, o.err)
			} else {
				reported = fmt.Errorf("worker %q stopped unexpectedly", o.name)
			}
			stopWorkers()
		}
	}
	return reported
}

// awaitShutdown decides why run stops serving. It reads exactly one of:
// ctx's own cancellation (the normal shutdown signal, no error), an HTTP
// listen failure on serverErr, or a worker stopping early on workerDone.
// workerReceived reports whether workerDone was the channel that fired,
// so the caller knows not to read it again while it waits for a worker
// that has already reported in.
func awaitShutdown(ctx context.Context, serverErr, workerDone <-chan error) (workerReceived bool, err error) {
	select {
	case <-ctx.Done():
		return false, nil
	case err := <-serverErr:
		return false, fmt.Errorf("http server: %w", err)
	case err := <-workerDone:
		return true, err
	}
}

// newMux wires the recorder's one HTTP mux: /healthz always answers 200
// with a bus field; /readyz answers 200 only when migrations finished,
// Postgres answered, and the bus is connected; /metrics sits behind the
// metrics-token middleware. sqlDB may be nil, which readyz treats as
// not ready.
func newMux(migrated *atomic.Bool, sqlDB *sql.DB, busConnected func() bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/healthz", telemetry.HealthzHandlerWithBus(busConnected))
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !migrated.Load() {
			http.Error(w, "migrations not finished", http.StatusServiceUnavailable)
			return
		}
		if sqlDB == nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := sqlDB.PingContext(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		if !busConnected() {
			http.Error(w, "bus unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/metrics", telemetry.MetricsAuthMiddleware(promhttp.Handler()))
	return mux
}
