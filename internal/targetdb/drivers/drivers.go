// Package drivers composes the target-specific connectors and schema readers.
package drivers

import (
	"context"
	"database/sql"

	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb/mysql"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb/postgres"
)

type Connector struct {
	postgres *postgres.Connector
	mysql    *mysql.Connector
}

func NewConnector() (*Connector, error) {
	pg, err := postgres.NewConnector(postgres.Options{})
	if err != nil {
		return nil, err
	}
	return &Connector{postgres: pg, mysql: mysql.NewConnector()}, nil
}

func (c *Connector) Open(ctx context.Context, config database.ConnectionConfig) (*sql.DB, error) {
	config, err := database.NormalizeConnectionConfig(config)
	if err != nil {
		return nil, err
	}
	switch config.DBMS {
	case database.PostgreSQL:
		return c.postgres.Open(ctx, config)
	case database.MySQL:
		return c.mysql.Open(ctx, config)
	default:
		return nil, database.ErrInvalidInput
	}
}

type Registry interface {
	GetConnectionConfig(context.Context, string) (database.ConnectionConfig, error)
}

type PoolProvider interface {
	Get(context.Context, string) (*sql.DB, error)
}

type SchemaReader interface {
	Schemas(context.Context, string) ([]database.Schema, error)
	Tables(context.Context, string, string) ([]database.Table, error)
	Table(context.Context, string, string, string) (database.TableDetails, error)
	Data(context.Context, string, string, string, database.PageOptions) (database.DataPage, error)
}

type Inspector struct {
	registry Registry
	readers  map[database.DBMS]SchemaReader
}

func NewInspector(registry Registry, pools PoolProvider) *Inspector {
	return &Inspector{registry: registry, readers: map[database.DBMS]SchemaReader{
		database.PostgreSQL: postgres.NewInspector(pools),
		database.MySQL:      mysql.NewInspector(pools),
	}}
}

func (i *Inspector) reader(ctx context.Context, id string) (SchemaReader, error) {
	config, err := i.registry.GetConnectionConfig(ctx, id)
	if err != nil {
		return nil, targetdb.ConnectionError(err)
	}
	reader := i.readers[config.DBMS]
	if reader == nil {
		return nil, database.ErrInvalidInput
	}
	return reader, nil
}

func (i *Inspector) Schemas(ctx context.Context, id string) ([]database.Schema, error) {
	reader, err := i.reader(ctx, id)
	if err != nil {
		return nil, err
	}
	return reader.Schemas(ctx, id)
}

func (i *Inspector) Tables(ctx context.Context, id, schema string) ([]database.Table, error) {
	reader, err := i.reader(ctx, id)
	if err != nil {
		return nil, err
	}
	return reader.Tables(ctx, id, schema)
}

func (i *Inspector) Table(ctx context.Context, id, schema, table string) (database.TableDetails, error) {
	reader, err := i.reader(ctx, id)
	if err != nil {
		return database.TableDetails{}, err
	}
	return reader.Table(ctx, id, schema, table)
}

func (i *Inspector) Data(ctx context.Context, id, schema, table string, options database.PageOptions) (database.DataPage, error) {
	reader, err := i.reader(ctx, id)
	if err != nil {
		return database.DataPage{}, err
	}
	return reader.Data(ctx, id, schema, table, options)
}
