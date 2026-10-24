package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

type PoolProvider interface {
	Get(context.Context, string) (*sql.DB, error)
}

// Inspector reads target metadata. Its callers must authorize database access
// before invoking any method. It never runs user-provided SQL or closes pools.
type Inspector struct{ pools PoolProvider }

func NewInspector(pools PoolProvider) *Inspector { return &Inspector{pools: pools} }

const visibleSchema = `n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'`
const tableKinds = `c.relkind IN ('r', 'p', 'v', 'm', 'f')`
const tableType = `CASE c.relkind WHEN 'r' THEN 'table' WHEN 'p' THEN 'partitioned_table'
	WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized_view' WHEN 'f' THEN 'foreign_table' END`

func (i *Inspector) Schemas(ctx context.Context, id string) ([]database.Schema, error) {
	pool, err := i.pools.Get(ctx, id)
	if err != nil {
		return nil, targetdb.ConnectionError(err)
	}
	rows, err := pool.QueryContext(ctx, `SELECT n.nspname FROM pg_catalog.pg_namespace n
		WHERE `+visibleSchema+` AND pg_catalog.has_schema_privilege(n.oid, 'USAGE') ORDER BY n.nspname`)
	if err != nil {
		return nil, inspectionError(err)
	}
	defer rows.Close()
	schemas := make([]database.Schema, 0)
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
	var allowed bool
	err = pool.QueryRowContext(ctx, `SELECT pg_catalog.has_schema_privilege(n.oid, 'USAGE')
		FROM pg_catalog.pg_namespace n WHERE n.nspname = $1 AND `+visibleSchema, schema).Scan(&allowed)
	if err != nil {
		return nil, inspectionError(err)
	}
	if !allowed {
		return nil, targetdb.ErrPermissionDenied
	}
	rows, err := pool.QueryContext(ctx, `SELECT n.nspname, c.relname, `+tableType+`
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND `+visibleSchema+` AND `+tableKinds+` ORDER BY c.relname`, schema)
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

// Table reads columns, constraints, and indexes from a single read-only catalog
// snapshot so one response cannot mix metadata from separate schema versions.
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
	detail := database.TableDetails{Table: database.Table{Schema: schema, Name: name}}
	var oid uint32
	var allowed bool
	err = tx.QueryRowContext(ctx, `SELECT c.oid, `+tableType+`, pg_catalog.has_schema_privilege(n.oid, 'USAGE')
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND `+visibleSchema+` AND `+tableKinds, schema, name).
		Scan(&oid, &detail.Type, &allowed)
	if err != nil {
		return database.TableDetails{}, inspectionError(err)
	}
	if !allowed {
		return database.TableDetails{}, targetdb.ErrPermissionDenied
	}
	if detail.Columns, err = readColumns(ctx, tx, oid); err != nil {
		return database.TableDetails{}, inspectionError(err)
	}
	if detail.Constraints, err = readConstraints(ctx, tx, oid); err != nil {
		return database.TableDetails{}, inspectionError(err)
	}
	if detail.Indexes, err = readIndexes(ctx, tx, oid); err != nil {
		return database.TableDetails{}, inspectionError(err)
	}
	if err := tx.Commit(); err != nil {
		return database.TableDetails{}, inspectionError(err)
	}
	return detail, nil
}

func readColumns(ctx context.Context, tx *sql.Tx, oid uint32) ([]database.Column, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT a.attname, a.attnum, pg_catalog.format_type(a.atttypid, a.atttypmod),
		       NOT (a.attnotnull OR (t.typtype = 'd' AND t.typnotnull)),
		       pg_catalog.pg_get_expr(d.adbin, d.adrelid),
		       CASE a.attidentity WHEN 'a' THEN 'always' WHEN 'd' THEN 'by_default' ELSE '' END,
		       CASE a.attgenerated WHEN 's' THEN 'stored' WHEN 'v' THEN 'virtual' ELSE '' END
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_type t ON t.oid = a.atttypid
		LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make([]database.Column, 0)
	for rows.Next() {
		var column database.Column
		var expression sql.NullString
		if err := rows.Scan(&column.Name, &column.Position, &column.DataType, &column.Nullable,
			&expression, &column.Identity, &column.Generated); err != nil {
			return nil, err
		}
		if expression.Valid {
			if column.Generated != "" {
				column.GenerationExpression = &expression.String
			} else {
				column.Default = &expression.String
			}
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func readConstraints(ctx context.Context, tx *sql.Tx, oid uint32) ([]database.Constraint, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT con.conname, CASE con.contype
		       WHEN 'p' THEN 'primary_key' WHEN 'u' THEN 'unique' WHEN 'f' THEN 'foreign_key'
		       WHEN 'c' THEN 'check' WHEN 'x' THEN 'exclusion' WHEN 't' THEN 'constraint_trigger'
		       WHEN 'n' THEN 'not_null' ELSE con.contype::text END,
		       pg_catalog.pg_get_constraintdef(con.oid, true), con.convalidated
		FROM pg_catalog.pg_constraint con WHERE con.conrelid = $1 ORDER BY con.conname`, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	constraints := make([]database.Constraint, 0)
	for rows.Next() {
		var constraint database.Constraint
		if err := rows.Scan(&constraint.Name, &constraint.Type, &constraint.Definition, &constraint.Validated); err != nil {
			return nil, err
		}
		constraints = append(constraints, constraint)
	}
	return constraints, rows.Err()
}

func readIndexes(ctx context.Context, tx *sql.Tx, oid uint32) ([]database.Index, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT c.relname, pg_catalog.pg_get_indexdef(i.indexrelid), i.indisunique, i.indisprimary, i.indisvalid
		FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
		WHERE i.indrelid = $1 ORDER BY c.relname`, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	indexes := make([]database.Index, 0)
	for rows.Next() {
		var index database.Index
		if err := rows.Scan(&index.Name, &index.Definition, &index.Unique, &index.Primary, &index.Valid); err != nil {
			return nil, err
		}
		indexes = append(indexes, index)
	}
	return indexes, rows.Err()
}

func validObjectName(name string) bool {
	// Preserve case, whitespace, quotes, and punctuation in quoted identifiers.
	return name != "" && len(name) <= 63 && utf8.ValidString(name) && !strings.ContainsRune(name, '\x00')
}

func inspectionError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return database.ErrObjectNotFound
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) && (state.SQLState() == "3F000" || state.SQLState() == "42P01") {
		return database.ErrObjectNotFound
	}
	return targetdb.ConnectionError(err)
}
