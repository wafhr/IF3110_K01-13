//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/database"
)

func TestDatabaseManagementIntegration(t *testing.T) {
	f := newFixture(t)
	admin := f.login(t, "admin", adminPassword)
	var registered, hidden struct {
		Database database.Database `json:"database"`
	}
	decode(t, admin.do(t, "POST", "/admin/databases", registration("Main target", f.settings), 201), &registered)
	decode(t, admin.do(t, "POST", "/admin/databases", registration("Hidden target", f.settings), 201), &hidden)
	id := registered.Database.ID
	if id == "" || hidden.Database.ID == "" {
		t.Fatal("registration did not return IDs")
	}
	var user struct {
		User access.User `json:"user"`
	}
	decode(t, admin.do(t, "POST", "/admin/users", map[string]any{"username": "member", "password": memberPassword}, 201), &user)
	admin.do(t, "POST", "/admin/users", map[string]any{"username": "outsider", "password": memberPassword}, 201)
	var viewerUser struct {
		User access.User `json:"user"`
	}
	decode(t, admin.do(t, "POST", "/admin/users", map[string]any{"username": "viewer", "password": memberPassword}, 201), &viewerUser)
	var readerGroupID string
	for _, permission := range []string{"view_only", "view_edit"} {
		var group struct {
			Group access.Group `json:"group"`
		}
		decode(t, admin.do(t, "POST", "/admin/groups", map[string]string{"name": permission}, 201), &group)
		admin.do(t, "PUT", "/admin/groups/"+group.Group.ID+"/members/"+user.User.ID, nil, 204)
		admin.do(t, "PUT", "/admin/groups/"+group.Group.ID+"/databases/"+id, map[string]string{"permission": permission}, 204)
		if permission == "view_only" {
			readerGroupID = group.Group.ID
		}
	}
	admin.do(t, "PUT", "/admin/groups/"+readerGroupID+"/members/"+viewerUser.User.ID, nil, 204)
	member := f.login(t, "member", memberPassword)
	outsider := f.login(t, "outsider", memberPassword)
	viewer := f.login(t, "viewer", memberPassword)
	path := tableURL(id, quotedSchema, quotedTable)
	readPage := func(t *testing.T, endpoint string) database.DataPage {
		t.Helper()
		var result struct {
			Data database.DataPage `json:"data"`
		}
		decode(t, member.do(t, "GET", endpoint, nil, 200), &result)
		return result.Data
	}

	t.Run("admin registry and encrypted credentials", func(t *testing.T) {
		admin.do(t, "GET", "/admin/databases/"+id, nil, 200)
		var list struct {
			Databases []database.Database `json:"databases"`
		}
		decode(t, admin.do(t, "GET", "/admin/databases", nil, 200), &list)
		if len(list.Databases) != 2 {
			t.Fatalf("registered count = %d, want 2", len(list.Databases))
		}
		var ciphertext []byte
		if err := f.platform.QueryRow(`SELECT password_ciphertext FROM managed_databases WHERE id=$1`, id).Scan(&ciphertext); err != nil {
			t.Fatal(err)
		}
		if len(ciphertext) == 0 || bytes.Contains(ciphertext, []byte(targetPassword)) {
			t.Fatal("registry did not encrypt target credentials")
		}
		admin.do(t, "POST", "/admin/databases", registration("Main target", f.settings), 409)
		admin.do(t, "POST", "/admin/databases", registration("No CSRF", f.settings), 403, func(r *http.Request) { r.Header.Del("X-CSRF-Token") })
		member.do(t, "GET", "/admin/databases", nil, 403)
	})

	t.Run("reject grant to unregistered database", func(t *testing.T) {
		admin.do(t, "PUT", "/admin/groups/"+readerGroupID+"/databases/00000000-0000-4000-8000-000000000001", map[string]string{"permission": "view_only"}, 404)
		assertCount(t, f.platform, `SELECT count(*) FROM access_group_database_permissions WHERE database_id=$1`, 0, "00000000-0000-4000-8000-000000000001")
	})

	t.Run("group derived access and admin boundary", func(t *testing.T) {
		var list struct {
			Databases []database.AccessibleDatabase `json:"databases"`
		}
		decode(t, member.do(t, "GET", "/databases", nil, 200), &list)
		if len(list.Databases) != 1 || list.Databases[0].ID != id || list.Databases[0].Permission != "view_edit" {
			t.Fatalf("unexpected accessible list: %+v", list.Databases)
		}
		decode(t, viewer.do(t, "GET", "/databases", nil, 200), &list)
		if len(list.Databases) != 1 || list.Databases[0].Permission != "view_only" {
			t.Fatalf("unexpected read-only access: %+v", list.Databases)
		}
		viewer.do(t, "GET", path+"/data", nil, 200)
		for _, client := range []*apiClient{admin, outsider} {
			client.do(t, "GET", path, nil, 403)
		}
		member.do(t, "GET", "/databases/"+hidden.Database.ID+"/schemas", nil, 403)
		f.anonymous().do(t, "GET", path, nil, 401)
		decode(t, outsider.do(t, "GET", "/databases", nil, 200), &list)
		if len(list.Databases) != 0 {
			t.Fatal("outsider can see target registrations")
		}
	})

	t.Run("catalog inspection with quoted identifiers", func(t *testing.T) {
		var schemas struct {
			Schemas []database.Schema `json:"schemas"`
		}
		decode(t, member.do(t, "GET", "/databases/"+id+"/schemas", nil, 200), &schemas)
		found := false
		for _, schema := range schemas.Schemas {
			found = found || schema.Name == quotedSchema
		}
		if !found {
			t.Fatal("quoted schema missing")
		}
		var tables struct {
			Tables []database.Table `json:"tables"`
		}
		decode(t, member.do(t, "GET", "/databases/"+id+"/schemas/"+url.PathEscape(quotedSchema)+"/tables", nil, 200), &tables)
		if len(tables.Tables) != 1 || tables.Tables[0].Name != quotedTable {
			t.Fatalf("unexpected tables: %+v", tables.Tables)
		}
		var details struct {
			Table database.TableDetails `json:"table"`
		}
		decode(t, member.do(t, "GET", path, nil, 200), &details)
		columns := map[string]database.Column{}
		for _, column := range details.Table.Columns {
			columns[column.Name] = column
		}
		if len(columns) != 9 || columns["id"].Identity != "by_default" || columns["amount"].DataType != "numeric(30,10)" || columns["amount"].Default == nil || columns["amount"].Nullable || !columns["note"].Nullable || columns["doubled"].Generated != "stored" || columns["doubled"].GenerationExpression == nil {
			t.Fatalf("incorrect catalog columns: %+v", details.Table.Columns)
		}
		constraints := map[string]string{}
		for _, constraint := range details.Table.Constraints {
			constraints[constraint.Name] = constraint.Type
		}
		if constraints["orders_pk"] != "primary_key" || constraints["orders_fk"] != "foreign_key" || constraints["orders_amount_check"] != "check" {
			t.Fatalf("incorrect constraints: %+v", constraints)
		}
		primary, secondary := false, false
		for _, index := range details.Table.Indexes {
			primary = primary || (index.Name == "orders_pk" && index.Primary && index.Unique && index.Valid)
			secondary = secondary || index.Name == "orders_customer_idx"
		}
		if !primary || !secondary {
			t.Fatalf("incorrect indexes: %+v", details.Table.Indexes)
		}
		member.do(t, "GET", tableURL(id, quotedSchema, "missing"), nil, 404)
	})

	t.Run("pagination preserves numeric precision and nulls", func(t *testing.T) {
		first := readPage(t, path+"/data?limit=2")
		if len(first.Rows) != 2 || !first.HasMore || first.Limit != 2 || first.Offset != 0 {
			t.Fatalf("unexpected first page: %+v", first)
		}
		cell := func(row, column int) string {
			if first.Rows[row][column] == nil {
				t.Fatalf("unexpected NULL at %d,%d", row, column)
			}
			return *first.Rows[row][column]
		}
		if cell(0, 0) != "1" || cell(1, 0) != "2" || cell(0, 2) != "12345678901234567890.1234567890" || first.Rows[0][3] != nil || cell(1, 3) != "" || cell(0, 7) != `\x00ff` || cell(0, 8) != "true" {
			t.Fatalf("values lost precision, ordering or NULL semantics: %+v", first.Rows)
		}
		last := readPage(t, path+"/data?limit=2&offset=2")
		if len(last.Rows) != 1 || last.HasMore || last.Rows[0][0] == nil || *last.Rows[0][0] != "9223372036854775807" {
			t.Fatalf("unexpected last page: %+v", last)
		}
		past := readPage(t, path+"/data?offset=100")
		if past.Rows == nil || len(past.Rows) != 0 || len(past.Columns) != 9 || past.HasMore {
			t.Fatalf("unexpected past-end page: %+v", past)
		}
		member.do(t, "GET", path+"/data?limit=201", nil, 400)
	})

	t.Run("heap views materialized and empty tables", func(t *testing.T) {
		for _, test := range []struct {
			name          string
			rows, columns int
		}{
			{"no_pk", 2, 2}, {"order_view", 3, 2}, {"order_summary", 3, 1}, {"empty_table", 0, 1}, {"zero_columns", 2, 0},
		} {
			t.Run(test.name, func(t *testing.T) {
				page := readPage(t, tableURL(id, "public", test.name)+"/data")
				if len(page.Rows) != test.rows || len(page.Columns) != test.columns || page.Rows == nil || page.Columns == nil {
					t.Fatalf("unexpected page: %+v", page)
				}
				for _, row := range page.Rows {
					if row == nil || len(row) != test.columns {
						t.Fatalf("unexpected row: %+v", row)
					}
				}
			})
		}
	})

	t.Run("target privileges and read only transaction", func(t *testing.T) {
		member.do(t, "GET", tableURL(id, "public", "secret_data")+"/data", nil, 403)
		member.do(t, "GET", tableURL(id, "public", "mutating_view")+"/data", nil, 502)
		assertCount(t, f.target, `SELECT count(*) FROM public.side_effects`, 0)
	})

	t.Run("saved unsaved authentication and unreachable connections", func(t *testing.T) {
		admin.do(t, "POST", "/admin/databases/"+id+"/test-connection", nil, 200)
		input := registration("", f.settings)
		delete(input, "name")
		admin.do(t, "POST", "/admin/databases/test-connection", input, 200)
		input["password"] = "wrong-integration-password"
		admin.do(t, "POST", "/admin/databases/test-connection", input, 502)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		input["host"], input["port"], input["password"] = "127.0.0.1", port, targetPassword
		admin.do(t, "POST", "/admin/databases/test-connection", input, 502)
	})

	t.Run("update invalidates cached target credentials", func(t *testing.T) {
		oldPool, err := f.pools.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		input := registration("Main target", f.settings)
		input["password"] = "wrong-integration-password"
		admin.do(t, "PUT", "/admin/databases/"+id, input, 200)
		if oldPool.Ping() == nil {
			t.Fatal("old pool still open after update")
		}
		member.do(t, "GET", path+"/data", nil, 502)
		admin.do(t, "PUT", "/admin/databases/"+id, registration("Main target", f.settings), 200)
		readPage(t, path+"/data")
	})

	t.Run("canceled query releases target connection", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := f.inspector.Data(ctx, id, "public", "slow_view", database.PageOptions{})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("query cancellation = %v", err)
		}
		readPage(t, path+"/data")
	})

	t.Run("delete rolls back then revokes grants and sessions", func(t *testing.T) {
		execFixture(t, f.platform, `CREATE FUNCTION integration_reject_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'integration rollback probe'; END $$;
CREATE TRIGGER integration_reject_delete BEFORE DELETE ON access_group_database_permissions FOR EACH ROW EXECUTE FUNCTION integration_reject_delete();`)
		admin.do(t, "DELETE", "/admin/databases/"+id, nil, 500)
		admin.do(t, "GET", "/admin/databases/"+id, nil, 200)
		member.do(t, "GET", "/auth/me", nil, 200)
		assertCount(t, f.platform, `SELECT count(*) FROM access_group_database_permissions WHERE database_id=$1`, 2, id)
		execFixture(t, f.platform, `DROP TRIGGER integration_reject_delete ON access_group_database_permissions; DROP FUNCTION integration_reject_delete();`)
		admin.do(t, "DELETE", "/admin/databases/"+id, nil, 204)
		admin.do(t, "GET", "/admin/databases/"+id, nil, 404)
		member.do(t, "GET", "/auth/me", nil, 401)
		viewer.do(t, "GET", "/auth/me", nil, 401)
		assertCount(t, f.platform, `SELECT count(*) FROM access_group_database_permissions WHERE database_id=$1`, 0, id)
		assertCount(t, f.platform, `SELECT count(*) FROM access_sessions WHERE user_id=$1`, 0, user.User.ID)
		assertCount(t, f.target, `SELECT count(*) FROM "Sales / Data"."Order""Items"`, 3)
		admin.do(t, "DELETE", "/admin/databases/"+hidden.Database.ID, nil, 204)
	})
}
