package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/app"
	"github.com/wafhr/IF3110_K01-13/internal/config"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	databasehttp "github.com/wafhr/IF3110_K01-13/internal/database/http"
	"github.com/wafhr/IF3110_K01-13/internal/platformdb"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb/postgres"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
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
	targetConnector, err := postgres.NewConnector(postgres.Options{})
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	accessHandler := access.NewHTTPHandler(accessService, cfg.SecureCookie)
	databaseHandler := databasehttp.NewHandler(databaseRegistry, targetConnections, accessHandler)
	viewerHandler := databasehttp.NewViewerHandler(databaseRegistry, postgres.NewInspector(targetConnections), accessHandler)
	server := &http.Server{
		Addr:              cfg.Address(),
		Handler:           app.RoutesWithDatabaseViewer(accessHandler, databaseHandler, viewerHandler),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	slog.Info("starting HTTP server", "address", server.Addr)
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		stop() // A second interrupt uses the default signal behavior.
	}
	slog.Info("shutting down HTTP server")
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
