package mysql

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

type PoolProvider interface {
	Get(context.Context, string) (*sql.DB, error)
}

type Inspector struct{ pools PoolProvider }

func NewInspector(pools PoolProvider) *Inspector { return &Inspector{pools: pools} }

func (i *Inspector) Schemas(ctx context.Context, id string) ([]database.Schema, error) {
	pool, err := i.pools.Get(ctx, id)
	if err != nil {
		return nil, targetdb.ConnectionError(err)
	}
	rows, err := pool.QueryContext(ctx, `SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = DATABASE()`)
	if err != nil {
		return nil, inspectionError(err)
	}
	defer rows.Close()
	schemas := make([]database.Schema, 0, 1)
	for rows.Next() {
		var schema database.Schema
		if err := rows.Scan(&schema.Name); err != nil {
			return nil, inspectionError(err)
		}
		schemas = append(schemas, schema)
	}
	if err := rows.Err(); err != nil {
		return nil, inspectionError(err)
	}
	return schemas, nil
}

func (i *Inspector) Tables(ctx context.Context, id, schema string) ([]database.Table, error) {
	if !validObjectName(schema) {
		return nil, database.ErrInvalidInput
	}
	pool, err := i.pools.Get(ctx, id)
	if err != nil {
		return nil, targetdb.ConnectionError(err)
	}
	rows, err := pool.QueryContext(ctx, `SELECT TABLE_SCHEMA, TABLE_NAME, CASE TABLE_TYPE WHEN 'VIEW' THEN 'view' ELSE 'table' END
		FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_SCHEMA = ?
		AND TABLE_TYPE IN ('BASE TABLE', 'VIEW') ORDER BY TABLE_NAME`, schema)
	if err != nil {
		return nil, inspectionError(err)
	}
	defer rows.Close()
	tables := make([]database.Table, 0)
	for rows.Next() {
		var table database.Table
		if err := rows.Scan(&table.Schema, &table.Name, &table.Type); err != nil {
			return nil, inspectionError(err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, inspectionError(err)
	}
	return tables, nil
}

func (i *Inspector) Table(ctx context.Context, id, schema, name string) (database.TableDetails, error) {
	if !validObjectName(schema) || !validObjectName(name) {
		return database.TableDetails{}, database.ErrInvalidInput
	}
	pool, err := i.pools.Get(ctx, id)
	if err != nil {
		return database.TableDetails{}, targetdb.ConnectionError(err)
	}
	tx, err := pool.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return database.TableDetails{}, inspectionError(err)
	}
	defer tx.Rollback()
	table, err := readTable(ctx, tx, schema, name)
	if err != nil {
		return database.TableDetails{}, inspectionError(err)
	}
	detail := database.TableDetails{Table: table}
	if detail.Columns, err = readColumns(ctx, tx, schema, name); err != nil {
		return database.TableDetails{}, inspectionError(err)
	}
	if detail.Constraints, err = readConstraints(ctx, tx, schema, name); err != nil {
		return database.TableDetails{}, inspectionError(err)
	}
	if detail.Indexes, err = readIndexes(ctx, tx, schema, name); err != nil {
		return database.TableDetails{}, inspectionError(err)
	}
	if err := tx.Commit(); err != nil {
		return database.TableDetails{}, inspectionError(err)
	}
	return detail, nil
}

func readTable(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, schema, name string) (database.Table, error) {
	table := database.Table{Schema: schema, Name: name}
	var kind string
	err := q.QueryRowContext(ctx, `SELECT TABLE_TYPE FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_SCHEMA = ? AND TABLE_NAME = ?`, schema, name).Scan(&kind)
	if err != nil {
		return database.Table{}, err
	}
	if kind == "VIEW" {
		table.Type = "view"
	} else {
		table.Type = "table"
	}
	return table, nil
}

func readColumns(ctx context.Context, tx interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, schema, name string) ([]database.Column, error) {
	rows, err := tx.QueryContext(ctx, `SELECT COLUMN_NAME, ORDINAL_POSITION, COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT,
		EXTRA, GENERATION_EXPRESSION FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`, schema, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make([]database.Column, 0)
	for rows.Next() {
		var column database.Column
		var nullable string
		var defaultValue, generation sql.NullString
		var extra sql.NullString
		if err := rows.Scan(&column.Name, &column.Position, &column.DataType, &nullable, &defaultValue, &extra, &generation); err != nil {
			return nil, err
		}
		column.Nullable = nullable == "YES"
		if defaultValue.Valid {
			column.Default = &defaultValue.String
		}
		if strings.Contains(strings.ToLower(extra.String), "auto_increment") {
			column.Identity = "by_default"
		}
		if strings.Contains(strings.ToLower(extra.String), "virtual generated") {
			column.Generated = "virtual"
		} else if strings.Contains(strings.ToLower(extra.String), "stored generated") {
			column.Generated = "stored"
		}
		if generation.Valid && generation.String != "" {
			column.GenerationExpression = &generation.String
			column.Default = nil
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func readConstraints(ctx context.Context, tx interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, schema, name string) ([]database.Constraint, error) {
	rows, err := tx.QueryContext(ctx, `SELECT tc.CONSTRAINT_NAME, tc.CONSTRAINT_TYPE,
		COALESCE(MAX(cc.CHECK_CLAUSE), CASE WHEN tc.CONSTRAINT_TYPE = 'FOREIGN KEY'
		THEN CONCAT('FOREIGN KEY (', GROUP_CONCAT(kcu.COLUMN_NAME ORDER BY kcu.ORDINAL_POSITION), ') REFERENCES ',
		MAX(kcu.REFERENCED_TABLE_SCHEMA), '.', MAX(kcu.REFERENCED_TABLE_NAME), ' (',
		GROUP_CONCAT(kcu.REFERENCED_COLUMN_NAME ORDER BY kcu.ORDINAL_POSITION), ')')
		ELSE CONCAT(tc.CONSTRAINT_TYPE, ' (', GROUP_CONCAT(kcu.COLUMN_NAME ORDER BY kcu.ORDINAL_POSITION), ')') END)
		FROM information_schema.TABLE_CONSTRAINTS tc
		LEFT JOIN information_schema.KEY_COLUMN_USAGE kcu ON kcu.CONSTRAINT_SCHEMA = tc.CONSTRAINT_SCHEMA
		AND kcu.TABLE_NAME = tc.TABLE_NAME AND kcu.CONSTRAINT_NAME = tc.CONSTRAINT_NAME
		LEFT JOIN information_schema.CHECK_CONSTRAINTS cc ON cc.CONSTRAINT_SCHEMA = tc.CONSTRAINT_SCHEMA
		AND cc.CONSTRAINT_NAME = tc.CONSTRAINT_NAME
		WHERE tc.TABLE_SCHEMA = DATABASE() AND tc.TABLE_SCHEMA = ? AND tc.TABLE_NAME = ?
		GROUP BY tc.CONSTRAINT_NAME, tc.CONSTRAINT_TYPE ORDER BY tc.CONSTRAINT_NAME`, schema, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	constraints := make([]database.Constraint, 0)
	for rows.Next() {
		var item database.Constraint
		if err := rows.Scan(&item.Name, &item.Type, &item.Definition); err != nil {
			return nil, err
		}
		switch item.Type {
		case "PRIMARY KEY":
			item.Type = "primary_key"
		case "FOREIGN KEY":
			item.Type = "foreign_key"
		case "UNIQUE":
			item.Type = "unique"
		case "CHECK":
			item.Type = "check"
		}
		item.Validated = true
		constraints = append(constraints, item)
	}
	return constraints, rows.Err()
}

func readIndexes(ctx context.Context, tx interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, schema, name string) ([]database.Index, error) {
	rows, err := tx.QueryContext(ctx, `SELECT INDEX_NAME,
		CONCAT(INDEX_TYPE, ' (', GROUP_CONCAT(COALESCE(EXPRESSION, COLUMN_NAME) ORDER BY SEQ_IN_INDEX), ')'),
		MIN(NON_UNIQUE) = 0, INDEX_NAME = 'PRIMARY'
		FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_SCHEMA = ? AND TABLE_NAME = ?
		GROUP BY INDEX_NAME, INDEX_TYPE ORDER BY INDEX_NAME`, schema, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	indexes := make([]database.Index, 0)
	for rows.Next() {
		var item database.Index
		if err := rows.Scan(&item.Name, &item.Definition, &item.Unique, &item.Primary); err != nil {
			return nil, err
		}
		item.Valid = true
		indexes = append(indexes, item)
	}
	return indexes, rows.Err()
}

func validObjectName(name string) bool {
	return name != "" && utf8.ValidString(name) && utf8.RuneCountInString(name) <= 64 && !strings.ContainsRune(name, '\x00')
}

func inspectionError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return database.ErrObjectNotFound
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) && state.SQLState() == "42S02" {
		return database.ErrObjectNotFound
	}
	return connectionError(err)
}

func connectionError(err error) error {
	var mysqlError *drivermysql.MySQLError
	if errors.As(err, &mysqlError) {
		switch mysqlError.Number {
		case 1044, 1142, 1143, 1227:
			return targetdb.ErrPermissionDenied
		}
	}
	return targetdb.ConnectionError(err)
}
