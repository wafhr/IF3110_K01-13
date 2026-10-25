// Package mysql connects to MySQL target databases.
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strconv"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/wafhr/IF3110_K01-13/internal/database"
)

type Connector struct{}

func NewConnector() *Connector { return &Connector{} }

func (c *Connector) Open(ctx context.Context, settings database.ConnectionConfig) (*sql.DB, error) {
	settings, err := database.NormalizeConnectionConfig(settings)
	if err != nil || settings.DBMS != database.MySQL {
		return nil, database.ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config, err := c.connectionConfig(settings)
	if err != nil {
		return nil, err
	}
	connector, err := drivermysql.NewConnector(config)
	if err != nil {
		return nil, database.ErrInvalidInput
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, connectionError(err)
	}
	return db, nil
}

func (c *Connector) connectionConfig(settings database.ConnectionConfig) (*drivermysql.Config, error) {
	settings, err := database.NormalizeConnectionConfig(settings)
	if err != nil || settings.DBMS != database.MySQL {
		return nil, database.ErrInvalidInput
	}
	tlsMode := map[string]string{"disable": "false", "prefer": "preferred", "require": "true"}[settings.SSLMode]
	if tlsMode == "" {
		return nil, errors.New("unsupported MySQL TLS mode")
	}
	config := drivermysql.NewConfig()
	config.User = settings.Username
	config.Passwd = settings.Password
	config.Net = "tcp"
	config.Addr = net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port))
	config.DBName = settings.DatabaseName
	config.Timeout = 5 * time.Second
	config.ReadTimeout = 10 * time.Second
	config.WriteTimeout = 10 * time.Second
	config.TLSConfig = tlsMode
	config.ParseTime = true
	config.Loc = time.UTC
	config.MultiStatements = true
	return config, nil
}
