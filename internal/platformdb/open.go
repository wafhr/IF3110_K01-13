package platformdb

import (
	"context"
	"database/sql"
	"errors"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func Open(ctx context.Context, connectionURL string) (*sql.DB, error) {
	if connectionURL == "" {
		return nil, errors.New("PLATFORM_DATABASE_URL is required")
	}
	db, err := sql.Open("pgx", connectionURL)
	if err != nil {
		return nil, errors.New("configure platform database connection")
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, errors.New("connect to platform database")
	}
	return db, nil
}
