package postgres

import (
	"context"
	"database/sql"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

// Data reads one bounded page in a read-only snapshot. Callers must authorize
// View Only access first; the target PostgreSQL role must also have SELECT access.
func (i *Inspector) Data(ctx context.Context, id, schema, name string, options database.PageOptions) (database.DataPage, error) {
	if !validObjectName(schema) || !validObjectName(name) {
		return database.DataPage{}, database.ErrInvalidInput
	}
	options, err := database.NormalizePageOptions(options)
	if err != nil {
		return database.DataPage{}, err
	}
	pool, err := i.pools.Get(ctx, id)
	if err != nil {
		return database.DataPage{}, targetdb.ConnectionError(err)
	}
	tx, err := pool.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return database.DataPage{}, inspectionError(err)
	}
	defer tx.Rollback()
	relation, oid, err := readRelation(ctx, tx, schema, name)
	if err != nil {
		return database.DataPage{}, inspectionError(err)
	}
	columns, err := readColumns(ctx, tx, oid)
	if err != nil {
		return database.DataPage{}, inspectionError(err)
	}
	keys, err := readPrimaryKey(ctx, tx, oid)
	if err != nil {
		return database.DataPage{}, inspectionError(err)
	}
	page := database.DataPage{Schema: schema, Table: name, Limit: options.Limit, Offset: options.Offset,
		Columns: make([]database.DataColumn, 0, len(columns)), Rows: make([][]*string, 0)}
	for _, column := range columns {
		page.Columns = append(page.Columns, database.DataColumn{Name: column.Name, DataType: column.DataType})
	}
	query := pageQuery(relation, columns, keys)
	// A look-ahead row detects the next page without an expensive COUNT(*).
	rows, err := tx.QueryContext(ctx, query, options.Limit+1, options.Offset)
	if err != nil {
		return database.DataPage{}, inspectionError(err)
	}
	defer rows.Close()
	scanCount := max(1, len(columns)) // Zero-column tables use one discarded dummy value.
	values := make([]sql.NullString, scanCount)
	destinations := make([]any, scanCount)
	for index := range values {
		destinations[index] = &values[index]
	}
	for rows.Next() {
		if len(page.Rows) == options.Limit {
			page.HasMore = true
			break
		}
		if err := rows.Scan(destinations...); err != nil {
			return database.DataPage{}, inspectionError(err)
		}
		row := make([]*string, len(columns))
		for index := range columns {
			if values[index].Valid {
				value := values[index].String
				row[index] = &value
			}
		}
		page.Rows = append(page.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return database.DataPage{}, inspectionError(err)
	}
	// Release the result set before committing the transaction on the same connection.
	if err := rows.Close(); err != nil {
		return database.DataPage{}, inspectionError(err)
	}
	if err := tx.Commit(); err != nil {
		return database.DataPage{}, inspectionError(err)
	}
	return page, nil
}

func readPrimaryKey(ctx context.Context, tx *sql.Tx, oid uint32) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT a.attname FROM pg_catalog.pg_constraint con
		CROSS JOIN LATERAL pg_catalog.unnest(con.conkey) WITH ORDINALITY AS key_column(attnum, ordinal)
		JOIN pg_catalog.pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = key_column.attnum
		WHERE con.conrelid = $1 AND con.contype = 'p' ORDER BY key_column.ordinal`, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make([]string, 0)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func pageQuery(table database.Table, columns []database.Column, keys []string) string {
	projection := make([]string, 0, len(columns))
	for _, column := range columns {
		projection = append(projection, pgx.Identifier{"_browse", column.Name}.Sanitize()+"::pg_catalog.text")
	}
	if len(projection) == 0 {
		projection = append(projection, "NULL::pg_catalog.text")
	}
	order := make([]string, 0, len(keys))
	for _, key := range keys {
		order = append(order, pgx.Identifier{"_browse", key}.Sanitize())
	}
	if len(order) == 0 {
		switch table.Type {
		case "table", "partitioned_table", "materialized_view":
			order = append(order, `"_browse".tableoid`, `"_browse".ctid`)
		default:
			order = append(order, `pg_catalog.row_to_json("_browse")::pg_catalog.text COLLATE pg_catalog."C"`)
		}
	}
	return "SELECT " + strings.Join(projection, ", ") + " FROM " + pgx.Identifier{table.Schema, table.Name}.Sanitize() +
		` AS "_browse" ORDER BY ` + strings.Join(order, ", ") + " LIMIT $1 OFFSET $2"
}
