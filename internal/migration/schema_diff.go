package migration

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/wafhr/IF3110_K01-13/internal/database"
)

// DesiredSchema is a deliberately small, reviewable schema description. Types
// are validated before being interpolated into generated SQL.
type DesiredSchema struct {
	Tables []DesiredTable `json:"tables"`
}
type DesiredTable struct {
	Schema  string          `json:"schema"`
	Name    string          `json:"name"`
	Columns []DesiredColumn `json:"columns"`
}
type DesiredColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
}
type SchemaChange struct {
	Kind        string `json:"kind"`
	Object      string `json:"object"`
	Detail      string `json:"detail"`
	Destructive bool   `json:"destructive"`
}
type SchemaDiff struct {
	Changes []SchemaChange `json:"changes"`
	SQL     string         `json:"sql"`
	Notes   []string       `json:"notes"`
}

var safeSQLType = regexp.MustCompile(`(?i)^(smallint|int|integer|bigint|tinyint|mediumint|serial|bigserial|real|float|double( precision)?|boolean|bool|date|time|time( with| without)? time zone|timestamp|timestamp( with| without)? time zone|datetime|year|char(acter)?( varying)?\s*\(\s*[0-9]+\s*\)|varchar\s*\(\s*[0-9]+\s*\)|character varying\s*\(\s*[0-9]+\s*\)|numeric\s*\(\s*[0-9]+(\s*,\s*[0-9]+)?\s*\)|decimal\s*\(\s*[0-9]+(\s*,\s*[0-9]+)?\s*\)|text|tinytext|mediumtext|longtext|uuid|json|jsonb|bytea|blob|tinyblob|mediumblob|longblob)(\s+unsigned)?$`)

func BuildSchemaDiff(dbms database.DBMS, current []database.TableDetails, desired DesiredSchema) (SchemaDiff, error) {
	if len(desired.Tables) == 0 || len(desired.Tables) > 200 {
		return SchemaDiff{}, fmt.Errorf("desired schema must contain 1 to 200 tables")
	}
	currentByName := make(map[string]database.TableDetails, len(current))
	for _, table := range current {
		currentByName[table.Schema+"\x00"+table.Name] = table
	}
	changes, statements := []SchemaChange{}, []string{}
	desiredTables := map[string]bool{}
	for _, table := range desired.Tables {
		if !validIdentifier(table.Schema, 63) || !validIdentifier(table.Name, 64) || len(table.Columns) == 0 || len(table.Columns) > 200 {
			return SchemaDiff{}, fmt.Errorf("invalid table or empty columns")
		}
		key := table.Schema + "\x00" + table.Name
		if desiredTables[key] {
			return SchemaDiff{}, fmt.Errorf("duplicate desired table")
		}
		desiredTables[key] = true
		columns := map[string]bool{}
		for _, col := range table.Columns {
			if !validIdentifier(col.Name, 64) || !safeSQLType.MatchString(strings.TrimSpace(col.Type)) {
				return SchemaDiff{}, fmt.Errorf("invalid column name or type")
			}
			if columns[col.Name] {
				return SchemaDiff{}, fmt.Errorf("duplicate desired column")
			}
			columns[col.Name] = true
		}
		actual, exists := currentByName[key]
		if !exists {
			defs := make([]string, 0, len(table.Columns))
			for _, col := range table.Columns {
				defs = append(defs, quoteIdent(dbms, col.Name)+" "+strings.TrimSpace(col.Type)+nullableSQL(col.Nullable))
			}
			changes = append(changes, SchemaChange{Kind: "create_table", Object: table.Schema + "." + table.Name, Detail: "Table is missing"})
			statements = append(statements, "CREATE TABLE "+qualifiedIdent(dbms, table.Schema, table.Name)+" ("+strings.Join(defs, ", ")+");")
			continue
		}
		actualCols := map[string]database.Column{}
		for _, col := range actual.Columns {
			actualCols[col.Name] = col
		}
		for _, col := range table.Columns {
			found, ok := actualCols[col.Name]
			object := table.Schema + "." + table.Name + "." + col.Name
			if !ok {
				changes = append(changes, SchemaChange{Kind: "add_column", Object: object, Detail: "Column is missing"})
				statements = append(statements, "ALTER TABLE "+qualifiedIdent(dbms, table.Schema, table.Name)+" ADD COLUMN "+quoteIdent(dbms, col.Name)+" "+strings.TrimSpace(col.Type)+nullableSQL(col.Nullable)+";")
			} else if !strings.EqualFold(strings.TrimSpace(found.DataType), strings.TrimSpace(col.Type)) || found.Nullable != col.Nullable {
				changes = append(changes, SchemaChange{Kind: "column_mismatch", Object: object, Detail: fmt.Sprintf("Current: %s %s; desired: %s %s", found.DataType, nullableLabel(found.Nullable), strings.TrimSpace(col.Type), nullableLabel(col.Nullable))})
			}
		}
		for _, col := range actual.Columns {
			if !columns[col.Name] {
				changes = append(changes, SchemaChange{Kind: "extra_column", Object: table.Schema + "." + table.Name + "." + col.Name, Detail: "Current column is omitted from desired definition; review manually", Destructive: true})
			}
		}
	}
	for key, table := range currentByName {
		if !desiredTables[key] {
			changes = append(changes, SchemaChange{Kind: "extra_table", Object: table.Schema + "." + table.Name, Detail: "Current table is omitted from desired definition; review manually", Destructive: true})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Object == changes[j].Object {
			return changes[i].Kind < changes[j].Kind
		}
		return changes[i].Object < changes[j].Object
	})
	if len(statements) == 0 {
		statements = append(statements, "-- No safe automatic SQL was generated. Review reported differences manually.")
	}
	return SchemaDiff{Changes: changes, SQL: strings.Join(statements, "\n"), Notes: []string{"Generated SQL only creates missing tables and adds missing columns.", "Type or nullability changes and objects omitted from the desired schema are reported for manual review; destructive SQL is never generated automatically."}}, nil
}

func parseDesiredSchema(raw json.RawMessage) (DesiredSchema, error) {
	var desired DesiredSchema
	err := json.Unmarshal(raw, &desired)
	return desired, err
}
func validIdentifier(s string, max int) bool {
	return s != "" && utf8.ValidString(s) && utf8.RuneCountInString(s) <= max && !strings.ContainsRune(s, '\x00')
}
func quoteIdent(dbms database.DBMS, s string) string {
	q := "\""
	if dbms == database.MySQL {
		q = "`"
	}
	return q + strings.ReplaceAll(s, q, q+q) + q
}
func qualifiedIdent(dbms database.DBMS, schema, table string) string {
	return quoteIdent(dbms, schema) + "." + quoteIdent(dbms, table)
}
func nullableSQL(nullable bool) string {
	if nullable {
		return ""
	}
	return " NOT NULL"
}
func nullableLabel(nullable bool) string {
	if nullable {
		return "NULL"
	}
	return "NOT NULL"
}
