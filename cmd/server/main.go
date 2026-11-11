package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/app"
	"github.com/wafhr/IF3110_K01-13/internal/config"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	databasehttp "github.com/wafhr/IF3110_K01-13/internal/database/http"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/platformdb"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb/drivers"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
	"github.com/wafhr/IF3110_K01-13/web"
)

func main() {
	slog.SetDefault(telemetry.NewLogger(os.Stdout, "server"))
	if err := run(); err != nil {
		slog.Error("server_stopped", "actor", "system", "status", "failed", "error", err)
		os.Exit(1)
	}
	slog.Info("server_stopped", "actor", "system", "status", "success")
}

func run() (runErr error) {
	if err := config.LoadDotEnv(); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	slog.SetDefault(telemetry.NewLogger(os.Stdout, "server",
		cfg.PlatformDatabaseURL, cfg.DatabaseEncryptionKey, cfg.BootstrapAdminPassword))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Error("telemetry_failed", "actor", "system", "status", "failed", "error", err)
	}))
	tracerProvider, err := telemetry.NewTracerProvider(os.Stderr, "server",
		cfg.PlatformDatabaseURL, cfg.DatabaseEncryptionKey, cfg.BootstrapAdminPassword)
	if err != nil {
		return err
	}
	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer func() {
		// Context shutdown terpisah agar pembatalan request tidak membatalkan flush.
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
	databaseRegistry, err := database.NewRepository(db, cfg.DatabaseEncryptionKey)
	if err != nil {
		return err
	}
	if err := databaseRegistry.Initialize(startupCtx); err != nil {
		return err
	}
	targetConnector, err := drivers.NewConnector()
	if err != nil {
		return err
	}
	targetConnections, err := targetdb.NewManager(databaseRegistry, targetConnector)
	if err != nil {
		return err
	}
	defer targetConnections.Close()
	accessService := access.NewService(db)
	if err := accessService.Initialize(startupCtx, cfg.BootstrapAdminUsername, cfg.BootstrapAdminPassword); err != nil {
		return err
	}
	migrationRepository, err := migration.NewRepository(db)
	if err != nil {
		return err
	}
	if err := migrationRepository.Initialize(startupCtx); err != nil {
		return err
	}
	migrationService := migration.NewServiceWithExecution(func(ctx context.Context, id string) (*sql.DB, database.DBMS, error) {
		connection, err := databaseRegistry.GetConnectionConfig(ctx, id)
		if err != nil {
			return nil, "", err
		}
		db, err := targetConnector.Open(ctx, connection)
		return db, connection.DBMS, err
	}, migrationRepository, migration.ExecutionDependencies{Authorizer: accessService})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	accessHandler := access.NewHTTPHandler(accessService, cfg.SecureCookie)
	databaseHandler := databasehttp.NewHandler(databaseRegistry, targetConnections, accessHandler)
	viewerHandler := databasehttp.NewViewerHandler(databaseRegistry, drivers.NewInspector(databaseRegistry, targetConnections), accessHandler)
	schemaInspector := drivers.NewInspector(databaseRegistry, targetConnections)
	migrationHandler := migration.NewHTTPHandler(migrationService, accessHandler, schemaInspector)
	server := &http.Server{
		Addr:              cfg.Address(),
		Handler:           telemetry.HTTPHandler(app.RoutesWithModulesAndFrontend(accessHandler, databaseHandler, viewerHandler, migrationHandler, web.NewHandler(accessService, accessHandler))),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	slog.Info("server_starting", "actor", "system", "status", "running", "address", server.Addr)
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		stop() // Interupsi kedua mengikuti perilaku sinyal bawaan.
	}
	slog.Info("server_stopping", "actor", "system", "status", "running")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		return fmt.Errorf("shutdown HTTP server: %w", err)
	}
	if err := <-result; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP: %w", err)
	}
	return nil
}
