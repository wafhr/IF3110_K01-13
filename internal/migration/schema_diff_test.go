package migration

import (
	"strings"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/database"
)

func currentTable(schema, name string, columns ...database.Column) database.TableDetails {
	return database.TableDetails{Table: database.Table{Schema: schema, Name: name, Type: "table"}, Columns: columns}
}

func TestBuildSchemaDiffGeneratesOnlyAdditiveSQL(t *testing.T) {
	current := []database.TableDetails{
		currentTable("public", "orders",
			database.Column{Name: "id", DataType: "bigint", Nullable: false},
			database.Column{Name: "total", DataType: "numeric(10,2)", Nullable: true},
			database.Column{Name: "legacy_code", DataType: "text", Nullable: true}),
		currentTable("public", "audit_log", database.Column{Name: "id", DataType: "bigint"}),
		currentTable("public", "platform_migration_executions", database.Column{Name: "id", DataType: "uuid"}),
		currentTable("public", "platform_migrations", database.Column{Name: "version", DataType: "bigint"}),
	}
	desired := DesiredSchema{Tables: []DesiredTable{
		{Schema: "public", Name: "orders", Columns: []DesiredColumn{
			{Name: "id", Type: "bigint"},
			{Name: "total", Type: "numeric(12,2)"},
			{Name: "note", Type: "varchar(200)", Nullable: true},
		}},
		{Schema: "public", Name: "suppliers", Columns: []DesiredColumn{{Name: "id", Type: "bigint"}, {Name: "title", Type: "varchar(200)"}}},
	}}
	diff, err := BuildSchemaDiff(database.PostgreSQL, current, desired)
	if err != nil {
		t.Fatal(err)
	}
	wantSQL := `ALTER TABLE "public"."orders" ADD COLUMN "note" varchar(200);` + "\n" +
		`CREATE TABLE "public"."suppliers" ("id" bigint NOT NULL, "title" varchar(200) NOT NULL);`
	if !sameStatements(diff.SQL, wantSQL) {
		t.Fatalf("SQL = %q", diff.SQL)
	}
	kinds := map[string]SchemaChange{}
	for _, change := range diff.Changes {
		kinds[change.Kind+" "+change.Object] = change
	}
	for _, want := range []string{"add_column public.orders.note", "create_table public.suppliers", "column_mismatch public.orders.total",
		"extra_column public.orders.legacy_code", "extra_table public.audit_log"} {
		if _, ok := kinds[want]; !ok {
			t.Errorf("missing change %q in %+v", want, diff.Changes)
		}
	}
	if !kinds["extra_table public.audit_log"].Destructive || strings.Contains(diff.SQL, "DROP") || strings.Contains(diff.SQL, "ALTER COLUMN") {
		t.Fatal("destructive changes must be reported, never generated")
	}
	for key := range kinds {
		if strings.Contains(key, "platform_migration") {
			t.Fatalf("platform history table reported as a schema change: %q", key)
		}
	}
}

func sameStatements(got, want string) bool {
	normalize := func(value string) []string {
		lines := strings.Split(strings.TrimSpace(value), "\n")
		for i := range lines {
			lines[i] = strings.TrimSpace(lines[i])
		}
		return lines
	}
	a, b := normalize(got), normalize(want)
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, line := range a {
		seen[line]++
	}
	for _, line := range b {
		if seen[line] == 0 {
			return false
		}
		seen[line]--
	}
	return true
}

func TestBuildSchemaDiffQuotesMySQLIdentifiers(t *testing.T) {
	diff, err := BuildSchemaDiff(database.MySQL, nil, DesiredSchema{Tables: []DesiredTable{
		{Schema: "shop", Name: "products", Columns: []DesiredColumn{{Name: "id", Type: "int unsigned"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.SQL, "CREATE TABLE `shop`.`products` (`id` int unsigned NOT NULL);") {
		t.Fatalf("SQL = %q", diff.SQL)
	}
}

func TestBuildSchemaDiffRejectsUnsafeInput(t *testing.T) {
	column := []DesiredColumn{{Name: "id", Type: "bigint"}}
	tests := map[string]DesiredSchema{
		"no tables":         {},
		"injected type":     {Tables: []DesiredTable{{Schema: "public", Name: "t", Columns: []DesiredColumn{{Name: "id", Type: "int); DROP TABLE users; --"}}}}},
		"empty table name":  {Tables: []DesiredTable{{Schema: "public", Name: "", Columns: column}}},
		"empty column name": {Tables: []DesiredTable{{Schema: "public", Name: "t", Columns: []DesiredColumn{{Name: "", Type: "int"}}}}},
		"no columns":        {Tables: []DesiredTable{{Schema: "public", Name: "t"}}},
		"duplicate table":   {Tables: []DesiredTable{{Schema: "public", Name: "t", Columns: column}, {Schema: "public", Name: "t", Columns: column}}},
		"duplicate columns": {Tables: []DesiredTable{{Schema: "public", Name: "t", Columns: append(column, column...)}}},
	}
	for name, desired := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildSchemaDiff(database.PostgreSQL, nil, desired); err == nil {
				t.Fatal("unsafe desired schema was accepted")
			}
		})
	}
}

func TestBuildSchemaDiffEscapesQuotedIdentifiers(t *testing.T) {
	diff, err := BuildSchemaDiff(database.PostgreSQL, nil, DesiredSchema{Tables: []DesiredTable{
		{Schema: "public", Name: `odd"; DROP TABLE users; --`, Columns: []DesiredColumn{{Name: "id", Type: "bigint"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.SQL, `CREATE TABLE "public"."odd""; DROP TABLE users; --"`) {
		t.Fatalf("identifier was not escaped: %q", diff.SQL)
	}
}
