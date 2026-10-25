package mysql

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

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
	table, err := readTable(ctx, tx, schema, name)
	if err != nil {
		return database.DataPage{}, inspectionError(err)
	}
	columns, err := readColumns(ctx, tx, schema, name)
	if err != nil {
		return database.DataPage{}, inspectionError(err)
	}
	keys, err := readPrimaryKey(ctx, tx, schema, name)
	if err != nil {
		return database.DataPage{}, inspectionError(err)
	}
	page := database.DataPage{Schema: schema, Table: name, Limit: options.Limit, Offset: options.Offset,
		Columns: make([]database.DataColumn, 0, len(columns)), Rows: make([][]*string, 0)}
	for _, column := range columns {
		page.Columns = append(page.Columns, database.DataColumn{Name: column.Name, DataType: column.DataType})
	}
	query := pageQuery(table, columns, keys)
	rows, err := tx.QueryContext(ctx, query, options.Limit+1, options.Offset)
	if err != nil {
		return database.DataPage{}, inspectionError(err)
	}
	defer rows.Close()
	values := make([]sql.NullString, max(1, len(columns)))
	destinations := make([]any, len(values))
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
	if err := rows.Close(); err != nil {
		return database.DataPage{}, inspectionError(err)
	}
	if err := tx.Commit(); err != nil {
		return database.DataPage{}, inspectionError(err)
	}
	return page, nil
}

func readPrimaryKey(ctx context.Context, tx interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, schema, name string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_SCHEMA = ? AND TABLE_NAME = ? AND CONSTRAINT_NAME = 'PRIMARY'
		ORDER BY ORDINAL_POSITION`, schema, name)
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
		identifier := quoteIdentifier(column.Name)
		typeName := strings.ToLower(column.DataType)
		if fields := strings.Fields(typeName); len(fields) > 0 {
			typeName = fields[0]
		}
		if strings.Contains(typeName, "blob") || typeName == "binary" || typeName == "varbinary" || typeName == "bit" {
			projection = append(projection, "CONCAT('0x', HEX("+identifier+"))")
		} else if strings.Contains(" geometry point linestring polygon multipoint multilinestring multipolygon geometrycollection ", " "+typeName+" ") {
			projection = append(projection, "ST_AsText("+identifier+")")
		} else {
			projection = append(projection, "CAST("+identifier+" AS CHAR)")
		}
	}
	if len(projection) == 0 {
		projection = append(projection, "NULL")
	}
	order := make([]string, 0, len(keys))
	for _, key := range keys {
		order = append(order, quoteIdentifier(key))
	}
	if len(order) == 0 {
		for index := range projection {
			order = append(order, strconv.Itoa(index+1))
		}
	}
	return "SELECT " + strings.Join(projection, ", ") + " FROM " + quoteIdentifier(table.Schema) + "." + quoteIdentifier(table.Name) +
		" ORDER BY " + strings.Join(order, ", ") + " LIMIT ? OFFSET ?"
}

func quoteIdentifier(value string) string { return "`" + strings.ReplaceAll(value, "`", "``") + "`" }
