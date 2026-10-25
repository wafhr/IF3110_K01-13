// Package postgres opens PostgreSQL target connections using registered settings.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

type Options struct {
	ConnectTimeout  time.Duration
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

type Connector struct {
	options Options
	openDB  func(pgx.ConnConfig) *sql.DB
}

// NewConnector uses defaults for zero-valued options. The timeout covers the
// initial connection and ping; caller deadlines take precedence when earlier.
func NewConnector(options Options) (*Connector, error) {
	if options.ConnectTimeout < 0 || options.MaxOpenConns < 0 || options.MaxIdleConns < 0 ||
		options.ConnMaxLifetime < 0 || options.ConnMaxIdleTime < 0 {
		return nil, errors.New("target connection options must not be negative")
	}
	if options.ConnectTimeout == 0 {
		options.ConnectTimeout = 5 * time.Second
	}
	if options.MaxOpenConns == 0 {
		options.MaxOpenConns = 5
	}
	if options.MaxIdleConns == 0 {
		options.MaxIdleConns = min(2, options.MaxOpenConns)
	}
	if options.ConnMaxLifetime == 0 {
		options.ConnMaxLifetime = 30 * time.Minute
	}
	if options.ConnMaxIdleTime == 0 {
		options.ConnMaxIdleTime = 5 * time.Minute
	}
	if options.MaxIdleConns > options.MaxOpenConns {
		return nil, errors.New("target idle connections must not exceed maximum open connections")
	}
	return &Connector{options: options, openDB: func(config pgx.ConnConfig) *sql.DB { return stdlib.OpenDB(config) }}, nil
}

func (c *Connector) Open(ctx context.Context, settings database.ConnectionConfig) (*sql.DB, error) {
	ctx, cancel := context.WithTimeout(ctx, c.options.ConnectTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config, err := c.connectionConfig(settings)
	if err != nil {
		return nil, err
	}
	db := c.openDB(*config)
	db.SetMaxOpenConns(c.options.MaxOpenConns)
	db.SetMaxIdleConns(c.options.MaxIdleConns)
	db.SetConnMaxLifetime(c.options.ConnMaxLifetime)
	db.SetConnMaxIdleTime(c.options.ConnMaxIdleTime)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, targetdb.ConnectionError(err)
	}
	return db, nil
}

func (c *Connector) connectionConfig(settings database.ConnectionConfig) (*pgx.ConnConfig, error) {
	settings, err := database.NormalizeConnectionConfig(settings)
	if err != nil {
		return nil, err
	}
	if settings.DBMS != database.PostgreSQL {
		return nil, database.ErrInvalidInput
	}
	// Encode individual values rather than accepting or concatenating raw DSNs.
	values := url.Values{
		"host": {settings.Host}, "port": {strconv.Itoa(settings.Port)},
		"dbname": {settings.DatabaseName}, "user": {settings.Username}, "password": {settings.Password},
		"sslmode": {settings.SSLMode}, "connect_timeout": {"5"},
		"sslcert": {""}, "sslkey": {""}, "sslrootcert": {""}, "passfile": {""},
		"target_session_attrs": {"any"}, "application_name": {"if3110_database_manager"},
	}
	config, err := pgx.ParseConfig("postgres:///?" + values.Encode())
	if err != nil {
		// Parse errors may include credentials or file paths.
		return nil, database.ErrInvalidInput
	}
	config.Password = settings.Password // Never inherit a password from a local pgpass file.
	config.ConnectTimeout = c.options.ConnectTimeout
	config.DialFunc = (&net.Dialer{Timeout: c.options.ConnectTimeout}).DialContext
	return config, nil
}
