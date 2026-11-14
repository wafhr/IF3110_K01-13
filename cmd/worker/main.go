package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/wafhr/IF3110_K01-13/internal/app"
	"github.com/wafhr/IF3110_K01-13/internal/config"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/job"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/platformdb"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb/drivers"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
	"github.com/wafhr/IF3110_K01-13/internal/worker"
)

func main() {
	slog.SetDefault(telemetry.NewLogger(os.Stdout, "worker"))
	if err := run(); err != nil {
		slog.Error("worker_stopped", "actor", "system", "status", "failed", "error", err)
		os.Exit(1)
	}
	slog.Info("worker_stopped", "actor", "system", "status", "success")
}

func run() (runErr error) {
	if err := config.LoadDotEnv(); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	workerCfg, err := config.LoadWorker()
	if err != nil {
		return err
	}
	slog.SetDefault(telemetry.NewLogger(os.Stdout, "worker", cfg.PlatformDatabaseURL, cfg.DatabaseEncryptionKey))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Error("telemetry_failed", "actor", "system", "status", "failed", "error", err)
	}))
	tracerProvider, err := telemetry.NewTracerProvider(os.Stderr, "worker", cfg.PlatformDatabaseURL, cfg.DatabaseEncryptionKey)
	if err != nil {
		return err
	}
	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := tracerProvider.Shutdown(shutdownCtx); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("shutdown tracing: %w", err))
		}
	}()

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelStartup()
	db, err := platformdb.Open(startupCtx, cfg.PlatformDatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	runner, queue, err := assemble(startupCtx, db, cfg.DatabaseEncryptionKey)
	if err != nil {
		return err
	}
	consumer, err := worker.New(queue, runner, worker.Config{
		ID: workerCfg.ID, Concurrency: workerCfg.Concurrency, PollInterval: workerCfg.PollInterval,
		Lease: workerCfg.Lease, DrainTimeout: cfg.ShutdownTimeout,
		Retry: worker.RetryPolicy{MaxAttempts: workerCfg.MaxAttempts, BaseDelay: time.Second, MaxDelay: time.Minute},
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("worker_starting", "actor", "system", "status", "running", "worker_id", workerCfg.ID, "concurrency", workerCfg.Concurrency)
	return consumer.Run(ctx)
}

// assemble builds the executor on the same migration service as the server so
// every attempt is revalidated against current permissions and approvals. The
// server initializes the access-control schema.
func assemble(ctx context.Context, db *sql.DB, encryptionKey string) (*worker.Executor, *job.Repository, error) {
	registry, err := database.NewRepository(db, encryptionKey)
	if err != nil {
		return nil, nil, err
	}
	if err := registry.Initialize(ctx); err != nil {
		return nil, nil, err
	}
	definitions, err := migration.NewRepository(db)
	if err != nil {
		return nil, nil, err
	}
	if err := definitions.Initialize(ctx); err != nil {
		return nil, nil, err
	}
	queue, err := job.NewRepository(db)
	if err != nil {
		return nil, nil, err
	}
	if err := queue.Initialize(ctx); err != nil {
		return nil, nil, err
	}
	connector, err := drivers.NewConnector()
	if err != nil {
		return nil, nil, err
	}
	migrations, openTarget := app.NewMigrationService(db, registry, connector, definitions, nil)
	executor, err := worker.NewExecutor(openTarget, worker.MigrationPreparer(migrations))
	if err != nil {
		return nil, nil, err
	}
	return executor, queue, nil
}
