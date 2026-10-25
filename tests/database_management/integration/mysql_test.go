//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb/mysql"
)

func TestMySQLTargetIntegration(t *testing.T) {
	f := newFixture(t)
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Fatal("TEST_MYSQL_DSN is required; see docs/database-management.md#integration-test")
	}
	dsnConfig, err := drivermysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("TEST_MYSQL_DSN is not a valid MySQL connection string")
	}
	host, portText, err := net.SplitHostPort(dsnConfig.Addr)
	if err != nil {
		t.Fatal("TEST_MYSQL_DSN must use a TCP address")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal("TEST_MYSQL_DSN must include a numeric TCP port")
	}
	settings := database.ConnectionConfig{DBMS: database.MySQL, Host: host, Port: port,
		DatabaseName: dsnConfig.DBName, Username: dsnConfig.User, Password: dsnConfig.Passwd, SSLMode: "disable"}
	connector := mysql.NewConnector()
	db, err := connector.Open(context.Background(), settings)
	if err != nil {
		t.Fatalf("connect to disposable MySQL target: %v", err)
	}
	var table, migrationTable string
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if table != "" && migrationTable != "" {
			_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS `"+table+"`, `"+migrationTable+"`, `platform_migrations`")
		}
		_ = db.Close()
	})
	suffixBytes := make([]byte, 5)
	if _, err := rand.Read(suffixBytes); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(suffixBytes)
	table = "if3110_it_data_" + suffix
	migrationTable = "if3110_it_migration_" + suffix
	ddl := "CREATE TABLE `" + table + "` (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, amount DECIMAL(30,10) NOT NULL, note TEXT NULL, UNIQUE KEY amount_unique (amount)) ENGINE=InnoDB"
	execFixture(t, db, ddl)
	execFixture(t, db, "INSERT INTO `"+table+"` (amount,note) VALUES ('12345678901234567890.1234567890',NULL),('2.0000000000','second')")

	admin := f.login(t, "admin", adminPassword)
	var registered struct {
		Database database.Database `json:"database"`
	}
	decode(t, admin.do(t, "POST", "/admin/databases", registration("MySQL target", settings), 201), &registered)
	var current struct {
		User access.User `json:"user"`
	}
	decode(t, admin.do(t, "GET", "/auth/me", nil, 200), &current)
	var group struct {
		Group access.Group `json:"group"`
	}
	decode(t, admin.do(t, "POST", "/admin/groups", map[string]string{"name": "mysql integration"}, 201), &group)
	admin.do(t, "PUT", "/admin/groups/"+group.Group.ID+"/members/"+current.User.ID, nil, 204)
	admin.do(t, "PUT", "/admin/groups/"+group.Group.ID+"/databases/"+registered.Database.ID,
		map[string]string{"permission": "view_edit"}, 204)

	var accessible struct {
		Databases []database.AccessibleDatabase `json:"databases"`
	}
	decode(t, admin.do(t, "GET", "/databases", nil, 200), &accessible)
	if len(accessible.Databases) != 1 || accessible.Databases[0].DBMS != database.MySQL {
		t.Fatalf("MySQL target missing from accessible list: %+v", accessible.Databases)
	}
	databaseID := registered.Database.ID
	var schemas struct {
		Schemas []database.Schema `json:"schemas"`
	}
	decode(t, admin.do(t, "GET", "/databases/"+databaseID+"/schemas", nil, 200), &schemas)
	if len(schemas.Schemas) != 1 || schemas.Schemas[0].Name != settings.DatabaseName {
		t.Fatalf("unexpected MySQL schemas: %+v", schemas.Schemas)
	}
	path := tableURL(databaseID, settings.DatabaseName, table)
	var tables struct {
		Tables []database.Table `json:"tables"`
	}
	decode(t, admin.do(t, "GET", "/databases/"+databaseID+"/schemas/"+settings.DatabaseName+"/tables", nil, 200), &tables)
	found := false
	for _, item := range tables.Tables {
		found = found || item.Name == table
	}
	if !found {
		t.Fatalf("MySQL table missing from viewer: %+v", tables.Tables)
	}
	var details struct {
		Table database.TableDetails `json:"table"`
	}
	decode(t, admin.do(t, "GET", path, nil, 200), &details)
	if details.Table.Type != "table" || len(details.Table.Columns) != 3 || len(details.Table.Constraints) == 0 || len(details.Table.Indexes) < 2 {
		t.Fatalf("incomplete MySQL table metadata: %+v", details.Table)
	}
	var page struct {
		Data database.DataPage `json:"data"`
	}
	decode(t, admin.do(t, "GET", path+"/data?limit=1", nil, 200), &page)
	if len(page.Data.Rows) != 1 || !page.Data.HasMore || page.Data.Rows[0][1] == nil || *page.Data.Rows[0][1] != "12345678901234567890.1234567890" || page.Data.Rows[0][2] != nil {
		t.Fatalf("MySQL data lost precision, pagination, or NULL semantics: %+v", page.Data)
	}

	migrationPath := "/databases/" + databaseID + "/migrations"
	planEndpoint := migrationPath + "/explain"
	assertCount(t, db, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='platform_migrations'", 0)
	for _, analyze := range []bool{false, true} {
		var response struct {
			QueryPlan struct {
				Mode       string          `json:"mode"`
				Plan       json.RawMessage `json:"plan"`
				RolledBack bool            `json:"rolled_back"`
			} `json:"query_plan"`
		}
		mode := "explain"
		if analyze {
			mode = "explain_analyze"
		}
		planSQL := "SELECT amount FROM `" + table + "` WHERE id = 1"
		if analyze {
			planSQL = "UPDATE `" + table + "` AS target JOIN `" + table + "` AS source ON source.id = target.id SET target.note = 'preview-only' WHERE target.id = 1"
		}
		body := admin.do(t, "POST", planEndpoint, map[string]any{
			"sql": planSQL, "analyze": analyze,
		}, 200)
		decode(t, body, &response)
		plan := response.QueryPlan
		if plan.Mode != mode || !plan.RolledBack || !json.Valid(plan.Plan) {
			t.Fatalf("unexpected MySQL %s response: %+v", mode, plan)
		}
		if analyze && (len(plan.Plan) == 0 || plan.Plan[0] != '"') {
			t.Fatalf("MySQL EXPLAIN ANALYZE should return its native tree text: %s", plan.Plan)
		}
		if !analyze && (len(plan.Plan) == 0 || plan.Plan[0] != '{') {
			t.Fatalf("MySQL EXPLAIN should return its native JSON tree: %s", plan.Plan)
		}
		if analyze {
			assertCount(t, db, "SELECT COUNT(*) FROM `"+table+"` WHERE id = 1 AND note IS NULL", 1)
		}
	}
	admin.do(t, "POST", planEndpoint, map[string]any{"sql": "SELECT 1; SELECT 2"}, 400)
	assertCount(t, db, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='platform_migrations'", 0)
	rejectedSQL := "INSERT INTO `" + table + "` (amount,note) VALUES (3,'marker; COMMIT'); /* runner owns the transaction */ COMMIT; SELECT * FROM `missing_integration_table`"
	admin.do(t, "POST", migrationPath, map[string]string{"name": "reject transaction control", "sql": rejectedSQL}, 400)
	assertCount(t, db, "SELECT COUNT(*) FROM `"+table+"`", 2)
	autocommitSQL := "INSERT INTO `" + table + "` (amount,note) VALUES (4,'autocommit'); SET @migration_flag = 1, autocommit = 1; SELECT * FROM `missing_integration_table`"
	admin.do(t, "POST", migrationPath, map[string]string{"name": "reject autocommit change", "sql": autocommitSQL}, 400)
	assertCount(t, db, "SELECT COUNT(*) FROM `"+table+"`", 2)

	createdSQL := "CREATE TABLE `" + migrationTable + "` (id INTEGER PRIMARY KEY, note VARCHAR(80) DEFAULT 'literal; COMMIT') ENGINE=InnoDB"
	var result struct {
		Migration migration.Record `json:"migration"`
	}
	decode(t, admin.do(t, "POST", migrationPath, map[string]string{"name": "create mysql target table", "sql": createdSQL}, 201), &result)
	if result.Migration.Status != migration.Success || result.Migration.Version != 1 || result.Migration.ExecutedBy != current.User.ID {
		t.Fatalf("unexpected MySQL migration result: %+v", result.Migration)
	}
	var viewerUser struct {
		User access.User `json:"user"`
	}
	decode(t, admin.do(t, "POST", "/admin/users", map[string]string{"username": "mysql_viewer", "password": memberPassword}, 201), &viewerUser)
	var viewerGroup struct {
		Group access.Group `json:"group"`
	}
	decode(t, admin.do(t, "POST", "/admin/groups", map[string]string{"name": "mysql viewer"}, 201), &viewerGroup)
	admin.do(t, "PUT", "/admin/groups/"+viewerGroup.Group.ID+"/members/"+viewerUser.User.ID, nil, 204)
	admin.do(t, "PUT", "/admin/groups/"+viewerGroup.Group.ID+"/databases/"+databaseID,
		map[string]string{"permission": "view_only"}, 204)
	viewer := f.login(t, "mysql_viewer", memberPassword)
	viewer.do(t, "GET", "/databases/"+databaseID+"/schemas", nil, 200)
	viewer.do(t, "POST", migrationPath, map[string]string{"name": "must be denied", "sql": "COMMIT"}, 403)
	viewer.do(t, "POST", planEndpoint, map[string]any{"sql": "SELECT 1"}, 403)

	failedSQL := "INSERT INTO `" + table + "` (amount) VALUES (3); SELECT * FROM `missing_integration_table`"
	decode(t, admin.do(t, "POST", migrationPath, map[string]string{"name": "rollback mysql insert", "sql": failedSQL}, 422), &result)
	if result.Migration.Status != migration.Failed || result.Migration.Version != 2 {
		t.Fatalf("unexpected failed MySQL migration: %+v", result.Migration)
	}
	var history struct {
		Migrations []migration.Record `json:"migrations"`
	}
	decode(t, admin.do(t, "GET", migrationPath, nil, 200), &history)
	if len(history.Migrations) != 2 || history.Migrations[0].Status != migration.Failed || history.Migrations[1].Status != migration.Success {
		t.Fatalf("unexpected MySQL migration history: %+v", history.Migrations)
	}
	var detail struct {
		Migration migration.Record `json:"migration"`
	}
	decode(t, admin.do(t, "GET", migrationPath+"/1", nil, 200), &detail)
	if detail.Migration.SQL != createdSQL || detail.Migration.ExecutedBy != current.User.ID {
		t.Fatalf("unexpected MySQL migration detail: %+v", detail.Migration)
	}
	assertCount(t, db, "SELECT COUNT(*) FROM `"+table+"`", 2)
}
