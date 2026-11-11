package platformdb

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

func Open(ctx context.Context, connectionURL string) (*sql.DB, error) {
	if connectionURL == "" {
		return nil, errors.New("PLATFORM_DATABASE_URL is required")
	}
	config, err := pgx.ParseConfig(connectionURL)
	if err != nil {
		return nil, errors.New("configure platform database connection")
	}
	db := sql.OpenDB(telemetry.SQLConnector(stdlib.GetConnector(*config), "postgresql", "platform", config.Database))
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, errors.New("connect to platform database")
	}
	return db, nil
}
